#pragma once
#include "esp_err.h"
#include "ubx.h"
#include "gnss_status.h"
// The receiver model the manifest lists, as MON-VER names it ("NEO-M9N", "ZED-X20P",
// "MAX-M10S"), or NULL, which configures no receiver. Call before receiver_start.
void receiver_set_module(const char *name);
esp_err_t receiver_start(ubx_parser_t *parser);
bool receiver_alive(void);
void receiver_status(gnss_status_t *out);
