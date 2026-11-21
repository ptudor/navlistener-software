#include "panel_boot.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

static panel_lamps_t at(const panel_boot_t *b, uint32_t ms)
{
    panel_lamps_t lamps = {.status = 0xee, .green = 0xee, .yellow = 0xee};
    assert(panel_boot_lamps(b, ms, &lamps));
    return lamps;
}
static bool same(panel_lamps_t a, uint8_t status, uint8_t green, uint8_t yellow)
{ return a.status == status && a.green == green && a.yellow == yellow; }
static unsigned lit(panel_lamps_t a)
{ return (unsigned)(__builtin_popcount(a.status) + __builtin_popcount(a.green) + __builtin_popcount(a.yellow)); }

// The chase, step by step: every lamp of the panel is lit for PANEL_BOOT_CHASE_LAMPS steps in a
// row and no others, the run is never longer than that, and the sequence ends dark.
static void every_lamp_once(const panel_boot_t *b, unsigned lamps)
{
    const unsigned steps = lamps + PANEL_BOOT_CHASE_LAMPS - 1 + PANEL_BOOT_CHASE_DARK;
    unsigned steps_lit[24] = {0}, first[24], last[24];
    for (unsigned step = 0; step < steps; step++) {
        const panel_lamps_t early = at(b, step * PANEL_BOOT_CHASE_MS), late = at(b, step * PANEL_BOOT_CHASE_MS + PANEL_BOOT_CHASE_MS - 1);
        assert(same(late, early.status, early.green, early.yellow)); // a step holds for its whole time
        assert(lit(early) <= PANEL_BOOT_CHASE_LAMPS);
        const uint32_t bits = (uint32_t)early.status << 16 | (uint32_t)early.yellow << 8 | early.green;
        for (unsigned bit = 0; bit < 24; bit++) {
            if (!(bits >> bit & 1)) continue;
            if (!steps_lit[bit]++) first[bit] = step;
            last[bit] = step;
        }
    }
    unsigned seen = 0;
    for (unsigned bit = 0; bit < 24; bit++) {
        if (!steps_lit[bit]) continue;
        seen++;
        assert(steps_lit[bit] == PANEL_BOOT_CHASE_LAMPS && last[bit] - first[bit] == PANEL_BOOT_CHASE_LAMPS - 1);
    }
    assert(seen == lamps);
    for (unsigned dark = 0; dark < PANEL_BOOT_CHASE_DARK; dark++) assert(same(at(b, (steps - 1 - dark) * PANEL_BOOT_CHASE_MS), 0, 0, 0));
}

