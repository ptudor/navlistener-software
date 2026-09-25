#include "rtc_max31328.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A MAX31328 register file: calendar 0x00-0x06, control 0x0e, status 0x0f, aging 0x10.
typedef struct {
    uint8_t regs[0x13];
    bool osf_read_only, frozen;
    unsigned operations, fail_at, writes, elapsed_ms;
} chip_t;
static bool chip_read(void *ctx, uint8_t reg, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    assert(reg + length <= sizeof c->regs);
    if (++c->operations == c->fail_at) return false;
    memcpy(out, c->regs + reg, length);
    return true;
}
static bool chip_write(void *ctx, uint8_t reg, const uint8_t *data, size_t length)
{
    chip_t *c = ctx;
    assert(reg + length <= sizeof c->regs);
    c->writes++;
    for (size_t i = 0; i < length; i++) {
        uint8_t at = reg + i, value = data[i];
        if (at == MAX31328_STATUS) {
            // OSF, A2F and A1F only clear when written 0; BSY is read-only.
            uint8_t sticky = MAX31328_A2F | MAX31328_A1F | (c->osf_read_only ? 0 : MAX31328_OSF);
            uint8_t kept = c->regs[at] & (MAX31328_BSY | (c->osf_read_only ? MAX31328_OSF : 0));
            value = kept | (value & MAX31328_EN32KHZ) | (c->regs[at] & value & sticky);
        }
        c->regs[at] = value;
    }
    if (++c->operations == c->fail_at) return false;
    return true;
}
static void chip_delay(void *ctx, unsigned ms)
{
    chip_t *c = ctx; int64_t epoch;
    if (c->frozen || (c->regs[MAX31328_CONTROL] & MAX31328_EOSC) || !max31328_decode(c->regs, &epoch)) return;
    c->elapsed_ms += ms;
    if (c->elapsed_ms >= 1000) {
        assert(max31328_encode(epoch + c->elapsed_ms / 1000, c->regs));
        c->elapsed_ms %= 1000;
    }
}
static rtc_io_t chip_io(chip_t *c)
{
    memset(c, 0, sizeof *c);
    // Power-on state (datasheet pp. 9, 20-21): 01/01/00 01 00:00:00, INTCN and RS set,
    // OSF and EN32kHz set. Aging and alarm flags are left for the firmware to preserve.
    const uint8_t calendar[7] = {0, 0, 0, 1, 1, 1, 0};
    memcpy(c->regs, calendar, 7);
    c->regs[MAX31328_CONTROL] = 0x1c;
    c->regs[MAX31328_STATUS] = MAX31328_OSF | MAX31328_EN32KHZ | MAX31328_A1F;
    c->regs[MAX31328_AGING] = 0xf3;
    return (rtc_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay = chip_delay};
}

static void calendar_test(void)
{
    uint8_t r[7]; int64_t epoch;
    assert(max31328_encode(1704067200, r) && (r[3] & 0xf8) == 0 && !(r[5] & 0x80));
    assert(max31328_decode(r, &epoch) && epoch == 1704067200);
    for (unsigned i = 0; i < 7; i++) {
        // Every unused bit, and the century bit, rejects the calendar.
        static const uint8_t unused[7] = {0x80, 0x80, 0x80, 0xf8, 0xc0, 0xe0, 0};
        for (unsigned bit = 0; bit < 8; bit++) {
            if (!(unused[i] & (1u << bit))) continue;
            uint8_t bad[7]; memcpy(bad, r, 7); bad[i] |= 1u << bit;
            assert(!max31328_decode(bad, &epoch));
        }
    }
    uint8_t twelve[7]; memcpy(twelve, r, 7);
    twelve[2] = 0x72; // 12 PM in 12-hour mode
    assert(max31328_decode(twelve, &epoch) && epoch == 1704067200 + 12 * 3600);
    assert(!max31328_encode(4102444800LL, r) && !max31328_encode(946684799, r));
    assert(max31328_encode(1704067200, r));
    assert(max31328_running(r, 0, 0, &epoch));
    assert(!max31328_running(r, MAX31328_EOSC, 0, &epoch));
    assert(!max31328_running(r, 0, MAX31328_OSF, &epoch));
}

