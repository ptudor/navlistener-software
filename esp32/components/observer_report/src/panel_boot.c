#include "panel_boot.h"
bool panel_boot_lamp_test(uint32_t boot)
{ return boot <= PANEL_BOOT_LAMP_TESTS || boot % PANEL_BOOT_LAMP_TESTS == 0; }
uint32_t panel_boot_identity(const uint8_t *serial, size_t length)
{
    if (!serial || length < 4) return 0;
    const uint8_t *last = serial + length - 4;
    return (uint32_t)last[0] << 24 | (uint32_t)last[1] << 16 | (uint32_t)last[2] << 8 | last[3];
}
// The lamp test's single lamps: eight green, eight amber and, with the status driver, RTK and ATTENTION.
static unsigned single_lamps(const panel_boot_t *b) { return 16u + (b->status_byte ? 2u : 0u); }
// The singles, the three rows (every green, every amber, every lamp) and the dark step.
static uint32_t lamp_test_ms(const panel_boot_t *b)
{ return b->lamp_test ? single_lamps(b) * PANEL_BOOT_LAMP_MS + 3u * PANEL_BOOT_ROW_MS + PANEL_BOOT_LAMP_MS : 0; }
uint32_t panel_boot_ms(const panel_boot_t *b)
{ return lamp_test_ms(b) + (b->identity_valid ? PANEL_BOOT_IDENTITY_BITS * PANEL_BOOT_IDENTITY_MS : 0); }
bool panel_boot_lamps(const panel_boot_t *b, uint32_t elapsed_ms, panel_lamps_t *out)
{
    if (elapsed_ms >= panel_boot_ms(b)) return false;
    panel_lamps_t lamps = {0};
    const uint32_t test_ms = lamp_test_ms(b), singles_ms = single_lamps(b) * PANEL_BOOT_LAMP_MS;
    if (elapsed_ms < test_ms && elapsed_ms < singles_ms) {
        unsigned step = elapsed_ms / PANEL_BOOT_LAMP_MS;
        if (step < 8) lamps.green = (uint8_t)(1u << step);
        else if (step < 16) lamps.yellow = (uint8_t)(1u << (step - 8));
        else lamps.status = step == 16 ? PANEL_STATUS_RTK : PANEL_STATUS_ATTENTION;
    } else if (elapsed_ms < test_ms) {
        unsigned row = (elapsed_ms - singles_ms) / PANEL_BOOT_ROW_MS; // 3 is the dark step
        if (row == 0 || row == 2) lamps.green = 0xff;
        if (row == 1 || row == 2) lamps.yellow = 0xff;
        if (row == 2 && b->status_byte) lamps.status = PANEL_STATUS_RTK | PANEL_STATUS_ATTENTION;
    } else {
        unsigned step = (elapsed_ms - test_ms) / PANEL_BOOT_IDENTITY_MS;
        uint8_t column = (uint8_t)(1u << (step % 8));
        if (b->identity >> (PANEL_BOOT_IDENTITY_BITS - 1 - step) & 1) lamps.green = column;
        else lamps.yellow = column;
    }
    *out = lamps;
    return true;
}
