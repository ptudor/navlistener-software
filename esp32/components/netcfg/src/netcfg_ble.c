#include "netcfg_ble.h"

#include <stdlib.h>
#include <string.h>

#include "bootloader_random.h"
#include "esp_log.h"
#include "esp_srp.h"
#include "esp_system.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "netcfg.h"
#include "netcfg_prov_payload.h"
#include "netcfg_tunnel.h"
#include "sdkconfig.h"
#include "wifi_provisioning/manager.h"
#include "wifi_provisioning/scheme_ble.h"

#ifndef CONFIG_NVF_WIREGUARD
#define CONFIG_NVF_WIREGUARD 0
#endif

#define NAV_CONFIG_ENDPOINT "nav-config"
#define NAV_TUNNEL_ENDPOINT "nav-tunnel"
#define SECURITY2_SALT_LEN 16

static const char *TAG = "netcfg_ble";

typedef struct {
    SemaphoreHandle_t lock;
    netcfg_t app_config;
    netcfg_tunnel_t tunnel;   // nav-tunnel, optional; merged into the record at save time
    char wifi_ssid[33];
    char wifi_password[65];
    bool app_received;
    bool tunnel_received;
    bool wifi_received;
    bool wifi_connected;
    bool saved;
    bool restart_scheduled;
    char *salt;
    char *verifier;
    wifi_prov_security2_params_t security;
} ble_context_t;

static ble_context_t context;

static void restart_task(void *arg)
{
    (void)arg;
    // Let the encrypted custom-endpoint or Wi-Fi result response leave the
    // radio before tearing down the provisioning session.
    vTaskDelay(pdMS_TO_TICKS(1000));
    esp_restart();
}

// Called with context.lock held. Both ordering choices work: the app may send
// nav-config before or after the standard Wi-Fi endpoint reports success.
static netcfg_prov_status_t finish_if_ready(void)
{
    if (context.saved)
        return context.restart_scheduled ? NETCFG_PROV_SAVED_REBOOTING
                                         : NETCFG_PROV_SAVED_RESTART_REQUIRED;
    if (!context.app_received || !context.wifi_received || !context.wifi_connected)
        return NETCFG_PROV_WAITING_FOR_WIFI;

    netcfg_t complete = context.app_config;
    memcpy(complete.wifi_ssid, context.wifi_ssid, sizeof complete.wifi_ssid);
    memcpy(complete.wifi_pass, context.wifi_password, sizeof complete.wifi_pass);
    // nav-config decodes to a record with the tunnel off; a profile that arrived through
    // nav-tunnel before this save is carried in whichever order the two endpoints came.
    if (context.tunnel_received) complete.tunnel = context.tunnel;
    if (!netcfg_validate(&complete, NULL, 0)) {
        ESP_LOGE(TAG, "complete BLE provisioning record failed validation");
        return NETCFG_PROV_INVALID_REQUEST;
    }
    esp_err_t err = netcfg_save(&complete);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "BLE provisioning record save failed: %s", esp_err_to_name(err));
        return NETCFG_PROV_STORAGE_FAILED;
    }

    context.saved = true;
    context.restart_scheduled =
        xTaskCreate(restart_task, "prov_restart", 2048, NULL, 5, NULL) == pdPASS;
    if (!context.restart_scheduled)
        ESP_LOGE(TAG, "configuration saved but restart task creation failed; power-cycle required");
    else
        ESP_LOGI(TAG, "BLE provisioning complete; rebooting into station mode");
    return context.restart_scheduled ? NETCFG_PROV_SAVED_REBOOTING
                                     : NETCFG_PROV_SAVED_RESTART_REQUIRED;
}

static esp_err_t nav_config_handler(uint32_t session_id,
                                    const uint8_t *inbuf, ssize_t inlen,
                                    uint8_t **outbuf, ssize_t *outlen,
                                    void *priv_data)
{
    (void)session_id;
    (void)priv_data;
    if (!outbuf || !outlen) return ESP_ERR_INVALID_ARG;

    uint8_t *response = malloc(NETCFG_PROV_RESPONSE_SIZE);
    if (!response) return ESP_ERR_NO_MEM;

    netcfg_t requested;
    char reason[NETCFG_ERR_CAP] = {0};
    netcfg_prov_status_t status = NETCFG_PROV_INVALID_REQUEST;
    if (inlen >= 0 && netcfg_prov_decode(inbuf, (size_t)inlen, &requested,
                                         reason, sizeof reason)) {
        xSemaphoreTake(context.lock, portMAX_DELAY);
        context.app_config = requested;
        context.app_received = true;
        status = finish_if_ready();
        xSemaphoreGive(context.lock);
    } else {
        ESP_LOGW(TAG, "rejected nav-config request: %s", reason[0] ? reason : "invalid length");
    }

    netcfg_prov_encode_response(response, status);
    *outbuf = response;
    *outlen = NETCFG_PROV_RESPONSE_SIZE;
    return ESP_OK;
}

