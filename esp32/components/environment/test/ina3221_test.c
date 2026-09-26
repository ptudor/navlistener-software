#include "ina3221.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated INA3221: 16-bit registers read one at a time MSB first, a configuration a
// power cycle resets, and a conversion-ready flag set once a cycle completes after
// configuration and cleared by reading Mask/Enable.
typedef struct {
    uint16_t reg[0x100];
    bool absent, fail_read;
    unsigned since_config;   // ms since the configuration was written
    unsigned reads;
} chip_t;
static void chip_power_on(chip_t *c)
{
    memset(c->reg, 0, sizeof c->reg);
    c->reg[0x00] = 0x7127; c->reg[0x0f] = 0x0002; c->reg[0xfe] = 0x5449; c->reg[0xff] = 0x3220;
    c->since_config = 1000;
}
static bool chip_write(void *ctx, uint8_t address, uint8_t reg, const uint8_t *data, size_t length)
{
    chip_t *c = ctx;
    assert(address == INA3221_ADDRESS && reg == 0x00 && length == 2);
    if (c->absent) return false;
    c->reg[0] = (uint16_t)(data[0] << 8 | data[1]);
    c->reg[0x0f] &= ~1; c->since_config = 0;
    return true;
}
static bool chip_read(void *ctx, uint8_t address, uint8_t reg, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    assert(address == INA3221_ADDRESS && length == 2);
    if (c->absent || c->fail_read) return false;
    if (reg == 0x0f && c->since_config >= 422) c->reg[0x0f] |= 1;
    out[0] = c->reg[reg] >> 8; out[1] = (uint8_t)c->reg[reg];
    if (reg == 0x0f) c->reg[0x0f] &= ~1;
    c->reads++;
    return true;
}
static void chip_delay(void *ctx, unsigned ms) { ((chip_t *)ctx)->since_config += ms; }
static env_io_t chip_io(chip_t *c)
{
    memset(c, 0, sizeof *c);
    chip_power_on(c);
    return (env_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay_ms = chip_delay};
}

static void scaling_test(void)
{
    // SBOS576B's example: -80 mV is C180h. Full scale is 7FF8h: 163.8 mV and 32.76 V.
    assert(ina3221_shunt_uv(0xc180) == -80000);
    assert(ina3221_shunt_uv(0x7ff8) == 163800 && ina3221_shunt_uv(0x8000) == -163840);
    assert(ina3221_bus_mv(0x7ff8) == 32760 && ina3221_bus_mv(0x1388) == 5000);
    // One LSB each, and the reserved low bits ignored.
    assert(ina3221_shunt_uv(0x0008) == 40 && ina3221_shunt_uv(0xfff8) == -40 && ina3221_shunt_uv(0x000f) == 40);
    assert(ina3221_bus_mv(0x0008) == 8 && ina3221_bus_mv(0xfff8) == -8);
}

static void driver_test(void)
{
    chip_t c; env_io_t io = chip_io(&c);
    ina3221_sample_t s;
    assert(ina3221_init(&io) && c.reg[0] == 0x7727);
    // 5.008 V with 12.28 mV across 20 mOhm (614 mA); 3.296 V and 5.0 mV across 50 mOhm
    // (100 mA); 3.304 V and 4.0 mV across 20 mOhm (200 mA).
    c.reg[1] = 307 << 3; c.reg[2] = 626 << 3;
    c.reg[3] = 125 << 3; c.reg[4] = 412 << 3;
    c.reg[5] = 100 << 3; c.reg[6] = 413 << 3;
    // The first cycle after configuration is waited for.
    assert(ina3221_read(&io, &s) && c.since_config >= 422);
    assert(s.shunt_uv[0] == 12280 && s.bus_mv[0] == 5008 && s.shunt_uv[0] / 20 == 614);
    assert(s.shunt_uv[1] == 5000 && s.bus_mv[1] == 3296 && s.shunt_uv[1] / 50 == 100);
    assert(s.shunt_uv[2] == 4000 && s.bus_mv[2] == 3304 && s.shunt_uv[2] / 20 == 200);
    // Later cycles are ready at once.
    c.since_config = 30000; c.reads = 0;
    assert(ina3221_read(&io, &s) && c.reads == 8);
    // A reverse current through a shunt reads negative.
    c.reg[3] = (uint16_t)(0x10000 - (125 << 3)); c.since_config = 60000;
    assert(ina3221_read(&io, &s) && s.shunt_uv[1] == -5000);

    // A power cycle restores the default configuration: no reading until configured again.
    chip_power_on(&c);
    assert(!ina3221_read(&io, &s) && s.bus_mv[0] == 0);
    assert(ina3221_init(&io) && ina3221_read(&io, &s));
    // A failed transfer gives no reading, and no partial one.
    c.since_config = 90000; c.fail_read = true;
    assert(!ina3221_read(&io, &s) && s.shunt_uv[0] == 0 && s.bus_mv[2] == 0);
    c.fail_read = false;

    // Another part, or no part.
    io = chip_io(&c); c.reg[0xfe] = 0x5448;
    assert(!ina3221_init(&io));
    io = chip_io(&c); c.reg[0xff] = 0x2260;
    assert(!ina3221_init(&io));
    io = chip_io(&c); c.absent = true;
    assert(!ina3221_init(&io) && !ina3221_read(&io, &s));
}

// A part whose conversion-ready flag never sets.
static bool slow_read(void *ctx, uint8_t address, uint8_t reg, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    assert(address == INA3221_ADDRESS && length == 2);
    uint16_t v = reg == 0x0f ? 0 : c->reg[reg];
    out[0] = v >> 8; out[1] = (uint8_t)v;
    return true;
}

static void timeout_test(void)
{
    chip_t c; env_io_t io = chip_io(&c);
    assert(ina3221_init(&io));
    io.read = slow_read;
    ina3221_sample_t s;
    c.since_config = 0;
    assert(!ina3221_read(&io, &s));
    // It gives up after about 0.6 s, longer than the 0.47 s worst-case cycle.
    assert(c.since_config >= 470 && c.since_config <= 650);
}

int main(void)
{
    scaling_test(); driver_test(); timeout_test();
    puts("INA3221 scaling, configuration, conversion-ready wait and fault handling passed");
    return 0;
}