int main(void)
{
    const uint8_t serial[16] = {0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80,
                                0x90, 0xa0, 0xb0, 0xc0, 0x89, 0xab, 0xcd, 0xef};
    assert(panel_boot_identity(serial, sizeof serial) == 0x89abcdefu);
    assert(panel_boot_identity(serial, 4) == 0x10203040u);
    assert(panel_boot_identity(serial, 3) == 0 && panel_boot_identity(NULL, 16) == 0);

    // Nothing to show: the sequence has ended before it starts, and *out is left alone.
    panel_boot_t none = {0};
    panel_lamps_t untouched = {.status = 1, .green = 2, .yellow = 3};
    assert(panel_boot_ms(&none) == 0 && !panel_boot_lamps(&none, 0, &untouched));
    assert(same(untouched, 1, 2, 3));

    // The identity sweep alone: one column per 100 ms, four passes, green for 1 and amber for 0.
    // 0x89abcdef is 10001001 10101011 11001101 11101111.
    panel_boot_t sweep = {.identity_valid = true, .identity = 0x89abcdefu};
    assert(panel_boot_ms(&sweep) == 3200);
    const char *bits = "10001001101010111100110111101111";
    for (unsigned step = 0; step < 32; step++) {
        uint8_t column = (uint8_t)(1u << (step % 8));
        bool one = bits[step] == '1';
        for (uint32_t ms = step * 100; ms < step * 100 + 100; ms += 33)
            assert(same(at(&sweep, ms), 0, one ? column : 0, one ? 0 : column));
    }
    assert(same(at(&sweep, 3199), 0, 0x80, 0));
    assert(!panel_boot_lamps(&sweep, 3200, &untouched) && !panel_boot_lamps(&sweep, UINT32_MAX, &untouched));
    // A status driver adds nothing to the sweep.
    sweep.status_byte = true;
    assert(panel_boot_ms(&sweep) == 3200 && same(at(&sweep, 0), 0, 0x01, 0));

    // A 24-bit panel's chase: a run of five lamps, one lamp per 150 ms, along the top row (the
    // eight green lamps, then RTK) and then the bottom row (the eight amber lamps, then
    // ATTENTION); four steps for the run to leave, two dark steps, then the sweep.
    panel_boot_t full = {.chase = true, .status_byte = true, .identity_valid = true, .identity = 0x89abcdefu};
    assert(panel_boot_ms(&full) == (18 + 4 + 2) * 150 + 3200);
    assert(same(at(&full, 0), 0, 0x01, 0) && same(at(&full, 149), 0, 0x01, 0)); // the head enters alone
    assert(same(at(&full, 150), 0, 0x03, 0) && same(at(&full, 4 * 150), 0, 0x1f, 0));
    assert(same(at(&full, 5 * 150), 0, 0x3e, 0) && same(at(&full, 7 * 150), 0, 0xf8, 0));
    assert(same(at(&full, 8 * 150), PANEL_STATUS_RTK, 0xf0, 0));        // the head on RTK, at the top row's end
    assert(same(at(&full, 9 * 150), PANEL_STATUS_RTK, 0xe0, 0x01));     // and down to the bottom row's first lamp
    assert(same(at(&full, 12 * 150), PANEL_STATUS_RTK, 0, 0x0f));
    assert(same(at(&full, 13 * 150), 0, 0, 0x1f) && same(at(&full, 16 * 150), 0, 0, 0xf8));
    assert(same(at(&full, 17 * 150), PANEL_STATUS_ATTENTION, 0, 0xf0)); // the head on ATTENTION, the last lamp
    assert(same(at(&full, 18 * 150), PANEL_STATUS_ATTENTION, 0, 0xe0));
    assert(same(at(&full, 21 * 150), PANEL_STATUS_ATTENTION, 0, 0));    // the run's last lamp, alone
    assert(same(at(&full, 22 * 150), 0, 0, 0) && same(at(&full, 24 * 150 - 1), 0, 0, 0));
    assert(same(at(&full, 3600), 0, 0x01, 0) && same(at(&full, 3700), 0, 0, 0x02));
    assert(same(at(&full, 6799), 0, 0x80, 0) && !panel_boot_lamps(&full, 6800, &untouched));
    every_lamp_once(&full, 18);

    // A 16-bit chain has no status lamps: the run goes from the last green lamp to the first amber one.
    panel_boot_t chain16 = {.chase = true, .identity_valid = true, .identity = 0x89abcdefu};
    assert(panel_boot_ms(&chain16) == (16 + 4 + 2) * 150 + 3200);
    assert(same(at(&chain16, 7 * 150), 0, 0xf8, 0) && same(at(&chain16, 8 * 150), 0, 0xf0, 0x01));
    assert(same(at(&chain16, 15 * 150), 0, 0, 0xf8) && same(at(&chain16, 19 * 150), 0, 0, 0x80));
    assert(same(at(&chain16, 20 * 150), 0, 0, 0) && same(at(&chain16, 3299), 0, 0, 0));
    assert(same(at(&chain16, 3300), 0, 0x01, 0));
    every_lamp_once(&chain16, 16);

    // A chase without a board identity ends after the dark steps.
    panel_boot_t unread = {.chase = true, .status_byte = true};
    assert(panel_boot_ms(&unread) == 3600 && same(at(&unread, 3599), 0, 0, 0));
    assert(!panel_boot_lamps(&unread, 3600, &untouched));
    every_lamp_once(&unread, 18);
    puts("panel boot sequence tests passed");
}
