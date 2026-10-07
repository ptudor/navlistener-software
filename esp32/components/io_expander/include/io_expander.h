#ifndef NVF_IO_EXPANDER_H
#define NVF_IO_EXPANDER_H
// Microchip MCP23008 8-bit I/O expander (DS20001919), the 162mm board's U39 at I2C 0x24.
// It collects the board's active-low alert lines on GP1-GP5 behind one interrupt (INT to
// GPIO3) and reads the LED panel chain's serial output back on GP0 (esp32/docs/IO-EXPANDER.md).
// This part is pure C over a register interface, with host tests in test/;
// io_expander_service.h is the FreeRTOS task and interrupt glue.
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

enum { MCP23008_ADDRESS = 0x24 };
// Register addresses with IOCON.BANK = 0 (Table 1-3).
enum {
    MCP23008_IODIR = 0x00, MCP23008_IPOL = 0x01, MCP23008_GPINTEN = 0x02, MCP23008_DEFVAL = 0x03,
    MCP23008_INTCON = 0x04, MCP23008_IOCON = 0x05, MCP23008_GPPU = 0x06, MCP23008_INTF = 0x07,
    MCP23008_INTCAP = 0x08, MCP23008_GPIO = 0x09,
};
enum {
    IO_EXPANDER_PANEL_SDO = 0x01, // GP0: the last TLC5916's SDO, sampled by the panel readback
    IO_EXPANDER_SOURCES = 0x3e,   // GP1-GP5: the alert lines, each interrupting on change
};
// The register programme for IODIR through GPPU, written and read back in that order: all
// inputs, no inversion, interrupt-on-change (INTCON = 0) on GP1-GP5 only, IOCON 0 (sequential
// addressing, slew-rate control, INT push-pull active-low), pull-ups on GP0, GP6 and GP7 only.
// GP1-GP5 come from the 3V3_ETH and 3V3_SENS domains, so their internal pull-ups stay off.
extern const uint8_t io_expander_programme[7];

// The alert lines, GP1-GP5 in order; each is active low.
typedef enum { IO_SOURCE_ETH, IO_SOURCE_BARO, IO_SOURCE_HUM, IO_SOURCE_TEMP, IO_SOURCE_PWR, IO_SOURCE_COUNT } io_source_t;
#define IO_SOURCE_PIN(source) (1u << ((source) + 1))
// "ETH", "BARO", "HUM", "TEMP" or "PWR".
const char *io_source_name(io_source_t source);

// One register per transfer. int_low reports the INT line (GPIO3) reading low.
typedef struct {
    void *ctx;
    bool (*read)(void *ctx, uint8_t reg, uint8_t *value);
    bool (*write)(void *ctx, uint8_t reg, uint8_t value);
    bool (*int_low)(void *ctx);
} io_expander_io_t;

typedef struct {
    uint32_t count;  // changes recorded since boot
    int64_t last_ms; // uptime of the latest; 0 before the first
    uint8_t level;   // the line's level at the latest: 0 low (asserted) or 1 high
} io_expander_line_t;

// The panel chain readback. A sample vector holds GP0 before each clock of a 24-bit frame
// write, bit k before clock k. The frame is the panel word (status << 16 | amber << 8 |
// green), shifted most significant bit first, so clock k shifts frame bit 23 - k.
enum { IO_EXPANDER_FRAME_BITS = 24 };
typedef struct {
    uint8_t length;      // the adopted chain length, 24 or 16; 0 until detected
    uint32_t runs;       // frames whose samples were compared
    uint32_t mismatches; // runs at the adopted length that differed
    uint32_t faults;     // runs before detection that matched neither length
} io_expander_chain_t;
typedef enum {
    IO_CHAIN_MATCH,        // the samples match the adopted length
    IO_CHAIN_MISMATCH,     // they do not; *diff holds the clocks that differ
    IO_CHAIN_DETECTED,     // the first run that matched one length: it is adopted
    IO_CHAIN_INCONCLUSIVE, // both lengths expect the same samples for this frame; nothing is decided
    IO_CHAIN_FAULT,        // neither length matched
    IO_CHAIN_UNDETECTED,   // neither matched for the third time: reported once
} io_chain_result_t;

