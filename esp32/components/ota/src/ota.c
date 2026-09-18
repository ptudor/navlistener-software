#include "ota.h"
#include "ota_policy.h"
#include "ota_download.h"
#include "update_runtime.h"
#include "update_api.h"
#include "sdkconfig.h"
#if CONFIG_NVF_OTA
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <errno.h>
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
#include "lwip/inet.h"
#include "lwip/sockets.h"
#include "mbedtls/md.h"
#include "mbedtls/sha256.h"
#include "nvs.h"
#include "spool.h"
#include "journal.h"
#include "mcu_identity.h"

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
static bool request_on_setup_ap(httpd_req_t *req)
{
    struct sockaddr_storage local = {0};
    socklen_t length = sizeof local;
    int socket = httpd_req_to_sockfd(req);
    if (socket < 0 || getsockname(socket, (struct sockaddr *)&local, &length) < 0 ||
        local.ss_family != AF_INET)
        return false;
    const struct sockaddr_in *address = (const struct sockaddr_in *)&local;
    return address->sin_addr.s_addr == inet_addr("192.168.4.1");
}
static esp_err_t pairing_post(httpd_req_t *req)
{
    wifi_mode_t mode;
    if (esp_wifi_get_mode(&mode) != ESP_OK ||
        (mode != WIFI_MODE_AP && mode != WIFI_MODE_APSTA) ||
        !request_on_setup_ap(req))
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
static bool authenticated(httpd_req_t *req, const char *body, const char *prefix, const char *binding)
{
    char provided_nonce[65], signature[65]; uint8_t provided[32], computed[32];
    if (!nonce[0] || esp_timer_get_time() >= nonce_deadline ||
        httpd_req_get_hdr_value_str(req, "X-OTA-Nonce", provided_nonce, sizeof provided_nonce) != ESP_OK ||
        httpd_req_get_hdr_value_str(req, "X-OTA-Authorization", signature, sizeof signature) != ESP_OK ||
        strlen(provided_nonce) != 64 || strcmp(provided_nonce, nonce) || strlen(signature) != 64 ||
        !nvf_ota_unhex(signature, 64, provided)) return false;
    char message[256 + NVF_OTA_BODY_CAP];
    size_t n = strlen(prefix);
    if (n > 32) return false;
    memcpy(message, prefix, n); memcpy(message + n, nonce, 64); n += 64;
    message[n++] = '\n';
    if(binding){size_t b=strlen(binding);if(b>128)return false;memcpy(message+n,binding,b);n+=b;}
    memcpy(message + n, body, req->content_len); n += req->content_len;
    if (mbedtls_md_hmac(mbedtls_md_info_from_type(MBEDTLS_MD_SHA256), update_key,
                        sizeof update_key, (const unsigned char *)message, n, computed)) return false;
    unsigned diff = 0;
    for (size_t i = 0; i < sizeof computed; i++) diff |= provided[i] ^ computed[i];
    return nvf_ota_consume_nonce(nonce, nonce_deadline, provided_nonce, esp_timer_get_time(), diff == 0);
}

static void json_u64(cJSON *json, const char *name, uint64_t value)
{
    char text[24]; snprintf(text,sizeof text,"%llu",(unsigned long long)value);
    cJSON_AddStringToObject(json,name,text); // exact even above JavaScript's 53-bit integer limit
}
static esp_err_t journal_post(httpd_req_t *req)
{
    char body[48];
    if (!receive_body(req,body,sizeof body)) return error_response(req,"400 Bad Request","invalid cursor");
    if (!authenticated(req,body,"navfeeder-journal-v1\n",NULL))
        return error_response(req,"403 Forbidden","invalid or expired authorization");
    unsigned lane;
    const char *number;
    if (!strncmp(body,"life\n",5)) { lane=0; number=body+5; }
    else if (!strncmp(body,"health\n",7)) { lane=1; number=body+7; }
    else return error_response(req,"400 Bad Request","expected life or health and a cursor");
    if (!*number || strspn(number,"0123456789") != strlen(number))
        return error_response(req,"400 Bad Request","invalid cursor");
    errno=0; uint64_t before=strtoull(number,NULL,10);
    if (errno) return error_response(req,"400 Bad Request","cursor overflow");
    journal_record_t rows[8]; unsigned count; uint64_t next;
    if (journal_page(lane,before,rows,&count,&next) != ESP_OK)
        return error_response(req,"503 Service Unavailable","journal unavailable; GNSS service is independent");
    cJSON *json=cJSON_CreateObject(), *records=cJSON_CreateArray();
    if (!json || !records) { cJSON_Delete(json); cJSON_Delete(records); return ESP_ERR_NO_MEM; }
    cJSON_AddItemToObject(json,"records",records); json_u64(json,"next",next);
    cJSON_AddNumberToObject(json,"capacity",lane ? JOURNAL_HEALTH_CAP : JOURNAL_LIFE_CAP);
    for (unsigned i=0; i<count; i++) {
        const journal_record_t *r=&rows[i]; cJSON *row=cJSON_CreateObject();
        if (!row) { cJSON_Delete(json); return ESP_ERR_NO_MEM; }
        cJSON_AddItemToArray(records,row);
        json_u64(row,"sequence",r->sequence); json_u64(row,"boot",r->boot);
        json_u64(row,"uptime_ms",r->uptime_ms); json_u64(row,"utc",r->utc);
        json_u64(row,"dropped",r->dropped);
        cJSON_AddStringToObject(row,"firmware",r->firmware);
        cJSON_AddStringToObject(row,"partition",r->partition);
        char hash[65]; nvf_ota_hex(r->elf_sha256,32,hash);
        cJSON_AddStringToObject(row,"elf_sha256",hash);
        cJSON_AddNumberToObject(row,"event",r->event); cJSON_AddNumberToObject(row,"time_source",r->time_source);
        cJSON_AddNumberToObject(row,"reset_reason",r->reset_reason);
        cJSON_AddNumberToObject(row,"flags",r->flags); cJSON_AddNumberToObject(row,"queued",r->queued);
        cJSON_AddNumberToObject(row,"internal_free",r->internal_free);
        cJSON_AddNumberToObject(row,"environment",r->environment); cJSON_AddNumberToObject(row,"rtc",r->rtc);
        cJSON_AddNumberToObject(row,"rng",r->rng); cJSON_AddNumberToObject(row,"manifest",r->manifest);
        cJSON_AddNumberToObject(row,"error",r->error);
        cJSON_AddNumberToObject(row,"timing_flags",r->timing_flags);
        cJSON_AddNumberToObject(row,"timing_elapsed_s",r->timing_elapsed_s);
        json_u64(row,"gnss_pulses",r->gnss_pulses); json_u64(row,"rtc_pulses",r->rtc_pulses);
        cJSON_AddNumberToObject(row,"timing_dropped",r->timing_dropped);
        cJSON_AddNumberToObject(row,"timing_hz",r->timing_hz);
        cJSON_AddNumberToObject(row,"rtc_minus_gnss_ticks",r->timing_phase_ticks);
    }
    char *response=cJSON_PrintUnformatted(json); cJSON_Delete(json);
    if (!response) return ESP_ERR_NO_MEM;
    httpd_resp_set_type(req,"application/json"); httpd_resp_set_hdr(req,"Cache-Control","no-store");
    esp_err_t err=httpd_resp_sendstr(req,response); free(response); return err;
}

static void update_task(void *arg)
{
    nvf_ota_request_t *request = arg;
    esp_err_t err = nvf_ota_download(request);
    free(request);
    atomic_store(&last_error, err);
    if (err == ESP_OK) {
        journal_event(JOURNAL_OTA_READY,0);
        atomic_store(&state, REBOOTING);
        size_t depth = 0; spool_stats(NULL, NULL, &depth);
        ESP_LOGW(TAG, "verified update selected; rebooting (%u records remain in volatile spool)", (unsigned)depth);
        vTaskDelay(pdMS_TO_TICKS(1000));
        esp_restart();
    }
    ESP_LOGE(TAG, "update failed: %s; current firmware continues", esp_err_to_name(err));
    journal_event(JOURNAL_OTA_FAILED,err);
    atomic_store(&state, FAILED);
    nvf_update_release();
    vTaskDelete(NULL);
}
static esp_err_t update_post(httpd_req_t *req)
{
    char body[NVF_OTA_BODY_CAP];
    if (!receive_body(req, body, sizeof body)) return error_response(req, "400 Bad Request", "invalid request body");
    if (!authenticated(req, body, "navfeeder-ota-v1\n",NULL)) return error_response(req, "403 Forbidden", "invalid or expired authorization");
    // Authentication consumed the nonce before validation or task creation.
    int current = atomic_load(&state);
    if (!atomic_load(&confirmed) || current == DOWNLOADING || current == REBOOTING || nvf_update_busy())
        return error_response(req, "409 Conflict", "firmware not confirmed or an update is already active");
    nvf_ota_request_t *request = malloc(sizeof *request);
    if (!request) return ESP_ERR_NO_MEM;
    if (!nvf_ota_parse_request(body, req->content_len, request->url, request->hash)) {
        free(request); return error_response(req, "400 Bad Request", "expected SHA-256, newline, HTTPS URL");
    }
    if(!nvf_update_claim()){free(request);return error_response(req,"409 Conflict","another update is active");}
    atomic_store(&last_error, ESP_OK); atomic_store(&state, DOWNLOADING);
    if (xTaskCreate(update_task, "ota", 8192, request, 3, NULL) != pdPASS) {
        free(request); nvf_update_release(); atomic_store(&state, FAILED); atomic_store(&last_error, ESP_ERR_NO_MEM);
        return error_response(req, "503 Service Unavailable", "could not start update task");
    }
    return error_response(req, "202 Accepted", "update accepted; GET /ota for status\n");
}
static const char *const update_modes[]={"invalid","manual","download","install"};
static const char *const update_channels[]={"stable","canary","lab"};
static const char *const update_states[]={"idle","checking","available","downloading","staged","waiting-safe",
    "quiescing","reboot-pending","trial-boot","confirmed","rolled-back","failed"};
static esp_err_t status_v1(httpd_req_t *req) {
    if(req->content_len || strcmp(req->uri,"/ota/v1/status"))return error_response(req,"400 Bad Request","expected an empty status request without a query");
    if(!nonce[0] || esp_timer_get_time()>=nonce_deadline) {
        uint8_t random[32];esp_fill_random(random,sizeof random);nvf_ota_hex(random,sizeof random,nonce);
        nonce_deadline=esp_timer_get_time()+60000000;
    }
    nvf_update_status_t s;nvf_update_status(&s);
    cJSON *json=cJSON_CreateObject();if(!json)return ESP_ERR_NO_MEM;
    cJSON_AddStringToObject(json,"nonce",nonce);
    cJSON_AddStringToObject(json,"mode",s.mode<=UP_INSTALL?update_modes[s.mode]:"invalid");
    cJSON_AddStringToObject(json,"channel",s.channel<3?update_channels[s.channel]:"invalid");
    cJSON_AddStringToObject(json,"state",s.state<=UP_FAILED?update_states[s.state]:"invalid");
    cJSON_AddBoolToObject(json,"confirmed",atomic_load(&confirmed));
    cJSON_AddBoolToObject(json,"busy",nvf_update_busy());
    cJSON_AddStringToObject(json,"version",esp_app_get_description()->version);
    cJSON_AddStringToObject(json,"error",nvf_update_error_name(s.error));
    cJSON_AddNumberToObject(json,"error_domain",s.error/1000);cJSON_AddNumberToObject(json,"error_reason",s.error%1000);
    cJSON_AddNumberToObject(json,"security_flags",s.security);cJSON_AddNumberToObject(json,"partition_layout_id",s.layout);
    cJSON_AddStringToObject(json,"trust_profile",nvf_update_profile_name(nvf_update_profile()));
    // trust_profile is this build's own claim. hardware_trust is what the collector last
    // concluded from the evidence this board presented (docs/COMMISSIONING.md); both are
    // empty until a collector has answered on this boot.
    nvf_mcu_identity_status_t identity;nvf_mcu_identity_status(&identity);
    cJSON_AddStringToObject(json,"hardware_trust",identity.hardware_trust);
    cJSON_AddStringToObject(json,"evidence_error",identity.evidence_error);
    cJSON_AddBoolToObject(json,"commissioning_record",identity.record);
    json_u64(json,"running_release",s.running);json_u64(json,"available_release",s.available.sequence);
    json_u64(json,"staged_release",s.staged.sequence);json_u64(json,"failed_release",s.failed);
    json_u64(json,"channel_generation",s.available.generation);json_u64(json,"last_command",s.last_command);
    json_u64(json,"last_check",s.last_check);json_u64(json,"next_check",s.next_check);json_u64(json,"error_time",s.error_time);
    cJSON_AddNumberToObject(json,"bytes_received",s.received);
    json_u64(json,"artifact_length",s.staged.sequence?s.staged.length:s.available.length);
    cJSON_AddStringToObject(json,"advisory",s.available.advisory);
    cJSON_AddStringToObject(json,"next_action",s.error/1000==5?"Service the update storage before enabling automatic attempts":
        s.error==UP_TRUST_UNCONFIGURED?"Install an explicitly configured trust profile during USB commissioning":
        s.error/1000==6?"Wait for a durable collector connection and an empty observation queue":
        s.error==UP_TRIAL_FAILED?"Inspect the diagnostic journal before trying a newer release":
        s.error?"Check again after the reported retry time":"No action required");
    char *body=cJSON_PrintUnformatted(json);cJSON_Delete(json);if(!body)return ESP_ERR_NO_MEM;
    httpd_resp_set_type(req,"application/json");httpd_resp_set_hdr(req,"Cache-Control","no-store");
    esp_err_t err=httpd_resp_sendstr(req,body);free(body);return err;
}
static esp_err_t mutate_v1(httpd_req_t *req) {
    char content_type[64],body[257],binding[128];
    if(httpd_req_get_hdr_value_str(req,"Content-Type",content_type,sizeof content_type)!=ESP_OK ||
       (strcmp(content_type,"application/json") && strcmp(content_type,"application/json; charset=utf-8")))
        return error_response(req,"415 Unsupported Media Type","expected application/json");
    if(!receive_body(req,body,sizeof body))return error_response(req,"400 Bad Request","invalid JSON body");
    const char *method=req->method==HTTP_PUT?"PUT":"POST";
    nvf_update_api_request_t request;
    if(!nvf_update_api_parse(method,req->uri,body,req->content_len,&request))
        return error_response(req,"400 Bad Request","request does not match the versioned update schema");
    int n=snprintf(binding,sizeof binding,"%s\n%s\n",method,req->uri);
    if(n<0 || n>=(int)sizeof binding || !authenticated(req,body,"navfeeder-ota-api-v1\n",binding))
        return error_response(req,"403 Forbidden","invalid or expired authorization");
    if(!atomic_load(&confirmed))return error_response(req,"409 Conflict","running firmware is not confirmed");
    bool accepted=request.mode?nvf_update_policy(request.mode,request.channel):
        nvf_update_request(request.action,request.release,request.discard);
    if(!accepted)return error_response(req,"409 Conflict","updater unavailable, request ineligible, or queue full; inspect status");
    if(request.discard) {
        uint32_t records;
        if(!nvf_update_discard_result(&records,60000))return error_response(req,"409 Conflict","install was not prepared; inspect status");
        char reply[160];snprintf(reply,sizeof reply,"{\"accepted\":true,\"backlog_records_at_pause\":%lu,\"reboot_pending\":true}",(unsigned long)records);
        httpd_resp_set_type(req,"application/json");httpd_resp_set_hdr(req,"Cache-Control","no-store");
        esp_err_t err=httpd_resp_sendstr(req,reply);nvf_update_discard_response(err==ESP_OK);return err;
    }
    httpd_resp_set_type(req,"application/json");httpd_resp_set_status(req,"202 Accepted");
    httpd_resp_set_hdr(req,"Cache-Control","no-store");return httpd_resp_sendstr(req,"{\"accepted\":true}");
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
    httpd_config_t config = HTTPD_DEFAULT_CONFIG(); config.stack_size = 12288; config.max_uri_handlers = 12;
    config.recv_wait_timeout = 3; config.send_wait_timeout = 3;
    httpd_handle_t server = NULL;
    err = httpd_start(&server, &config);
    if (err != ESP_OK) return err;
    httpd_uri_t get = {.uri = "/ota", .method = HTTP_GET, .handler = challenge_get};
    httpd_uri_t post = {.uri = "/ota", .method = HTTP_POST, .handler = update_post};
    httpd_uri_t journal = {.uri = "/journal", .method = HTTP_POST, .handler = journal_post};
    err = httpd_register_uri_handler(server, &get);
    if (err == ESP_OK) err = httpd_register_uri_handler(server, &post);
    if (err == ESP_OK) err = httpd_register_uri_handler(server, &journal);
    const httpd_uri_t v1[]={
        {.uri="/ota/v1/status",.method=HTTP_GET,.handler=status_v1},
        {.uri="/ota/v1/check",.method=HTTP_POST,.handler=mutate_v1},
        {.uri="/ota/v1/download",.method=HTTP_POST,.handler=mutate_v1},
        {.uri="/ota/v1/install",.method=HTTP_POST,.handler=mutate_v1},
        {.uri="/ota/v1/cancel",.method=HTTP_POST,.handler=mutate_v1},
        {.uri="/ota/v1/policy",.method=HTTP_PUT,.handler=mutate_v1}};
    for(unsigned i=0;err==ESP_OK && i<sizeof v1/sizeof v1[0];i++)err=httpd_register_uri_handler(server,&v1[i]);
    if (err != ESP_OK) httpd_stop(server);
    else ESP_LOGI(TAG, "authenticated OTA control available on port 80");
    return err;
}
esp_err_t nvf_ota_confirm_boot(void)
{
    if(!nvf_update_boot_ready())return ESP_ERR_INVALID_STATE;
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
        nvf_update_confirmed();
        ESP_LOGI(TAG, "local startup checks passed; firmware confirmed");
        journal_event(JOURNAL_CONFIRMED,0);
    }
    return err;
}
#else
esp_err_t nvf_ota_register_pairing(httpd_handle_t server) { (void)server; return ESP_OK; }
esp_err_t nvf_ota_start(void) { return ESP_ERR_NOT_SUPPORTED; }
esp_err_t nvf_ota_confirm_boot(void) { return ESP_OK; }
#endif
