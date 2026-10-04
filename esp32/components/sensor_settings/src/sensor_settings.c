#include "sensor_settings.h"
#include <string.h>
#include "nvs.h"

#define NAMESPACE "nvf_sensor"

bool sensor_settings_valid(const sensor_settings_t *s)
{
    return s && (s->mains_hz == 50 || s->mains_hz == 60) &&
           (s->motion == SENSOR_MOTION_SURFACE || s->motion == SENSOR_MOTION_AERIAL);
}

bool sensor_settings_parse_mains(const char *text, uint8_t *hz)
{
    if (!text || !hz) return false;
    if (!strcmp(text, "50")) *hz = 50;
    else if (!strcmp(text, "60")) *hz = 60;
    else return false;
    return true;
}

bool sensor_settings_parse_motion(const char *text, sensor_motion_t *motion)
{
    if (!text || !motion) return false;
    if (!strcmp(text, "surface")) *motion = SENSOR_MOTION_SURFACE;
    else if (!strcmp(text, "aerial")) *motion = SENSOR_MOTION_AERIAL;
    else return false;
    return true;
}

const char *sensor_motion_name(sensor_motion_t motion)
{
    return motion == SENSOR_MOTION_AERIAL ? "aerial" : "surface";
}

esp_err_t sensor_settings_load(sensor_settings_t *out)
{
    if (!out) return ESP_ERR_INVALID_ARG;
    *out = SENSOR_SETTINGS_DEFAULT;
    nvs_handle_t handle;
    esp_err_t err = nvs_open(NAMESPACE, NVS_READONLY, &handle);
    if (err == ESP_ERR_NVS_NOT_FOUND) return ESP_OK;
    if (err != ESP_OK) return err;
    sensor_settings_t stored = SENSOR_SETTINGS_DEFAULT;
    uint8_t motion = (uint8_t)stored.motion;
    err = nvs_get_u8(handle, "mains_hz", &stored.mains_hz);
    if (err == ESP_ERR_NVS_NOT_FOUND) err = ESP_OK;
    if (err == ESP_OK) err = nvs_get_u8(handle, "motion", &motion);
    if (err == ESP_ERR_NVS_NOT_FOUND) err = ESP_OK;
    nvs_close(handle);
    if (err != ESP_OK) return err;
    stored.motion = (sensor_motion_t)motion;
    if (!sensor_settings_valid(&stored)) return ESP_ERR_INVALID_ARG;
    *out = stored;
    return ESP_OK;
}

esp_err_t sensor_settings_save(const sensor_settings_t *s)
{
    if (!sensor_settings_valid(s)) return ESP_ERR_INVALID_ARG;
    nvs_handle_t handle;
    esp_err_t err = nvs_open(NAMESPACE, NVS_READWRITE, &handle);
    if (err != ESP_OK) return err;
    err = nvs_set_u8(handle, "mains_hz", s->mains_hz);
    if (err == ESP_OK) err = nvs_set_u8(handle, "motion", (uint8_t)s->motion);
    if (err == ESP_OK) err = nvs_commit(handle);
    nvs_close(handle);
    return err;
}
