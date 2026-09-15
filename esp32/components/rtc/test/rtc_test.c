#include "rtc_io.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

static void calendar_test(void)
{
    const struct { unsigned y, m, d, h, min, sec; int64_t epoch; } cases[] = {
        {2000, 1, 1, 0, 0, 0, 946684800},
        {2000, 2, 29, 0, 0, 0, 951782400},
        {2024, 2, 29, 12, 34, 56, 1709210096},
        {2038, 1, 19, 3, 14, 8, 2147483648LL},
        {2099, 12, 31, 23, 59, 59, 4102444799LL},
    };
    for (unsigned i = 0; i < sizeof cases / sizeof cases[0]; i++) {
        int64_t epoch; uint8_t r[7];
        assert(rtc_calendar_epoch(cases[i].y, cases[i].m, cases[i].d,
            cases[i].h, cases[i].min, cases[i].sec, &epoch));
        assert(epoch == cases[i].epoch);
        assert(rtc_encode(epoch, r));
        assert(!(r[0] & 0x80) && (r[3] & 0x18) == 8);
        assert(!rtc_running(r, &epoch));
        r[0] |= 0x80; r[3] |= 0x20;
        assert(rtc_running(r, &epoch) && epoch == cases[i].epoch);
    }
    int64_t epoch; uint8_t r[7];
    assert(!rtc_calendar_epoch(2023, 2, 29, 0, 0, 0, &epoch));
    assert(!rtc_calendar_epoch(2100, 2, 28, 0, 0, 0, &epoch));
    assert(!rtc_calendar_epoch(1999, 12, 31, 0, 0, 0, &epoch));
    assert(!rtc_calendar_epoch(2024, 0, 1, 0, 0, 0, &epoch));
    assert(!rtc_calendar_epoch(2024, 13, 1, 0, 0, 0, &epoch));
    assert(!rtc_calendar_epoch(2024, 4, 31, 0, 0, 0, &epoch));
    assert(!rtc_calendar_epoch(2024, 4, 30, 24, 0, 0, &epoch));
    assert(!rtc_calendar_epoch(2024, 4, 30, 0, 60, 0, &epoch));
    assert(!rtc_calendar_epoch(2024, 4, 30, 0, 0, 60, &epoch));
    assert(!rtc_encode(946684799, r));
    assert(!rtc_encode(4102444800LL, r));
    assert(rtc_encode(946684800, r));
    assert((r[3] & 7) == 7); // Saturday, Sunday=1
    r[2] = 0x52; // 12 AM in 12-hour mode
    assert(rtc_decode(r, &epoch) && epoch == 946684800);
    r[2] = 0x72; // 12 PM
    assert(rtc_decode(r, &epoch) && epoch == 946728000);
    r[2] = 0x61; // 1 PM
    assert(rtc_decode(r, &epoch) && epoch == 946731600);
    r[2] = 0x40; assert(!rtc_decode(r, &epoch)); // no hour zero in 12-hour mode
    r[2] = 0; r[0] = 0x1a; assert(!rtc_decode(r, &epoch)); // malformed BCD
    r[0] = 0x60; assert(!rtc_decode(r, &epoch));
    r[0] = 0; r[3] = 0; assert(!rtc_decode(r, &epoch));
}
static void put32(uint8_t *p, uint32_t n)
{ for (unsigned i = 0; i < 4; i++) p[i] = n >> (8 * i); }
static void pvt(uint8_t p[92], unsigned sec)
{
    memset(p, 0, 92);
    p[4] = 0xe8; p[5] = 7; // 2024-01-01
    p[6] = 1; p[7] = 1; p[10] = sec; p[11] = 7;
    put32(p + 12, 1000); p[20] = 3; p[21] = 1;
}
static void gnss_test(void)
{
    rtc_candidate_t c = {0}; gnss_status_t s = {0}; uint8_t p[92]; int64_t epoch;
    assert(!rtc_gnss_candidate(&c, &s, 0, &epoch)); // no received time, no fallback
    for (unsigned i = 0; i < 3; i++) {
        pvt(p, i); gnss_status_feed(&s, 1, 7, p, sizeof p, 1000 + 1000 * i);
        assert(rtc_gnss_candidate(&c, &s, s.fix_ms, &epoch) == (i == 2));
        assert(!rtc_gnss_candidate(&c, &s, s.fix_ms + 10, &epoch)); // snapshot counted once
    }
    assert(epoch == 1704067202);
    gnss_status_feed(&s, 1, 7, p, 91, 5000);
    assert(s.fix_ms == 3000); // malformed payload cannot refresh UTC
    assert(!rtc_gnss_candidate(&c, &s, 5001, &epoch) && c.samples == 0);
    // Each missing validity condition must reject the solution even with a position.
    for (unsigned bad = 0; bad < 10; bad++) {
        pvt(p, 0);
        switch (bad) {
        case 0: p[11] &= ~1; break;
        case 1: p[11] &= ~2; break;
        case 2: p[11] &= ~4; break;
        case 3: p[21] = 0; break;
        case 4: p[20] = 5; break; // time-only is not a GPS position lock
        case 5: put32(p + 12, 100000001); break;
        case 6: p[10] = 60; break; // defer leap-second sample
        case 7: p[6] = 2; p[7] = 30; break;
        case 8: put32(p + 16, 1000000000); break;
        case 9: p[78] = 1; break; // invalid LLH
        }
        gnss_status_feed(&s, 1, 7, p, sizeof p, 1000);
        assert(!rtc_gnss_candidate(&c, &s, 1000, &epoch) && c.samples == 0);
    }
    pvt(p, 10); gnss_status_feed(&s, 1, 7, p, sizeof p, 1000);
    assert(!rtc_gnss_candidate(&c, &s, 999, &epoch));
    assert(!rtc_gnss_candidate(&c, &s, 1000, &epoch));
    pvt(p, 20); gnss_status_feed(&s, 1, 7, p, sizeof p, 2000);
    assert(!rtc_gnss_candidate(&c, &s, 2000, &epoch) && c.samples == 1); // UTC jump
    gnss_status_feed(&s, 1, 7, p, sizeof p, 2100);
    assert(!rtc_gnss_candidate(&c, &s, 2100, &epoch) && c.samples == 1); // frozen UTC
    c = (rtc_candidate_t){0};
    for (unsigned i = 0; i < 3; i++) {
        pvt(p, i + 1); put32(p + 16, (uint32_t)-100000000);
        gnss_status_feed(&s, 1, 7, p, sizeof p, 1000 + 1000 * i);
        assert(rtc_gnss_candidate(&c, &s, s.fix_ms + 200, &epoch) == (i == 2));
    }
    assert(epoch == 1704067203); // fractional UTC normalized, receive age applied
    c = (rtc_candidate_t){0};
    for (unsigned i = 0; i < 3; i++) {
        pvt(p, 0); put32(p + 16, 100000000 * i);
        gnss_status_feed(&s, 1, 7, p, sizeof p, 1000 + 100 * i);
        assert(!rtc_gnss_candidate(&c, &s, s.fix_ms, &epoch)); // two-second span required
    }
}

