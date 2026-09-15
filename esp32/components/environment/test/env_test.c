#include "env_sensors.h"
#include <assert.h>
#include <math.h>
#include <stdio.h>
#include <string.h>
typedef struct {
    uint8_t mcp[16][2], hdc[256], bmp[256];
    bool fail, hdc_stuck, bmp_stuck, hdc_pending, bmp_pending;
    unsigned delay_ms;
} fake_t;
static bool read_bus(void *ctx, uint8_t address, uint8_t reg, uint8_t *p, size_t n)
{
    fake_t *f = ctx;
    if (f->fail) return false;
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
        assert(n == 1 && (reg == 0xe || reg == 0xf)); f->hdc[reg] = *p;
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
static void setup(fake_t *f, env_sensors_t *s)
{
    memset(f, 0, sizeof *f);
    f->mcp[6][1] = 0x54; f->mcp[7][0] = 4;
    f->mcp[5][0] = 0xe1; f->mcp[5][1] = 0x98; // +25.5 C with all alert flags set
    le16(f->hdc + 0xfc, 0x5449); le16(f->hdc + 0xfe, 0x07d0);
    le16(f->hdc, 0x8000); le16(f->hdc + 2, 0x8000); // TI half-scale: 42.12 C, 50 %RH at nominal 3.3 V
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
    env_sensors_init(s, &io);
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
    env_sensors_init(&s, &io);
    assert(!s.mcp_ready && !s.hdc_ready && !s.bmp_ready);
    puts("Environment: units/sign/alerts, Bosch trim compensation, fresh conversions and fault isolation passed");
    return 0;
}
