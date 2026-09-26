// netcfg — NVS config + BLE/SoftAP provisioning. See include/netcfg.h.

#include "netcfg.h"
#include "netcfg_form.h"
#include "netcfg_setup.h"
#include "netcfg_tunnel.h"
#include "sdkconfig.h"
#if CONFIG_NVF_BOARD_GNSS_COLOR
#include "netcfg_ble.h"
#endif
#if CONFIG_NVF_BOARD_GNSS_COLOR_MAX
#include "sensor_settings.h"
#endif

#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
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
#include "lwip/inet.h"
#include "lwip/sockets.h"
#include "ota.h"
#include "esp_check.h"
#include "esp_log.h"

static const char *TAG = "netcfg";

// A Kconfig bool left at 'n' emits no #define.
#ifndef CONFIG_NVF_INSECURE
#define CONFIG_NVF_INSECURE 0
#endif
#ifndef CONFIG_NVF_ALLOW_INSECURE_PORTAL
#define CONFIG_NVF_ALLOW_INSECURE_PORTAL 0
#endif
#ifndef CONFIG_NVF_WIREGUARD
#define CONFIG_NVF_WIREGUARD 0
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

// The optional WireGuard profile (ESP32-S3): the operator pastes the wg-quick .conf their
// collector operator generated, and the device parses and validates it (netcfg_tunnel).
// An empty box leaves the tunnel off and clears a stored one; a POST without the field
// keeps whatever is stored. The C6 build has no tunnel, so it has no field either.
#if CONFIG_NVF_WIREGUARD
#define PORTAL_TUNNEL_FIELD \
    "<label>WireGuard profile (optional: paste the .conf; AllowedIPs is the collector's /32)" \
    "<textarea name=wg rows=10 maxlength=1000 spellcheck=false autocomplete=off " \
    "placeholder='[Interface]&#10;PrivateKey = ...&#10;Address = 10.77.0.12/24&#10;&#10;" \
    "[Peer]&#10;PublicKey = ...&#10;Endpoint = wg.collector.invalid:51820&#10;" \
    "AllowedIPs = 10.77.0.1/32&#10;PersistentKeepalive = 25'></textarea></label>"
#else
#define PORTAL_TUNNEL_FIELD ""
#endif

// A board with its own Ethernet port runs without a Wi-Fi network (netcfg_validate).
#if NETCFG_WIRED_UPLINK
#define PORTAL_SSID_FIELD "<label>WiFi SSID (optional: leave empty to use Ethernet only)" \
    "<input name=ssid maxlength=32></label>"
#else
#define PORTAL_SSID_FIELD "<label>WiFi SSID<input name=ssid maxlength=32 required></label>"
#endif

static const char PORTAL_HTML_HEAD[] =
    "<!doctype html><meta name=viewport content='width=device-width,initial-scale=1'>"
    "<title>navfeeder-esp setup</title>"
    "<style>body{font-family:sans-serif;max-width:32em;margin:2em auto;padding:0 1em}"
    "label{display:block;margin:.8em 0 .2em}input,textarea{width:100%;padding:.5em;box-sizing:border-box}"
    "textarea{font-family:monospace}"
    "button{margin-top:1.2em;padding:.7em 1.4em}</style>"
    "<h2>navfeeder-esp setup</h2>"
    "<form method=POST action=/save>"
    PORTAL_SSID_FIELD
    "<label>WiFi password<input name=pass type=password maxlength=64></label>"
    "<label>Collector host<input name=host maxlength=63 required placeholder='collector.host.invalid'></label>"
    "<label>Collector port<input name=port type=number value=5580></label>"
    "<label>Station id<input name=station maxlength=32 required></label>"
    "<label>Bearer token<input name=token maxlength=128 required></label>"
    PORTAL_TUNNEL_FIELD
    PORTAL_INSECURE_FIELD;
static const char PORTAL_HTML_TAIL[] = "<button type=submit>Save &amp; reboot</button></form>";

#if CONFIG_NVF_BOARD_GNSS_COLOR_MAX
// The MAX board's per-unit sensor settings, with the stored values selected. They are
// kept apart from the network record, so a network reset leaves them.
static void portal_sensor_fields(char *out, size_t cap)
{
    sensor_settings_t s;
    if (sensor_settings_load(&s) != ESP_OK) s = SENSOR_SETTINGS_DEFAULT;
    const char *on = " selected";
    snprintf(out, cap,
        "<label>Mains frequency where the thermocouple is used<select name=mains_hz>"
        "<option value=60%s>60 Hz</option><option value=50%s>50 Hz</option></select></label>"
        "<label>Motion profile<select name=motion>"
        "<option value=surface%s>Surface: vehicles and vessels (12.5 Hz still, 50 Hz moving)</option>"
        "<option value=aerial%s>Aerial: aircraft and drones (12.5 Hz still, 100 Hz moving)</option>"
        "</select></label>",
        s.mains_hz == 60 ? on : "", s.mains_hz == 50 ? on : "",
        s.motion == SENSOR_MOTION_SURFACE ? on : "", s.motion == SENSOR_MOTION_AERIAL ? on : "");
}
#endif

