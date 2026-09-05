// One NVS blob is the configuration transaction, including the reset marker.
// ESP-IDF 5.5 NVS writes the alternate blob chunk generation, publishes its
// index, then retires the old index. Reboot recovery resolves duplicate indices.
// nvs_commit is NOT a rollback boundary: even a failed set/commit can leave the
// complete new blob durable. Never split credentials across independently set keys.
#include "netcfg.h"
#include "nvs.h"
#include "sdkconfig.h"
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/lock.h>

#ifndef CONFIG_NVF_INSECURE
#define CONFIG_NVF_INSECURE 0
#endif
#define NVS_NS "navfeeder"
#define RECORD_KEY "config_v1"
#define RECORD_SIZE 348
static _lock_t storage_lock;

static uint64_t get_le(const uint8_t *p, size_t n)
{
    uint64_t v = 0;
    for (size_t i = 0; i < n; i++) v |= (uint64_t)p[i] << (8 * i);
    return v;
}
static void put_le(uint8_t *p, uint64_t v, size_t n)
{
    for (size_t i = 0; i < n; i++) p[i] = (uint8_t)(v >> (8 * i));
}
static uint32_t record_crc(const uint8_t *p, size_t n)
{
    uint32_t crc = UINT32_MAX;
    for (size_t i = 0; i < n; i++) {
        crc ^= p[i];
        for (int j = 0; j < 8; j++) crc = (crc >> 1) ^ (0xedb88320u & (0u - (crc & 1u)));
    }
    return ~crc;
}
static bool terminated(const netcfg_t *c)
{
    return c && memchr(c->wifi_ssid, 0, sizeof c->wifi_ssid) &&
        memchr(c->wifi_pass, 0, sizeof c->wifi_pass) && memchr(c->host, 0, sizeof c->host) &&
        memchr(c->token, 0, sizeof c->token) && memchr(c->station, 0, sizeof c->station);
}
static void encode(uint8_t data[RECORD_SIZE], const netcfg_t *cfg, uint64_t generation, bool reset)
{
    memset(data, 0, RECORD_SIZE);
    memcpy(data, "NFC1", 4);
    put_le(data + 4, 1, 2); // format version
    put_le(data + 6, RECORD_SIZE, 2);
    put_le(data + 8, generation, 8);
    data[16] = reset;
    data[17] = cfg->insecure;
    put_le(data + 18, cfg->port, 2);
    size_t offset = 20;
#define FIELD(name) do { memcpy(data + offset, cfg->name, strlen(cfg->name)); offset += sizeof cfg->name; } while (0)
    FIELD(wifi_ssid); FIELD(wifi_pass); FIELD(host); FIELD(token); FIELD(station);
#undef FIELD
    put_le(data + RECORD_SIZE - 4, record_crc(data, RECORD_SIZE - 4), 4);
}
static esp_err_t read_record(nvs_handle_t h, netcfg_t *out, uint64_t *generation, bool *reset)
{
    uint8_t data[RECORD_SIZE];
    size_t size = sizeof data;
    esp_err_t rc = nvs_get_blob(h, RECORD_KEY, data, &size);
    if (rc == ESP_ERR_NVS_INVALID_LENGTH) return ESP_ERR_INVALID_STATE;
    if (rc != ESP_OK) return rc;
    if (size != RECORD_SIZE || memcmp(data, "NFC1", 4) || get_le(data + 4, 2) != 1 ||
        get_le(data + 6, 2) != RECORD_SIZE || get_le(data + 8, 8) == 0 ||
        data[16] > 1 || data[17] > 1 ||
        get_le(data + RECORD_SIZE - 4, 4) != record_crc(data, RECORD_SIZE - 4)) return ESP_ERR_INVALID_STATE;
    memset(out, 0, sizeof *out);
    out->port = (int)get_le(data + 18, 2);
    out->insecure = data[17];
    size_t offset = 20;
#define FIELD(name) do { memcpy(out->name, data + offset, sizeof out->name); offset += sizeof out->name; } while (0)
    FIELD(wifi_ssid); FIELD(wifi_pass); FIELD(host); FIELD(token); FIELD(station);
#undef FIELD
    *generation = get_le(data + 8, 8);
    *reset = data[16];
    if (!terminated(out) || (!*reset && !netcfg_validate(out, NULL, 0))) return ESP_ERR_INVALID_STATE;
    if (*reset) memset(out, 0, sizeof *out);
    return ESP_OK;
}

