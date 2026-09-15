#ifndef PANEL_SETTINGS_H
#define PANEL_SETTINGS_H
#include "esp_err.h"

// Call after NVS initialization, before enabling the panel. Missing settings
// use PANEL_DEFAULT_BRIGHTNESS; read errors also leave that safe default set.
esp_err_t panel_brightness_load(unsigned *percent);
// The board task saves changes; loading a preference never writes flash.
esp_err_t panel_brightness_save(unsigned percent);
#endif