static void restart_task(void *arg)
{
    (void)arg;
    vTaskDelay(pdMS_TO_TICKS(1500));
    esp_restart();
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

static esp_err_t root_get(httpd_req_t *req)
{
    if (!request_on_setup_ap(req)) {
        httpd_resp_send_err(req, HTTPD_403_FORBIDDEN,
                            "setup requires the provisioning AP");
        return ESP_FAIL;
    }
    httpd_resp_set_type(req, "text/html");
    esp_err_t err = httpd_resp_send_chunk(req, PORTAL_HTML_HEAD, HTTPD_RESP_USE_STRLEN);
#if CONFIG_NVF_BOARD_GNSS_COLOR_MAX
    char sensors[640];
    portal_sensor_fields(sensors, sizeof sensors);
    if (err == ESP_OK) err = httpd_resp_send_chunk(req, sensors, HTTPD_RESP_USE_STRLEN);
#endif
    if (err == ESP_OK) err = httpd_resp_send_chunk(req, PORTAL_HTML_TAIL, HTTPD_RESP_USE_STRLEN);
    if (err == ESP_OK) err = httpd_resp_send_chunk(req, NULL, 0);
    return err;
}

// body_cap must exceed the worst-case URL-encoded form: token[129] + wifi_pass[65] +
// host[64] + wifi_ssid[33] + station[33] fields, each up to 3x under %XX-encoding, plus
// field names/delimiters — under 1024 — and, on the S3, the pasted WireGuard profile
// (up to NETCFG_TUNNEL_CONF_CAP, likewise up to 3x encoded). A silently truncated body
// would parse trailing fields wrong/empty rather than fail loudly, on a headless
// provisioning flow where nobody is watching the response.
#define SAVE_POST_BODY_CAP 4096

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
    if (!request_on_setup_ap(req)) {
        httpd_resp_send_err(req, HTTPD_403_FORBIDDEN,
                            "setup requires the provisioning AP");
        return ESP_FAIL;
    }
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
    // every negative result — too long, or malformed
    // percent-encoding / a control byte — refuses the whole POST before anything
    // reaches NVS, with a reason that never echoes submitted bytes back.
    const struct { const char *name; char *dst; size_t cap; } text_fields[] = {
        { "ssid", cfg.wifi_ssid, sizeof cfg.wifi_ssid },
        { "pass", cfg.wifi_pass, sizeof cfg.wifi_pass },
        { "host", cfg.host, sizeof cfg.host },
    };
    for (size_t i = 0; i < sizeof text_fields / sizeof *text_fields; i++) {
        form_result_t r = netcfg_form_field(body, text_fields[i].name,
                                            text_fields[i].dst, text_fields[i].cap);
        if (r < 0) {
            httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, netcfg_form_error(r));
            return ESP_FAIL;
        }
    }
    char port[8] = {0}; // fresh buffer, not a pre-seeded cfg field: must self-init
    form_result_t port_result = netcfg_form_field(body, "port", port, sizeof port);
    if (port_result < 0) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, netcfg_form_error(port_result));
        return ESP_FAIL;
    }
    if (port_result == FORM_OK && port[0]) {
        // a 99999/negative/garbage port must not be stored verbatim into a
        // provisioned-but-unconnectable unit. atoi's 0-on-garbage lands in the same
        // rejected range, so the single netcfg_validate rule below catches every case
        // (regression fix — this used to be a second, separate bound check right here).
        cfg.port = atoi(port);
    }
    const struct { const char *name; char *dst; size_t cap; } id_fields[] = {
        { "station", cfg.station, sizeof cfg.station },
        { "token", cfg.token, sizeof cfg.token },
    };
    for (size_t i = 0; i < sizeof id_fields / sizeof *id_fields; i++) {
        form_result_t r = netcfg_form_field(body, id_fields[i].name,
                                            id_fields[i].dst, id_fields[i].cap);
        if (r < 0) {
            httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, netcfg_form_error(r));
            return ESP_FAIL;
        }
    }
