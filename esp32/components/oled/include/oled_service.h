#ifndef OLED_SERVICE_H
#define OLED_SERVICE_H
// The status display's task: a 128x64 SSD1306 module on a board's display header, on the
// shared I2C bus. The module is an accessory that may be fitted, unplugged or plugged in at
// any time, so the task looks for it, configures it when it answers, and looks again when it
// stops; a board runs the same with or without one.
//
// Once a second it asks the application for the model (oled_pages.h), draws the screen that
// is due and writes the bytes that changed. The bus is everyone's: each write is at most
// SSD1306_CHUNK display bytes (3 ms at 100 kHz) and the task steps aside for a tick after
// every one, so a sensor read never waits longer than that, and oled_service_hold keeps the
// display off the bus altogether for a transfer that must not be stretched.
#include <stdbool.h>
#include <stdint.h>
#include "driver/i2c_master.h"
#include "esp_err.h"
#include "oled_pages.h"

typedef struct {
    i2c_master_bus_handle_t bus;
    uint8_t address; // the module's, 0x3c or 0x3d
    // Fills the model for the frame about to be drawn; called from the display task. The
    // model is zeroed before the call.
    void (*model)(oled_model_t *out);
} oled_service_config_t;
// Adds the device (100 kHz, as the bus's other parts) and starts the task. Every other call
// is a no-op before this has succeeded.
esp_err_t oled_service_start(const oled_service_config_t *config);
// Keeps the display's transfers off the bus until oled_service_release, waiting for one in
// progress to finish first: for the panel chain's readback, whose I2C reads hold the lamps
// dark, and for a session with the secure element, which the display's zero bytes would wake
// and whose commands they would fall between. Callers may hold at the same time and do not
// wait for each other. False, with nothing held and nothing to release, while the service is
// not running.
bool oled_service_hold(void);
void oled_service_release(void);
// Shows the next page now and holds it for OLED_HOLD_MS: a button press.
void oled_service_next_page(void);
// Turns the picture half a turn or back, at once, and saves the choice (NVS namespace
// nvf_oled), which the unit keeps: how its module is mounted.
esp_err_t oled_service_set_rotated(bool rotated);
// Turns the panel off, with its charge pump, and keeps the display off the bus, or turns it
// on again; until the next start, and not saved. For the bench: reception with the panel dark
// and the bus quiet, against reception with the display at work.
void oled_service_set_power(bool on);

typedef struct {
    bool started, present, rotated, status_tracks, powered;
    uint8_t address, status, contrast;
    oled_screen_t screen;
    oled_page_t page;
    // Since start: times the module was configured, times it stopped answering, times it
    // was found reset, frames drawn, and the traffic they took.
    uint32_t attached, lost, resets, frames, writes, bytes, failures;
} oled_service_status_t;
void oled_service_status(oled_service_status_t *out);
// The frame as last drawn, or with a page given, that page drawn from the latest model.
// False before the first frame.
bool oled_service_frame(const oled_page_t *page, oled_canvas_t *out);
#endif
