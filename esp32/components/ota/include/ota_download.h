#pragma once
#include "ota_policy.h"
#include "esp_err.h"
typedef struct { char url[NVF_OTA_URL_CAP]; uint8_t hash[32]; } nvf_ota_request_t;
esp_err_t nvf_ota_download(const nvf_ota_request_t *request);