static void set_test(void)
{
    chip_t c; rtc_io_t io = chip_io(&c); int64_t epoch;
    assert(max31328_set_verified(&io, 1704067200) == MAX31328_SET_OK);
    unsigned operations = c.operations;
    assert(!(c.regs[MAX31328_STATUS] & MAX31328_OSF) && (c.regs[MAX31328_STATUS] & MAX31328_A1F));
    assert(max31328_running(c.regs, c.regs[MAX31328_CONTROL], c.regs[MAX31328_STATUS], &epoch) &&
           epoch == 1704067201);
    assert(c.regs[MAX31328_AGING] == 0xf3 && c.regs[MAX31328_CONTROL] == 0x1c);
    // The oscillator is re-enabled for battery operation.
    io = chip_io(&c); c.regs[MAX31328_CONTROL] |= MAX31328_EOSC;
    assert(max31328_set_verified(&io, 1704067200) == MAX31328_SET_OK && !(c.regs[MAX31328_CONTROL] & MAX31328_EOSC));
    // A datasheet-literal read-only OSF: reported, and the calendar is left invalid.
    io = chip_io(&c); c.osf_read_only = true;
    assert(max31328_set_verified(&io, 1704067200) == MAX31328_SET_OSF_STUCK && !max31328_decode(c.regs, &epoch));
    io = chip_io(&c); c.frozen = true;
    assert(max31328_set_verified(&io, 1704067200) == MAX31328_SET_UNVERIFIED && !max31328_decode(c.regs, &epoch));
    io = chip_io(&c);
    assert(max31328_set_verified(&io, 0) == MAX31328_SET_UNVERIFIED && c.writes == 0);
    // A fault at any transfer never reports success.
    for (unsigned fail = 1; fail <= operations; fail++) {
        io = chip_io(&c); c.fail_at = fail;
        assert(max31328_set_verified(&io, 1704067200) != MAX31328_SET_OK);
    }
}

static void square_test(void)
{
    chip_t c; rtc_io_t io = chip_io(&c); uint8_t control, aging;
    assert(max31328_square_wave(&io, &control, &aging) == 2 && c.writes == 0); // OSF set at power-on
    assert(max31328_set_verified(&io, 1704067200) == MAX31328_SET_OK);
    c.regs[MAX31328_STATUS] |= MAX31328_A2F;
    unsigned writes = c.writes;
    assert(max31328_square_wave(&io, &control, &aging) == 1 && control == 0 && aging == 0xf3);
    assert(c.regs[MAX31328_STATUS] == (MAX31328_A2F | MAX31328_A1F)); // 32 kHz off, alarm flags kept
    assert(c.writes == writes + 2);
    assert(max31328_square_wave(&io, &control, &aging) == 1 && c.writes == writes + 2); // idempotent
    c.regs[MAX31328_CONTROL] = MAX31328_INTCN | MAX31328_A1IE; // someone else's alarm
    assert(max31328_square_wave(&io, &control, &aging) == 3 && c.regs[MAX31328_CONTROL] == (MAX31328_INTCN | MAX31328_A1IE));
    for (unsigned fail = 1; fail <= 5; fail++) {
        io = chip_io(&c);
        assert(max31328_set_verified(&io, 1704067200) == MAX31328_SET_OK);
        c.operations = 0; c.fail_at = fail;
        assert(max31328_square_wave(&io, &control, &aging) == 4);
    }
}

int main(void)
{
    calendar_test(); set_test(); square_test();
    puts("MAX31328 calendar, OSF, verified set, square wave and I2C fault tests passed");
    return 0;
}
