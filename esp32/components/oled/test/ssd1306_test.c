#include "ssd1306.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// A model of the controller as far as the driver uses it: the display memory, the column
// and page window with its pointer, and the settings the setup sequence writes. A command
// it does not know, or a parameter out of range, fails the test.
typedef struct {
    uint8_t ram[OLED_PAGES][OLED_WIDTH];
    unsigned column_first, column_last, page_first, page_last, column, page;
    bool on, horizontal, pump, remap, scan_reversed, scrolling, inverted, follow_ram;
    uint8_t contrast, multiplex, offset, start_line, com_pins, clock, precharge, vcomh;
    unsigned writes, data_bytes, longest_write;
    unsigned fail_at; // the write (counted from 1) that fails once; 0 for none
} panel_t;

static void panel_reset(panel_t *p)
{
    // The controller's own reset values where the driver depends on overwriting them, and
    // noise in the memory, as after power-up.
    *p = (panel_t){.column_last = 127, .page_last = 7, .scrolling = true, .inverted = true, .multiplex = 0x20,
                   .offset = 9, .start_line = 5, .com_pins = 0x02, .contrast = 0x7f};
    for (unsigned i = 0; i < sizeof p->ram; i++) ((uint8_t *)p->ram)[i] = (uint8_t)(rand() & 0xff);
}

static bool panel_write(void *ctx, const uint8_t *bytes, size_t length)
{
    panel_t *p = ctx;
    assert(length >= 2);
    p->writes++;
    if (length > p->longest_write) p->longest_write = (unsigned)length;
    if (p->fail_at && p->writes == p->fail_at) {
        p->fail_at = 0;
        return false;
    }
    if (bytes[0] == SSD1306_DATA) {
        for (size_t i = 1; i < length; i++) {
            p->ram[p->page][p->column] = bytes[i];
            p->data_bytes++;
            assert(p->horizontal);
            if (p->column++ == p->column_last) {
                p->column = p->column_first;
                if (p->page++ == p->page_last) p->page = p->page_first;
            }
        }
        return true;
    }
    assert(bytes[0] == SSD1306_COMMANDS);
    for (size_t i = 1; i < length; i++) {
        const uint8_t command = bytes[i];
#define PARAMETER() (assert(i + 1 < length), bytes[++i])
        if (command == 0xae || command == 0xaf) p->on = command == 0xaf;
        else if (command == 0xd5) p->clock = PARAMETER();
        else if (command == 0xa8) { p->multiplex = PARAMETER(); assert(p->multiplex >= 15 && p->multiplex <= 63); }
        else if (command == 0xd3) { p->offset = PARAMETER(); assert(p->offset <= 63); }
        else if (command >= 0x40 && command <= 0x7f) p->start_line = command & 0x3f;
        else if (command == 0x8d) { const uint8_t v = PARAMETER(); assert(v == 0x14 || v == 0x10); p->pump = v == 0x14; }
        else if (command == 0x20) { const uint8_t v = PARAMETER(); assert(v <= 2); p->horizontal = v == 0; }
        else if (command == 0xa0 || command == 0xa1) p->remap = command == 0xa1;
        else if (command == 0xc0 || command == 0xc8) p->scan_reversed = command == 0xc8;
        else if (command == 0xda) { p->com_pins = PARAMETER(); assert((p->com_pins & 0xcf) == 0x02); }
        else if (command == 0x81) p->contrast = PARAMETER();
        else if (command == 0xd9) { p->precharge = PARAMETER(); assert((p->precharge & 0x0f) && (p->precharge & 0xf0)); }
        else if (command == 0xdb) { p->vcomh = PARAMETER(); assert(p->vcomh == 0x00 || p->vcomh == 0x20 || p->vcomh == 0x30); }
        else if (command == 0x2e) p->scrolling = false;
        else if (command == 0xa4 || command == 0xa5) p->follow_ram = command == 0xa4;
        else if (command == 0xa6 || command == 0xa7) p->inverted = command == 0xa7;
        else if (command == 0x21) {
            p->column_first = PARAMETER(); p->column_last = PARAMETER();
            assert(p->column_first <= p->column_last && p->column_last < OLED_WIDTH);
            p->column = p->column_first;
        } else if (command == 0x22) {
            p->page_first = PARAMETER(); p->page_last = PARAMETER();
            assert(p->page_first <= p->page_last && p->page_last < OLED_PAGES);
            p->page = p->page_first;
        } else {
            fprintf(stderr, "unexpected command 0x%02x\n", command);
            abort();
        }
#undef PARAMETER
    }
    return true;
}

