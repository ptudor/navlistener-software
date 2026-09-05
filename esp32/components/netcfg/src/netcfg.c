// netcfg — NVS config + SoftAP provisioning portal. See include/netcfg.h.

#include "netcfg.h"

#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include "esp_random.h"
#include "bootloader_random.h" // seed esp_random() before Wi-Fi is running

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_wifi.h"
#include "esp_netif.h"
#include "esp_event.h"
#include "esp_mac.h"
#include "esp_random.h"
#include "esp_system.h"
#include "esp_http_server.h"
#include "esp_check.h"
#include "esp_log.h"
#include "sdkconfig.h"

static const char *TAG = "netcfg";

// A Kconfig bool left at 'n' emits no #define.
#ifndef CONFIG_NVF_INSECURE
#define CONFIG_NVF_INSECURE 0
#endif
#ifndef CONFIG_NVF_ALLOW_INSECURE_PORTAL
#define CONFIG_NVF_ALLOW_INSECURE_PORTAL 0
#endif

// --- provisioning portal -----------------------------------------------------------------

// the "Skip TLS verify" checkbox is a permanent, field-settable MITM downgrade —
// anyone who can reach the SoftAP portal during provisioning could flip it. It is compiled
// into the shipped portal only for a dev/bench build (CONFIG_NVF_ALLOW_INSECURE_PORTAL);
// production builds (the default) never expose the control, though the underlying NVS
// field/config struct is unchanged for bench use via NVF_INSECURE or a direct NVS write.
#if CONFIG_NVF_ALLOW_INSECURE_PORTAL
#define PORTAL_INSECURE_FIELD \
    "<label><input type=checkbox name=insecure style='width:auto'> Skip TLS verify (dev only)</label>"
#else
#define PORTAL_INSECURE_FIELD ""
#endif

static const char PORTAL_HTML[] =
    "<!doctype html><meta name=viewport content='width=device-width,initial-scale=1'>"
    "<title>navfeeder-esp setup</title>"
    "<style>body{font-family:sans-serif;max-width:32em;margin:2em auto;padding:0 1em}"
    "label{display:block;margin:.8em 0 .2em}input{width:100%;padding:.5em;box-sizing:border-box}"
    "button{margin-top:1.2em;padding:.7em 1.4em}</style>"
    "<h2>navfeeder-esp setup</h2>"
    "<form method=POST action=/save>"
    "<label>WiFi SSID<input name=ssid maxlength=32 required></label>"
    "<label>WiFi password<input name=pass type=password maxlength=64></label>"
    "<label>Collector host<input name=host maxlength=63 required placeholder='collector.host.invalid'></label>"
    "<label>Collector port<input name=port type=number value=5580></label>"
    "<label>Station id<input name=station maxlength=32 required></label>"
    "<label>Bearer token<input name=token maxlength=128 required></label>"
    PORTAL_INSECURE_FIELD
    "<button type=submit>Save &amp; reboot</button></form>";

// url_decode decodes application/x-www-form-urlencoded text in place-safe form into dst.
// It returns false instead of silently truncating when the decoded value does not fit.
static bool url_decode(char *dst, size_t cap, const char *src, size_t srclen)
{
    size_t o = 0;
    for (size_t i = 0; i < srclen; i++) {
        if (o + 1 >= cap) {
            dst[o] = '\0';
            return false;
        }
        char c = src[i];
        if (c == '+') {
            dst[o++] = ' ';
        } else if (c == '%' && i + 2 < srclen) {
            char hex[3] = { src[i + 1], src[i + 2], 0 };
            dst[o++] = (char)strtol(hex, NULL, 16);
            i += 2;
        } else {
            dst[o++] = c;
        }
    }
    dst[o] = '\0';
    return true;
}

// form_field extracts one urlencoded field ("name=value&...") into dst (decoded), returning
// whether the key was present in the body at all. It no longer clears dst up front:
// on "not found" it leaves dst untouched, so a caller seeding dst from the current config
// (as save_post does) keeps that value for a field a partial POST omitted entirely, rather
// than silently wiping it to empty — the "unspecified fields keep their value" the comment
// there already claimed. Callers using a fresh, otherwise-uninitialized local buffer (not a
// pre-seeded config field) must zero it themselves before calling, since "not found" is now
// a true no-op.
typedef enum { FORM_TRUNCATED = -1, FORM_ABSENT = 0, FORM_OK = 1 } form_result_t;

static form_result_t form_field(const char *body, const char *name, char *dst, size_t cap)
{
    char key[24];
    int kn = snprintf(key, sizeof key, "%s=", name);
    const char *p = body;
    while ((p = strstr(p, key)) != NULL) {
        // Must be at the start or right after '&' to avoid matching a suffix.
        if (p == body || p[-1] == '&') {
            const char *v = p + kn;
            const char *end = strchr(v, '&');
            size_t vlen = end ? (size_t)(end - v) : strlen(v);
            return url_decode(dst, cap, v, vlen) ? FORM_OK : FORM_TRUNCATED;
        }
        p += kn;
    }
    return FORM_ABSENT;
}

