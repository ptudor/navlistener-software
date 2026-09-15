#pragma once
#include "esp_err.h"
#include "timing_report.h"
esp_err_t pulse_timing_start(void);
// Single board-task caller. Both capture channels share one APB timer.
void pulse_timing_poll(uint64_t now_us, report_timing_t *out);
