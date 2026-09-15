#include "panel_settings.h"
#include "panel_control.h"
#include "nvs.h"
#include <assert.h>
#include <stdbool.h>
#include <stdio.h>
#include <string.h>

static bool namespace_exists, value_exists, opened;
static int open_mode;
static uint8_t stored;
static unsigned writes, commits;
static esp_err_t open_error, read_error, write_error, commit_error;

esp_err_t nvs_open(const char *ns, int mode, nvs_handle_t *handle)
{
    assert(!opened && !strcmp(ns, "nvf_panel"));
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
    assert(opened && handle == 1 && !strcmp(key, "brightness"));
    if (read_error) return read_error;
    if (!value_exists) return ESP_ERR_NVS_NOT_FOUND;
    *value = stored;
    return ESP_OK;
}
esp_err_t nvs_set_u8(nvs_handle_t handle, const char *key, uint8_t value)
{
    assert(opened && handle == 1 && open_mode == NVS_READWRITE && !strcmp(key, "brightness"));
    writes++;
    if (write_error) return write_error;
    // NVS can persist a successful set even if a later commit reports failure.
    stored = value;
    value_exists = true;
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
    unsigned percent = 99;
    assert(panel_brightness_load(&percent) == ESP_OK && percent == 20);
    assert(!namespace_exists && !writes && !commits); // first boot does not write flash
    namespace_exists = true; // namespace already holds reception history
    assert(panel_brightness_load(&percent) == ESP_OK && percent == 20);

    const unsigned levels[] = {10, 50, 20};
    for (unsigned i = 0; i < sizeof levels / sizeof levels[0]; i++) {
        percent = panel_next_brightness(percent);
        assert(percent == levels[i]);
        assert(panel_brightness_save(percent) == ESP_OK);
        percent = 99; // discard runtime state, as on reboot
        assert(panel_brightness_load(&percent) == ESP_OK && percent == levels[i]);
        assert(writes == i + 1 && commits == i + 1 && !opened);
    }
    stored = 255;
    assert(panel_brightness_load(&percent) == ESP_ERR_INVALID_ARG && percent == 20);
    read_error = ESP_ERR_NVS_TYPE_MISMATCH;
    assert(panel_brightness_load(&percent) == read_error && percent == 20 && !opened);
    read_error = ESP_OK;
    open_error = ESP_FAIL;
    assert(panel_brightness_load(&percent) == ESP_FAIL && percent == 20);
    assert(panel_brightness_save(10) == ESP_FAIL && !opened);
    open_error = ESP_OK;
    write_error = ESP_FAIL;
    assert(panel_brightness_save(10) == ESP_FAIL && commits == 3 && !opened);
    write_error = ESP_OK;
    commit_error = ESP_FAIL;
    assert(panel_brightness_save(10) == ESP_FAIL && !opened);
    commit_error = ESP_OK;
    // Retrying the latest selection recovers even after an uncertain commit.
    assert(panel_brightness_save(50) == ESP_OK);
    assert(panel_brightness_load(&percent) == ESP_OK && percent == 50);
    unsigned previous_writes = writes;
    assert(panel_brightness_save(101) == ESP_ERR_INVALID_ARG && writes == previous_writes);
    assert(panel_brightness_load(NULL) == ESP_ERR_INVALID_ARG && !opened);
    puts("panel brightness persistence tests passed");
}