#if CONFIG_NVF_WIREGUARD
// nav-tunnel carries the optional WireGuard profile, private key included, inside the same
// Security 2 session as the enrollment token. It must arrive before the Wi-Fi credentials
// complete the save: once the record is durable the profile can no longer be applied and
// the app is told so (NETCFG_PROV_ALREADY_SAVED) rather than left to assume it was.
static esp_err_t nav_tunnel_handler(uint32_t session_id,
                                    const uint8_t *inbuf, ssize_t inlen,
                                    uint8_t **outbuf, ssize_t *outlen,
                                    void *priv_data)
{
    (void)session_id;
    (void)priv_data;
    if (!outbuf || !outlen) return ESP_ERR_INVALID_ARG;

    uint8_t *response = malloc(NETCFG_PROV_RESPONSE_SIZE);
    if (!response) return ESP_ERR_NO_MEM;

    netcfg_tunnel_t requested;
    char reason[NETCFG_ERR_CAP] = {0};
    netcfg_prov_status_t status = NETCFG_PROV_INVALID_REQUEST;
    if (inlen >= 0 && netcfg_tunnel_decode(inbuf, (size_t)inlen, &requested,
                                           reason, sizeof reason)) {
        xSemaphoreTake(context.lock, portMAX_DELAY);
        if (context.saved) {
            status = NETCFG_PROV_ALREADY_SAVED;
        } else {
            context.tunnel = requested;
            context.tunnel_received = true;
            status = finish_if_ready();
        }
        xSemaphoreGive(context.lock);
        if (status == NETCFG_PROV_ALREADY_SAVED)
            ESP_LOGW(TAG, "nav-tunnel arrived after the configuration was saved; not applied");
    } else {
        ESP_LOGW(TAG, "rejected nav-tunnel request: %s", reason[0] ? reason : "invalid length");
    }
    memset(&requested, 0, sizeof requested);

    netcfg_prov_encode_response(response, status);
    *outbuf = response;
    *outlen = NETCFG_PROV_RESPONSE_SIZE;
    return ESP_OK;
}
#endif

static void provisioning_event(void *user_data, wifi_prov_cb_event_t event,
                               void *event_data)
{
    (void)user_data;
    if (event == WIFI_PROV_CRED_RECV) {
        const wifi_sta_config_t *wifi = event_data;
        if (!wifi) return;
        xSemaphoreTake(context.lock, portMAX_DELAY);
        memset(context.wifi_ssid, 0, sizeof context.wifi_ssid);
        memset(context.wifi_password, 0, sizeof context.wifi_password);
        memcpy(context.wifi_ssid, wifi->ssid,
               strnlen((const char *)wifi->ssid, sizeof wifi->ssid));
        memcpy(context.wifi_password, wifi->password,
               strnlen((const char *)wifi->password, sizeof wifi->password));
        context.wifi_received = context.wifi_ssid[0] != '\0';
        context.wifi_connected = false;
        xSemaphoreGive(context.lock);
        ESP_LOGI(TAG, "received Wi-Fi credentials through encrypted BLE provisioning");
    } else if (event == WIFI_PROV_CRED_SUCCESS) {
        xSemaphoreTake(context.lock, portMAX_DELAY);
        context.wifi_connected = true;
        netcfg_prov_status_t status = finish_if_ready();
        xSemaphoreGive(context.lock);
        if (status == NETCFG_PROV_WAITING_FOR_WIFI)
            ESP_LOGI(TAG, "Wi-Fi verified; waiting for nav-config endpoint");
    } else if (event == WIFI_PROV_CRED_FAIL) {
        xSemaphoreTake(context.lock, portMAX_DELAY);
        context.wifi_connected = false;
        xSemaphoreGive(context.lock);
        esp_err_t err = wifi_prov_mgr_reset_sm_state_on_failure();
        if (err == ESP_OK)
            ESP_LOGW(TAG, "BLE-provided Wi-Fi credentials did not connect; client may retry");
        else
            ESP_LOGE(TAG, "failed to reset provisioning after Wi-Fi error: %s",
                     esp_err_to_name(err));
    }
}

