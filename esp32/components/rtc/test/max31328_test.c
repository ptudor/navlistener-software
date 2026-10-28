#include "rtc_max31328.h"
#include "rtc_service.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A MAX31328 register file: calendar 0x00-0x06, control 0x0e, status 0x0f, aging 0x10.
typedef struct {
    uint8_t regs[0x13];
    bool osf_read_only, frozen;
    unsigned operations, fail_at, writes, elapsed_ms;
    int64_t clock_ms; // the simulated monotonic clock every delay advances
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
    if (reg == 0) c->elapsed_ms = 0; // writing the seconds restarts the countdown chain
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
    c->clock_ms += ms;
    if (c->frozen || (c->regs[MAX31328_CONTROL] & MAX31328_EOSC) || !max31328_decode(c->regs, &epoch)) return;
    c->elapsed_ms += ms;
    if (c->elapsed_ms >= 1000) {
        assert(max31328_encode(epoch + c->elapsed_ms / 1000, c->regs));
        c->elapsed_ms %= 1000;
    }
}
static int64_t chip_now(void *ctx) { return ((chip_t *)ctx)->clock_ms; }
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
    return (rtc_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay = chip_delay, .now_ms = chip_now};
}
// A qualified UTC (milliseconds) as of the chip's clock now.
static rtc_time_ref_t utc_at(const chip_t *c, int64_t utc_ms)
{ return (rtc_time_ref_t){.utc_ns = utc_ms * 1000000, .sampled_ms = c->clock_ms}; }

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
    chip_t c; rtc_io_t io = chip_io(&c); int64_t epoch, written = 0;
    rtc_time_ref_t ref = utc_at(&c, 1704067200300); // 0.3 s into the second
    assert(max31328_set_verified(&io, &ref, &written) == MAX31328_SET_OK && written == 1704067200);
    unsigned operations = c.operations;
    assert(!(c.regs[MAX31328_STATUS] & MAX31328_OSF) && (c.regs[MAX31328_STATUS] & MAX31328_A1F));
    assert(max31328_running(c.regs, c.regs[MAX31328_CONTROL], c.regs[MAX31328_STATUS], &epoch) &&
           epoch == 1704067201);
    assert(c.regs[MAX31328_AGING] == 0xf3 && c.regs[MAX31328_CONTROL] == 0x1c);
    // Past the half second the next second is loaded, so the clock is within half a second
    // of UTC either way.
    io = chip_io(&c); ref = utc_at(&c, 1704067200700);
    assert(max31328_set_verified(&io, &ref, &written) == MAX31328_SET_OK && written == 1704067201);
    assert(max31328_running(c.regs, c.regs[MAX31328_CONTROL], c.regs[MAX31328_STATUS], &epoch) &&
           epoch == 1704067202);
    // The oscillator is re-enabled for battery operation.
    io = chip_io(&c); c.regs[MAX31328_CONTROL] |= MAX31328_EOSC; ref = utc_at(&c, 1704067200300);
    assert(max31328_set_verified(&io, &ref, NULL) == MAX31328_SET_OK && !(c.regs[MAX31328_CONTROL] & MAX31328_EOSC));
    // A datasheet-literal read-only OSF: reported, and the calendar is left invalid.
    io = chip_io(&c); c.osf_read_only = true; ref = utc_at(&c, 1704067200300);
    assert(max31328_set_verified(&io, &ref, NULL) == MAX31328_SET_OSF_STUCK && !max31328_decode(c.regs, &epoch));
    io = chip_io(&c); c.frozen = true; ref = utc_at(&c, 1704067200300);
    assert(max31328_set_verified(&io, &ref, NULL) == MAX31328_SET_UNVERIFIED && !max31328_decode(c.regs, &epoch));
    io = chip_io(&c); ref = utc_at(&c, 0);
    assert(max31328_set_verified(&io, &ref, NULL) == MAX31328_SET_UNVERIFIED && c.writes == 0);
    // A fault at any transfer never reports success.
    for (unsigned fail = 1; fail <= operations; fail++) {
        io = chip_io(&c); c.fail_at = fail; ref = utc_at(&c, 1704067200300);
        assert(max31328_set_verified(&io, &ref, NULL) != MAX31328_SET_OK);
    }
}

static void square_test(void)
{
    chip_t c; rtc_io_t io = chip_io(&c); uint8_t control, aging;
    rtc_time_ref_t ref = utc_at(&c, 1704067200300);
    assert(max31328_square_wave(&io, &control, &aging) == 2 && c.writes == 0); // OSF set at power-on
    assert(max31328_set_verified(&io, &ref, NULL) == MAX31328_SET_OK);
    c.regs[MAX31328_STATUS] |= MAX31328_A2F;
    unsigned writes = c.writes;
    assert(max31328_square_wave(&io, &control, &aging) == 1 && control == 0 && aging == 0xf3);
    assert(c.regs[MAX31328_STATUS] == (MAX31328_A2F | MAX31328_A1F)); // 32 kHz off, alarm flags kept
    assert(c.writes == writes + 2);
    assert(max31328_square_wave(&io, &control, &aging) == 1 && c.writes == writes + 2); // idempotent
    c.regs[MAX31328_CONTROL] = MAX31328_INTCN | MAX31328_A1IE; // someone else's alarm
    assert(max31328_square_wave(&io, &control, &aging) == 3 && c.regs[MAX31328_CONTROL] == (MAX31328_INTCN | MAX31328_A1IE));
    for (unsigned fail = 1; fail <= 5; fail++) {
        io = chip_io(&c); ref = utc_at(&c, 1704067200300);
        assert(max31328_set_verified(&io, &ref, NULL) == MAX31328_SET_OK);
        c.operations = 0; c.fail_at = fail;
        assert(max31328_square_wave(&io, &control, &aging) == 4);
    }
}

