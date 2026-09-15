#include "ota.h"
#include "ota_policy.h"
#include "ota_download.h"
#include "sdkconfig.h"
#if CONFIG_NVF_OTA
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include "cJSON.h"
#include "esp_app_desc.h"
#include "esp_crt_bundle.h"
#include "esp_http_client.h"
#include "esp_image_format.h"
#include "esp_log.h"
#include "esp_netif_sntp.h"
#include "esp_ota_ops.h"
#include "esp_random.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "mbedtls/md.h"
#include "mbedtls/sha256.h"
#include "nvs.h"
#include "spool.h"

#if !CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE || !CONFIG_MBEDTLS_HAVE_TIME_DATE
#error "OTA requires bootloader rollback and TLS certificate date validation"
#endif
_Static_assert(sizeof(esp_image_header_t) == 24, "OTA image header changed");
_Static_assert(sizeof(esp_image_segment_header_t) == 8, "OTA segment header changed");
_Static_assert(sizeof(esp_app_desc_t) == 256, "OTA application description changed");
_Static_assert(offsetof(esp_app_desc_t, project_name) == 48, "OTA project field moved");
static const char *TAG = "ota";
static uint8_t update_key[32];
static char nonce[65];
static int64_t nonce_deadline;
static atomic_bool confirmed;
// State and error are diagnostics. Only the HTTP task accepts update jobs;
// the worker publishes completion after releasing all update resources.
static atomic_int state, last_error;
enum { IDLE, DOWNLOADING, REBOOTING, FAILED };
static const char *states[] = {"idle", "downloading", "rebooting", "failed"};


static esp_err_t error_response(httpd_req_t *req, const char *status, const char *message)
{
    httpd_resp_set_status(req, status);
    httpd_resp_set_type(req, "text/plain");
    return httpd_resp_sendstr(req, message);
}
static bool receive_body(httpd_req_t *req, char *out, size_t cap)
{
    if (!req->content_len || req->content_len >= cap) return false;
    size_t received = 0;
    int64_t deadline = esp_timer_get_time() + 5000000;
    while (received < req->content_len) {
        int n = httpd_req_recv(req, out + received, req->content_len - received);
        if (n <= 0 || esp_timer_get_time() > deadline) return false;
        received += n;
    }
    out[received] = 0;
    return true;
}
static esp_err_t pairing_post(httpd_req_t *req)
{
    wifi_mode_t mode;
    if (esp_wifi_get_mode(&mode) != ESP_OK || mode != WIFI_MODE_AP)
        return error_response(req, "403 Forbidden", "pairing requires the provisioning AP");
    char origin[80];
    if (httpd_req_get_hdr_value_len(req, "Origin") &&
        (httpd_req_get_hdr_value_str(req, "Origin", origin, sizeof origin) != ESP_OK ||
         strcmp(origin, "http://192.168.4.1")))
        return error_response(req, "403 Forbidden", "cross-origin request rejected");
    char body[65]; uint8_t key[32];
    if (req->content_len != 64 || !receive_body(req, body, sizeof body) ||
        !nvf_ota_unhex(body, 64, key))
        return error_response(req, "400 Bad Request", "expected a 64-character lowercase hex key");
    nvs_handle_t nvs;
    esp_err_t err = nvs_open("nvf_ota", NVS_READWRITE, &nvs);
    if (err == ESP_OK) {
        err = nvs_set_blob(nvs, "key", key, sizeof key);
        if (err == ESP_OK) err = nvs_commit(nvs);
        nvs_close(nvs);
    }
    memset(key, 0, sizeof key); memset(body, 0, sizeof body);
    if (err != ESP_OK) return error_response(req, "500 Internal Server Error", "key storage failed; retry pairing");
    return httpd_resp_sendstr(req, "OTA key paired; complete network provisioning to start remote control\n");
}
esp_err_t nvf_ota_register_pairing(httpd_handle_t server)
{
    httpd_uri_t uri = {.uri = "/ota/pair", .method = HTTP_POST, .handler = pairing_post};
    return httpd_register_uri_handler(server, &uri);
}