typedef struct {
    bool configured;          // the programme read back; cleared by any failed transfer
    bool image_valid;         // image holds a read since the first configuration
    uint8_t image;            // GP0-GP7 at the latest GPIO or INTCAP read, adjusted per INTF
    uint8_t fell;             // alert lines that went low since io_expander_take_fell()
    uint8_t unlogged;         // alert lines with a change not yet logged
    uint8_t logged;           // alert lines logged at least once, at logged_ms
    io_expander_line_t lines[IO_SOURCE_COUNT];
    int64_t logged_ms[IO_SOURCE_COUNT];
    uint32_t stuck;           // services that ended with INT still low
    io_expander_chain_t chain;
} io_expander_t;

// Writes the programme register by register, reads each back, then reads GPIO, which clears
// an interrupt raised before or during it. The first configuration takes that image as the
// lines' starting levels and records any line already low as asserting; a later one records
// the changes since the previous image. False leaves it unconfigured: a transfer failed or a
// register read back different.
bool io_expander_configure(io_expander_t *x, const io_expander_io_t *io, int64_t now_ms);
// Writes GPINTEN = 0, so no line interrupts, and leaves the expander unconfigured. False when
// the write failed (as with the rail already off); it is unconfigured either way.
bool io_expander_disable(io_expander_t *x, const io_expander_io_t *io);
// Services an interrupt: reads INTF, then INTCAP, and records each flagged line at its new
// level; then reads GPIO once to confirm the levels and again while INT still reads low (a
// change arrived during handling), up to IO_EXPANDER_REREADS reads, recording what changed.
// Without a flagged line it only reads GPIO while INT is low. *low, if not NULL, reports INT
// still low at the end. False on a transfer failure, which leaves the expander unconfigured.
enum { IO_EXPANDER_REREADS = 4 };
bool io_expander_service(io_expander_t *x, const io_expander_io_t *io, int64_t now_ms, bool *low);
// Records the alert lines that changed in a GPIO image read outside io_expander_service (the
// panel readback): reading GPIO clears a pending interrupt, so its changes are recorded here.
void io_expander_gpio(io_expander_t *x, uint8_t gpio, int64_t now_ms);
// The alert lines (IO_SOURCE_PIN bits) that went low since the last call, cleared.
uint8_t io_expander_take_fell(io_expander_t *x);
// Lines with an unlogged change whose last log line is at least 10 s old, as IO_SOURCE_PIN
// bits, marked logged at now_ms. A change inside a line's 10 s is counted at once and waits:
// io_expander_log_wait_ms says how long, so the line's latest level is always logged.
enum { IO_EXPANDER_LOG_MS = 10000 };
uint8_t io_expander_take_log(io_expander_t *x, int64_t now_ms);
// Milliseconds until the earliest waiting change may be logged; -1 when none waits.
int64_t io_expander_log_wait_ms(const io_expander_t *x, int64_t now_ms);

// The expected samples for a frame through a chain of length bits (24 or 16): the chain holds
// the frame from the previous write, so before clock k GP0 shows the bit shifted length
// clocks earlier, frame position (k + 24 - length) mod 24 counted from the first shifted.
uint32_t io_expander_chain_expected(uint32_t frame, unsigned length);
// Compares one run's samples. Before detection: adopts 24 or 16 bits when exactly one matches,
// is inconclusive when both match (the frame cannot tell them apart), and otherwise counts a
// fault, returning IO_CHAIN_UNDETECTED at the third. After: compares with the adopted length.
// *diff receives the differing clocks for IO_CHAIN_MISMATCH and 0 otherwise.
io_chain_result_t io_expander_chain_check(io_expander_chain_t *c, uint32_t frame, uint32_t samples, uint32_t *diff);
#endif
