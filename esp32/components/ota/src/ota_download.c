#include "ota_download.h"
#include "../../../../common/endpoint_fallback.h"
#include "sdkconfig.h"
#if CONFIG_NVF_OTA
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include "esp_app_desc.h"
#include "esp_crt_bundle.h"
#include "esp_http_client.h"
#include "esp_ota_ops.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "mbedtls/sha256.h"
#include "spool.h"

// Download only an app image, with fixed Content-Length and no redirects.
// Boot selection is attempted only after verification. ESP-IDF checks the complete
// image again at esp_ota_end; the operator's authenticated SHA-256 binds its bytes.
static esp_err_t download_once(const nvf_ota_request_t *request, const char *url, bool *retry,
                              bool stage_only,nvf_ota_progress_fn progress,void *context)
{
    *retry = false;
    const esp_partition_t *running = esp_ota_get_running_partition();
    const esp_partition_t *slot = esp_ota_get_next_update_partition(NULL);
    if (!slot || slot == running || slot->type != ESP_PARTITION_TYPE_APP ||
        (slot->subtype != ESP_PARTITION_SUBTYPE_APP_OTA_0 && slot->subtype != ESP_PARTITION_SUBTYPE_APP_OTA_1) ||
        esp_ota_get_boot_partition() != running) return ESP_ERR_INVALID_STATE;
    // TLS certificate date checks need a plausible wall clock. SNTP runs in
    // station mode; its availability never controls first-boot confirmation.
    for (int i = 0; time(NULL) < 1704067200 && i < 30; i++) vTaskDelay(pdMS_TO_TICKS(1000));
    if (time(NULL) < 1704067200) return ESP_ERR_TIMEOUT;
    esp_http_client_config_t config = {
        .url = url, .crt_bundle_attach = esp_crt_bundle_attach,
        .transport_type = HTTP_TRANSPORT_OVER_SSL, .disable_auto_redirect = true,
        .timeout_ms = 10000, .buffer_size = 4096,
    };
    int64_t deadline = esp_timer_get_time() + 180000000;
    esp_http_client_handle_t client = esp_http_client_init(&config);
    if (!client) return ESP_ERR_NO_MEM;
    uint8_t *buffer = malloc(4096);
    esp_ota_handle_t handle = 0;
    mbedtls_sha256_context sha; mbedtls_sha256_init(&sha);
    esp_err_t err = buffer ? esp_http_client_open(client, 0) : ESP_ERR_NO_MEM;
    if (err != ESP_OK) { *retry = buffer != NULL; goto done; }
    int64_t length = esp_http_client_fetch_headers(client);
    if (esp_http_client_get_status_code(client) != 200 || length < NVF_OTA_PREFIX_SIZE ||
        length > (int64_t)slot->size || esp_http_client_is_chunked_response(client)) {
        *retry = true; err = ESP_ERR_INVALID_SIZE; goto done;
    }
    size_t prefix = 0;
    while (prefix < NVF_OTA_PREFIX_SIZE) {
        if (esp_timer_get_time() > deadline) { *retry = true; err = ESP_ERR_TIMEOUT; goto done; }
        int n = esp_http_client_read(client, (char *)buffer + prefix, NVF_OTA_PREFIX_SIZE - prefix);
        if (n <= 0) { *retry = true; err = ESP_ERR_INVALID_RESPONSE; goto done; }
        prefix += n;
    }
    if (!nvf_ota_image_compatible(buffer, prefix, CONFIG_IDF_FIRMWARE_CHIP_ID, 1,
                                 esp_app_get_description()->project_name)) {
        *retry = true; err = ESP_ERR_INVALID_VERSION; goto done;
    }
    // Sequential erase bounds flash pauses instead of erasing the entire slot at once.
    err = esp_ota_begin(slot, OTA_WITH_SEQUENTIAL_WRITES, &handle);
    if (err != ESP_OK) goto done;
    if (mbedtls_sha256_starts(&sha, 0)) { err = ESP_FAIL; goto done; }
    int64_t received = 0;
    size_t n = prefix;
    for (;;) {
        if (progress && !progress(received, length, context)) { err = ESP_ERR_INVALID_STATE; goto done; }
        if (esp_timer_get_time() > deadline) { *retry = true; err = ESP_ERR_TIMEOUT; goto done; }
        if (mbedtls_sha256_update(&sha, buffer, n)) { err = ESP_FAIL; goto done; }
        err = esp_ota_write(handle, buffer, n);
        if (err != ESP_OK) goto done;
        received += n;
        if (received == length) break;
        if (esp_timer_get_time() > deadline) { *retry = true; err = ESP_ERR_TIMEOUT; goto done; }
        size_t wanted = length - received > 4096 ? 4096 : (size_t)(length - received);
        int got = esp_http_client_read(client, (char *)buffer, wanted);
        if (got <= 0) { *retry = true; err = ESP_ERR_INVALID_RESPONSE; goto done; }
        n = got;
    }
    uint8_t digest[32];
    if (!esp_http_client_is_complete_data_received(client) ||
        mbedtls_sha256_finish(&sha, digest) || memcmp(digest, request->hash, 32)) {
        *retry = true; err = ESP_ERR_INVALID_CRC; goto done;
    }
    err = esp_ota_end(handle); handle = 0; // end frees its handle even on failure
    if (err != ESP_OK) goto done;
    if (progress && !progress(received, length, context)) { err = ESP_ERR_INVALID_STATE; goto done; }
    if (stage_only) goto done;
    // Allow an advancing collector to drain the pre-reboot backlog. This is
    // bounded best effort: both RAM tiers are volatile and new records continue.
    uint64_t watermark; spool_stats(&watermark, NULL, NULL);
    for (int i = 0; i < 50 && spool_acked() < watermark; i++) vTaskDelay(pdMS_TO_TICKS(100));
    err = esp_ota_set_boot_partition(slot);
done:
    if (handle) esp_ota_abort(handle);
    esp_http_client_cleanup(client);
    mbedtls_sha256_free(&sha); free(buffer);
    return err;
}

static esp_err_t transfer(const nvf_ota_request_t *request,bool stage_only,nvf_ota_progress_fn progress,void *context)
{
    bool retry;
    esp_err_t err = download_once(request, request->url, &retry,stage_only,progress,context);
    char secondary[NVF_OTA_URL_CAP];
    if (err != ESP_OK && retry &&
        nav_endpoint_secondary_url(request->url, secondary, sizeof secondary)) {
        // Restart at byte zero after aborting the old handle. The authorized
        // digest and every image/TLS check also apply to the second origin.
        // Storage and boot-selection failures never initiate another attempt.
        err = download_once(request, secondary, &retry,stage_only,progress,context);
    }
    return err;
}
esp_err_t nvf_ota_download(const nvf_ota_request_t *request) {return transfer(request,false,NULL,NULL);}
esp_err_t nvf_ota_stage(const nvf_ota_request_t *request,nvf_ota_progress_fn progress,void *context) {return transfer(request,true,progress,context);}
#endif
