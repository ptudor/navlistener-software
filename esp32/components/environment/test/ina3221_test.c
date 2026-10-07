#include "ina3221.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated INA3221: 16-bit registers read one at a time MSB first, a configuration and
// alert limits a power cycle resets, and a conversion-ready flag set once a cycle completes
// after configuration. Reading Mask/Enable clears conversion-ready and the alert flags;
// writing it changes only its settings (Table 8-34).
typedef struct {
    uint16_t reg[0x100];
    bool absent, fail_read, frozen_limits;
    unsigned since_config;   // ms since the configuration was written
    unsigned reads, attempts, fail_read_at, writes, fail_write_at;
} chip_t;
static void chip_power_on(chip_t *c)
{
    memset(c->reg, 0, sizeof c->reg);
    c->reg[0x00] = 0x7127; c->reg[0x0f] = 0x0002; c->reg[0xfe] = 0x5449; c->reg[0xff] = 0x3220;
    for (unsigned r = 0x07; r <= 0x0c; r++) c->reg[r] = 0x7ff8;
    c->reg[0x0e] = 0x7ffe; c->reg[0x10] = 0x2710; c->reg[0x11] = 0x2328;
    c->since_config = 1000;
}
static bool chip_write(void *ctx, uint8_t address, uint8_t reg, const uint8_t *data, size_t length)
{
    chip_t *c = ctx;
    assert(address == INA3221_ADDRESS && length == 2);
    assert(reg == 0x00 || (reg >= 0x07 && reg <= 0x0c) || reg == 0x0f);
    if (c->absent || ++c->writes == c->fail_write_at) return false;
    const uint16_t value = (uint16_t)(data[0] << 8 | data[1]);
    if (reg == 0x0f) c->reg[0x0f] = (uint16_t)((c->reg[0x0f] & 0x03ff) | (value & 0xfc00));
    else if (reg == 0x00) { c->reg[0] = value; c->reg[0x0f] &= ~1; c->since_config = 0; }
    else if (!c->frozen_limits) c->reg[reg] = value;
    return true;
}
static bool chip_read(void *ctx, uint8_t address, uint8_t reg, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    assert(address == INA3221_ADDRESS && length == 2);
    if (c->absent || c->fail_read || ++c->attempts == c->fail_read_at) return false;
    if (reg == 0x0f && c->since_config >= 422) c->reg[0x0f] |= 1;
    out[0] = c->reg[reg] >> 8; out[1] = (uint8_t)c->reg[reg];
    if (reg == 0x0f) c->reg[0x0f] &= (uint16_t)~(1 | INA3221_MASK_ALERTS);
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

static void limit_test(void)
{
    // 1300 mA through 20 mOhm is 26 000 uV: 650 steps of 40 uV, in bits 15-3.
    assert(ina3221_limit(1300 * 20) == 0x1450 && ina3221_limit(500 * 50) == 0x1388);
    // The X20 row's other limits: 1000 mA x 20 mOhm, 350 mA x 50 mOhm (437.5 steps, rounded
    // up), 800 mA x 20 mOhm.
    assert(ina3221_limit(1000 * 20) == 0x0fa0 && ina3221_limit(350 * 50) == 0x0db0 && ina3221_limit(800 * 20) == 0x0c80);
    // The nearest step; a limit decodes back to within half a step of what was asked.
    assert(ina3221_limit(40) == 0x0008 && ina3221_limit(19) == 0 && ina3221_limit(20) == 0x0008 && ina3221_limit(59) == 0x0008);
    for (int32_t uv = -163840; uv <= 163800; uv += 997)
        assert(ina3221_shunt_uv(ina3221_limit(uv)) - uv <= 20 && uv - ina3221_shunt_uv(ina3221_limit(uv)) <= 20);
    // Clamped to the register range: above 163.8 mV is 0x7FF8, below -163.84 mV is 0x8000.
    assert(ina3221_limit(163800) == 0x7ff8 && ina3221_limit(163821) == 0x7ff8 && ina3221_limit(INT32_MAX) == 0x7ff8);
    assert(ina3221_limit(-40) == 0xfff8 && ina3221_limit(-163840) == 0x8000 && ina3221_limit(INT32_MIN) == 0x8000);
    assert((ina3221_limit(12345) & 7) == 0);
}

static void decode_test(void)
{
    ina3221_alerts_t a;
    // 0x0C90: WEN (bit 11), CEN (bit 10), CF3 (bit 7) and WF2 (bit 4).
    ina3221_decode_alerts(0x0c90, &a);
    assert(a.mask_enable == 0x0c90 && a.critical_latch && a.warning_latch);
    assert(a.critical == 4 && a.warning == 2 && !a.summation && !a.power_valid && !a.timing_control && !a.conversion_ready);
    // CF1 is bit 9: CEN, WEN, CF1 and WF2 is 0x0E10.
    ina3221_decode_alerts(0x0e10, &a);
    assert(a.critical_latch && a.warning_latch && a.critical == 1 && a.warning == 2);
    // Every flag on its own bit.
    static const struct { uint16_t mask; uint8_t critical, warning; bool sf, pvf, tcf, cvrf; } bits[] = {
        {.mask = 0x0200, .critical = 1}, {.mask = 0x0100, .critical = 2}, {.mask = 0x0080, .critical = 4},
        {.mask = 0x0040, .sf = true}, {.mask = 0x0020, .warning = 1}, {.mask = 0x0010, .warning = 2},
        {.mask = 0x0008, .warning = 4}, {.mask = 0x0004, .pvf = true}, {.mask = 0x0002, .tcf = true},
        {.mask = 0x0001, .cvrf = true},
    };
    for (unsigned i = 0; i < sizeof bits / sizeof bits[0]; i++) {
        ina3221_decode_alerts(bits[i].mask, &a);
        assert(a.critical == bits[i].critical && a.warning == bits[i].warning && a.summation == bits[i].sf);
        assert(a.power_valid == bits[i].pvf && a.timing_control == bits[i].tcf && a.conversion_ready == bits[i].cvrf);
        assert(!a.critical_latch && !a.warning_latch);
    }
    ina3221_decode_alerts(0x7000, &a);
    assert(!a.critical && !a.warning && !a.critical_latch && !a.warning_latch);
}

static void alert_test(void)
{
    // The X20 row: +5V 1000/1300 mA over 20 mOhm, 3V3_GNSS 350/500 mA over 50 mOhm,
    // 3V3_SYS 800/1000 mA over 20 mOhm.
    const int32_t warn[3] = {1000 * 20, 350 * 50, 800 * 20}, crit[3] = {1300 * 20, 500 * 50, 1000 * 20};
    chip_t c; env_io_t io = chip_io(&c);
    uint16_t cleared = 0;
    assert(ina3221_init(&io));
    // The summation channels are kept as read; a flag left by an earlier limit is passed on.
    c.reg[0x0f] |= 0x5000 | 0x0200;
    assert(ina3221_set_alerts(&io, warn, crit, &cleared));
    assert(c.reg[0x07] == 0x1450 && c.reg[0x08] == 0x0fa0 && c.reg[0x09] == 0x1388 && c.reg[0x0a] == 0x0db0);
    assert(c.reg[0x0b] == 0x0fa0 && c.reg[0x0c] == 0x0c80);
    assert((c.reg[0x0f] & 0xfc00) == (0x5000 | INA3221_MASK_WEN | INA3221_MASK_CEN) && cleared == 0x0200);
    assert(c.writes == 1 + 6 + 1);

    // A channel whose limit is 0 is not programmed, and keeps the full-scale reset value.
    io = chip_io(&c); cleared = 0;
    const int32_t warn2[3] = {1000 * 20, 0, 800 * 20}, crit2[3] = {0, 0, 1000 * 20};
    assert(ina3221_init(&io) && ina3221_set_alerts(&io, warn2, crit2, &cleared));
    assert(c.reg[0x07] == 0x7ff8 && c.reg[0x08] == 0x0fa0 && c.reg[0x09] == 0x7ff8 && c.reg[0x0a] == 0x7ff8);
    assert(c.reg[0x0b] == 0x0fa0 && c.reg[0x0c] == 0x0c80 && c.writes == 1 + 3 + 1 && cleared == 0);

    // A latched warning on channel 2: the read decodes it and clears it, releasing the line.
    c.reg[0x0f] |= 0x0010;
    ina3221_alerts_t a;
    assert(ina3221_read_alerts(&io, &a) && a.warning == 2 && !a.critical && a.critical_latch && a.warning_latch);
    assert(!(c.reg[0x0f] & INA3221_MASK_ALERTS));
    assert(ina3221_read_alerts(&io, &a) && !a.warning && !a.critical);
    // A sample's conversion-ready reads clear the flags too, so it passes them on.
    c.reg[0x0f] |= 0x0100 | 0x0040; c.since_config = 30000;
    ina3221_sample_t s;
    assert(ina3221_read(&io, &s) && s.alert_flags == (0x0100 | 0x0040));
    assert(ina3221_read_alerts(&io, &a) && !a.critical && !a.summation);
    // ... even when a later transfer fails (configuration, Mask/Enable, then channel 1's shunt).
    c.reg[0x0f] |= 0x0020; c.attempts = 0; c.fail_read_at = 3;
    assert(!ina3221_read(&io, &s) && s.alert_flags == 0x0020 && s.shunt_uv[0] == 0 && s.bus_mv[0] == 0);
    c.fail_read_at = 0;
    // A lost configuration is found before Mask/Enable is read, so a flag stays for the next read.
    c.reg[0x0f] |= 0x0008; c.reg[0x00] = 0x7127;
    assert(!ina3221_read(&io, &s) && s.alert_flags == 0 && (c.reg[0x0f] & 0x0008));
    assert(ina3221_read_alerts(&io, &a) && a.warning == 4);

    // A power cycle resets the limits and the latch bits; configuring again restores them.
    chip_power_on(&c); cleared = 0;
    assert(c.reg[0x07] == 0x7ff8 && !(c.reg[0x0f] & INA3221_MASK_CEN));
    assert(ina3221_init(&io) && ina3221_set_alerts(&io, warn, crit, &cleared));
    assert(c.reg[0x07] == 0x1450 && (c.reg[0x0f] & (INA3221_MASK_CEN | INA3221_MASK_WEN)) == 0x0c00);

    // A part that does not take the limits, or a failed transfer, is not configured.
    io = chip_io(&c); c.frozen_limits = true; cleared = 0;
    assert(ina3221_init(&io) && !ina3221_set_alerts(&io, warn, crit, &cleared));
    for (unsigned fail = 2; fail <= 1 + 6 + 1; fail++) {
        io = chip_io(&c); cleared = 0;
        assert(ina3221_init(&io));
        c.fail_write_at = fail;
        assert(!ina3221_set_alerts(&io, warn, crit, &cleared));
    }
    io = chip_io(&c); c.absent = true; cleared = 0;
    assert(!ina3221_set_alerts(&io, warn, crit, &cleared) && !ina3221_read_alerts(&io, &a) && a.mask_enable == 0);
}

int main(void)
{
    scaling_test(); driver_test(); timeout_test(); limit_test(); decode_test(); alert_test();
    puts("INA3221 scaling, configuration, conversion-ready wait, alert limits, flags and fault handling passed");
    return 0;
}
