#ifndef NVF_BOARD_H
#define NVF_BOARD_H
#include "esp_err.h"
#include "hardware_manifest.h"
void observer_board_manifest(const hardware_manifest_result_t *manifest, uint64_t (*now_ns)(void));
// Thread-safe request, applied and saved by the board task. First-boot default
// 20%; subsequent boots restore the saved setting before enabling PWM.
void observer_board_set_brightness(unsigned percent);
void observer_board_cycle_brightness(void);
// Peripheral identification, GNSS-only RTC startup and the two-colour panel.
esp_err_t observer_board_start(void);
#endif
