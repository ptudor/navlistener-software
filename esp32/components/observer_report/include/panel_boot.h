#ifndef PANEL_BOOT_H
#define PANEL_BOOT_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
// The panel's boot sequence, before it shows reception.
//
// Every boot starts with a lamp chase, so that a lamp which does not light is seen. A run of
// PANEL_BOOT_CHASE_LAMPS lit lamps moves one lamp every PANEL_BOOT_CHASE_MS along the top row
// from left to right, then along the bottom row: the eight green lamps and, on a panel with
// the status driver, RTK at that row's end; then the eight amber lamps and ATTENTION. The
// run enters lamp by lamp at the first and leaves past the last, so each lamp is lit once,
// for PANEL_BOOT_CHASE_LAMPS steps, and a dark lamp is a gap in the run as it passes.
// PANEL_BOOT_CHASE_DARK dark steps end it. No step lights every lamp.
//
// Then the board identity is swept: one column per PANEL_BOOT_IDENTITY_MS steps left to
// right, four passes, lit green for a 1 bit and amber for a 0 bit of the serial's last 32
// bits, its final eight hex digits, most significant bit first. The same board shows the
// same sweep at every boot, and a board whose identity was not read shows none.
#define PANEL_BOOT_CHASE_MS 150u
#define PANEL_BOOT_CHASE_LAMPS 5u
#define PANEL_BOOT_CHASE_DARK 2u
#define PANEL_BOOT_IDENTITY_MS 100u
#define PANEL_BOOT_IDENTITY_BITS 32u
// The status byte's lamps on a 24-bit panel (the 162mm panel's U14).
#define PANEL_STATUS_RTK 0x01u
#define PANEL_STATUS_ATTENTION 0x02u

typedef struct { uint8_t status, green, yellow; } panel_lamps_t;
typedef struct {
    bool chase;       // the lamp chase is shown: false only where no panel is driven
    bool status_byte; // the panel has the status driver: its lamps are in the chase too
    bool identity_valid;
    uint32_t identity; // the serial's last 32 bits (panel_boot_identity)
} panel_boot_t;

// The last four bytes of a serial, the first of them most significant; 0 for fewer than four.
uint32_t panel_boot_identity(const uint8_t *serial, size_t length);
// The sequence's length; 0 when it shows nothing.
uint32_t panel_boot_ms(const panel_boot_t *b);
// The lamps elapsed_ms into the sequence; false, leaving *out alone, once it has ended.
bool panel_boot_lamps(const panel_boot_t *b, uint32_t elapsed_ms, panel_lamps_t *out);
#endif
