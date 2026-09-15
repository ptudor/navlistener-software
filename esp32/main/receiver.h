#pragma once
#include "esp_err.h"
#include "ubx.h"
#include "gnss_status.h"
esp_err_t receiver_start(ubx_parser_t *parser);
bool receiver_alive(void);
void receiver_status(gnss_status_t *out);
