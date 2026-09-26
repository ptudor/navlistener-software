#include "bmp5.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated BMP581: the registers the driver uses, the soft reset (not acknowledged over
// I2C), POR and NVM status, and forced conversions that set data ready after their time.
typedef struct {
    uint8_t reg[0x80];
    uint8_t t[3], p[3];      // the next conversion's TEMP_DATA and PRESS_DATA, XLSB first
    bool absent, fail_read, ack_reset, never_ready, nvm_error, no_por;
    unsigned since_forced;   // ms waited since forced mode was started
    bool converting;
    unsigned resets, conversions;
} chip_t;
static void chip_reset(chip_t *c)
{
    memset(c->reg, 0, sizeof c->reg);
    c->reg[0x01] = 0x50; c->reg[0x02] = 0x32; c->reg[0x13] = 0x30; c->reg[0x14] = 0x35;
    c->reg[0x28] = c->nvm_error ? 0x06 : 0x02; c->reg[0x27] = c->no_por ? 0 : 0x10; c->reg[0x30] = 0x03;
    c->reg[0x37] = 0x70;
    for (unsigned i = 0x1d; i <= 0x22; i++) c->reg[i] = 0x7f;
    c->converting = false; c->resets++;
}
static void chip_advance(chip_t *c)
{
    if (!c->converting || c->since_forced < 12 || c->never_ready) return;
    memcpy(c->reg + 0x1d, c->t, 3); memcpy(c->reg + 0x20, c->p, 3);
    c->reg[0x27] |= 0x01; c->reg[0x37] &= ~3; c->converting = false; c->conversions++;
}
static bool chip_write(void *ctx, uint8_t address, uint8_t reg, const uint8_t *data, size_t length)
{
    chip_t *c = ctx;
    assert(address == BMP5_ADDRESS && length == 1);
    if (c->absent) return false;
    if (reg == 0x7e) {
        assert(data[0] == 0xb6);
        chip_reset(c);
        return c->ack_reset;
    }
    assert(reg == 0x15 || reg == 0x36 || reg == 0x37);
    // Configuration is written only in standby.
    assert(!c->converting);
    c->reg[reg] = data[0];
    if (reg == 0x37 && (data[0] & 3) == 2) { c->converting = true; c->since_forced = 0; }
    return true;
}
static bool chip_read(void *ctx, uint8_t address, uint8_t reg, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    assert(address == BMP5_ADDRESS);
    if (c->absent || c->fail_read) return false;
    chip_advance(c);
    // A burst read of the data registers only; everything else one register at a time.
    assert(length == 1 || (reg == 0x1d && length == 6));
    memcpy(out, c->reg + reg, length);
    if (reg == 0x27) c->reg[0x27] = 0; // clear on read
    return true;
}
static void chip_delay(void *ctx, unsigned ms) { ((chip_t *)ctx)->since_forced += ms; }
static env_io_t chip_io(chip_t *c)
{
    memset(c, 0, sizeof *c);
    chip_reset(c); c->resets = 0;
    // 25.00 C and 100000.00 Pa.
    const uint8_t t[3] = {0x00, 0x00, 0x19}, p[3] = {0x00, 0xa8, 0x61};
    memcpy(c->t, t, 3); memcpy(c->p, p, 3);
    return (env_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay_ms = chip_delay};
}
static void set24(uint8_t out[3], int32_t value)
{
    uint32_t v = (uint32_t)value & 0xffffff;
    out[0] = v; out[1] = v >> 8; out[2] = v >> 16;
}

static void conversion_test(void)
{
    uint8_t raw[6];
    int32_t t, p;
    // Section 4.5: 25 C is 25 x 2^16; 100 kPa is 100000 x 2^6.
    set24(raw, 25 * 65536); set24(raw + 3, 100000 * 64);
    assert(bmp5_convert(raw, &t, &p) && t == 2500 && p == 100000);
    // Negative temperatures, and rounding half away from zero at 0.005 C and 0.5 Pa.
    set24(raw, -10 * 65536 - 328); set24(raw + 3, 101325 * 64 + 32);
    assert(bmp5_convert(raw, &t, &p) && t == -1001 && p == 101326);
    set24(raw, -10 * 65536 - 327); set24(raw + 3, 101325 * 64 + 31);
    assert(bmp5_convert(raw, &t, &p) && t == -1000 && p == 101325);
    // The ends of the operating range, and just past them.
    set24(raw, 85 * 65536); set24(raw + 3, 125000 * 64);
    assert(bmp5_convert(raw, &t, &p) && t == 8500 && p == 125000);
    set24(raw, -40 * 65536); set24(raw + 3, 30000 * 64);
    assert(bmp5_convert(raw, &t, &p) && t == -4000 && p == 30000);
    set24(raw, 85 * 65536 + 400); assert(!bmp5_convert(raw, &t, &p));
    set24(raw, 25 * 65536); set24(raw + 3, 29999 * 64); assert(!bmp5_convert(raw, &t, &p));
    // Registers never written read 0x7f7f7f: 127.5 C and 130.6 kPa.
    memset(raw, 0x7f, sizeof raw); assert(!bmp5_convert(raw, &t, &p));
}

