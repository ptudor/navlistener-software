#include "env_sensors.h"
#include <assert.h>
#include <math.h>
#include <stdio.h>
#include <string.h>
typedef struct {
    uint8_t mcp[16][2], hdc[256], bmp[256];
    bool fail, hdc_stuck, bmp_stuck, hdc_pending, bmp_pending, heater_stuck;
    uint8_t absent; // this address NACKs
    unsigned delay_ms;
} fake_t;
static bool read_bus(void *ctx, uint8_t address, uint8_t reg, uint8_t *p, size_t n)
{
    fake_t *f = ctx;
    if (f->fail || address == f->absent) return false;
    if (address == 0x18) { assert(reg < 16 && n <= 2); memcpy(p, f->mcp[reg], n); }
    else if (address == 0x40) {
        assert(reg + n <= 256); memcpy(p, f->hdc + reg, n);
        if (reg == 4) f->hdc[4] = 0;
    } else {
        assert(address == 0x76 && reg + n <= 256); memcpy(p, f->bmp + reg, n);
        if (reg == 4 && n >= 6) f->bmp[3] &= ~0x60;
    }
    return true;
}
static bool write_bus(void *ctx, uint8_t address, uint8_t reg, const uint8_t *p, size_t n)
{
    fake_t *f = ctx;
    if (f->fail) return false;
    if (address == 0x18) { assert(reg == 1 && n == 2); memcpy(f->mcp[reg], p, n); }
    else if (address == 0x40) {
        assert(n == 1 && (reg == 0xe || reg == 0xf) && !(reg == 0xe && (*p & 0x80))); // never SOFT_RES
        f->hdc[reg] = reg == 0xe && f->heater_stuck ? (*p & ~8) | (f->hdc[reg] & 8) : *p;
        if (reg == 0xf && (*p & 1)) f->hdc_pending = true;
    } else {
        assert(address == 0x76 && n == 1); // no interleaved multi-register writes
        assert(reg == 0x7e || reg == 0x1b || reg == 0x1c || reg == 0x1f);
        f->bmp[reg] = *p;
        if (reg == 0x1b && (*p & 0x30) == 0x10) f->bmp_pending = true;
    }
    return true;
}
static void delay(void *ctx, unsigned ms)
{
    fake_t *f = ctx; f->delay_ms += ms;
    if (f->hdc_pending && !f->hdc_stuck) {
        f->hdc[4] |= 0x80; f->hdc[0xf] &= ~1; f->hdc_pending = false;
    }
    if (f->bmp_pending && !f->bmp_stuck) {
        f->bmp[3] |= 0x60; f->bmp[0x1b] &= ~0x30; f->bmp_pending = false;
    }
}
static void le16(uint8_t *p, unsigned n) { p[0] = n; p[1] = n >> 8; }
static void le24(uint8_t *p, unsigned n) { le16(p, n); p[2] = n >> 16; }
static void setup_variant(fake_t *f, env_sensors_t *s, env_hdc_variant_t variant)
{
    memset(f, 0, sizeof *f);
    f->mcp[6][1] = 0x54; f->mcp[7][0] = 4;
    f->mcp[5][0] = 0xe1; f->mcp[5][1] = 0x98; // +25.5 C with all alert flags set
    le16(f->hdc + 0xfc, 0x5449); le16(f->hdc + 0xfe, 0x07d0);
    le16(f->hdc, 0x8000); le16(f->hdc + 2, 0x8000); // half-scale: HDC2080 42.12 C at 3.3 V; HDC2022 42.5 C; both 50 %RH
    f->hdc[0xe] = 0x7b; // auto mode + heater must be cleared, interrupt bits preserved
    f->bmp[0] = 0x50; f->bmp[3] = 0x10; // chip ID, command ready
    // Synthetic device trim: t1=25600*256, t2=2^-16, t3=0.
    // Raw temperature 8192000 yields exactly 25 C. Only pressure offset
    // p5=12500*8 is nonzero after coefficient quantization, yielding 100000 Pa.
    le16(f->bmp + 0x31, 25600); le16(f->bmp + 0x33, 16384);
    le16(f->bmp + 0x36, 16384); le16(f->bmp + 0x38, 16384);
    le16(f->bmp + 0x3c, 12500);
    le24(f->bmp + 4, 8000000); le24(f->bmp + 7, 8192000);
    env_io_t io = {.ctx=f, .read=read_bus, .write=write_bus, .delay_ms=delay};
    env_sensors_init(s, &io, variant);
}
static void setup(fake_t *f, env_sensors_t *s)
{
    setup_variant(f, s, ENV_HDC2080);
}
static void test_hdc_variants(void)
{
    // Identical ID registers and raw values must use the selected part's formula.
    const struct {
        uint16_t raw;
        double hdc2080_c, hdc2022_c;
        bool hdc2080_valid;
    } cases[] = {
        {0x0000, -40.38, -40.0, false},
        {0x4000, 0.87, 1.25, true},
        {0x8000, 42.12, 42.5, true},
        {0xc000, 83.37, 83.75, true},
        {0xfffc, 124.60992919921875, 124.98992919921875, true},
    };
    for (env_hdc_variant_t variant = ENV_HDC2080; variant <= ENV_HDC2022; variant++) {
        fake_t f; env_sensors_t s; env_sample_t sample;
        setup_variant(&f, &s, variant);
        assert(s.hdc_ready && s.hdc_variant == variant && f.hdc[0xe] == 3);
        for (unsigned i = 0; i < sizeof cases / sizeof cases[0]; i++) {
            le16(f.hdc, cases[i].raw);
            le16(f.hdc + 2, cases[i].raw);
            env_sensors_read(&s, &sample);
            double expected = variant == ENV_HDC2080 ? cases[i].hdc2080_c : cases[i].hdc2022_c;
            assert(fabs(sample.hdc_c - expected) < 1e-9);
            assert(sample.hdc_valid == (variant == ENV_HDC2022 || cases[i].hdc2080_valid));
            assert(fabs(sample.rh_percent - cases[i].raw * (100.0 / 65536.0)) < 1e-9);
            assert(sample.mcp_valid && sample.bmp_valid);
        }
        f.hdc_stuck = true; f.hdc[4] = 0x80;
        env_sensors_read(&s, &sample);
        assert(!sample.hdc_valid && sample.hdc_c == 0 && sample.rh_percent == 0);
        assert(sample.mcp_valid && sample.bmp_valid);
        f.hdc_stuck = false;
        env_sensors_read(&s, &sample); assert(sample.hdc_valid);
        f.fail = true;
        env_sensors_read(&s, &sample);
        assert(!sample.hdc_valid && sample.hdc_c == 0 && sample.rh_percent == 0);
        f.fail = false;
        env_io_t io = s.io;
        f.hdc[0xfe] = 0; // wrong device ID is rejected for either configured part
        env_sensors_init(&s, &io, variant);
        assert(!s.hdc_ready && s.mcp_ready && s.bmp_ready);
    }
    const env_hdc_variant_t unsupported[] = {ENV_HDC_NONE, (env_hdc_variant_t)99};
    for (unsigned i = 0; i < sizeof unsupported / sizeof unsupported[0]; i++) {
        fake_t f; env_sensors_t s; env_sample_t sample;
        setup_variant(&f, &s, unsupported[i]);
        assert(!s.hdc_ready && f.hdc[0xe] == 0x7b);
        env_sensors_read(&s, &sample);
        assert(!sample.hdc_valid && sample.hdc_c == 0 && sample.rh_percent == 0);
        assert(sample.mcp_valid && sample.bmp_valid);
    }
}
static void test_heater_register(void)
{
    fake_t f; env_sensors_t s; env_sample_t sample; bool on = true;
    setup(&f, &s);
    assert(env_hdc_heater_get(&s, &on) && !on && !s.io_error); // init leaves the heater off
    f.hdc[0xe] = 0x87; // interrupt bits set; SOFT_RES reads back set
    assert(env_hdc_heater_set(&s, true) && f.hdc[0xe] == 0x0f && env_hdc_heater_get(&s, &on) && on);
    f.hdc[0xe] |= 0x50; // measurement-mode bits are preserved too
    assert(env_hdc_heater_set(&s, false) && f.hdc[0xe] == 0x57 && env_hdc_heater_get(&s, &on) && !on);
    // Conversions still work with the heater on.
    f.hdc[0xe] = 0x0b; env_sensors_read(&s, &sample);
    assert(sample.hdc_valid && !sample.bus_error && f.hdc[0xe] == 0x0b);
    // A register that ignores the write is reported, not assumed.
    f.hdc[0xe] = 3; f.heater_stuck = true;
    assert(!env_hdc_heater_set(&s, true) && !s.io_error && f.hdc[0xe] == 3);
    f.hdc[0xe] = 0x0b;
    assert(!env_hdc_heater_set(&s, false) && !s.io_error);
    f.heater_stuck = false; f.fail = true;
    assert(!env_hdc_heater_set(&s, false) && s.io_error);
    assert(!env_hdc_heater_get(&s, &on) && s.io_error);
    env_sensors_read(&s, &sample); assert(sample.bus_error && !sample.hdc_valid);
    f.fail = false;
    env_sensors_read(&s, &sample); assert(!sample.bus_error && !s.io_error);
    // Sensors outside the mask are not converted and stay invalid.
    f.hdc[0xf] = 0; env_sensors_read_some(&s, &sample, 5);
    assert(sample.mcp_valid && !sample.hdc_valid && sample.bmp_valid && !f.hdc_pending && f.hdc[0xf] == 0);
    f.bmp[0x1b] = 0; env_sensors_read_some(&s, &sample, 3);
    assert(sample.mcp_valid && sample.hdc_valid && !sample.bmp_valid && f.bmp[0x1b] == 0);
    // An absent device's failed probe at initialization does not mark later good samples.
    setup(&f, &s); env_io_t io = s.io; f.absent = 0x76;
    env_sensors_init(&s, &io, ENV_HDC2080); assert(s.io_error && s.mcp_ready && s.hdc_ready && !s.bmp_ready);
    env_sensors_read(&s, &sample); assert(sample.mcp_valid && sample.hdc_valid && !sample.bus_error);
    // A reboot during a heater run: HEAT_EN is still set and the HDC does not answer at init. It is
    // not configured blind; a later retry identifies it and clears the heater, keeping INT bits.
    setup(&f, &s); io = s.io; f.hdc[0xe] = 0x0b; f.absent = 0x40;
    env_sensors_init(&s, &io, ENV_HDC2080); assert(!s.hdc_ready && f.hdc[0xe] == 0x0b);
    assert(!env_sensors_retry_hdc(&s) && !s.hdc_ready && s.io_error && f.hdc[0xe] == 0x0b);
    f.absent = 0;
    assert(env_sensors_retry_hdc(&s) && s.hdc_ready && !s.io_error && f.hdc[0xe] == 3);
    assert(env_sensors_retry_hdc(&s) && f.hdc[0xe] == 3); // ready: no further writes
    env_sensors_read(&s, &sample); assert(sample.hdc_valid);
    // A part at 0x40 that is not an HDC is never written.
    setup(&f, &s); io = s.io; f.hdc[0xe] = 0x0b; f.hdc[0xfc] = 0;
    env_sensors_init(&s, &io, ENV_HDC2080);
    assert(!env_sensors_retry_hdc(&s) && !s.hdc_ready && f.hdc[0xe] == 0x0b);
    // Not listed in the manifest: nothing is measured, but a heater a previous boot left
    // on is turned off, keeping the other bits.
    bool present = false;
    setup(&f, &s); io = s.io; f.hdc[0xe] = 0x8b;
    assert(env_hdc_heater_off_unlisted(&io, &present) && present && f.hdc[0xe] == 0x03);
    assert(env_hdc_heater_off_unlisted(&io, &present) && present && f.hdc[0xe] == 0x03); // already off
    // Nothing answering, or something that is not an HDC: nothing is written.
    setup(&f, &s); io = s.io; f.absent = 0x40; f.hdc[0xe] = 0x0b;
    assert(env_hdc_heater_off_unlisted(&io, &present) && !present && f.hdc[0xe] == 0x0b);
    setup(&f, &s); io = s.io; f.hdc[0xfc] = 0; f.hdc[0xe] = 0x0b;
    assert(env_hdc_heater_off_unlisted(&io, &present) && !present && f.hdc[0xe] == 0x0b);
    // A heater that will not clear is reported.
    setup(&f, &s); io = s.io; f.hdc[0xe] = 0x0b; f.heater_stuck = true;
    assert(!env_hdc_heater_off_unlisted(&io, &present) && present);
}
int main(void)
{
    fake_t f; env_sensors_t s; env_sample_t sample;
    setup(&f, &s);
    assert(s.mcp_ready && s.hdc_ready && s.bmp_ready);
    assert(f.hdc[0xe] == 3 && f.bmp[0x1c] == 0x0b); // heater off; pressure 8x, temperature 2x
    env_sensors_read(&s, &sample);
    assert(sample.mcp_valid && sample.hdc_valid && sample.bmp_valid);
    assert(sample.mcp_c == 25.5 && fabs(sample.hdc_c - 42.12) < 1e-9);
    assert(sample.rh_percent == 50 && sample.bmp_c == 25 && sample.pressure_pa == 100000);
    f.mcp[5][0] = 0x1f; f.mcp[5][1] = 0x60; // -10 C, sign is not an alert bit
    env_sensors_read(&s, &sample); assert(sample.mcp_valid && sample.mcp_c == -10);
    f.fail = true; env_sensors_read(&s, &sample);
    assert(!sample.mcp_valid && !sample.hdc_valid && !sample.bmp_valid);
    assert(sample.mcp_c == 0 && sample.pressure_pa == 0); // old numbers not reused on I/O failure
    setup(&f, &s); f.hdc_stuck = f.bmp_stuck = true;
    f.hdc[4] = 0x80; f.bmp[3] |= 0x60; // old ready flags cannot authorize stale samples
    env_sensors_read(&s, &sample);
    assert(sample.mcp_valid && !sample.hdc_valid && !sample.bmp_valid && f.delay_ms < 1000);
    setup(&f, &s); f.bmp[2] = 4; // sensor configuration error
    env_sensors_read(&s, &sample); assert(sample.hdc_valid && !sample.bmp_valid);
    setup(&f, &s); le24(f.bmp + 7, 0xffffff); // out-of-range compensated temperature
    env_sensors_read(&s, &sample); assert(!sample.bmp_valid);
    setup(&f, &s);
    env_io_t io = s.io;
    f.mcp[6][1] = 0; f.hdc[0xfc] = 0; memset(f.bmp + 0x31, 0, 21);
    env_sensors_init(&s, &io, ENV_HDC2080);
    assert(!s.mcp_ready && !s.hdc_ready && !s.bmp_ready);
    test_hdc_variants();
    test_heater_register();
    puts("Environment: HDC2080/HDC2022 selection, units/sign/alerts, Bosch trim compensation, fresh conversions, "
         "heater register control, HDC retry and fault isolation passed");
    return 0;
}
