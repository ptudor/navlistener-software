#include "netcfg_setup.h"

#include <limits.h>
#include <stdio.h>
#include <string.h>
#include <sys/lock.h>

#include "nvs.h"

#define SETUP_NVS_NS "nvf_setup"
#define SETUP_RECORD_KEY "credential_v1"
#define SETUP_RECORD_SIZE 96
#define SETUP_PASSWORD_LEN 12

static _lock_t setup_lock;

static uint32_t record_crc(const uint8_t *data, size_t size)
{
    uint32_t crc = UINT32_MAX;
    for (size_t i = 0; i < size; i++) {
        crc ^= data[i];
        for (int bit = 0; bit < 8; bit++)
            crc = (crc >> 1) ^ (0xedb88320u & (0u - (crc & 1u)));
    }
    return ~crc;
}

static uint32_t get_le32(const uint8_t *p)
{
    return (uint32_t)p[0] | (uint32_t)p[1] << 8 |
           (uint32_t)p[2] << 16 | (uint32_t)p[3] << 24;
}

static void put_le32(uint8_t *p, uint32_t value)
{
    for (size_t i = 0; i < 4; i++) p[i] = (uint8_t)(value >> (8 * i));
}

static bool password_valid(const char *password)
{
    static const char alphabet[] =
        "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
    if (!password || strnlen(password, NETCFG_SETUP_PASSWORD_CAP) != SETUP_PASSWORD_LEN)
        return false;
    for (size_t i = 0; i < SETUP_PASSWORD_LEN; i++)
        if (!strchr(alphabet, password[i])) return false;
    return true;
}

static esp_err_t identity_for_mac(const uint8_t mac[6], char out[NETCFG_SETUP_NAME_CAP])
{
    int n = snprintf(out, NETCFG_SETUP_NAME_CAP, "navfeeder-%02X%02X%02X",
                     mac[3], mac[4], mac[5]);
    return n > 0 && n < NETCFG_SETUP_NAME_CAP ? ESP_OK : ESP_ERR_INVALID_SIZE;
}

static void generate_password(char out[NETCFG_SETUP_PASSWORD_CAP],
                              uint32_t (*random_word)(void))
{
    static const char alphabet[] =
        "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
    const uint32_t symbols = sizeof alphabet - 1;
    const uint32_t max_valid = (UINT32_MAX / symbols) * symbols;
    for (size_t i = 0; i < SETUP_PASSWORD_LEN; i++) {
        uint32_t value;
        do value = random_word(); while (value >= max_valid);
        out[i] = alphabet[value % symbols];
    }
    out[SETUP_PASSWORD_LEN] = '\0';
}

static void encode(uint8_t data[SETUP_RECORD_SIZE],
                   const netcfg_setup_credentials_t *credentials)
{
    memset(data, 0, SETUP_RECORD_SIZE);
    memcpy(data, "NFS1", 4);
    data[4] = 1;                         // little-endian format version
    data[6] = SETUP_RECORD_SIZE;         // little-endian record length
    memcpy(data + 8, credentials->name, sizeof credentials->name);
    memcpy(data + 32, credentials->username, sizeof credentials->username);
    memcpy(data + 56, credentials->password, sizeof credentials->password);
    put_le32(data + SETUP_RECORD_SIZE - 4,
             record_crc(data, SETUP_RECORD_SIZE - 4));
}

static esp_err_t decode(const uint8_t data[SETUP_RECORD_SIZE],
                        const char *expected_name,
                        netcfg_setup_credentials_t *out)
{
    if (memcmp(data, "NFS1", 4) || data[4] != 1 || data[5] != 0 ||
        data[6] != SETUP_RECORD_SIZE || data[7] != 0 ||
        get_le32(data + SETUP_RECORD_SIZE - 4) !=
            record_crc(data, SETUP_RECORD_SIZE - 4))
        return ESP_ERR_INVALID_STATE;

    memset(out, 0, sizeof *out);
    memcpy(out->name, data + 8, sizeof out->name);
    memcpy(out->username, data + 32, sizeof out->username);
    memcpy(out->password, data + 56, sizeof out->password);
    if (!memchr(out->name, 0, sizeof out->name) ||
        !memchr(out->username, 0, sizeof out->username) ||
        !memchr(out->password, 0, sizeof out->password) ||
        strcmp(out->name, expected_name) || strcmp(out->username, expected_name) ||
        !password_valid(out->password)) {
        memset(out, 0, sizeof *out);
        return ESP_ERR_INVALID_STATE;
    }
    return ESP_OK;
}

esp_err_t netcfg_setup_load_or_create(netcfg_setup_credentials_t *out,
                                      const uint8_t mac[6],
                                      uint32_t (*random_word)(void),
                                      bool *created)
{
    if (!out || !mac || !random_word || !created) return ESP_ERR_INVALID_ARG;
    *created = false;
    memset(out, 0, sizeof *out);

    char expected_name[NETCFG_SETUP_NAME_CAP];
    esp_err_t rc = identity_for_mac(mac, expected_name);
    if (rc != ESP_OK) return rc;

    _lock_acquire(&setup_lock);
    nvs_handle_t handle;
    rc = nvs_open(SETUP_NVS_NS, NVS_READWRITE, &handle);
    if (rc != ESP_OK) goto done;

    uint8_t data[SETUP_RECORD_SIZE];
    size_t size = sizeof data;
    rc = nvs_get_blob(handle, SETUP_RECORD_KEY, data, &size);
    if (rc == ESP_OK) {
        rc = size == sizeof data ? decode(data, expected_name, out)
                                 : ESP_ERR_INVALID_STATE;
    } else if (rc == ESP_ERR_NVS_NOT_FOUND) {
        snprintf(out->name, sizeof out->name, "%s", expected_name);
        snprintf(out->username, sizeof out->username, "%s", expected_name);
        generate_password(out->password, random_word);
        encode(data, out);
        rc = nvs_set_blob(handle, SETUP_RECORD_KEY, data, sizeof data);
        if (rc == ESP_OK) rc = nvs_commit(handle);
        if (rc == ESP_OK) *created = true;
        else memset(out, 0, sizeof *out);
    } else if (rc == ESP_ERR_NVS_INVALID_LENGTH) {
        rc = ESP_ERR_INVALID_STATE;
    }
    nvs_close(handle);

done:
    _lock_release(&setup_lock);
    return rc;
}

esp_err_t netcfg_setup_qr_payload(const netcfg_setup_credentials_t *credentials,
                                  char *out, size_t out_cap)
{
    if (!credentials || !out || !out_cap || !credentials->name[0] ||
        strcmp(credentials->name, credentials->username) ||
        !password_valid(credentials->password))
        return ESP_ERR_INVALID_ARG;
    int n = snprintf(out, out_cap,
                     "{\"ver\":\"v1\",\"name\":\"%s\",\"username\":\"%s\","
                     "\"pop\":\"%s\",\"transport\":\"ble\"}",
                     credentials->name, credentials->username,
                     credentials->password);
    return n >= 0 && (size_t)n < out_cap ? ESP_OK : ESP_ERR_INVALID_SIZE;
}