static esp_err_t challenge_get(httpd_req_t *req)
{
    if (!nonce[0] || esp_timer_get_time() >= nonce_deadline) {
        uint8_t random[32]; esp_fill_random(random, sizeof random);
        nvf_ota_hex(random, sizeof random, nonce);
        nonce_deadline = esp_timer_get_time() + 60000000;
    }
    cJSON *json = cJSON_CreateObject();
    if (!json) return ESP_ERR_NO_MEM;
    const esp_partition_t *running = esp_ota_get_running_partition();
    cJSON_AddStringToObject(json, "nonce", nonce);
    cJSON_AddStringToObject(json, "state", states[atomic_load(&state)]);
    cJSON_AddStringToObject(json, "error", esp_err_to_name(atomic_load(&last_error)));
    cJSON_AddStringToObject(json, "version", esp_app_get_description()->version);
    cJSON_AddStringToObject(json, "partition", running ? running->label : "unknown");
    cJSON_AddBoolToObject(json, "confirmed", atomic_load(&confirmed));
    char *body = cJSON_PrintUnformatted(json);
    cJSON_Delete(json);
    if (!body) return ESP_ERR_NO_MEM;
    httpd_resp_set_type(req, "application/json");
    httpd_resp_set_hdr(req, "Cache-Control", "no-store");
    esp_err_t err = httpd_resp_sendstr(req, body);
    free(body);
    return err;
}
static bool authenticated(httpd_req_t *req, const char *body)
{
    char provided_nonce[65], signature[65]; uint8_t provided[32], computed[32];
    if (!nonce[0] || esp_timer_get_time() >= nonce_deadline ||
        httpd_req_get_hdr_value_str(req, "X-OTA-Nonce", provided_nonce, sizeof provided_nonce) != ESP_OK ||
        httpd_req_get_hdr_value_str(req, "X-OTA-Authorization", signature, sizeof signature) != ESP_OK ||
        strlen(provided_nonce) != 64 || strcmp(provided_nonce, nonce) || strlen(signature) != 64 ||
        !nvf_ota_unhex(signature, 64, provided)) return false;
    const char prefix[] = "navfeeder-ota-v1\n";
    char message[sizeof(prefix) + 65 + NVF_OTA_BODY_CAP];
    size_t n = sizeof(prefix) - 1;
    memcpy(message, prefix, n); memcpy(message + n, nonce, 64); n += 64;
    message[n++] = '\n'; memcpy(message + n, body, req->content_len); n += req->content_len;
    if (mbedtls_md_hmac(mbedtls_md_info_from_type(MBEDTLS_MD_SHA256), update_key,
                        sizeof update_key, (const unsigned char *)message, n, computed)) return false;
    unsigned diff = 0;
    for (size_t i = 0; i < sizeof computed; i++) diff |= provided[i] ^ computed[i];
    return nvf_ota_consume_nonce(nonce, nonce_deadline, provided_nonce, esp_timer_get_time(), diff == 0);
}