static void driver_test(void)
{
    chip_t c; env_io_t io = chip_io(&c);
    int32_t t, p;
    // The unacknowledged reset still resets: the checks see its POR flag.
    assert(bmp5_init(&io) == BMP5_READY && c.resets == 1);
    assert(c.reg[0x36] == 0x61 && c.reg[0x15] == 0x01 && c.reg[0x37] == 0xf0 && c.reg[0x14] == 0x35);
    assert(bmp5_read(&io, &t, &p) && t == 2500 && p == 100000 && c.conversions == 1);
    // The part returns to standby after each forced conversion.
    assert((c.reg[0x37] & 3) == 0);
    // Lost configuration must be detected before issuing a conversion; plausible
    // data from an earlier cycle cannot hide the reset.
    c.reg[0x36] = 0;
    assert(!bmp5_read(&io, &t, &p) && c.conversions == 1);
    assert(bmp5_init(&io) == BMP5_READY);
    c.reg[0x27] = 0x11; // data-ready together with an unexpected reset
    assert(!bmp5_read(&io, &t, &p) && c.conversions == 1);
    assert(bmp5_init(&io) == BMP5_READY);
    set24(c.t, 2 * 65536 + 16384); set24(c.p, 87654 * 64);
    assert(bmp5_read(&io, &t, &p) && t == 225 && p == 87654 && c.conversions == 2);
    // No data ready: no reading, however long it is polled.
    c.never_ready = true;
    assert(!bmp5_read(&io, &t, &p));
    c.never_ready = false; c.fail_read = true;
    assert(!bmp5_read(&io, &t, &p));

    // An acknowledged reset is fine too.
    io = chip_io(&c); c.ack_reset = true;
    assert(bmp5_init(&io) == BMP5_READY);
    // Another part, an NVM error, or a reset that did not happen are rejected.
    io = chip_io(&c); c.reg[0x01] = 0x51;
    assert(bmp5_init(&io) == BMP5_READY); // the reset restores the real ID
    io = chip_io(&c); c.nvm_error = true;
    assert(bmp5_init(&io) == BMP5_REJECTED);
    io = chip_io(&c); c.no_por = true;
    assert(bmp5_init(&io) == BMP5_REJECTED);
    io = chip_io(&c); c.absent = true;
    assert(bmp5_init(&io) == BMP5_ABSENT && !bmp5_read(&io, &t, &p));
    io = chip_io(&c); c.fail_read = true;
    assert(bmp5_init(&io) == BMP5_ABSENT);
}

// A BMP3 part at the same address answers CHIP_ID 0x50 at register 0x00, not 0x01.
static bool other_read(void *ctx, uint8_t address, uint8_t reg, uint8_t *out, size_t length)
{
    (void)ctx; (void)address; assert(length == 1);
    *out = reg == 0x00 ? 0x50 : 0x00;
    return true;
}
static bool other_write(void *ctx, uint8_t address, uint8_t reg, const uint8_t *data, size_t length)
{
    (void)ctx; (void)address; (void)reg; (void)data; (void)length;
    return true;
}
static void other_delay(void *ctx, unsigned ms) { (void)ctx; (void)ms; }

static void identity_test(void)
{
    env_io_t io = {.read = other_read, .write = other_write, .delay_ms = other_delay};
    assert(bmp5_init(&io) == BMP5_REJECTED);
}

int main(void)
{
    conversion_test(); driver_test(); identity_test();
    puts("BMP5 scaling, reset checks, forced conversion and data-ready handling passed");
    return 0;
}
