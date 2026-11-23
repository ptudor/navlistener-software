#include "ssd1306.h"
#include <string.h>

size_t ssd1306_setup(uint8_t out[SSD1306_SETUP_MAX], bool off, bool rotated, uint8_t contrast)
{
    // Every setting is written, not only those that differ from the controller's reset
    // values: a restart of the ESP32 alone finds the controller as the last firmware left it.
    const uint8_t settings[] = {
        0xd5, 0x80,                       // clock divide ratio 1, oscillator frequency at its reset value
        0xa8, OLED_HEIGHT - 1,            // multiplex ratio: 64 rows
        0xd3, 0x00,                       // display offset 0
        0x40,                             // start line 0
        0x8d, 0x14,                       // charge pump on: the module makes its panel voltage from 3.3 V
        0x20, 0x00,                       // horizontal addressing: the column advances, then the page
        rotated ? 0xa0 : 0xa1,            // segment remap
        rotated ? 0xc0 : 0xc8,            // COM scan direction
        0xda, 0x12,                       // COM pins: alternative configuration, as a 128x64 panel is wired
        0x81, contrast,                   // contrast
        0xd9, 0xf1,                       // pre-charge 1 and discharge 15 clocks, for the charge pump
        0xdb, 0x20,                       // VCOMH deselect 0.77 x VCC, the reset value
        0x2e,                             // scrolling off
        0xa4,                             // show the display memory
        0xa6,                             // normal, not inverted
    };
    _Static_assert(2 + sizeof settings <= SSD1306_SETUP_MAX, "the setup sequence must fit its buffer");
    size_t n = 0;
    out[n++] = SSD1306_COMMANDS;
    if (off) out[n++] = 0xae;
    memcpy(out + n, settings, sizeof settings);
    return n + sizeof settings;
}

size_t ssd1306_power(uint8_t out[2], bool on)
{
    out[0] = SSD1306_COMMANDS;
    out[1] = on ? 0xaf : 0xae;
    return 2;
}

size_t ssd1306_contrast(uint8_t out[3], uint8_t contrast)
{
    out[0] = SSD1306_COMMANDS;
    out[1] = 0x81;
    out[2] = contrast;
    return 3;
}

static bool sent(ssd1306_write_fn write, void *ctx, const uint8_t *bytes, size_t length, ssd1306_traffic_t *traffic)
{
    if (!write(ctx, bytes, length)) return false;
    if (traffic) {
        traffic->writes++;
        traffic->bytes += (unsigned)length;
    }
    return true;
}

// Columns first..last of one page: the window, then the bytes.
static bool write_span(const uint8_t *row, unsigned page, unsigned first, unsigned last, ssd1306_write_fn write,
                       void *ctx, ssd1306_traffic_t *traffic)
{
    const uint8_t window[] = {SSD1306_COMMANDS, 0x21, (uint8_t)first, (uint8_t)last, 0x22, (uint8_t)page, (uint8_t)page};
    if (!sent(write, ctx, window, sizeof window, traffic)) return false;
    for (unsigned x = first; x <= last;) {
        uint8_t data[1 + SSD1306_CHUNK] = {SSD1306_DATA};
        const unsigned n = last - x + 1 > SSD1306_CHUNK ? SSD1306_CHUNK : last - x + 1;
        memcpy(data + 1, row + x, n);
        if (!sent(write, ctx, data, 1 + n, traffic)) return false;
        x += n;
    }
    return true;
}

bool ssd1306_flush(ssd1306_shadow_t *shadow, const oled_canvas_t *frame, ssd1306_write_fn write, void *ctx,
                   ssd1306_traffic_t *traffic)
{
    const bool known = shadow->valid;
    for (unsigned page = 0; page < OLED_PAGES; page++) {
        const uint8_t *want = frame->page[page];
        uint8_t *have = shadow->page[page];
        for (unsigned x = 0; x < OLED_WIDTH;) {
            if (known && have[x] == want[x]) {
                x++;
                continue;
            }
            // A span runs from this changed byte to the last one that no long unchanged run
            // separates from it.
            unsigned last = x, unchanged = 0;
            for (unsigned i = x + 1; i < OLED_WIDTH && unchanged <= SSD1306_JOIN; i++) {
                if (known && have[i] == want[i]) {
                    unchanged++;
                } else {
                    last = i;
                    unchanged = 0;
                }
            }
            if (!write_span(want, page, x, last, write, ctx, traffic)) {
                shadow->valid = false;
                return false;
            }
            memcpy(have + x, want + x, last - x + 1);
            x = last + 1;
        }
    }
    shadow->valid = true;
    return true;
}