#if CONFIG_NVF_WIREGUARD
    // The pasted profile is the one multi-line field. Absent keeps the stored profile (a
    // partial POST), present-but-blank turns the tunnel off and drops its keys, and
    // anything else must parse and validate whole, or the POST fails with the parser's
    // fixed reason — never the submitted bytes, which include a private key.
    char profile[NETCFG_TUNNEL_CONF_CAP];
    form_result_t wg_result = netcfg_form_field_text(body, "wg", profile, sizeof profile);
    if (wg_result < 0) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, netcfg_form_error(wg_result));
        return ESP_FAIL;
    }
    if (wg_result == FORM_OK) {
        if (strspn(profile, " \t\r\n") == strlen(profile)) {
            memset(&cfg.tunnel, 0, sizeof cfg.tunnel);
        } else {
            char why[NETCFG_ERR_CAP];
            if (!netcfg_tunnel_parse_conf(profile, &cfg.tunnel, why, sizeof why)) {
                httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, why);
                return ESP_FAIL;
            }
        }
    }
#endif
#if CONFIG_NVF_ALLOW_INSECURE_PORTAL
    char ins[8] = {0}; // fresh buffer: must self-init
    form_result_t ins_result = netcfg_form_field(body, "insecure", ins, sizeof ins);
    if (ins_result < 0) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, netcfg_form_error(ins_result));
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
#if CONFIG_NVF_BOARD_GNSS_COLOR_MAX
    // An absent field keeps the stored setting. They are saved before the network record,
    // so a refused sensor setting leaves the whole form to be submitted again.
    sensor_settings_t sensors;
    (void)sensor_settings_load(&sensors); // an unreadable record is replaced with valid values
    char choice[16] = {0};
    form_result_t mains = netcfg_form_field(body, "mains_hz", choice, sizeof choice);
    if (mains < 0 || (mains == FORM_OK && !sensor_settings_parse_mains(choice, &sensors.mains_hz))) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "invalid mains frequency");
        return ESP_FAIL;
    }
    memset(choice, 0, sizeof choice);
    form_result_t motion = netcfg_form_field(body, "motion", choice, sizeof choice);
    if (motion < 0 || (motion == FORM_OK && !sensor_settings_parse_motion(choice, &sensors.motion))) {
        httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "invalid motion profile");
        return ESP_FAIL;
    }
    if (sensor_settings_save(&sensors) != ESP_OK) {
        httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "sensor settings not saved");
        return ESP_FAIL;
    }
    ESP_LOGI(TAG, "sensor settings: %u Hz mains notch, %s motion profile", sensors.mains_hz,
             sensor_motion_name(sensors.motion));
#endif

    esp_err_t err = netcfg_save(&cfg);
    if (err != ESP_OK) {
        httpd_resp_send_err(req, HTTPD_500_INTERNAL_SERVER_ERROR, "save failed");
        return err;
    }
    // schedule the reboot BEFORE promising it. The return value used
    // to be discarded, so under heap/task exhaustion the config was committed and
    // the operator was told the unit was rebooting while it sat in provisioning
    // mode indefinitely, waiting for a power cycle nobody knew to perform. The
    // save is atomic and already succeeded, so it is never rolled back; only the
    // message changes. restart_task's own delay is what lets the response flush.
    BaseType_t started = xTaskCreate(restart_task, "restart", 2048, NULL, 5, NULL);
    httpd_resp_set_type(req, "text/html");
    if (started != pdPASS) {
        ESP_LOGE(TAG, "configuration saved but the restart task could not be created; "
                      "power-cycle the device to apply it");
        httpd_resp_sendstr(req, "<meta name=viewport content='width=device-width'>"
                                "<h3>Saved, but the reboot could not be scheduled.</h3>"
                                "<p>Power-cycle the device to apply the new configuration.</p>");
        return ESP_OK;
    }
    httpd_resp_sendstr(req, "<meta name=viewport content='width=device-width'>"
                            "<h3>Saved. Rebooting into station mode...</h3>");
    ESP_LOGI(TAG, "provisioned: ssid='%s' host='%s:%d' station='%s'%s — rebooting",
             cfg.wifi_ssid, cfg.host, cfg.port, cfg.station,
             cfg.tunnel.enabled ? " tunnel=on" : "");
    return ESP_OK;
}

