// display_st7789 — ST7789 status display for the Waveshare ESP32-C6-LCD-1.47.
//
// 1.47" 172x320 IPS panel over SPI2, driven landscape (320x172) via native esp_lcd (no
// LVGL). Ported from apps/shepherdprotocol/esp32's driver for this exact board — same panel,
// same wiring, same 34-px dead-offset handling — with a navfeeder-specific dashboard.
// Backlight is PWM-capped at 50 % duty (panel overheating warning). The 8x8 font is a
// bring-up font; the "nicer fonts" upgrade (u8g2, docs/PLAN.md P4) drops in behind display_text.

#ifndef DISPLAY_ST7789_H
#define DISPLAY_ST7789_H

#include <stdint.h>
#include <stdbool.h>
#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

// RGB565 colors (standard order; the driver byte-swaps for the panel).
#define DISPLAY_BLACK   0x0000
#define DISPLAY_WHITE   0xFFFF
#define DISPLAY_RED     0xF800
#define DISPLAY_GREEN   0x07E0
#define DISPLAY_BLUE    0x001F
#define DISPLAY_YELLOW  0xFFE0
#define DISPLAY_CYAN    0x07FF
#define DISPLAY_ORANGE  0xFD20
#define DISPLAY_GREY    0x8410

#define DISPLAY_RGB(r, g, b) \
    ((uint16_t)(((((uint16_t)(r)) & 0xF8) << 8) | ((((uint16_t)(g)) & 0xFC) << 3) | (((uint16_t)(b)) >> 3)))

// display_init brings up SPI + the ST7789 panel + backlight, clears the screen, and draws the
// standing banners. Safe to call once at boot; on failure the display stays inactive and every
// other call becomes a no-op.
esp_err_t display_init(void);
bool display_is_ready(void);

void display_clear(uint16_t color);
void display_text(int x, int y, const char *s, uint16_t fg, uint16_t bg, int scale);
void display_backlight_set_percent(uint8_t percent);

// nvf_status_t is the live dashboard snapshot the app refreshes.
typedef struct {
    const char *station;    // observer/station id
    const char *collector;  // "host:port", or NULL if unprovisioned
    bool wifi_up;
    bool link_up;           // push link to the collector is authenticated + streaming
    uint32_t nav;           // total nav frames framed
    uint32_t nav_rate;      // nav frames in the last refresh window
    uint32_t telem;         // total telemetry frames
    uint32_t bad_ck;        // UBX checksum failures
    unsigned spool_depth;   // unacked records buffered
    uint64_t dropped;       // records lost to spool overflow
} nvf_status_t;

// display_render_status redraws the dashboard from st. Cheap enough to call every 1-2 s.
void display_render_status(const nvf_status_t *st);

#ifdef __cplusplus
}
#endif

#endif // DISPLAY_ST7789_H
