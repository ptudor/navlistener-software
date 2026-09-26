#include "sensor_settings.h"
#include "nvs.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

static bool namespace_exists, opened;
static int open_mode;
static struct { const char *key; bool exists; uint8_t stored; } values[] = {{"mains_hz", false, 0}, {"motion", false, 0}};
static unsigned writes, commits;
static esp_err_t open_error, read_error, write_error, commit_error;

static unsigned slot(const char *key)
{
    for (unsigned i = 0; i < 2; i++) if (!strcmp(key, values[i].key)) return i;
    assert(!"unexpected key"); return 0;
}
esp_err_t nvs_open(const char *ns, int mode, nvs_handle_t *handle)
{
    assert(!opened && !strcmp(ns, "nvf_sensor"));
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

static bool same(sensor_settings_t a, sensor_settings_t b) { return a.mains_hz == b.mains_hz && a.motion == b.motion; }

int main(void)
{
    sensor_settings_t s = {.mains_hz = 1, .motion = SENSOR_MOTION_AERIAL};
    const sensor_settings_t defaults = SENSOR_SETTINGS_DEFAULT;
    assert(defaults.mains_hz == 60 && defaults.motion == SENSOR_MOTION_SURFACE);
    // An unconfigured unit uses the defaults without writing flash.
    assert(sensor_settings_load(&s) == ESP_OK && same(s, defaults) && !writes && !namespace_exists);
    namespace_exists = true;
    values[0].exists = true; values[0].stored = 50; // a notch saved alone keeps the default profile
    assert(sensor_settings_load(&s) == ESP_OK && s.mains_hz == 50 && s.motion == SENSOR_MOTION_SURFACE);

    const sensor_settings_t choices[] = {{50, SENSOR_MOTION_AERIAL}, {60, SENSOR_MOTION_AERIAL}, {60, SENSOR_MOTION_SURFACE}};
    for (unsigned i = 0; i < 3; i++) {
        assert(sensor_settings_save(&choices[i]) == ESP_OK);
        s = (sensor_settings_t){0};
        assert(sensor_settings_load(&s) == ESP_OK && same(s, choices[i]));
        assert(writes == 2 * (i + 1) && commits == i + 1 && !opened);
    }
    // Corrupt stored values fall back to the defaults and say so.
    values[0].stored = 55;
    assert(sensor_settings_load(&s) == ESP_ERR_INVALID_ARG && same(s, defaults) && !opened);
    values[0].stored = 50; values[1].stored = 2;
    assert(sensor_settings_load(&s) == ESP_ERR_INVALID_ARG && same(s, defaults));
    values[1].stored = 1;
    read_error = ESP_ERR_NVS_TYPE_MISMATCH;
    assert(sensor_settings_load(&s) == read_error && same(s, defaults) && !opened);
    read_error = ESP_OK;
    open_error = ESP_FAIL;
    assert(sensor_settings_load(&s) == ESP_FAIL && same(s, defaults));
    assert(sensor_settings_save(&choices[0]) == ESP_FAIL && !opened);
    open_error = ESP_OK;
    write_error = ESP_FAIL;
    assert(sensor_settings_save(&choices[0]) == ESP_FAIL && !opened);
    write_error = ESP_OK; commit_error = ESP_FAIL;
    assert(sensor_settings_save(&choices[0]) == ESP_FAIL && !opened);
    commit_error = ESP_OK;
    unsigned before = writes;
    const sensor_settings_t bad_notch = {45, SENSOR_MOTION_SURFACE}, bad_motion = {60, (sensor_motion_t)7};
    assert(sensor_settings_save(&bad_notch) == ESP_ERR_INVALID_ARG && sensor_settings_save(&bad_motion) == ESP_ERR_INVALID_ARG);
    assert(sensor_settings_save(NULL) == ESP_ERR_INVALID_ARG && writes == before);
    assert(sensor_settings_load(NULL) == ESP_ERR_INVALID_ARG && !opened);

    uint8_t hz = 0; sensor_motion_t motion = SENSOR_MOTION_SURFACE;
    assert(sensor_settings_parse_mains("50", &hz) && hz == 50 && sensor_settings_parse_mains("60", &hz) && hz == 60);
    assert(!sensor_settings_parse_mains("", &hz) && !sensor_settings_parse_mains("600", &hz) && !sensor_settings_parse_mains(NULL, &hz));
    assert(sensor_settings_parse_motion("aerial", &motion) && motion == SENSOR_MOTION_AERIAL);
    assert(sensor_settings_parse_motion("surface", &motion) && motion == SENSOR_MOTION_SURFACE);
    assert(!sensor_settings_parse_motion("Aerial", &motion) && !sensor_settings_parse_motion("", &motion));
    assert(!strcmp(sensor_motion_name(SENSOR_MOTION_AERIAL), "aerial") && !strcmp(sensor_motion_name(SENSOR_MOTION_SURFACE), "surface"));
    puts("sensor settings: defaults, persistence, corruption fallback and setup-page values passed");
    return 0;
}