esp_err_t netcfg_start_provisioning(netcfg_provisioning_info_t *info)
{
    if (!info) return ESP_ERR_INVALID_ARG;
    memset(info, 0, sizeof *info);

    uint8_t mac[6] = {0};
    ESP_RETURN_ON_ERROR(esp_read_mac(mac, ESP_MAC_WIFI_SOFTAP), TAG, "read setup MAC");
    netcfg_setup_credentials_t setup;
    bool created = false;
    // The RF drivers are not running yet. Enable their boot-time entropy source
    // while a missing credential may be generated, and always disable it before
    // Wi-Fi or BLE starts.
    bootloader_random_enable();
    esp_err_t err = netcfg_setup_load_or_create(&setup, mac, esp_random, &created);
    bootloader_random_disable();
    ESP_RETURN_ON_ERROR(err, TAG, "load persistent setup credential");
    ESP_RETURN_ON_ERROR(netcfg_setup_qr_payload(&setup, info->qr_payload,
                                                sizeof info->qr_payload),
                        TAG, "format setup QR payload");
    snprintf(info->name, sizeof info->name, "%s", setup.name);
    snprintf(info->password, sizeof info->password, "%s", setup.password);
    info->credential_created = created;

    ESP_RETURN_ON_ERROR(esp_netif_init(), TAG, "initialize network stack");
    ESP_RETURN_ON_ERROR(esp_event_loop_create_default(), TAG, "create event loop");
#if CONFIG_NVF_BOARD_GNSS_COLOR
    if (!esp_netif_create_default_wifi_sta()) return ESP_ERR_NO_MEM;
#endif
    if (!esp_netif_create_default_wifi_ap()) return ESP_ERR_NO_MEM;
    wifi_init_config_t ic = WIFI_INIT_CONFIG_DEFAULT();
    ESP_RETURN_ON_ERROR(esp_wifi_init(&ic), TAG, "initialize Wi-Fi");

#if CONFIG_NVF_BOARD_GNSS_COLOR
    err = netcfg_ble_start(&setup);
    if (err == ESP_OK) {
        info->ble_active = true;
    } else {
        // The browser path remains usable even if the optional BLE stack cannot
        // allocate resources or start. netcfg_ble_start leaves Wi-Fi stopped.
        ESP_LOGE(TAG, "BLE provisioning unavailable (%s); continuing with browser fallback",
                 esp_err_to_name(err));
    }
#endif

    wifi_config_t ap = {0};
    snprintf((char *)ap.ap.ssid, sizeof ap.ap.ssid, "%s", setup.name);
    ap.ap.ssid_len = strlen(setup.name);
    snprintf((char *)ap.ap.password, sizeof ap.ap.password, "%s", setup.password);
    ap.ap.max_connection = 2;
    ap.ap.authmode = WIFI_AUTH_WPA2_PSK;
    ap.ap.channel = 1;

    ESP_RETURN_ON_ERROR(esp_wifi_set_mode(info->ble_active ? WIFI_MODE_APSTA : WIFI_MODE_AP),
                        TAG, "enable setup AP");
    ESP_RETURN_ON_ERROR(esp_wifi_set_config(WIFI_IF_AP, &ap), TAG, "configure setup AP");
    if (!info->ble_active)
        ESP_RETURN_ON_ERROR(esp_wifi_start(), TAG, "start setup AP");

    httpd_handle_t server = NULL;
    httpd_config_t hcfg = HTTPD_DEFAULT_CONFIG();
    // save_post keeps its locals on this task's stack: body[SAVE_POST_BODY_CAP] (4 KiB),
    // the pasted profile (1 KiB), a netcfg_t (~0.7 KiB) and scratch, and the httpd default
    // stack is 4096 — far too tight once httpd's own frames and the NVS/log calls
    // underneath the handler are added. Size it generously rather than heap-allocating
    // the body (the portal runs pre-provisioning, when RAM is otherwise idle).
    hcfg.stack_size = 12288;
    ESP_RETURN_ON_ERROR(httpd_start(&server, &hcfg), TAG, "start setup HTTP server");
    httpd_uri_t root = { .uri = "/", .method = HTTP_GET, .handler = root_get };
    httpd_uri_t save = { .uri = "/save", .method = HTTP_POST, .handler = save_post };
    ESP_RETURN_ON_ERROR(httpd_register_uri_handler(server, &root), TAG, "register setup root");
    ESP_RETURN_ON_ERROR(httpd_register_uri_handler(server, &save), TAG, "register setup save");
    ESP_RETURN_ON_ERROR(nvf_ota_register_pairing(server), TAG, "register OTA pairing");

    // The password is returned for the local display and app_main's configured
    // setup-label logging policy. Development setup boots can reprint it.
    ESP_LOGI(TAG, "browser provisioning ready: SSID='%s' (pass %d chars) -> http://192.168.4.1/",
             setup.name, (int)strlen(setup.password));
    return ESP_OK;
}
