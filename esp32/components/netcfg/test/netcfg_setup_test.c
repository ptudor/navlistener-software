#include "netcfg_setup.h"
#include "nvs.h"

#include <assert.h>
#include <stdio.h>
#include <string.h>

static uint8_t durable[96];
static size_t durable_size;
static unsigned writes;
static unsigned random_calls;
static esp_err_t read_error;
static esp_err_t write_error;
static esp_err_t commit_error;

esp_err_t nvs_open(const char *ns, int mode, nvs_handle_t *handle)
{
    assert(!strcmp(ns, "nvf_setup") && mode == NVS_READWRITE);
    *handle = 7;
    return ESP_OK;
}

void nvs_close(nvs_handle_t handle) { assert(handle == 7); }

esp_err_t nvs_get_blob(nvs_handle_t handle, const char *key, void *out, size_t *size)
{
    assert(handle == 7 && !strcmp(key, "credential_v1"));
    if (read_error) return read_error;
    if (!durable_size) return ESP_ERR_NVS_NOT_FOUND;
    if (*size < durable_size) return ESP_ERR_NVS_INVALID_LENGTH;
    memcpy(out, durable, durable_size);
    *size = durable_size;
    return ESP_OK;
}

esp_err_t nvs_set_blob(nvs_handle_t handle, const char *key,
                       const void *data, size_t size)
{
    assert(handle == 7 && !strcmp(key, "credential_v1") && size == sizeof durable);
    writes++;
    if (write_error) return write_error;
    memcpy(durable, data, size);
    durable_size = size;
    return ESP_OK;
}

esp_err_t nvs_commit(nvs_handle_t handle)
{
    assert(handle == 7);
    return commit_error;
}

static uint32_t deterministic_random(void)
{
    return 0x10203040u + 7919u * random_calls++;
}

static void clear_store(void)
{
    memset(durable, 0, sizeof durable);
    durable_size = writes = random_calls = 0;
    read_error = write_error = commit_error = ESP_OK;
}

int main(void)
{
    const uint8_t mac[] = { 0x02, 0x00, 0x00, 0xA1, 0xB2, 0xC3 };
    netcfg_setup_credentials_t first, loaded;
    bool created = false;

    clear_store();
    assert(netcfg_setup_load_or_create(&first, mac, deterministic_random, &created) == ESP_OK);
    assert(created && writes == 1 && random_calls >= 12);
    assert(!strcmp(first.name, "navfeeder-A1B2C3"));
    assert(!strcmp(first.username, first.name));
    assert(strlen(first.password) == 12);

    char qr[NETCFG_SETUP_QR_CAP];
    assert(netcfg_setup_qr_payload(&first, qr, sizeof qr) == ESP_OK);
    assert(strstr(qr, "\"ver\":\"v1\"") && strstr(qr, "\"transport\":\"ble\""));
    assert(strstr(qr, first.name) && strstr(qr, first.password));
    char tiny[8];
    assert(netcfg_setup_qr_payload(&first, tiny, sizeof tiny) == ESP_ERR_INVALID_SIZE);

    unsigned calls_after_create = random_calls;
    created = true;
    assert(netcfg_setup_load_or_create(&loaded, mac, deterministic_random, &created) == ESP_OK);
    assert(!created && writes == 1 && random_calls == calls_after_create);
    assert(!memcmp(&loaded, &first, sizeof first));

    // A damaged or transplanted record fails closed. It must never silently
    // mint a password that no longer matches the physical recovery label.
    durable[20] ^= 1;
    assert(netcfg_setup_load_or_create(&loaded, mac, deterministic_random, &created) ==
           ESP_ERR_INVALID_STATE);
    assert(!loaded.password[0] && writes == 1 && random_calls == calls_after_create);
    durable[20] ^= 1;
    const uint8_t other_mac[] = { 0x02, 0, 0, 0x11, 0x22, 0x33 };
    assert(netcfg_setup_load_or_create(&loaded, other_mac, deterministic_random, &created) ==
           ESP_ERR_INVALID_STATE);
    assert(!loaded.password[0]);

    read_error = ESP_FAIL;
    assert(netcfg_setup_load_or_create(&loaded, mac, deterministic_random, &created) == ESP_FAIL);
    assert(random_calls == calls_after_create);

    clear_store();
    write_error = ESP_ERR_NVS_NOT_ENOUGH_SPACE;
    assert(netcfg_setup_load_or_create(&loaded, mac, deterministic_random, &created) ==
           ESP_ERR_NVS_NOT_ENOUGH_SPACE);
    assert(!created && !loaded.password[0]);

    puts("netcfg setup credential: create-once, stable load, QR, and corruption refusal PASS");
    return 0;
}
