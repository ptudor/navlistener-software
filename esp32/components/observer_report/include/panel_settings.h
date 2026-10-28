#ifndef PANEL_SETTINGS_H
#define PANEL_SETTINGS_H
#include <stdint.h>
#include "esp_err.h"

// Call after NVS initialization, before enabling the panel. Missing settings use
// PANEL_DEFAULT_BRIGHTNESS and no trimmer reference (0); read errors also leave those
// safe defaults set. trimmer is the brightness trimmer's position when the level was
// last chosen (panel_brightness_t), on boards that have one.
esp_err_t panel_brightness_load(unsigned *percent, unsigned *trimmer);
// The board task saves changes, both values in one commit; loading never writes flash.
esp_err_t panel_brightness_save(unsigned percent, unsigned trimmer);
// Counts this boot for the panel's boot sequence (panel_boot.h), one commit per boot, and
// reports its number, 1 for the first. *boot is 0 when the saved count cannot be read, and
// holds this boot's number when only saving it failed. The count stops at UINT32_MAX.
esp_err_t panel_boot_count(uint32_t *boot);
#endif
