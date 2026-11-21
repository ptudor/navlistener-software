#include "panel_boot.h"
uint32_t panel_boot_identity(const uint8_t *serial, size_t length)
{
    if (!serial || length < 4) return 0;
    const uint8_t *last = serial + length - 4;
    return (uint32_t)last[0] << 24 | (uint32_t)last[1] << 16 | (uint32_t)last[2] << 8 | last[3];
}
// The lamps of one row, in the order the chase lights them: the eight columns and, with the
// status driver, that row's status lamp (RTK on the top row, ATTENTION on the bottom).
static unsigned row_lamps(const panel_boot_t *b) { return b->status_byte ? 9u : 8u; }
static void light(const panel_boot_t *b, unsigned lamp, panel_lamps_t *lamps)
{
    const bool top = lamp < row_lamps(b);
    const unsigned column = lamp % row_lamps(b);
    if (column == 8) lamps->status |= top ? PANEL_STATUS_RTK : PANEL_STATUS_ATTENTION;
    else if (top) lamps->green |= (uint8_t)(1u << column);
    else lamps->yellow |= (uint8_t)(1u << column);
}
// One step for each lamp as the run's head reaches it, the steps its tail takes to leave, and
// the dark steps.
static uint32_t chase_ms(const panel_boot_t *b)
{
    if (!b->chase) return 0;
    return (2u * row_lamps(b) + PANEL_BOOT_CHASE_LAMPS - 1u + PANEL_BOOT_CHASE_DARK) * PANEL_BOOT_CHASE_MS;
}
uint32_t panel_boot_ms(const panel_boot_t *b)
{ return chase_ms(b) + (b->identity_valid ? PANEL_BOOT_IDENTITY_BITS * PANEL_BOOT_IDENTITY_MS : 0); }
bool panel_boot_lamps(const panel_boot_t *b, uint32_t elapsed_ms, panel_lamps_t *out)
{
    if (elapsed_ms >= panel_boot_ms(b)) return false;
    panel_lamps_t lamps = {0};
    const uint32_t chase = chase_ms(b);
    if (elapsed_ms < chase) {
        // The head is on lamp `step`; the run is the head and the lamps behind it that exist.
        const unsigned step = elapsed_ms / PANEL_BOOT_CHASE_MS, count = 2u * row_lamps(b);
        for (unsigned behind = 0; behind < PANEL_BOOT_CHASE_LAMPS; behind++)
            if (step >= behind && step - behind < count) light(b, step - behind, &lamps);
    } else {
        unsigned step = (elapsed_ms - chase) / PANEL_BOOT_IDENTITY_MS;
        uint8_t column = (uint8_t)(1u << (step % 8));
        if (b->identity >> (PANEL_BOOT_IDENTITY_BITS - 1 - step) & 1) lamps.green = column;
        else lamps.yellow = column;
    }
    *out = lamps;
    return true;
}
