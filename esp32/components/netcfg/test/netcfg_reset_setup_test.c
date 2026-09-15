// Verify the user-facing configuration reset cannot rotate the setup password.
// The two production records share an NVS partition but use separate namespaces.
#include "netcfg.h"
#include "netcfg_setup.h"
#include "nvs.h"

#include <assert.h>
#include <stdio.h>
#include <string.h>

static uint8_t config_record[512];
static size_t config_size;
static uint8_t setup_record[96];
static size_t setup_size;
static unsigned setup_writes;
static unsigned random_calls;

esp_err_t nvs_open(const char *ns, int mode, nvs_handle_t *handle)
{
    assert(mode == NVS_READONLY || mode == NVS_READWRITE);
    if (!strcmp(ns, "navfeeder")) *handle = 1;
    else if (!strcmp(ns, "nvf_setup")) *handle = 2;
    else assert(!"unexpected NVS namespace");
    return ESP_OK;
}

void nvs_close(nvs_handle_t handle) { assert(handle == 1 || handle == 2); }

esp_err_t nvs_get_blob(nvs_handle_t handle, const char *key,
                       void *out, size_t *size)
{
    const uint8_t *record;
    size_t record_size;
    if (handle == 1) {
        assert(!strcmp(key, "config_v1"));
        record = config_record;
        record_size = config_size;
    } else {
        assert(handle == 2 && !strcmp(key, "credential_v1"));
        record = setup_record;
        record_size = setup_size;
    }
    if (!record_size) return ESP_ERR_NVS_NOT_FOUND;
    if (*size < record_size) return ESP_ERR_NVS_INVALID_LENGTH;
    memcpy(out, record, record_size);
    *size = record_size;
    return ESP_OK;
}

esp_err_t nvs_set_blob(nvs_handle_t handle, const char *key,
                       const void *data, size_t size)
{
    if (handle == 1) {
        assert(!strcmp(key, "config_v1") && size <= sizeof config_record);
        memcpy(config_record, data, size);
        config_size = size;
    } else {
        assert(handle == 2 && !strcmp(key, "credential_v1") &&
               size == sizeof setup_record);
        memcpy(setup_record, data, size);
        setup_size = size;
        setup_writes++;
    }
    return ESP_OK;
}

esp_err_t nvs_commit(nvs_handle_t handle)
{
    assert(handle == 1 || handle == 2);
    return ESP_OK;
}

esp_err_t nvs_get_str(nvs_handle_t handle, const char *key,
                      char *out, size_t *size)
{
    (void)key;
    (void)out;
    (void)size;
    assert(handle == 1);
    return ESP_ERR_NVS_NOT_FOUND;
}

esp_err_t nvs_get_u8(nvs_handle_t handle, const char *key, uint8_t *value)
{
    (void)key;
    (void)value;
    assert(handle == 1);
    return ESP_ERR_NVS_NOT_FOUND;
}

esp_err_t nvs_get_i32(nvs_handle_t handle, const char *key, int32_t *value)
{
    (void)key;
    (void)value;
    assert(handle == 1);
    return ESP_ERR_NVS_NOT_FOUND;
}

esp_err_t nvs_erase_key(nvs_handle_t handle, const char *key)
{
    (void)key;
    assert(handle == 1);
    return ESP_ERR_NVS_NOT_FOUND;
}

static uint32_t deterministic_random(void)
{
    return 0x31415926u + 104729u * random_calls++;
}

int main(void)
{
    const uint8_t mac[] = {0x02, 0, 0, 0xA1, 0xB2, 0xC3};
    netcfg_setup_credentials_t before, after;
    bool created = false;
    assert(netcfg_setup_load_or_create(&before, mac, deterministic_random,
                                       &created) == ESP_OK);
    assert(created && setup_writes == 1);

    const netcfg_t config = {
        .wifi_ssid = "field-network",
        .wifi_pass = "field-password",
        .host = "collector.example",
        .port = 5580,
        .token = "enrollment-token",
        .station = "station-123",
        .insecure = false,
    };
    assert(netcfg_save(&config) == ESP_OK);
    assert(netcfg_reset_provisioning() == ESP_OK);

    unsigned calls_after_create = random_calls;
    created = true;
    assert(netcfg_setup_load_or_create(&after, mac, deterministic_random,
                                       &created) == ESP_OK);
    assert(!created && setup_writes == 1 && random_calls == calls_after_create);
    assert(!memcmp(&before, &after, sizeof before));

    netcfg_t loaded;
    assert(!netcfg_load(&loaded, NULL, 0));
    assert(!loaded.wifi_ssid[0] && !loaded.token[0]);
    puts("netcfg reset: setup credential survives unchanged PASS");
    return 0;
}
