#include "panel_settings.h"
#include "panel_control.h"
#include "nvs.h"
#include <assert.h>
#include <stdbool.h>
#include <stdio.h>
#include <string.h>

static bool namespace_exists, opened;
static int open_mode;
static struct { const char *key; bool exists; uint8_t stored; } values[] = {{"brightness", false, 0}, {"trimmer", false, 0}};
static unsigned writes, commits;
static esp_err_t open_error, read_error, write_error, commit_error;

static unsigned slot(const char *key)
{
    for (unsigned i = 0; i < 2; i++) if (!strcmp(key, values[i].key)) return i;
    assert(!"unexpected key"); return 0;
}
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
    assert(opened && handle == 1);
    unsigned i = slot(key);
    if (read_error) return read_error;
    if (!values[i].exists) return ESP_ERR_NVS_NOT_FOUND;
    *value = values[i].stored;
    return ESP_OK;
}
esp_err_t nvs_set_u8(nvs_handle_t handle, const char *key, uint8_t value)
{
    assert(opened && handle == 1 && open_mode == NVS_READWRITE);
    unsigned i = slot(key);
    writes++;
    if (write_error) return write_error;
    // NVS can persist a successful set even if a later commit reports failure.
    values[i].stored = value;
    values[i].exists = true;
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
    unsigned percent = 99, trimmer = 99;
    assert(panel_brightness_load(&percent, &trimmer) == ESP_OK && percent == 20 && trimmer == 0);
    assert(!namespace_exists && !writes && !commits); // first boot does not write flash
    namespace_exists = true; // namespace already holds reception history
    assert(panel_brightness_load(&percent, &trimmer) == ESP_OK && percent == 20 && trimmer == 0);
    // A brightness saved before the trimmer reference existed loads with none.
    values[0].exists = true; values[0].stored = 50;
    assert(panel_brightness_load(&percent, &trimmer) == ESP_OK && percent == 50 && trimmer == 0);

    const unsigned levels[] = {10, 50, 20}, references[] = {0, 64, 12};
    for (unsigned i = 0; i < sizeof levels / sizeof levels[0]; i++) {
        assert(panel_brightness_save(levels[i], references[i]) == ESP_OK);
        percent = trimmer = 99; // discard runtime state, as on reboot
        assert(panel_brightness_load(&percent, &trimmer) == ESP_OK && percent == levels[i] && trimmer == references[i]);
        assert(writes == 2 * (i + 1) && commits == i + 1 && !opened);
    }
    values[0].stored = 255;
    assert(panel_brightness_load(&percent, &trimmer) == ESP_ERR_INVALID_ARG && percent == 20 && trimmer == 0);
    values[0].stored = 20; values[1].stored = 101;
    assert(panel_brightness_load(&percent, &trimmer) == ESP_ERR_INVALID_ARG && percent == 20 && trimmer == 0);
    values[1].stored = 12;
    read_error = ESP_ERR_NVS_TYPE_MISMATCH;
    assert(panel_brightness_load(&percent, &trimmer) == read_error && percent == 20 && !opened);
    read_error = ESP_OK;
    open_error = ESP_FAIL;
    assert(panel_brightness_load(&percent, &trimmer) == ESP_FAIL && percent == 20);
    assert(panel_brightness_save(10, 0) == ESP_FAIL && !opened);
    open_error = ESP_OK;
    write_error = ESP_FAIL;
    assert(panel_brightness_save(10, 0) == ESP_FAIL && commits == 3 && !opened);
    write_error = ESP_OK;
    commit_error = ESP_FAIL;
    assert(panel_brightness_save(10, 0) == ESP_FAIL && !opened);
    commit_error = ESP_OK;
    // Retrying the latest selection recovers even after an uncertain commit.
    assert(panel_brightness_save(50, 33) == ESP_OK);
    assert(panel_brightness_load(&percent, &trimmer) == ESP_OK && percent == 50 && trimmer == 33);
    unsigned previous_writes = writes;
    assert(panel_brightness_save(101, 0) == ESP_ERR_INVALID_ARG && writes == previous_writes);
    assert(panel_brightness_save(50, 101) == ESP_ERR_INVALID_ARG && writes == previous_writes);
    assert(panel_brightness_load(NULL, &trimmer) == ESP_ERR_INVALID_ARG && !opened);
    assert(panel_brightness_load(&percent, NULL) == ESP_ERR_INVALID_ARG && !opened);
    puts("panel brightness persistence tests passed");
}