static void cleanup_failed_start(bool manager_initialized)
{
    if (manager_initialized) wifi_prov_mgr_deinit();
    (void)esp_wifi_stop();
    free(context.salt);
    free(context.verifier);
    if (context.lock) vSemaphoreDelete(context.lock);
    memset(&context, 0, sizeof context);
}

esp_err_t netcfg_ble_start(const netcfg_setup_credentials_t *credentials)
{
    if (!credentials || !credentials->name[0] || !credentials->username[0] ||
        !credentials->password[0])
        return ESP_ERR_INVALID_ARG;
    memset(&context, 0, sizeof context);
    context.lock = xSemaphoreCreateMutex();
    if (!context.lock) return ESP_ERR_NO_MEM;

    int verifier_len = 0;
    bootloader_random_enable();
    esp_err_t err = esp_srp_gen_salt_verifier(
        credentials->username, strlen(credentials->username),
        credentials->password, strlen(credentials->password),
        &context.salt, SECURITY2_SALT_LEN,
        &context.verifier, &verifier_len);
    bootloader_random_disable();
    if (err != ESP_OK || verifier_len <= 0 || verifier_len > UINT16_MAX) {
        ESP_LOGE(TAG, "Security 2 verifier generation failed: %s", esp_err_to_name(err));
        cleanup_failed_start(false);
        return err == ESP_OK ? ESP_ERR_INVALID_SIZE : err;
    }
    context.security = (wifi_prov_security2_params_t) {
        .salt = context.salt,
        .salt_len = SECURITY2_SALT_LEN,
        .verifier = context.verifier,
        .verifier_len = (uint16_t)verifier_len,
    };

    wifi_prov_mgr_config_t manager_config = {
        .scheme = wifi_prov_scheme_ble,
        .scheme_event_handler = WIFI_PROV_SCHEME_BLE_EVENT_HANDLER_FREE_BTDM,
        .app_event_handler = {
            .event_cb = provisioning_event,
            .user_data = NULL,
        },
        .wifi_prov_conn_cfg = {
            .wifi_conn_attempts = 3,
        },
    };
    // The stock BLE scheme switches to STA-only when credentials arrive. This
    // product also serves its protected browser fallback, so retain APSTA for
    // scans, connection attempts, and retries.
    manager_config.scheme.wifi_mode = WIFI_MODE_APSTA;
    err = wifi_prov_mgr_init(manager_config);
    if (err != ESP_OK) {
        cleanup_failed_start(false);
        return err;
    }

    // The app checks these capabilities before sending anything: a build without tunnel
    // support advertises no nav-tunnel-v1, so a profile is refused app-side, never dropped.
#if CONFIG_NVF_WIREGUARD
    const char *capabilities[] = { "nav-config-v1", "nav-tunnel-v1" };
#else
    const char *capabilities[] = { "nav-config-v1" };
#endif
    const uint8_t capability_count = sizeof capabilities / sizeof capabilities[0];
    if ((err = wifi_prov_mgr_set_app_info("navfeeder", "1", capabilities, capability_count)) != ESP_OK ||
        (err = wifi_prov_mgr_endpoint_create(NAV_CONFIG_ENDPOINT)) != ESP_OK ||
#if CONFIG_NVF_WIREGUARD
        (err = wifi_prov_mgr_endpoint_create(NAV_TUNNEL_ENDPOINT)) != ESP_OK ||
#endif
        (err = wifi_prov_mgr_disable_auto_stop(1000)) != ESP_OK ||
        (err = wifi_prov_mgr_start_provisioning(WIFI_PROV_SECURITY_2,
                                                &context.security,
                                                credentials->name, NULL)) != ESP_OK ||
        (err = wifi_prov_mgr_endpoint_register(NAV_CONFIG_ENDPOINT,
                                               nav_config_handler, NULL)) != ESP_OK ||
#if CONFIG_NVF_WIREGUARD
        (err = wifi_prov_mgr_endpoint_register(NAV_TUNNEL_ENDPOINT,
                                               nav_tunnel_handler, NULL)) != ESP_OK ||
#endif
        false) {
        ESP_LOGE(TAG, "BLE provisioning manager start failed: %s", esp_err_to_name(err));
        cleanup_failed_start(true);
        return err;
    }

    ESP_LOGI(TAG, "BLE provisioning ready: name='%s', Security 2, endpoints='%s'%s",
             credentials->name, NAV_CONFIG_ENDPOINT,
             CONFIG_NVF_WIREGUARD ? ", '" NAV_TUNNEL_ENDPOINT "'" : "");
    return ESP_OK;
}