typedef struct {
    uint8_t regs[32], saved_calendar[7], saved_stamps[8];
    unsigned operations, fail_at, writes, elapsed_ms;
    bool stuck_running, stuck_stopped, frozen, corrupt_calendar, saved, save_failed;
} fake_t;
static bool fake_read(void *ctx, uint8_t reg, uint8_t *out, size_t length)
{
    fake_t *f = ctx; assert(reg + length <= sizeof f->regs);
    if (++f->operations == f->fail_at) return false;
    memcpy(out, f->regs + reg, length); return true;
}
static bool fake_write(void *ctx, uint8_t reg, const uint8_t *data, size_t length)
{
    fake_t *f = ctx; assert(length && reg + length <= 8);
    ++f->writes;
    if (reg == 0 && length == 7) assert(!(f->regs[3] & 0x20));
    uint8_t running = f->regs[3] & 0x20;
    if (reg <= 3 && reg + length > 3) {
        if (f->regs[3] & 0x10) assert(f->saved); // durable evidence before destructive write
        memset(f->regs + 0x18, 0, 8);
    }
    // Simulate even a failed transfer having changed some hardware state.
    memcpy(f->regs + reg, data, length);
    if (reg <= 3 && reg + length > 3) f->regs[3] &= ~0x10;
    f->regs[3] = (f->regs[3] & ~0x20) | running;
    if (f->stuck_running || (!f->stuck_stopped && ((f->regs[0] & 0x80) || (f->regs[7] & 8))))
        f->regs[3] |= 0x20;
    else f->regs[3] &= ~0x20;
    if (reg == 0 && length == 7 && f->corrupt_calendar) f->regs[4] = 0;
    if (++f->operations == f->fail_at) return false;
    return true;
}
static void fake_delay(void *ctx, unsigned ms)
{
    fake_t *f = ctx; int64_t epoch;
    if (!(f->regs[3] & 0x20) || f->frozen || !rtc_decode(f->regs, &epoch)) return;
    f->elapsed_ms += ms;
    unsigned seconds = f->elapsed_ms / 1000;
    f->elapsed_ms %= 1000;
    if (seconds) {
        uint8_t power_fail = f->regs[3] & 0x10;
        assert(rtc_encode(epoch + seconds, f->regs));
        f->regs[3] |= power_fail;
        f->regs[0] |= 0x80; f->regs[3] |= 0x20;
    }
}
static bool fake_save(void *ctx, const uint8_t calendar[7], const uint8_t stamps[8])
{
    fake_t *f = ctx;
    assert(f->writes == 0); // save failure must leave the RTC entirely untouched
    if (f->save_failed) return false;
    memcpy(f->saved_calendar, calendar, 7); memcpy(f->saved_stamps, stamps, 8);
    f->saved = true; return true;
}
static rtc_io_t fake_io(fake_t *f)
{
    memset(f, 0, sizeof *f);
    memset(f->regs + 8, 0xa5, sizeof f->regs - 8); // untouched alarms/SRAM/timestamps
    f->regs[3] = 0x10; // preserve power-fail flag
    f->regs[7] = 0xb7; // preserve unrelated CONTROL bits
    return (rtc_io_t){.ctx=f, .read=fake_read, .write=fake_write, .delay=fake_delay,
        .save_power_failure=fake_save};
}
static void io_test(void)
{
    fake_t f; rtc_io_t io = fake_io(&f); int64_t epoch;
    assert(rtc_set_verified(&io, 1704067200));
    unsigned operations = f.operations;
    assert(rtc_running(f.regs, &epoch) && epoch == 1704067201);
    assert((f.regs[3] & 0x18) == 8 && f.regs[7] == 0xb7);
    for (unsigned i = 8; i < 0x18; i++) assert(f.regs[i] == 0xa5);
    assert(f.saved && (f.saved_calendar[3] & 0x10));
    for (unsigned i = 0; i < 8; i++) assert(f.saved_stamps[i] == 0xa5 && f.regs[0x18 + i] == 0);
    // Fault at every read/write boundary must never report initialization success.
    for (unsigned fail = 1; fail <= operations; fail++) {
        io = fake_io(&f); f.fail_at = fail;
        assert(!rtc_set_verified(&io, 1704067200));
        assert(!(f.regs[0] & 0x80)); // cleanup stopped an unverified calendar
    }
    io = fake_io(&f); f.stuck_stopped = true;
    assert(!rtc_set_verified(&io, 1704067200));
    io = fake_io(&f); f.stuck_running = true; f.regs[3] |= 0x20;
    assert(!rtc_set_verified(&io, 1704067200) && f.writes == 3);
    io = fake_io(&f); f.frozen = true;
    assert(!rtc_set_verified(&io, 1704067200) && !(f.regs[0] & 0x80));
    io = fake_io(&f); f.corrupt_calendar = true;
    assert(!rtc_set_verified(&io, 1704067200));
    io = fake_io(&f); f.regs[7] |= 8; f.regs[3] |= 0x20; // unexpected external clock
    assert(rtc_set_verified(&io, 1704067200) && !(f.regs[7] & 8));
    io = fake_io(&f);
    assert(!rtc_set_verified(&io, 0) && f.writes == 0);
    f.save_failed = true;
    assert(!rtc_set_verified(&io, 1704067200) && f.writes == 0);
    assert(!rtc_enable_backup(&io) && f.writes == 0);
    io.save_power_failure = NULL;
    assert(!rtc_set_verified(&io, 1704067200) && f.writes == 0);
    io = fake_io(&f);
    assert(rtc_encode(1704067200, f.regs));
    f.regs[0] |= 0x80; f.regs[3] = (f.regs[3] | 0x30) & ~8;
    uint8_t before[32]; memcpy(before, f.regs, sizeof before);
    assert(rtc_enable_backup(&io) && f.writes == 1);
    before[3] = (before[3] | 8) & ~0x10; memset(before + 0x18, 0, 8);
    assert(!memcmp(before, f.regs, sizeof before) && f.saved);
    assert(rtc_enable_backup(&io) && f.writes == 1); // retained calendar not rewritten
}
static void square_test(void)
{
    fake_t f; rtc_io_t io=fake_io(&f); uint8_t control,trim;
    f.regs[7]=0x80;
    assert(rtc_square_wave(&io,&control,&trim)==2 && f.writes==0);
    assert(rtc_encode(1704067200,f.regs)); f.regs[0]|=0x80; f.regs[3]|=0x30;
    uint8_t before[32]; memcpy(before,f.regs,32);
    assert(rtc_square_wave(&io,&control,&trim)==1 && control==0xc0 && trim==0xa5);
    before[7]=0xc0; assert(!memcmp(before,f.regs,32)); // PWRFAIL/calendar/trim retained
    assert(f.writes==1);
    assert(rtc_square_wave(&io,&control,&trim)==1 && f.writes==1);
    const uint8_t conflicts[]={0x90,0xa0,0x84};
    for (unsigned i=0;i<sizeof conflicts;i++) {
        f.regs[7]=conflicts[i];
        assert(rtc_square_wave(&io,&control,&trim)==3 && f.writes==1);
    }
    for (unsigned fail=1;fail<=4;fail++) {
        io=fake_io(&f); f.regs[7]=0x80;
        assert(rtc_encode(1704067200,f.regs)); f.regs[0]|=0x80; f.regs[3]|=0x20;
        f.fail_at=fail;
        assert(rtc_square_wave(&io,&control,&trim)==4);
    }
}
int main(void)
{
    calendar_test(); gnss_test(); io_test(); square_test();
    puts("RTC calendar, GNSS qualification and I2C fault tests passed");
    return 0;
}
