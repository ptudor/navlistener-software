#ifndef NVF_IO_EXPANDER_SERVICE_H
#define NVF_IO_EXPANDER_SERVICE_H
// The MCP23008's FreeRTOS glue: the register programme, a task woken by the INT line's
// falling edge that reads and logs the alert lines, the panel chain readback, and a status
// for diagnostics. One mutex guards the state and serializes the expander's transfers, so
// the interrupt task, the board task and the panel refresh task's readback never interleave
// them; ESP-IDF 5.5's i2c_master driver serializes transactions on the shared bus across
// device handles. Every call is a no-op (false, or an empty status) before io_expander_start.
#include <stdbool.h>
#include <stdint.h>
#include "driver/i2c_master.h"
#include "esp_err.h"
#include "io_expander.h"

typedef struct {
    bool configured, suspended;
    uint8_t chain_length;    // the panel chain, 24 or 16 bits once detected; 0 before
    uint8_t gpio;            // the last GPIO image (GP0-GP7)
    io_expander_line_t lines[IO_SOURCE_COUNT];
    uint32_t stuck;          // interrupt services that ended with INT still low
    uint32_t readback_runs, readback_mismatches, readback_faults, readback_errors;
    uint16_t rail_alert;     // the latest INA3221 alert flags (Mask/Enable CF, SF, WF)
    int64_t rail_alert_ms;   // their uptime; 0 before the first
} io_expander_status_t;

// Adds the expander at 0x24 (100 kHz), starts its task and configures it. The falling-edge
// handler on int_gpio, already an input, is installed once the programme reads back; a
// configuration that fails is retried by io_expander_poll.
esp_err_t io_expander_start(i2c_master_bus_handle_t bus, int int_gpio);
// From the board task, every pass: configures the expander again while it is unconfigured
// and not suspended. True when PWR_ALERT_N (the INA3221's alerts) has gone low since the last
// call; the caller then reads the INA3221's flags, which releases the latched line.
bool io_expander_poll(void);
// The register programme is in place.
bool io_expander_ready(void);
// Any future SENS_EN control must call these around the rail change: GP2-GP5 fall to 0 V
// with 3V3_SENS and the I2C pull-ups go with it. Suspend writes GPINTEN = 0 (best effort)
// and stops handling interrupts; resume runs the register programme again, true once it
// reads back (io_expander_poll retries otherwise).
void io_expander_suspend(void);
bool io_expander_resume(void);
// Shifts a 24-bit panel frame out through shift(), most significant bit first; while the
// expander is configured it reads GPIO before each clock and checks the GP0 samples against
// the chain (io_expander_chain_check). The frame is always shifted in full; sampling stops
// at a failed read, which leaves the expander for the board task to configure again. The
// caller holds the output enables off and latches. Results are logged by the expander's task.
// True when the samples were compared.
bool io_expander_readback(uint32_t frame, void (*shift)(int bit, void *ctx), void *ctx);
// Records the INA3221 alert flags the board task decoded, for the status.
void io_expander_rail_alert(uint16_t flags, int64_t now_ms);
void io_expander_status(io_expander_status_t *out);
// The status as one INFO line; when says why ("at boot", "(hourly)").
void io_expander_log_status(const char *when);
#endif
