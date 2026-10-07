#include "ota_download.h"
#include "../../../../common/endpoint_fallback.h"
#include "sdkconfig.h"
#include "nvf_board.h"
#if CONFIG_NVF_OTA
#include <stdlib.h>
#include <string.h>
#include <strings.h>
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
static esp_err_t header(esp_http_client_event_t *event) {
    if(event->event_id==HTTP_EVENT_ON_HEADER && !strcasecmp(event->header_key,"Content-Encoding") &&
       strcasecmp(event->header_value,"identity"))*(bool*)event->user_data=true;
    return ESP_OK;
}

// One attempt is bounded two ways. NVF_OTA_STALL_US without a byte declares the link dead
// (the pusher's OP_DEADLINE_S): the socket's own 10 s timeout delivering nothing is waited
// out until then rather than failing the attempt. NVF_OTA_TRANSFER_US caps the whole
// restart-from-zero transfer however slowly it keeps progressing, so a 4 MiB artifact needs
// about 2.4 KB/s (19 kbit/s) sustained. Time the progress callback spends yielding to
// collection under spool pressure counts against neither (SOFTWARE-UPDATES.md, download).
#define NVF_OTA_STALL_US    (30LL * 1000000)
#define NVF_OTA_TRANSFER_US (30LL * 60 * 1000000)
typedef struct { int64_t stall, transfer; } ota_deadlines;
static void deadlines_start(ota_deadlines *d)
{
    int64_t now = esp_timer_get_time();
    d->stall = now + NVF_OTA_STALL_US;
    d->transfer = now + NVF_OTA_TRANSFER_US;
}
static bool deadlines_passed(const ota_deadlines *d)
{
    int64_t now = esp_timer_get_time();
    return now > d->stall || now > d->transfer;
}
// yield_to_collection runs the progress callback; its back-pressure pause is not transfer time.
static bool yield_to_collection(nvf_ota_progress_fn progress, void *context, size_t received,
                                size_t total, ota_deadlines *d)
{
    if (!progress) return true;
    int64_t started = esp_timer_get_time();
    bool ok = progress(received, total, context);
    int64_t spent = esp_timer_get_time() - started;
    d->stall += spent;
    d->transfer += spent;
    return ok;
}
// read_body reads up to wanted bytes. A socket timeout that delivered nothing is retried
// until the stall deadline. Returns the count, 0 when the connection ended before the body
// did, -1 on a transport error and -2 when a deadline passed.
static int read_body(esp_http_client_handle_t client, uint8_t *out, size_t wanted, ota_deadlines *d)
{
    for (;;) {
        if (deadlines_passed(d)) return -2;
        int got = esp_http_client_read(client, (char *)out, (int)wanted);
        if (got == -ESP_ERR_HTTP_EAGAIN) continue;
        if (got > 0) d->stall = esp_timer_get_time() + NVF_OTA_STALL_US;
        return got < 0 ? -1 : got;
    }
}
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
    bool encoded=false;
    esp_http_client_config_t config = {
        .event_handler=header,.user_data=&encoded,
        .url = url, .crt_bundle_attach = esp_crt_bundle_attach,
        .transport_type = HTTP_TRANSPORT_OVER_SSL, .disable_auto_redirect = true,
        .timeout_ms = 10000, .buffer_size = 4096,
    };
    ota_deadlines deadlines;
    deadlines_start(&deadlines);
    esp_http_client_handle_t client = esp_http_client_init(&config);
    if (!client) return ESP_ERR_NO_MEM;
    esp_http_client_set_header(client,"Accept-Encoding","identity");
    uint8_t *buffer = malloc(4096);
    esp_ota_handle_t handle = 0;
    mbedtls_sha256_context sha; mbedtls_sha256_init(&sha);
    esp_err_t err = buffer ? esp_http_client_open(client, 0) : ESP_ERR_NO_MEM;
    if (err != ESP_OK) { *retry = buffer != NULL; goto done; }
    int64_t length = esp_http_client_fetch_headers(client);
    if (esp_http_client_get_status_code(client) != 200 || length < NVF_OTA_PREFIX_SIZE ||
        length > (int64_t)slot->size || encoded || esp_http_client_is_chunked_response(client)) {
        *retry = true; err = ESP_ERR_INVALID_SIZE; goto done;
    }
    size_t prefix = 0;
    while (prefix < NVF_OTA_PREFIX_SIZE) {
        int n = read_body(client, buffer + prefix, NVF_OTA_PREFIX_SIZE - prefix, &deadlines);
        if (n == -2) { *retry = true; err = ESP_ERR_TIMEOUT; goto done; }
        if (n <= 0) { *retry = true; err = ESP_ERR_INVALID_RESPONSE; goto done; }
        prefix += n;
    }
    if (!nvf_ota_image_compatible(buffer, prefix, CONFIG_IDF_FIRMWARE_CHIP_ID, nvf_board_device_id(),
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
        if (!yield_to_collection(progress, context, received, length, &deadlines)) { err = ESP_ERR_INVALID_STATE; goto done; }
        if (deadlines_passed(&deadlines)) { *retry = true; err = ESP_ERR_TIMEOUT; goto done; }
        if (mbedtls_sha256_update(&sha, buffer, n)) { err = ESP_FAIL; goto done; }
        err = esp_ota_write(handle, buffer, n);
        if (err != ESP_OK) goto done;
        received += n;
        if (received == length) break;
        size_t wanted = length - received > 4096 ? 4096 : (size_t)(length - received);
        int got = read_body(client, buffer, wanted, &deadlines);
        if (got == -2) { *retry = true; err = ESP_ERR_TIMEOUT; goto done; }
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
    if (!yield_to_collection(progress, context, received, length, &deadlines)) { err = ESP_ERR_INVALID_STATE; goto done; }
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