static void defaults(netcfg_t *out)
{
    memset(out, 0, sizeof *out);
    snprintf(out->wifi_ssid, sizeof out->wifi_ssid, "%s", CONFIG_NVF_WIFI_SSID);
    snprintf(out->wifi_pass, sizeof out->wifi_pass, "%s", CONFIG_NVF_WIFI_PASS);
    snprintf(out->host, sizeof out->host, "%s", CONFIG_NVF_COLLECTOR_HOST);
    out->port = CONFIG_NVF_COLLECTOR_PORT;
    snprintf(out->token, sizeof out->token, "%s", CONFIG_NVF_TOKEN);
    snprintf(out->station, sizeof out->station, "%s", CONFIG_NVF_STATION);
    out->insecure = CONFIG_NVF_INSECURE;
}

// Migration reads the existing effective configuration once. Legacy keys remain
// untouched until a complete new blob is published; once present, that blob is
// authoritative, including reset state. Corrupt/unreadable blobs never fall back
// to stale per-field keys or compiled credentials.
static esp_err_t legacy(nvs_handle_t h, netcfg_t *out)
{
    uint8_t reset = 0;
    esp_err_t rc = nvs_get_u8(h, "reset", &reset);
    if (rc != ESP_OK && rc != ESP_ERR_NVS_NOT_FOUND) return rc;
    if (reset == 1) { memset(out, 0, sizeof *out); return ESP_OK; }
#define READ(call) do { rc = (call); if (rc != ESP_OK && rc != ESP_ERR_NVS_NOT_FOUND) return rc; } while (0)
#define FIELD(key, name) do { size_t cap = sizeof out->name; READ(nvs_get_str(h, key, out->name, &cap)); } while (0)
    FIELD("ssid", wifi_ssid); FIELD("pass", wifi_pass); FIELD("host", host);
    FIELD("token", token); FIELD("station", station);
#undef FIELD
    int32_t port = out->port;
    READ(nvs_get_i32(h, "port", &port));
    out->port = port;
    uint8_t insecure = out->insecure;
    READ(nvs_get_u8(h, "insecure", &insecure));
    if (insecure > 1) return ESP_ERR_INVALID_STATE;
    out->insecure = insecure;
#undef READ
    return ESP_OK;
}

bool netcfg_load(netcfg_t *out, char *err, size_t errcap)
{
    _lock_acquire(&storage_lock);
    defaults(out);
    nvs_handle_t h;
    esp_err_t rc = nvs_open(NVS_NS, NVS_READONLY, &h);
    if (rc == ESP_OK) {
        uint64_t generation;
        bool reset;
        rc = read_record(h, out, &generation, &reset);
        if (rc == ESP_ERR_NVS_NOT_FOUND) rc = legacy(h, out);
        nvs_close(h);
    } else if (rc == ESP_ERR_NVS_NOT_FOUND) rc = ESP_OK;
    if (rc != ESP_OK) {
        memset(out, 0, sizeof *out);
        if (err && errcap) snprintf(err, errcap, "stored config unreadable");
    }
    bool valid = rc == ESP_OK && netcfg_validate(out, err, errcap);
    _lock_release(&storage_lock);
    return valid;
}

static esp_err_t replace(const netcfg_t *cfg, bool reset)
{
    nvs_handle_t h;
    esp_err_t rc = nvs_open(NVS_NS, NVS_READWRITE, &h);
    if (rc != ESP_OK) return rc;
    netcfg_t prior;
    uint64_t generation = 0;
    bool prior_reset;
    rc = read_record(h, &prior, &generation, &prior_reset);
    // An explicitly supplied complete replacement or physical reset can repair
    // an invalid versioned record, but never turns a transient read error into
    // permission to write. Unknown future formats fail closed in netcfg_load.
    if (rc == ESP_ERR_NVS_NOT_FOUND || rc == ESP_ERR_INVALID_STATE) { generation = 0; rc = ESP_OK; }
    if (rc == ESP_OK && generation == UINT64_MAX) rc = ESP_ERR_INVALID_STATE;
    if (rc == ESP_OK) {
        uint8_t data[RECORD_SIZE];
        encode(data, cfg, generation + 1, reset);
        rc = nvs_set_blob(h, RECORD_KEY, data, sizeof data);
        if (rc == ESP_OK) rc = nvs_commit(h);
    }
    nvs_close(h);
    return rc;
}
esp_err_t netcfg_save(const netcfg_t *cfg)
{
    if (!terminated(cfg) || !netcfg_validate(cfg, NULL, 0)) return ESP_ERR_INVALID_ARG;
    _lock_acquire(&storage_lock);
    esp_err_t rc = replace(cfg, false);
    _lock_release(&storage_lock);
    return rc;
}
esp_err_t netcfg_reset_provisioning(void)
{
    const netcfg_t empty = {0};
    _lock_acquire(&storage_lock);
    esp_err_t rc = replace(&empty, true);
    _lock_release(&storage_lock);
    return rc;
}
