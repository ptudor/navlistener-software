#include "oled_canvas.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

static unsigned lit(const oled_canvas_t *c)
{
    unsigned count = 0;
    for (int y = 0; y < OLED_HEIGHT; y++)
        for (int x = 0; x < OLED_WIDTH; x++) count += oled_get(c, x, y);
    return count;
}
static unsigned glyph_cells(unsigned char ch)
{
    unsigned count = 0;
    for (const char *cell = oled_glyph(ch); *cell; cell++) count += *cell == '#';
    return count;
}

int main(void)
{
    // The font: one glyph for each character from space to the plus-minus sign, each exactly
    // 5 by 8 cells of '#' and '.', and none beyond. Capitals and digits leave the descender
    // row empty, so a line of them is seven pixels tall.
    for (unsigned ch = 0; ch < 256; ch++) {
        const char *glyph = oled_glyph((unsigned char)ch);
        if (ch < 32 || ch > 128) {
            assert(!glyph);
            continue;
        }
        assert(glyph && strlen(glyph) == OLED_GLYPH_W * OLED_GLYPH_H);
        assert(strspn(glyph, "#.") == OLED_GLYPH_W * OLED_GLYPH_H);
        if ((ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9'))
            assert(!memchr(glyph + OLED_GLYPH_W * (OLED_GLYPH_H - 1), '#', OLED_GLYPH_W));
        if (ch != ' ') assert(glyph_cells((unsigned char)ch) > 0);
    }
    assert(glyph_cells(' ') == 0);
    // No two characters share a glyph: each can be told from every other.
    for (unsigned a = 32; a <= 128; a++)
        for (unsigned b = a + 1; b <= 128; b++) assert(strcmp(oled_glyph((unsigned char)a), oled_glyph((unsigned char)b)));
    assert(!strcmp(OLED_DEGREE "C", "\x7f" "C") && strlen(OLED_PLUS_MINUS "1") == 2 && (unsigned char)OLED_PLUS_MINUS[0] == 128);

    // The frame is the controller's memory: page p, column x, bit n is row 8p + n.
    static oled_canvas_t c;
    oled_clear(&c);
    oled_pixel(&c, 0, 0, true);
    oled_pixel(&c, 127, 63, true);
    oled_pixel(&c, 5, 10, true);
    assert(c.page[0][0] == 0x01 && c.page[7][127] == 0x80 && c.page[1][5] == 0x04 && lit(&c) == 3);
    assert(oled_get(&c, 5, 10) && !oled_get(&c, 5, 11) && !oled_get(&c, -1, 0) && !oled_get(&c, 128, 0) && !oled_get(&c, 0, 64));
    oled_pixel(&c, 5, 10, false);
    assert(!c.page[1][5] && lit(&c) == 2);
    // Outside the frame nothing is drawn, and nothing else is touched.
    oled_pixel(&c, -1, 0, true); oled_pixel(&c, 128, 0, true); oled_pixel(&c, 0, -1, true); oled_pixel(&c, 0, 64, true);
    assert(lit(&c) == 2);

    oled_clear(&c);
    oled_hline(&c, 10, 3, 5, true);
    oled_vline(&c, 20, 6, 4, true);
    assert(lit(&c) == 9 && oled_get(&c, 10, 3) && oled_get(&c, 14, 3) && !oled_get(&c, 15, 3));
    assert(oled_get(&c, 20, 6) && oled_get(&c, 20, 9) && !oled_get(&c, 20, 10)); // across the page boundary at row 8
    oled_clear(&c);
    oled_fill(&c, 120, 60, 20, 20, true); // clipped to the 8 by 4 corner that is on the panel
    assert(lit(&c) == 32);
    oled_invert(&c, 118, 58, 4, 4);       // two lit pixels by two go dark, the other twelve light
    assert(lit(&c) == 32 - 4 + 12 && !oled_get(&c, 120, 60) && oled_get(&c, 118, 58));

    // Text: glyphs six pixels apart, the top-left corner where asked, the return value the
    // next glyph's x, and the width without the trailing blank column.
    oled_clear(&c);
    assert(oled_text(&c, 3, 4, 1, "AB") == 15);
    assert(lit(&c) == glyph_cells('A') + glyph_cells('B'));
    const char *a = oled_glyph('A'), *b = oled_glyph('B');
    for (int row = 0; row < OLED_GLYPH_H; row++)
        for (int column = 0; column < OLED_GLYPH_W; column++) {
            assert(oled_get(&c, 3 + column, 4 + row) == (a[row * OLED_GLYPH_W + column] == '#'));
            assert(oled_get(&c, 9 + column, 4 + row) == (b[row * OLED_GLYPH_W + column] == '#'));
        }
    assert(oled_text_width("AB", 1) == 11 && oled_text_width("AB", 2) == 22 && oled_text_width("", 1) == 0 &&
           oled_text_width(NULL, 1) == 0);
    // At scale 2 every cell is a 2 by 2 block.
    oled_clear(&c);
    assert(oled_text(&c, 0, 0, 2, "8") == 12 && lit(&c) == 4 * glyph_cells('8'));
    assert(oled_get(&c, 2, 0) && oled_get(&c, 3, 1) && !oled_get(&c, 0, 0));
    // Right-aligned text ends in the column before x_end.
    oled_clear(&c);
    oled_text_right(&c, 126, 0, 1, "H");
    assert(oled_get(&c, 125, 0) && oled_get(&c, 121, 0) && !oled_get(&c, 126, 0) && !oled_get(&c, 120, 0));
    // A character without a glyph is drawn as a question mark; text off the panel is clipped.
    oled_clear(&c);
    (void)oled_text(&c, 0, 0, 1, "\x01");
    assert(lit(&c) == glyph_cells('?'));
    oled_clear(&c);
    (void)oled_text(&c, 125, 60, 1, "W");
    assert(lit(&c) > 0 && lit(&c) < glyph_cells('W'));
    (void)oled_text(&c, 0, 0, 0, "W"); // no scale, nothing drawn
    (void)oled_text(&c, 0, 0, 1, NULL);

    // dx and dy move everything drawn, and the clipping with it.
    oled_clear(&c);
    c.dx = 2; c.dy = 1;
    oled_pixel(&c, 0, 0, true);
    oled_pixel(&c, 126, 0, true); // lands outside
    assert(lit(&c) == 1 && oled_get(&c, 2, 1));
    oled_invert(&c, 0, 0, 1, 1);
    assert(lit(&c) == 0);
    puts("oled canvas and font: ok");
    return 0;
}
