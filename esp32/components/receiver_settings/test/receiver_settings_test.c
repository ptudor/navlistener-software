#include "receiver_settings.h"
#include "nvs.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

static bool namespace_exists, opened, exists;
static int open_mode;
static uint8_t stored;
static unsigned writes, commits;
static esp_err_t open_error, read_error, write_error, commit_error;

esp_err_t nvs_open(const char *ns, int mode, nvs_handle_t *handle)
{
    assert(!opened && !strcmp(ns, "nvf_rx"));
    if (open_error) return open_error;
    if (!namespace_exists && mode == NVS_READONLY) return ESP_ERR_NVS_NOT_FOUND;
    namespace_exists = opened = true;
    open_mode = mode;
    *handle = 1;
    return ESP_OK;
}
void nvs_close(nvs_handle_t handle) { assert(opened && handle == 1); opened = false; }
esp_err_t nvs_get_u8(nvs_handle_t handle, const char *key, uint8_t *value)
{
    assert(opened && handle == 1 && !strcmp(key, "lna"));
    if (read_error) return read_error;
    if (!exists) return ESP_ERR_NVS_NOT_FOUND;
    *value = stored;
    return ESP_OK;
}
esp_err_t nvs_set_u8(nvs_handle_t handle, const char *key, uint8_t value)
{
    assert(opened && handle == 1 && open_mode == NVS_READWRITE && !strcmp(key, "lna"));
    writes++;
    if (write_error) return write_error;
    stored = value;
    exists = true;
    return ESP_OK;
}
esp_err_t nvs_commit(nvs_handle_t handle)
{
    assert(opened && handle == 1 && open_mode == NVS_READWRITE);
    commits++;
    return commit_error;
}

int main(void)
{
    receiver_settings_t s = {.lna = RECEIVER_LNA_NORMAL};
    const receiver_settings_t defaults = RECEIVER_SETTINGS_DEFAULT;
    assert(defaults.lna == RECEIVER_LNA_LOW);
    // An unconfigured unit uses the default, low gain, without writing flash: with no
    // namespace, and with a namespace that holds no value.
    assert(receiver_settings_load(&s) == ESP_OK && s.lna == RECEIVER_LNA_LOW && !writes && !namespace_exists);
    namespace_exists = true;
    s.lna = RECEIVER_LNA_NORMAL;
    assert(receiver_settings_load(&s) == ESP_OK && s.lna == RECEIVER_LNA_LOW && !writes && !opened);

    const receiver_lna_t choices[] = {RECEIVER_LNA_NORMAL, RECEIVER_LNA_LOW, RECEIVER_LNA_NORMAL};
    for (unsigned i = 0; i < 3; i++) {
        const receiver_settings_t chosen = {.lna = choices[i]};
        assert(receiver_settings_save(&chosen) == ESP_OK);
        s.lna = choices[i] == RECEIVER_LNA_LOW ? RECEIVER_LNA_NORMAL : RECEIVER_LNA_LOW;
        assert(receiver_settings_load(&s) == ESP_OK && s.lna == choices[i]);
        assert(writes == i + 1 && commits == i + 1 && !opened);
    }
    // A corrupt stored value falls back to the default and says so.
    stored = 2;
    assert(receiver_settings_load(&s) == ESP_ERR_INVALID_ARG && s.lna == RECEIVER_LNA_LOW && !opened);
    stored = 1;
    read_error = ESP_ERR_NVS_TYPE_MISMATCH;
    assert(receiver_settings_load(&s) == read_error && s.lna == RECEIVER_LNA_LOW && !opened);
    read_error = ESP_OK;
    open_error = ESP_FAIL;
    const receiver_settings_t normal = {.lna = RECEIVER_LNA_NORMAL};
    assert(receiver_settings_load(&s) == ESP_FAIL && s.lna == RECEIVER_LNA_LOW);
    assert(receiver_settings_save(&normal) == ESP_FAIL && !opened);
    open_error = ESP_OK;
    write_error = ESP_FAIL;
    assert(receiver_settings_save(&normal) == ESP_FAIL && !opened);
    write_error = ESP_OK; commit_error = ESP_FAIL;
    assert(receiver_settings_save(&normal) == ESP_FAIL && !opened);
    commit_error = ESP_OK;
    const unsigned before = writes;
    const receiver_settings_t bad = {.lna = (receiver_lna_t)7};
    assert(receiver_settings_save(&bad) == ESP_ERR_INVALID_ARG && receiver_settings_save(NULL) == ESP_ERR_INVALID_ARG);
    assert(writes == before && receiver_settings_load(NULL) == ESP_ERR_INVALID_ARG && !opened);
    assert(!receiver_settings_valid(&bad) && !receiver_settings_valid(NULL) && receiver_settings_valid(&normal));

    receiver_lna_t lna = RECEIVER_LNA_LOW;
    assert(receiver_settings_parse_lna("normal", &lna) && lna == RECEIVER_LNA_NORMAL);
    assert(receiver_settings_parse_lna("low", &lna) && lna == RECEIVER_LNA_LOW);
    assert(!receiver_settings_parse_lna("Low", &lna) && !receiver_settings_parse_lna("", &lna));
    assert(!receiver_settings_parse_lna("low ", &lna) && !receiver_settings_parse_lna(NULL, &lna));
    assert(!receiver_settings_parse_lna("low", NULL) && lna == RECEIVER_LNA_LOW);
    assert(!strcmp(receiver_lna_name(RECEIVER_LNA_LOW), "low") && !strcmp(receiver_lna_name(RECEIVER_LNA_NORMAL), "normal"));
    puts("receiver settings: default, persistence, corruption fallback and console values passed");
    return 0;
}
