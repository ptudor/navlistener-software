// status_led — the onboard WS2812 status indicator (Waveshare ESP32-C6-LCD-1.47, GPIO8).
//
// Driven by the RMT peripheral via ESP-IDF's built-in bytes encoder (no FastLED/NeoPixel, no
// external component). A single addressable LED whose color encodes the feeder's state at a
// glance — the LED counterpart of the display dashboard.

#ifndef STATUS_LED_H
#define STATUS_LED_H

#include <stdint.h>
#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

typedef enum {
    LED_BOOT = 0,      // blue   — booting
    LED_NO_WIFI,       // red    — no WiFi
    LED_NO_LINK,       // yellow — WiFi up, collector link down
    LED_SPOOL_FILLING, // orange — streaming but the spool is backing up
    LED_STREAMING,     // green  — connected and draining
} led_state_t;

esp_err_t status_led_init(void);
void status_led_set_rgb(uint8_t r, uint8_t g, uint8_t b);
void status_led_state(led_state_t s);

#ifdef __cplusplus
}
#endif

#endif // STATUS_LED_H
