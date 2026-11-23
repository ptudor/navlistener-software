#ifndef SSD1306_H
#define SSD1306_H
// The SSD1306 as the status display uses it: its configuration, and a flush that writes only
// the display memory that changed. Pure C: every transfer goes through a callback, so the
// host tests run the flush against a model of the controller (test/ssd1306_test.c).
// Solomon Systech SSD1306 Rev 1.1, sections 8.1.5 (I2C), 9 and 10 (commands).
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "oled_canvas.h"

// A write's first byte says what follows it until STOP (8.1.5.2, Co = 0).
#define SSD1306_COMMANDS 0x00 // command bytes
#define SSD1306_DATA     0x40 // display memory bytes
// Display memory bytes per write. At 100 kHz a full write holds the shared bus for 3 ms,
// which is as long as another part's transfer can then wait for it.
#define SSD1306_CHUNK 32
// The status register (table 9-2): D6 is set while the display is off. A controller that has
// lost its supply comes back off, so this bit is how a re-plugged module is seen.
#define SSD1306_STATUS_OFF 0x40
// A run of unchanged bytes this long or shorter inside a changed span is written again:
// addressing a second span costs about as much bus time as nine bytes.
#define SSD1306_JOIN 8

// One write to the controller, the control byte included. False ends the operation.
typedef bool (*ssd1306_write_fn)(void *ctx, const uint8_t *bytes, size_t length);

// The whole configuration. With off it starts by turning the display off: sent once after
// the module answers, before its memory is written, so that what the memory held at power-up
// is never shown. Without, it leaves the display as it is: the same settings again, to put
// right whatever a disturbance on the bus may have changed. rotated turns the picture half a
// turn, for a module mounted with its header at the bottom; the memory must be written again
// after a change of it, since the controller maps columns as they are written. Returns the
// length written to out.
#define SSD1306_SETUP_MAX 32
size_t ssd1306_setup(uint8_t out[SSD1306_SETUP_MAX], bool off, bool rotated, uint8_t contrast);
// Display on or off; the memory is kept either way. Two bytes.
size_t ssd1306_power(uint8_t out[2], bool on);
// Contrast, 0 to 255. Three bytes.
size_t ssd1306_contrast(uint8_t out[3], uint8_t contrast);

// What the controller's memory holds, as far as the writes that succeeded tell.
typedef struct {
    bool valid; // false: nothing is known, and the next flush writes every byte
    uint8_t page[OLED_PAGES][OLED_WIDTH];
} ssd1306_shadow_t;
typedef struct { unsigned writes, bytes; } ssd1306_traffic_t;
// Writes the spans of frame that differ from the shadow, page by page: for each, the column
// and page window (21h, 22h) and then its bytes, SSD1306_CHUNK to a write. The shadow follows
// each span as it completes. False when a write failed; the shadow is then invalid, since the
// controller may hold part of a span. traffic, when given, is added to.
bool ssd1306_flush(ssd1306_shadow_t *shadow, const oled_canvas_t *frame, ssd1306_write_fn write, void *ctx,
                   ssd1306_traffic_t *traffic);
#endif
