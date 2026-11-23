#ifndef OLED_CANVAS_H
#define OLED_CANVAS_H
// A 128x64 one-bit frame in the SSD1306's own memory layout, and the drawing the status
// pages need: pixels, lines, boxes and text in one font at whole-number scales. Pure C with
// host tests in test/; nothing here touches the bus.
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define OLED_WIDTH 128
#define OLED_HEIGHT 64
#define OLED_PAGES (OLED_HEIGHT / 8)
// A glyph is 5 columns by 8 rows. Capitals and digits fill the first seven rows; the eighth
// holds the descenders of g, j, p, q and y. Text advances one blank column after each glyph.
#define OLED_GLYPH_W 5
#define OLED_GLYPH_H 8
#define OLED_ADVANCE (OLED_GLYPH_W + 1)
// The two characters beyond ASCII the pages use, as literals to splice into a string. They
// are octal escapes so that a letter after one is never read as part of it.
#define OLED_DEGREE "\177"
#define OLED_PLUS_MINUS "\200"

typedef struct {
    // page[p][x] is column x of rows 8p..8p+7, row 8p in bit 0: the controller's layout.
    uint8_t page[OLED_PAGES][OLED_WIDTH];
    // Added to every coordinate drawn. The pages move the whole picture a pixel or two from
    // time to time, so that no pixel is lit in one place for the life of the display.
    int8_t dx, dy;
} oled_canvas_t;

void oled_clear(oled_canvas_t *c);
// Drawing outside the frame is clipped, pixel by pixel.
void oled_pixel(oled_canvas_t *c, int x, int y, bool on);
void oled_hline(oled_canvas_t *c, int x, int y, int w, bool on);
void oled_vline(oled_canvas_t *c, int x, int y, int h, bool on);
void oled_fill(oled_canvas_t *c, int x, int y, int w, int h, bool on);
void oled_invert(oled_canvas_t *c, int x, int y, int w, int h);
// Draws s with its top-left corner at (x, y) and returns the x after its last glyph's
// advance. A character the font does not have is drawn as '?'.
int oled_text(oled_canvas_t *c, int x, int y, int scale, const char *s);
// The same, with its last lit column at x_end - 1: right-aligned.
void oled_text_right(oled_canvas_t *c, int x_end, int y, int scale, const char *s);
// The width s takes at that scale, without the blank column after its last glyph.
int oled_text_width(const char *s, int scale);
// A pixel of the stored frame, where it was put: dx and dy are not applied. False outside it.
bool oled_get(const oled_canvas_t *c, int x, int y);
// The glyph for one character as OLED_GLYPH_W * OLED_GLYPH_H cells, row by row, '#' lit;
// NULL when the font does not have it.
const char *oled_glyph(unsigned char ch);
#endif
