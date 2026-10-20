#include "sht3x.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated SHT3x-DIS at one address: 16-bit commands written MSB first, the status word and
// measurement results read back without a register pointer, each word followed by its CRC
// (datasheet sections 4.3-4.12). A read before a conversion has finished is not acknowledged,
// as without clock stretching; periodic mode accepts only a break.
typedef struct {
    uint8_t address;
    bool absent, periodic, heater, corrupt;
    uint16_t temperature, humidity;
    unsigned slept_ms, measured_at, resets, breaks;
    enum { NOTHING, STATUS, RESULT } pending;
} chip_t;
static void word(uint8_t *out, uint16_t value, bool corrupt)
{
    out[0] = value >> 8; out[1] = (uint8_t)value;
    out[2] = sht3x_crc(out) ^ (corrupt ? 1 : 0);
}
static bool chip_write(void *ctx, uint8_t address, uint8_t reg, const uint8_t *data, size_t length)
{
    chip_t *c = ctx;
    if (c->absent || address != c->address || length != 1) return false;
    const uint16_t code = (uint16_t)(reg << 8 | data[0]);
    if (code == 0x3093) {                   // break
        if (!c->periodic) return false;     // declined in single-shot mode
        c->periodic = false; c->breaks++;
        return true;
    }
    if (c->periodic) return false;
    switch (code) {
    case 0x30A2: c->heater = false; c->resets++; c->pending = NOTHING; return true;
    case 0xF32D: c->pending = STATUS; return true;
    case 0x2400: c->pending = RESULT; c->measured_at = c->slept_ms; return true;
    default: assert(!"unexpected command"); return false;
    }
}
static bool chip_receive(void *ctx, uint8_t address, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    if (c->absent || address != c->address) return false;
    if (c->pending == STATUS) {
        assert(length == 3);
        word(out, (uint16_t)(0x8010 | (c->heater ? 0x2000 : 0)), c->corrupt); // alert pending, reset seen
    } else if (c->pending == RESULT && c->slept_ms - c->measured_at >= 15) {
        assert(length == 6);
        word(out, c->temperature, false);
        word(out + 3, c->humidity, c->corrupt);
    } else {
        return false;
    }
    c->pending = NOTHING;
    return true;
}
static bool chip_read(void *ctx, uint8_t address, uint8_t reg, uint8_t *out, size_t length)
{
    (void)ctx; (void)address; (void)reg; (void)out; (void)length;
    assert(!"a register-pointer read would send the part half a command");
    return false;
}
static void chip_delay(void *ctx, unsigned ms) { ((chip_t *)ctx)->slept_ms += ms; }
static env_io_t chip_io(chip_t *c)
{
    *c = (chip_t){.address = 0x44, .temperature = 26214, .humidity = 32768};
    return (env_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay_ms = chip_delay,
                      .receive = chip_receive};
}

int main(void)
{
    const uint8_t example[2] = {0xBE, 0xEF};
    assert(sht3x_crc(example) == 0x92); // Table 19's example

    chip_t c;
    int16_t t;
    uint16_t rh;
    // Idle in single-shot mode: the break is declined and not waited for, then a soft reset.
    env_io_t io = chip_io(&c);
    assert(sht3x_start(&io, 0x44) && c.resets == 1 && !c.breaks && c.slept_ms == 2);
    assert(sht3x_read(&io, 0x44, &t, &rh) && t == 2500 && rh == 5000);
    c.temperature = 32768; c.humidity = 65535;
    assert(sht3x_read(&io, 0x44, &t, &rh) && t == 4250 && rh == 10000);
    c.humidity = 0;
    assert(sht3x_read(&io, 0x44, &t, &rh) && rh == 0);
    // The operating range's edges, in the part's own steps.
    c.temperature = 1873; assert(sht3x_read(&io, 0x44, &t, &rh) && t == -4000);
    c.temperature = 1872; assert(!sht3x_read(&io, 0x44, &t, &rh));
    c.temperature = 63666; assert(sht3x_read(&io, 0x44, &t, &rh) && t == 12500);
    c.temperature = 63667; assert(!sht3x_read(&io, 0x44, &t, &rh));
    // A corrupted word is never used.
    c.temperature = 26214; c.corrupt = true;
    assert(!sht3x_read(&io, 0x44, &t, &rh));
    c.corrupt = false;
    assert(sht3x_read(&io, 0x44, &t, &rh) && t == 2500);

    // Left in periodic mode with the heater on: stopped, reset, and confirmed off.
    io = chip_io(&c); c.periodic = true; c.heater = true;
    assert(sht3x_start(&io, 0x44) && c.breaks == 1 && c.resets == 1 && !c.heater && c.slept_ms == 17);
    // A status word that fails its CRC does not identify the part.
    io = chip_io(&c); c.corrupt = true;
    assert(!sht3x_start(&io, 0x44));
    // Absent, at another address, or on a bus without register-less reads: no part.
    io = chip_io(&c); c.absent = true;
    assert(!sht3x_start(&io, 0x44) && !sht3x_read(&io, 0x44, &t, &rh) && c.slept_ms == 0);
    io = chip_io(&c);
    assert(!sht3x_start(&io, 0x45));
    io = chip_io(&c); io.receive = NULL;
    assert(!sht3x_start(&io, 0x44) && !sht3x_read(&io, 0x44, &t, &rh));
    puts("SHT3x CRC, break and soft reset, heater off, conversion, range and CRC rejection passed");
    return 0;
}