static bool shows(const panel_t *p, const oled_canvas_t *frame) { return !memcmp(p->ram, frame->page, sizeof p->ram); }

int main(void)
{
    static panel_t panel;
    static oled_canvas_t frame;
    static ssd1306_shadow_t shadow;
    uint8_t bytes[SSD1306_SETUP_MAX];
    srand(1306);

    // The setup: every setting the picture depends on, whatever the controller held, and the
    // display left off until its memory has been written.
    panel_reset(&panel);
    panel.on = true;
    size_t n = ssd1306_setup(bytes, true, false, 0x5a);
    assert(n <= SSD1306_SETUP_MAX && bytes[0] == SSD1306_COMMANDS && panel_write(&panel, bytes, n));
    assert(!panel.on && panel.horizontal && panel.pump && panel.remap && panel.scan_reversed && !panel.scrolling);
    assert(!panel.inverted && panel.follow_ram && panel.contrast == 0x5a && panel.multiplex == 63 && panel.offset == 0);
    assert(panel.start_line == 0 && panel.com_pins == 0x12 && panel.clock == 0x80 && panel.precharge == 0xf1 && panel.vcomh == 0x20);
    // Rotated: the other segment and COM directions, and nothing else different.
    uint8_t rotated[SSD1306_SETUP_MAX];
    assert(ssd1306_setup(rotated, true, true, 0x5a) == n);
    unsigned differing = 0;
    for (size_t i = 0; i < n; i++) differing += bytes[i] != rotated[i];
    assert(differing == 2 && panel_write(&panel, rotated, n) && !panel.remap && !panel.scan_reversed);
    assert(ssd1306_power(bytes, true) == 2 && panel_write(&panel, bytes, 2) && panel.on);
    // The same settings without the display-off: a lit display stays lit through them.
    uint8_t again[SSD1306_SETUP_MAX];
    assert(ssd1306_setup(again, false, false, 0x33) == n - 1 && again[0] == SSD1306_COMMANDS);
    assert(panel_write(&panel, again, n - 1));
    assert(panel.on && panel.remap && panel.scan_reversed && panel.contrast == 0x33 && panel.horizontal);
    assert(ssd1306_power(bytes, false) == 2 && panel_write(&panel, bytes, 2) && !panel.on);
    assert(ssd1306_contrast(bytes, 0x12) == 3 && panel_write(&panel, bytes, 3) && panel.contrast == 0x12);

    // The first flush knows nothing of the memory and writes all of it: eight windows and
    // thirty-two full chunks, no write longer than a chunk and its control byte.
    oled_clear(&frame);
    (void)oled_text(&frame, 0, 0, 1, "NAVLISTEN 0123456789");
    (void)oled_text(&frame, 0, 20, 2, "12:34:56");
    oled_fill(&frame, 100, 50, 28, 14, true);
    panel.writes = panel.data_bytes = panel.longest_write = 0;
    ssd1306_traffic_t traffic = {0};
    assert(!shadow.valid && ssd1306_flush(&shadow, &frame, panel_write, &panel, &traffic) && shadow.valid);
    assert(shows(&panel, &frame));
    assert(panel.writes == 8 + 32 && panel.data_bytes == 1024 && panel.longest_write == 1 + SSD1306_CHUNK);
    assert(traffic.writes == 40 && traffic.bytes == 8 * 7 + 32 * 33);

    // Nothing changed: nothing is sent.
    panel.writes = 0;
    assert(ssd1306_flush(&shadow, &frame, panel_write, &panel, NULL) && panel.writes == 0);

    // One changed byte: its window and one data byte.
    frame.page[3][40] ^= 0x10;
    panel.writes = panel.data_bytes = 0;
    assert(ssd1306_flush(&shadow, &frame, panel_write, &panel, NULL) && shows(&panel, &frame));
    assert(panel.writes == 2 && panel.data_bytes == 1);

    // Changes close together go as one span, the unchanged bytes between them rewritten;
    // changes far apart go as two.
    frame.page[5][10] ^= 1;
    frame.page[5][10 + SSD1306_JOIN + 1] ^= 1; // SSD1306_JOIN unchanged bytes between
    panel.writes = panel.data_bytes = 0;
    assert(ssd1306_flush(&shadow, &frame, panel_write, &panel, NULL) && shows(&panel, &frame));
    assert(panel.writes == 2 && panel.data_bytes == SSD1306_JOIN + 2);
    frame.page[5][10] ^= 1;
    frame.page[5][10 + SSD1306_JOIN + 2] ^= 1; // one more than is joined
    panel.writes = panel.data_bytes = 0;
    assert(ssd1306_flush(&shadow, &frame, panel_write, &panel, NULL) && shows(&panel, &frame));
    assert(panel.writes == 4 && panel.data_bytes == 2);

    // A span longer than a chunk is cut into chunks, and the last column of a page is reached.
    for (unsigned x = 60; x < OLED_WIDTH; x++) frame.page[7][x] ^= 0xff;
    panel.writes = panel.data_bytes = panel.longest_write = 0;
    assert(ssd1306_flush(&shadow, &frame, panel_write, &panel, NULL) && shows(&panel, &frame));
    assert(panel.writes == 1 + 3 && panel.data_bytes == 68 && panel.longest_write == 1 + SSD1306_CHUNK);

    // Many frames of scattered changes: the memory always ends equal to the frame.
    for (unsigned round = 0; round < 500; round++) {
        const unsigned changes = (unsigned)rand() % 40;
        for (unsigned i = 0; i < changes; i++)
            frame.page[rand() % OLED_PAGES][rand() % OLED_WIDTH] ^= (uint8_t)(1u << (rand() % 8));
        assert(ssd1306_flush(&shadow, &frame, panel_write, &panel, NULL) && shows(&panel, &frame));
    }

    // A write that fails ends the flush and invalidates the shadow, at the window as in the
    // data; the flush after that writes everything, so the memory is right again whatever
    // part of the span arrived.
    for (unsigned fail = 1; fail <= 3; fail++) {
        frame.page[2][17] ^= 0xff;
        frame.page[2][90] ^= 0xff;
        panel.writes = 0;
        panel.fail_at = fail;
        assert(!ssd1306_flush(&shadow, &frame, panel_write, &panel, NULL) && !shadow.valid);
        panel.writes = panel.data_bytes = 0;
        assert(ssd1306_flush(&shadow, &frame, panel_write, &panel, NULL) && shadow.valid && shows(&panel, &frame));
        assert(panel.data_bytes == 1024);
    }

    // A module that was unplugged comes back with noise in its memory: the caller invalidates
    // the shadow and the next flush repaints it.
    panel_reset(&panel);
    n = ssd1306_setup(bytes, true, false, 0x7f);
    assert(panel_write(&panel, bytes, n) && !shows(&panel, &frame));
    shadow.valid = false;
    assert(ssd1306_flush(&shadow, &frame, panel_write, &panel, NULL) && shows(&panel, &frame));
    puts("ssd1306 setup and flush: ok");
    return 0;
}
