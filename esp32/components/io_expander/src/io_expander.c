#include "io_expander.h"

const uint8_t io_expander_programme[7] = {
    0xff,                // IODIR: all inputs
    0x00,                // IPOL: no inversion; the lines are active low and reported as such
    IO_EXPANDER_SOURCES, // GPINTEN: interrupt-on-change on GP1-GP5 only
    0x00,                // DEFVAL: unused with INTCON = 0
    0x00,                // INTCON: compare with the previous value, so a steady level does not retrigger
    0x00,                // IOCON: sequential addressing, slew-rate control, INT push-pull active-low
    0xc1,                // GPPU: GP0 reads 1 with no chain attached; GP6 and GP7 never float
};

const char *io_source_name(io_source_t source)
{
    static const char *const names[IO_SOURCE_COUNT] = {"ETH", "BARO", "HUM", "TEMP", "PWR"};
    return (unsigned)source < IO_SOURCE_COUNT ? names[source] : "?";
}

// Records a change on each alert line in changed, at its level in levels.
static void record(io_expander_t *x, uint8_t changed, uint8_t levels, int64_t now)
{
    for (unsigned s = 0; s < IO_SOURCE_COUNT; s++) {
        const uint8_t pin = IO_SOURCE_PIN(s);
        if (!(changed & pin)) continue;
        io_expander_line_t *line = &x->lines[s];
        line->count++;
        line->last_ms = now;
        line->level = (levels & pin) != 0;
        if (!line->level) x->fell |= pin;
        x->unlogged |= pin;
    }
}

void io_expander_gpio(io_expander_t *x, uint8_t gpio, int64_t now)
{
    if (x->image_valid) record(x, (uint8_t)(x->image ^ gpio) & IO_EXPANDER_SOURCES, gpio, now);
    x->image = gpio;
    x->image_valid = true;
}

bool io_expander_configure(io_expander_t *x, const io_expander_io_t *io, int64_t now)
{
    x->configured = false;
    for (uint8_t reg = 0; reg < sizeof io_expander_programme; reg++)
        if (!io->write(io->ctx, reg, io_expander_programme[reg])) return false;
    for (uint8_t reg = 0; reg < sizeof io_expander_programme; reg++) {
        uint8_t value;
        if (!io->read(io->ctx, reg, &value) || value != io_expander_programme[reg]) return false;
    }
    uint8_t gpio;
    if (!io->read(io->ctx, MCP23008_GPIO, &gpio)) return false;
    if (x->image_valid) {
        io_expander_gpio(x, gpio, now);
    } else {
        x->image = gpio;
        x->image_valid = true;
        record(x, (uint8_t)~gpio & IO_EXPANDER_SOURCES, gpio, now);
    }
    x->configured = true;
    return true;
}

bool io_expander_disable(io_expander_t *x, const io_expander_io_t *io)
{
    x->configured = false;
    return io->write(io->ctx, MCP23008_GPINTEN, 0);
}

bool io_expander_service(io_expander_t *x, const io_expander_io_t *io, int64_t now, bool *low)
{
    uint8_t intf, intcap, gpio;
    bool still = false;
    if (!io->read(io->ctx, MCP23008_INTF, &intf)) goto failed;
    intf &= IO_EXPANDER_SOURCES;
    bool confirm = intf != 0;
    if (confirm) {
        // Reading INTCAP clears the interrupt. With INTCON = 0 every flagged line has changed
        // state since the image: the line whose change INTCAP captured shows its new level
        // there, and a line flagged after the capture still shows its old one (DS20001919
        // section 1.6.8). Either way the new level is the image's inverse.
        if (!io->read(io->ctx, MCP23008_INTCAP, &intcap)) goto failed;
        const uint8_t levels = x->image ^ intf;
        record(x, intf, levels, now);
        x->image = (uint8_t)((intcap & ~IO_EXPANDER_SOURCES) | (levels & IO_EXPANDER_SOURCES));
    }
    // One GPIO read confirms the levels: a line can change back before the service, as the
    // INA3221's alert does when a rail sample reads its flags first, and that return raises
    // no interrupt of its own. A change that arrived during handling holds INT low again, so
    // GPIO is read until it rises.
    for (unsigned reads = 0;; reads++) {
        still = io->int_low(io->ctx);
        if ((!still && !confirm) || reads == IO_EXPANDER_REREADS) break;
        confirm = false;
        if (!io->read(io->ctx, MCP23008_GPIO, &gpio)) goto failed;
        io_expander_gpio(x, gpio, now);
    }
    if (still) x->stuck++;
    if (low) *low = still;
    return true;
failed:
    x->configured = false;
    if (low) *low = io->int_low(io->ctx);
    return false;
}

uint8_t io_expander_take_fell(io_expander_t *x)
{
    const uint8_t fell = x->fell;
    x->fell = 0;
    return fell;
}

uint8_t io_expander_take_log(io_expander_t *x, int64_t now)
{
    uint8_t due = 0;
    for (unsigned s = 0; s < IO_SOURCE_COUNT; s++) {
        const uint8_t pin = IO_SOURCE_PIN(s);
        if (!(x->unlogged & pin)) continue;
        if ((x->logged & pin) && now - x->logged_ms[s] < IO_EXPANDER_LOG_MS) continue;
        due |= pin;
        x->unlogged &= (uint8_t)~pin;
        x->logged |= pin;
        x->logged_ms[s] = now;
    }
    return due;
}

int64_t io_expander_log_wait_ms(const io_expander_t *x, int64_t now)
{
    int64_t wait = -1;
    for (unsigned s = 0; s < IO_SOURCE_COUNT; s++) {
        const uint8_t pin = IO_SOURCE_PIN(s);
        if (!(x->unlogged & pin)) continue;
        int64_t left = (x->logged & pin) ? x->logged_ms[s] + IO_EXPANDER_LOG_MS - now : 0;
        if (left < 0) left = 0;
        if (wait < 0 || left < wait) wait = left;
    }
    return wait;
}

// The bit the write shifts at clock k, counted from the first shifted.
static uint32_t shifted(uint32_t frame, unsigned k)
{
    return (frame >> (IO_EXPANDER_FRAME_BITS - 1 - k)) & 1;
}

uint32_t io_expander_chain_expected(uint32_t frame, unsigned length)
{
    uint32_t samples = 0;
    for (unsigned k = 0; k < IO_EXPANDER_FRAME_BITS; k++)
        samples |= shifted(frame, (k + IO_EXPANDER_FRAME_BITS - length) % IO_EXPANDER_FRAME_BITS) << k;
    return samples;
}

io_chain_result_t io_expander_chain_check(io_expander_chain_t *c, uint32_t frame, uint32_t samples, uint32_t *diff)
{
    *diff = 0;
    c->runs++;
    if (c->length) {
        *diff = samples ^ io_expander_chain_expected(frame, c->length);
        if (!*diff) return IO_CHAIN_MATCH;
        c->mismatches++;
        return IO_CHAIN_MISMATCH;
    }
    const bool full = samples == io_expander_chain_expected(frame, 24);
    const bool half = samples == io_expander_chain_expected(frame, 16);
    if (full && half) return IO_CHAIN_INCONCLUSIVE;
    if (full || half) {
        c->length = full ? 24 : 16;
        return IO_CHAIN_DETECTED;
    }
    return ++c->faults == 3 ? IO_CHAIN_UNDETECTED : IO_CHAIN_FAULT;
}