static void restart_task(void *arg)
{
    (void)arg;
    vTaskDelay(pdMS_TO_TICKS(1500));
    esp_restart();
}

static esp_err_t root_get(httpd_req_t *req)
{
    httpd_resp_set_type(req, "text/html");
    return httpd_resp_send(req, PORTAL_HTML, HTTPD_RESP_USE_STRLEN);
}

// body_cap  must exceed the worst-case URL-encoded form: token[129] + wifi_pass[65] +
// host[64] + wifi_ssid[33] + station[33] fields, each up to 3x under %XX-encoding, plus
// field names/delimiters — comfortably under 2048. A silently truncated body would parse
// trailing fields wrong/empty rather than fail loudly, on a headless provisioning flow
// where nobody is watching the response.
#define SAVE_POST_BODY_CAP 2048

// origin_ok validates the request came from our own portal page, not a cross-origin page
// open in the operator's browser while it's joined to the provisioning AP : a plain
// HTML <form> POST needs no CORS preflight, so without this check any page open in a
// phone's browser during provisioning could silently POST to http://192.168.4.1/save in
// the background and rewrite the collector host/token. Origin (sent by browsers on
// cross-origin POSTs, and by modern browsers on same-origin ones too) must exactly match
// our own origin when present; when it's absent (older/simple form submits), Referer must
// at least point back at us. Rejecting when both are absent is the safe default — a
// legitimate browser POST from our own served form always sends one or the other.
static bool origin_ok(httpd_req_t *req)
{
    char buf[64];
    if (httpd_req_get_hdr_value_str(req, "Origin", buf, sizeof buf) == ESP_OK) {
        return strcmp(buf, "http://192.168.4.1") == 0;
    }
    if (httpd_req_get_hdr_value_str(req, "Referer", buf, sizeof buf) == ESP_OK) {
        return strncmp(buf, "http://192.168.4.1/", 19) == 0;
    }
    return false; // neither header present: fail closed
}

static esp_err_t save_post(httpd_req_t *req)
{
    if (!origin_ok(req)) {
        httpd_resp_send_err(req, HTTPD_403_FORBIDDEN, "cross-origin request rejected");
        return ESP_FAIL;
    }
    if (req->content_len > SAVE_POST_BODY_CAP - 1) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "form too large");
        return ESP_FAIL;
    }

    char body[SAVE_POST_BODY_CAP];
    int total = 0;
    while (total < (int)sizeof(body) - 1) {
        int r = httpd_req_recv(req, body + total, sizeof(body) - 1 - total);
        if (r <= 0) break;
        total += r;
    }
    body[total > 0 ? total : 0] = '\0';

    // the read loop breaks on r<=0, which includes httpd's recv timeout and peer
    // disconnect — a lost TCP segment yields a body cut mid-value. Reject a short read before
    // parsing, or a truncated ssid+host could persist a provisioned-but-unconnectable config
    // that never re-raises the portal (recoverable only by an NVS erase).
    if (total != (int)req->content_len) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "incomplete form body");
        return ESP_FAIL;
    }

    netcfg_t cfg;
    // Start from the current config so unspecified fields keep their value (    // form_field no longer clears dst on "not found", so a field genuinely absent from the
    // body leaves this NVS-loaded value untouched). The return value is deliberately
    // ignored here — an INVALID current config is the normal case on the portal path.
    netcfg_load(&cfg, NULL, 0);
    if (form_field(body, "ssid", cfg.wifi_ssid, sizeof cfg.wifi_ssid) == FORM_TRUNCATED ||
        form_field(body, "pass", cfg.wifi_pass, sizeof cfg.wifi_pass) == FORM_TRUNCATED ||
        form_field(body, "host", cfg.host, sizeof cfg.host) == FORM_TRUNCATED) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "form field too long");
        return ESP_FAIL;
    }
    char port[8] = {0}; // fresh buffer, not a pre-seeded cfg field: must self-init 
    form_result_t port_result = form_field(body, "port", port, sizeof port);
    if (port_result == FORM_TRUNCATED) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "form field too long");
        return ESP_FAIL;
    }
    if (port_result == FORM_OK && port[0]) {
        // a 99999/negative/garbage port must not be stored verbatim into a
        // provisioned-but-unconnectable unit. atoi's 0-on-garbage lands in the same
        // rejected range, so the single netcfg_validate rule below catches every case
        // (regression fix — this used to be a second, separate bound check right here).
        cfg.port = atoi(port);
    }
    if (form_field(body, "station", cfg.station, sizeof cfg.station) == FORM_TRUNCATED ||
        form_field(body, "token", cfg.token, sizeof cfg.token) == FORM_TRUNCATED) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "form field too long");
        return ESP_FAIL;
    }
