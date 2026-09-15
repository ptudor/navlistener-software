#pragma once
#include "ota_policy.h"
#include "esp_err.h"
typedef struct { char url[NVF_OTA_URL_CAP]; uint8_t hash[32]; } nvf_ota_request_t;
esp_err_t nvf_ota_download(const nvf_ota_request_t *request);
// Stage without selecting a boot partition. A false progress result cancels and
// aborts the write; the callback may yield while collection needs the CPU/spool.
typedef bool (*nvf_ota_progress_fn)(size_t received,size_t total,void *context);
esp_err_t nvf_ota_stage(const nvf_ota_request_t *,nvf_ota_progress_fn,void *context);
