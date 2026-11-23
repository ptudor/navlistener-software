#include "receiver_settings.h"
#include <string.h>
#include "nvs.h"

#define NAMESPACE "nvf_rx"

bool receiver_settings_valid(const receiver_settings_t *s)
{
    return s && (s->lna == RECEIVER_LNA_LOW || s->lna == RECEIVER_LNA_NORMAL);
}

bool receiver_settings_parse_lna(const char *text, receiver_lna_t *lna)
{
    if (!text || !lna) return false;
    if (!strcmp(text, "low")) *lna = RECEIVER_LNA_LOW;
    else if (!strcmp(text, "normal")) *lna = RECEIVER_LNA_NORMAL;
    else return false;
    return true;
}

const char *receiver_lna_name(receiver_lna_t lna)
{
    return lna == RECEIVER_LNA_NORMAL ? "normal" : "low";
}

esp_err_t receiver_settings_load(receiver_settings_t *out)
{
    if (!out) return ESP_ERR_INVALID_ARG;
    *out = RECEIVER_SETTINGS_DEFAULT;
    nvs_handle_t handle;
    esp_err_t err = nvs_open(NAMESPACE, NVS_READONLY, &handle);
    if (err == ESP_ERR_NVS_NOT_FOUND) return ESP_OK;
    if (err != ESP_OK) return err;
    receiver_settings_t stored = RECEIVER_SETTINGS_DEFAULT;
    uint8_t lna = (uint8_t)stored.lna;
    err = nvs_get_u8(handle, "lna", &lna);
    if (err == ESP_ERR_NVS_NOT_FOUND) err = ESP_OK;
    nvs_close(handle);
    if (err != ESP_OK) return err;
    stored.lna = (receiver_lna_t)lna;
    if (!receiver_settings_valid(&stored)) return ESP_ERR_INVALID_ARG;
    *out = stored;
    return ESP_OK;
}

esp_err_t receiver_settings_save(const receiver_settings_t *s)
{
    if (!receiver_settings_valid(s)) return ESP_ERR_INVALID_ARG;
    nvs_handle_t handle;
    esp_err_t err = nvs_open(NAMESPACE, NVS_READWRITE, &handle);
    if (err != ESP_OK) return err;
    err = nvs_set_u8(handle, "lna", (uint8_t)s->lna);
    if (err == ESP_OK) err = nvs_commit(handle);
    nvs_close(handle);
    return err;
}
