#ifndef PANEL_BOOT_H
#define PANEL_BOOT_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
// The panel's boot sequence, before it shows reception. Every boot sweeps the board
// identity: one column per PANEL_BOOT_IDENTITY_MS steps left to right, four passes, lit
// green for a 1 bit and amber for a 0 bit of the serial's last 32 bits, its final eight
// hex digits, most significant bit first. The same board shows the same sweep at every
// boot, and a board whose identity was not read shows none. The first
// PANEL_BOOT_LAMP_TESTS boots, and every PANEL_BOOT_LAMP_TESTS-th after, run a lamp test
// before it: each green lamp alone left to right, each amber lamp, the status lamps (RTK,
// then ATTENTION) where the panel has them, then every green, every amber and every lamp
// together, and a dark step.
#define PANEL_BOOT_LAMP_TESTS 10u
#define PANEL_BOOT_LAMP_MS 250u
#define PANEL_BOOT_ROW_MS 750u
#define PANEL_BOOT_IDENTITY_MS 100u
#define PANEL_BOOT_IDENTITY_BITS 32u
// The status byte's lamps on a 24-bit panel (the 162mm panel's U14).
#define PANEL_STATUS_RTK 0x01u
#define PANEL_STATUS_ATTENTION 0x02u

typedef struct { uint8_t status, green, yellow; } panel_lamps_t;
typedef struct {
    bool lamp_test;   // this boot runs the lamp test (panel_boot_lamp_test)
    bool status_byte; // the panel has the status driver: its lamps are tested too
    bool identity_valid;
    uint32_t identity; // the serial's last 32 bits (panel_boot_identity)
} panel_boot_t;

// Whether boot number boot (1 for the first) runs the lamp test; 0, an unknown count, does.
bool panel_boot_lamp_test(uint32_t boot);
// The last four bytes of a serial, the first of them most significant; 0 for fewer than four.
uint32_t panel_boot_identity(const uint8_t *serial, size_t length);
// The sequence's length; 0 when it shows nothing.
uint32_t panel_boot_ms(const panel_boot_t *b);
// The lamps elapsed_ms into the sequence; false, leaving *out alone, once it has ended.
bool panel_boot_lamps(const panel_boot_t *b, uint32_t elapsed_ms, panel_lamps_t *out);
#endif
