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

int main(void)
{
    // Boots 1-10 test the lamps, then every tenth; an unknown count does too.
    for (uint32_t boot = 0; boot <= 10; boot++) assert(panel_boot_lamp_test(boot));
    for (uint32_t boot = 11; boot < 20; boot++) assert(!panel_boot_lamp_test(boot));
    assert(panel_boot_lamp_test(20) && !panel_boot_lamp_test(21) && panel_boot_lamp_test(1000));
    assert(!panel_boot_lamp_test(UINT32_MAX) && panel_boot_lamp_test(4294967290u));

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

    // A 24-bit panel's lamp test: 18 single lamps of 250 ms, three 750 ms rows, a 250 ms dark
    // step, then the sweep.
    panel_boot_t full = {.lamp_test = true, .status_byte = true, .identity_valid = true, .identity = 0x89abcdefu};
    assert(panel_boot_ms(&full) == 18 * 250 + 3 * 750 + 250 + 3200);
    for (unsigned i = 0; i < 8; i++) {
        assert(same(at(&full, i * 250), 0, (uint8_t)(1u << i), 0));
        assert(same(at(&full, i * 250 + 249), 0, (uint8_t)(1u << i), 0));
        assert(same(at(&full, 2000 + i * 250 + 125), 0, 0, (uint8_t)(1u << i)));
    }
    assert(same(at(&full, 4000), PANEL_STATUS_RTK, 0, 0));
    assert(same(at(&full, 4250), PANEL_STATUS_ATTENTION, 0, 0));
    assert(same(at(&full, 4500), 0, 0xff, 0) && same(at(&full, 5249), 0, 0xff, 0));
    assert(same(at(&full, 5250), 0, 0, 0xff) && same(at(&full, 5999), 0, 0, 0xff));
    assert(same(at(&full, 6000), PANEL_STATUS_RTK | PANEL_STATUS_ATTENTION, 0xff, 0xff));
    assert(same(at(&full, 6749), PANEL_STATUS_RTK | PANEL_STATUS_ATTENTION, 0xff, 0xff));
    assert(same(at(&full, 6750), 0, 0, 0) && same(at(&full, 6999), 0, 0, 0));
    assert(same(at(&full, 7000), 0, 0x01, 0) && same(at(&full, 7100), 0, 0, 0x02));
    assert(same(at(&full, 10199), 0, 0x80, 0) && !panel_boot_lamps(&full, 10200, &untouched));

    // A 16-bit chain has no status lamps: the rows follow the amber singles directly.
    panel_boot_t chain16 = {.lamp_test = true, .identity_valid = true, .identity = 0x89abcdefu};
    assert(panel_boot_ms(&chain16) == 16 * 250 + 3 * 750 + 250 + 3200);
    assert(same(at(&chain16, 3999), 0, 0, 0x80) && same(at(&chain16, 4000), 0, 0xff, 0));
    assert(same(at(&chain16, 5500), 0, 0xff, 0xff) && same(at(&chain16, 6250), 0, 0, 0));
    assert(same(at(&chain16, 6500), 0, 0x01, 0));

    // A lamp test without a board identity ends after the dark step.
    panel_boot_t unread = {.lamp_test = true, .status_byte = true};
    assert(panel_boot_ms(&unread) == 7000 && same(at(&unread, 6999), 0, 0, 0));
    assert(!panel_boot_lamps(&unread, 7000, &untouched));
    puts("panel boot sequence tests passed");
}