static unsigned audits;
static bool service_audit(void *ctx,rtc_service_record_t *r)
{chip_t *c=ctx;if(++audits==1){assert(!c->writes);r->sequence=1;}return true;}
static void service_test(void)
{
    chip_t c;rtc_io_t io=chip_io(&c);rtc_service_record_t r;
    rtc_service_request_t req={.source=OBS_TIME_GNSS,.reference=utc_at(&c,1704067200300),.uncertainty_ns=1000000,.reference_id="receiver_utc",.reason="retry after oscillator service"};
    audits=0;c.osf_read_only=true;
    assert(rtc_service_execute(&io,true,&req,service_audit,&c,&r)==RTC_SERVICE_OSF_STUCK && r.audit_complete && !r.readback_valid);
    c.osf_read_only=false;c.writes=0;audits=0;req.reference=utc_at(&c,1704067200300);
    assert(rtc_service_execute(&io,true,&req,service_audit,&c,&r)==RTC_SERVICE_OK && r.readback_valid);
    io=chip_io(&c);audits=0;req.reference=utc_at(&c,1704067200300);
    assert(rtc_service_execute(&io,true,&req,service_audit,&c,&r)==RTC_SERVICE_OK);
    unsigned operations=c.operations;
    for(unsigned fail=1;fail<=operations;fail++) {
        io=chip_io(&c);c.fail_at=fail;audits=0;req.reference=utc_at(&c,1704067200300);
        // Every injected I/O failure is recorded, without claiming success.
        assert(rtc_service_execute(&io,true,&req,service_audit,&c,&r)!=RTC_SERVICE_OK);
    }
}
// Status 0x0f, aging 0x10, then the temperature pair 0x11-0x12.
static unsigned temperature(uint8_t status, uint8_t msb, uint8_t lsb, const int16_t *previous, int16_t *centi_c)
{
    const uint8_t regs[4] = {status, 0, msb, lsb};
    *centi_c = 12345; // untouched unless OK
    return max31328_temperature(regs, previous, centi_c);
}
static void temperature_test(void)
{
    int16_t c, near_zero = 25, far = 2425;
    // The datasheet's example, 00011001 01b, is +25.25 C.
    assert(temperature(0, 0x19, 0x40, NULL, &c) == MAX31328_TEMPERATURE_OK && c == 2525);
    assert(temperature(MAX31328_OSF, 0x18, 0xc0, NULL, &c) == MAX31328_TEMPERATURE_OK && c == 2475);
    // Two's complement: -0.25 C, -25.75 C, and the register's ends.
    assert(temperature(0, 0xff, 0xc0, NULL, &c) == MAX31328_TEMPERATURE_OK && c == -25);
    assert(temperature(0, 0xe6, 0x40, NULL, &c) == MAX31328_TEMPERATURE_OK && c == -2575);
    assert(temperature(0, 0x80, 0x00, NULL, &c) == MAX31328_TEMPERATURE_OK && c == -12800);
    assert(temperature(0, 0x7f, 0xc0, NULL, &c) == MAX31328_TEMPERATURE_OK && c == 12775);
    // Zero with OSF set may be the power-on default: withheld at boot, and after a reading far
    // from it, as when the rail dropped; kept beside a reading within 0.5 C, or with OSF clear.
    assert(temperature(MAX31328_OSF, 0, 0, NULL, &c) == MAX31328_TEMPERATURE_DEFAULT && c == 12345);
    assert(temperature(MAX31328_OSF, 0, 0, &far, &c) == MAX31328_TEMPERATURE_DEFAULT);
    assert(temperature(MAX31328_OSF, 0, 0, &near_zero, &c) == MAX31328_TEMPERATURE_OK && c == 0);
    near_zero = -50;
    assert(temperature(MAX31328_OSF, 0, 0, &near_zero, &c) == MAX31328_TEMPERATURE_OK && c == 0);
    near_zero = -75;
    assert(temperature(MAX31328_OSF, 0, 0, &near_zero, &c) == MAX31328_TEMPERATURE_DEFAULT);
    assert(temperature(MAX31328_A1F, 0, 0, NULL, &c) == MAX31328_TEMPERATURE_OK && c == 0);
    // The low six bits of 0x12 read zero; anything else is not this register.
    for (uint8_t bit = 1; bit < 0x40; bit <<= 1)
        assert(temperature(0, 0x19, 0x40 | bit, NULL, &c) == MAX31328_TEMPERATURE_INVALID && c == 12345);
}
int main(void)
{
    calendar_test(); set_test(); square_test();service_test(); temperature_test();
    puts("MAX31328 calendar, OSF, verified set, square wave, temperature and I2C fault tests passed");
    return 0;
}