#if CONFIG_NVF_ALLOW_INSECURE_PORTAL
    char ins[8] = {0}; // fresh buffer: must self-init 
    if (form_field(body, "insecure", ins, sizeof ins) == FORM_TRUNCATED) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "form field too long");
        return ESP_FAIL;
    }
    cfg.insecure = ins[0] != '\0'; // checkbox present => on
#endif
    // With the portal control compiled out (regression fix, the shipped default), cfg.insecure
    // stays exactly what netcfg_load already populated (NVS or the Kconfig/NVF_INSECURE
    // default) — the form cannot change it either way.

    // the same rule netcfg_load applies at boot. Saving a config the boot path
    // would reject is how a unit ends up unprovisionable without a serial cable — refuse it
    // here, with the specific field named, while the operator still has the portal open.
    char reason[NETCFG_ERR_CAP];
    if (!netcfg_validate(&cfg, reason, sizeof reason)) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, reason);
        return ESP_FAIL;
    }

    esp_err_t err = netcfg_save(&cfg);
    if (err != ESP_OK) {
        httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "save failed");
        return err;
    }
    httpd_resp_set_type(req, "text/html");
    httpd_resp_sendstr(req, "<meta name=viewport content='width=device-width'>"
                            "<h3>Saved. Rebooting into station mode...</h3>");
    ESP_LOGI(TAG, "provisioned: ssid='%s' host='%s:%d' station='%s' — rebooting",
             cfg.wifi_ssid, cfg.host, cfg.port, cfg.station);
    xTaskCreate(restart_task, "restart", 2048, NULL, 5, NULL);
    return ESP_OK;
}

// gen_password builds a strong AP password from an unambiguous alphabet (no 0/O/1/l/I),
// never a placeholder — the design rule (generate real secrets at creation). // rejection-sample so no symbol is favored by the modulo bias (esp_random() % 55 alone
// slightly over-weights the low symbols). The caller must ensure a seeded RNG first.
static void gen_password(char *dst, size_t n)
{
    static const char alpha[] =
        "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
    const uint32_t m = sizeof(alpha) - 1;
    const uint32_t max_valid = (UINT32_MAX / m) * m; // discard the top partial bucket
    for (size_t i = 0; i + 1 < n; i++) {
        uint32_t r;
        do { r = esp_random(); } while (r >= max_valid);
        dst[i] = alpha[r % m];
    }
    dst[n - 1] = '\0';
}

esp_err_t netcfg_start_portal(char ap_ssid[33], char ap_pass[16])
{
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_ap();
    wifi_init_config_t ic = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&ic));

    uint8_t mac[6] = {0};
    esp_read_mac(mac, ESP_MAC_WIFI_SOFTAP);
    snprintf(ap_ssid, 33, "navfeeder-%02X%02X%02X", mac[3], mac[4], mac[5]);
    // esp_random() is only truly random while an RF subsystem is running; here Wi-Fi
    // is init'd but not started, so seed the RNG from the bootloader entropy source for the
    // duration of password generation (disabled again before esp_wifi_start, which the RF
    // driver requires).
    bootloader_random_enable();
    gen_password(ap_pass, 13); // 12 chars + NUL (WPA2 needs >= 8)
    bootloader_random_disable();

    wifi_config_t ap = {0};
    snprintf((char *)ap.ap.ssid, sizeof ap.ap.ssid, "%s", ap_ssid);
    ap.ap.ssid_len = strlen(ap_ssid);
    snprintf((char *)ap.ap.password, sizeof ap.ap.password, "%s", ap_pass);
    ap.ap.max_connection = 2;
    ap.ap.authmode = WIFI_AUTH_WPA2_PSK;
    ap.ap.channel = 1;

    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_AP));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_AP, &ap));
    ESP_ERROR_CHECK(esp_wifi_start());

    httpd_handle_t server = NULL;
    httpd_config_t hcfg = HTTPD_DEFAULT_CONFIG();
    // regression fix follow-up: save_post keeps ~2.4 KB of locals on this task's stack
    // (body[SAVE_POST_BODY_CAP] + a netcfg_t + scratch), and the httpd default
    // stack is 4096 — too tight once httpd's own frames and the NVS/log calls
    // underneath the handler are added. Double it rather than heap-allocating
    // the body (the portal runs pre-provisioning, when RAM is otherwise idle).
    hcfg.stack_size = 8192;
    ESP_RETURN_ON_ERROR(httpd_start(&server, &hcfg), TAG, "httpd");
    httpd_uri_t root = { .uri = "/", .method = HTTP_GET, .handler = root_get };
    httpd_uri_t save = { .uri = "/save", .method = HTTP_POST, .handler = save_post };
    httpd_register_uri_handler(server, &root);
    httpd_register_uri_handler(server, &save);

    // The AP password is a secret shown on the local LCD, never logged (only its length).
    ESP_LOGI(TAG, "provisioning portal up: SSID='%s' (pass %d chars) -> http://192.168.4.1/",
             ap_ssid, (int)strlen(ap_pass));
    return ESP_OK;
}
