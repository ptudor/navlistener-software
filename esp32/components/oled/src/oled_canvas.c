#include "oled_canvas.h"
#include <string.h>

void oled_clear(oled_canvas_t *c) { memset(c->page, 0, sizeof c->page); }

// The byte and bit of a drawn pixel, or NULL outside the frame.
static uint8_t *cell(oled_canvas_t *c, int x, int y, uint8_t *bit)
{
    x += c->dx;
    y += c->dy;
    if (x < 0 || x >= OLED_WIDTH || y < 0 || y >= OLED_HEIGHT) return NULL;
    *bit = (uint8_t)(1u << (y & 7));
    return &c->page[y >> 3][x];
}

void oled_pixel(oled_canvas_t *c, int x, int y, bool on)
{
    uint8_t bit;
    uint8_t *byte = cell(c, x, y, &bit);
    if (!byte) return;
    if (on) *byte |= bit;
    else *byte &= (uint8_t)~bit;
}

void oled_hline(oled_canvas_t *c, int x, int y, int w, bool on) { oled_fill(c, x, y, w, 1, on); }
void oled_vline(oled_canvas_t *c, int x, int y, int h, bool on) { oled_fill(c, x, y, 1, h, on); }

void oled_fill(oled_canvas_t *c, int x, int y, int w, int h, bool on)
{
    for (int row = y; row < y + h; row++)
        for (int column = x; column < x + w; column++) oled_pixel(c, column, row, on);
}

void oled_invert(oled_canvas_t *c, int x, int y, int w, int h)
{
    for (int row = y; row < y + h; row++)
        for (int column = x; column < x + w; column++) {
            uint8_t bit;
            uint8_t *byte = cell(c, column, row, &bit);
            if (byte) *byte ^= bit;
        }
}

int oled_text(oled_canvas_t *c, int x, int y, int scale, const char *s)
{
    if (!s || scale < 1) return x;
    for (; *s; s++, x += OLED_ADVANCE * scale) {
        const char *glyph = oled_glyph((unsigned char)*s);
        if (!glyph) glyph = oled_glyph('?');
        for (int row = 0; row < OLED_GLYPH_H; row++)
            for (int column = 0; column < OLED_GLYPH_W; column++)
                if (glyph[row * OLED_GLYPH_W + column] == '#')
                    oled_fill(c, x + column * scale, y + row * scale, scale, scale, true);
    }
    return x;
}

int oled_text_width(const char *s, int scale)
{
    const size_t length = s ? strlen(s) : 0;
    return length && scale >= 1 ? (int)length * OLED_ADVANCE * scale - scale : 0;
}

void oled_text_right(oled_canvas_t *c, int x_end, int y, int scale, const char *s)
{
    (void)oled_text(c, x_end - oled_text_width(s, scale), y, scale, s);
}

bool oled_get(const oled_canvas_t *c, int x, int y)
{
    if (x < 0 || x >= OLED_WIDTH || y < 0 || y >= OLED_HEIGHT) return false;
    return (c->page[y >> 3][x] >> (y & 7)) & 1;
}
