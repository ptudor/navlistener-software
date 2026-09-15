#include "panel_settings.h"
#include "panel_control.h"
#include "nvs.h"

esp_err_t panel_brightness_load(unsigned *percent)
{
    if (!percent) return ESP_ERR_INVALID_ARG;
    *percent = PANEL_DEFAULT_BRIGHTNESS;
    nvs_handle_t handle;
    esp_err_t err = nvs_open("nvf_panel", NVS_READONLY, &handle);
    if (err == ESP_ERR_NVS_NOT_FOUND) return ESP_OK;
    if (err != ESP_OK) return err;
    uint8_t stored;
    err = nvs_get_u8(handle, "brightness", &stored);
    nvs_close(handle);
    if (err == ESP_ERR_NVS_NOT_FOUND) return ESP_OK;
    if (err != ESP_OK) return err;
    if (stored > 100) return ESP_ERR_INVALID_ARG;
    *percent = stored;
    return ESP_OK;
}

esp_err_t panel_brightness_save(unsigned percent)
{
    if (percent > 100) return ESP_ERR_INVALID_ARG;
    nvs_handle_t handle;
    esp_err_t err = nvs_open("nvf_panel", NVS_READWRITE, &handle);
    if (err != ESP_OK) return err;
    err = nvs_set_u8(handle, "brightness", (uint8_t)percent);
    if (err == ESP_OK) err = nvs_commit(handle);
    nvs_close(handle);
    return err;
}
