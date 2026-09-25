#include "panel_settings.h"
#include "panel_control.h"
#include "nvs.h"

static esp_err_t read_percent(nvs_handle_t handle, const char *key, unsigned *out)
{
    uint8_t stored;
    esp_err_t err = nvs_get_u8(handle, key, &stored);
    if (err == ESP_ERR_NVS_NOT_FOUND) return ESP_OK;
    if (err != ESP_OK) return err;
    if (stored > 100) return ESP_ERR_INVALID_ARG;
    *out = stored;
    return ESP_OK;
}

esp_err_t panel_brightness_load(unsigned *percent, unsigned *trimmer)
{
    if (!percent || !trimmer) return ESP_ERR_INVALID_ARG;
    *percent = PANEL_DEFAULT_BRIGHTNESS;
    *trimmer = 0;
    nvs_handle_t handle;
    esp_err_t err = nvs_open("nvf_panel", NVS_READONLY, &handle);
    if (err == ESP_ERR_NVS_NOT_FOUND) return ESP_OK;
    if (err != ESP_OK) return err;
    unsigned level = PANEL_DEFAULT_BRIGHTNESS, reference = 0;
    err = read_percent(handle, "brightness", &level);
    if (err == ESP_OK) err = read_percent(handle, "trimmer", &reference);
    nvs_close(handle);
    if (err != ESP_OK) return err;
    *percent = level;
    *trimmer = reference;
    return ESP_OK;
}

esp_err_t panel_brightness_save(unsigned percent, unsigned trimmer)
{
    if (percent > 100 || trimmer > 100) return ESP_ERR_INVALID_ARG;
    nvs_handle_t handle;
    esp_err_t err = nvs_open("nvf_panel", NVS_READWRITE, &handle);
    if (err != ESP_OK) return err;
    err = nvs_set_u8(handle, "brightness", (uint8_t)percent);
    if (err == ESP_OK) err = nvs_set_u8(handle, "trimmer", (uint8_t)trimmer);
    if (err == ESP_OK) err = nvs_commit(handle);
    nvs_close(handle);
    return err;
}