static void update_task(void *arg)
{
    nvf_ota_request_t *request = arg;
    esp_err_t err = nvf_ota_download(request);
    free(request);
    atomic_store(&last_error, err);
    if (err == ESP_OK) {
        atomic_store(&state, REBOOTING);
        size_t depth = 0; spool_stats(NULL, NULL, &depth);
        ESP_LOGW(TAG, "verified update selected; rebooting (%u records remain in volatile spool)", (unsigned)depth);
        vTaskDelay(pdMS_TO_TICKS(1000));
        esp_restart();
    }
    ESP_LOGE(TAG, "update failed: %s; current firmware continues", esp_err_to_name(err));
    atomic_store(&state, FAILED);
    vTaskDelete(NULL);
}
static esp_err_t update_post(httpd_req_t *req)
{
    char body[NVF_OTA_BODY_CAP];
    if (!receive_body(req, body, sizeof body)) return error_response(req, "400 Bad Request", "invalid request body");
    if (!authenticated(req, body)) return error_response(req, "403 Forbidden", "invalid or expired authorization");
    // Authentication consumed the nonce before validation or task creation.
    int current = atomic_load(&state);
    if (!atomic_load(&confirmed) || current == DOWNLOADING || current == REBOOTING)
        return error_response(req, "409 Conflict", "firmware not confirmed or an update is already active");
    nvf_ota_request_t *request = malloc(sizeof *request);
    if (!request) return ESP_ERR_NO_MEM;
    if (!nvf_ota_parse_request(body, req->content_len, request->url, request->hash)) {
        free(request); return error_response(req, "400 Bad Request", "expected SHA-256, newline, HTTPS URL");
    }
    atomic_store(&last_error, ESP_OK); atomic_store(&state, DOWNLOADING);
    if (xTaskCreate(update_task, "ota", 8192, request, 3, NULL) != pdPASS) {
        free(request); atomic_store(&state, FAILED); atomic_store(&last_error, ESP_ERR_NO_MEM);
        return error_response(req, "503 Service Unavailable", "could not start update task");
    }
    return error_response(req, "202 Accepted", "update accepted; GET /ota for status\n");
}
esp_err_t nvf_ota_start(void)
{
    // The provisioning server uses the same port in a separate boot path.
    wifi_mode_t mode;
    if (esp_wifi_get_mode(&mode) != ESP_OK || mode != WIFI_MODE_STA) return ESP_ERR_INVALID_STATE;
    esp_sntp_config_t sntp = ESP_NETIF_SNTP_DEFAULT_CONFIG("pool.ntp.org");
    esp_err_t err = esp_netif_sntp_init(&sntp);
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) return err;
    nvs_handle_t nvs;
    err = nvs_open("nvf_ota", NVS_READONLY, &nvs);
    if (err != ESP_OK) return err;
    size_t len = sizeof update_key;
    err = nvs_get_blob(nvs, "key", update_key, &len); nvs_close(nvs);
    if (err != ESP_OK || len != sizeof update_key) return err == ESP_OK ? ESP_ERR_INVALID_SIZE : err;
    httpd_config_t config = HTTPD_DEFAULT_CONFIG(); config.stack_size = 8192;
    config.recv_wait_timeout = 3; config.send_wait_timeout = 3;
    httpd_handle_t server = NULL;
    err = httpd_start(&server, &config);
    if (err != ESP_OK) return err;
    httpd_uri_t get = {.uri = "/ota", .method = HTTP_GET, .handler = challenge_get};
    httpd_uri_t post = {.uri = "/ota", .method = HTTP_POST, .handler = update_post};
    err = httpd_register_uri_handler(server, &get);
    if (err == ESP_OK) err = httpd_register_uri_handler(server, &post);
    if (err != ESP_OK) httpd_stop(server);
    else ESP_LOGI(TAG, "authenticated OTA control available on port 80");
    return err;
}
esp_err_t nvf_ota_confirm_boot(void)
{
    const esp_partition_t *running = esp_ota_get_running_partition();
    if (!running) return ESP_ERR_INVALID_STATE;
    esp_ota_img_states_t status;
    esp_err_t err = esp_ota_get_state_partition(running, &status);
    if (err == ESP_OK && status == ESP_OTA_IMG_PENDING_VERIFY)
        err = esp_ota_mark_app_valid_cancel_rollback();
    else if (running->subtype == ESP_PARTITION_SUBTYPE_APP_FACTORY) err = ESP_OK;
    else if (err == ESP_OK && status != ESP_OTA_IMG_VALID) err = ESP_ERR_INVALID_STATE;
    if (err == ESP_OK) {
        atomic_store(&confirmed, true);
        ESP_LOGI(TAG, "local startup checks passed; firmware confirmed");
    }
    return err;
}
#else
esp_err_t nvf_ota_register_pairing(httpd_handle_t server) { (void)server; return ESP_OK; }
esp_err_t nvf_ota_start(void) { return ESP_ERR_NOT_SUPPORTED; }
esp_err_t nvf_ota_confirm_boot(void) { return ESP_OK; }
#endif
