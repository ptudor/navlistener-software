#include "rtc_io.h"
#include "rtc_service.h"
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

static void trusted_utc_test(void)
{
    rtc_candidate_t c = {0}; gnss_status_t s = {0}; uint8_t p[92]; int64_t utc = 1;
    // A validated running RTC read within 30 s is used only without qualified GNSS.
    assert(rtc_trusted_utc(&c, &s, 19, 1704067000, 5000, 5000, &utc) == RTC_UTC_RTC && utc == 1704067000);
    assert(rtc_trusted_utc(&c, &s, 31, 1704067000, 5000, 35000, &utc) == RTC_UTC_RTC && utc == 1704067030);
    assert(rtc_trusted_utc(&c, &s, 31, 1704067000, 5000, 35001, &utc) == RTC_UTC_UNKNOWN && utc == 0);
    assert(rtc_trusted_utc(&c, &s, 31, 1704067000, 5000, 4999, &utc) == RTC_UTC_UNKNOWN); // read in the future
    const uint8_t untrusted[] = {3, 17, 18, 15, 0}; // unreadable, stopped or unvalidated calendar
    for (unsigned i = 0; i < sizeof untrusted; i++)
        assert(rtc_trusted_utc(&c, &s, untrusted[i], 1704067000, 5000, 5000, &utc) == RTC_UTC_UNKNOWN);
    // Three advancing NAV-PVT reports make GNSS the preferred source, extrapolated for age.
    for (unsigned i = 0; i < 3; i++) {
        pvt(p, i); gnss_status_feed(&s, 1, 7, p, sizeof p, 1000 + 1000 * i);
        assert(rtc_trusted_utc(&c, &s, 19, 1600000000, s.fix_ms, s.fix_ms, &utc) == (i == 2 ? RTC_UTC_GNSS : RTC_UTC_RTC));
    }
    assert(utc == 1704067202);
    assert(rtc_trusted_utc(&c, &s, 0, 0, 0, 5000, &utc) == RTC_UTC_GNSS && utc == 1704067204);
    assert(rtc_trusted_utc(&c, &s, 0, 0, 0, 5001, &utc) == RTC_UTC_UNKNOWN && utc == 0); // stale fix
    c=(rtc_candidate_t){0};s=(gnss_status_t){0};
    // High-rate reports can reach three samples before two seconds have elapsed.
    for(unsigned i=0;i<3;i++) {
        pvt(p,0);put32(p+16,i*100000000);gnss_status_feed(&s,1,7,p,sizeof p,1000+i*100);
        assert(rtc_trusted_utc(&c,&s,0,0,0,s.fix_ms,&utc)==RTC_UTC_UNKNOWN);
    }
}

typedef struct {
    uint8_t regs[32], saved_calendar[7], saved_stamps[8];
    unsigned operations, fail_at, writes, elapsed_ms;
    int64_t clock_ms; // the simulated monotonic clock every delay advances
    // OSCRUN follows the oscillator, not the ST bit: a stop shows after stop_latency_ms
    // (the calendar counting meanwhile), a start after start_latency_ms (nothing counted
    // until then). Zero is immediate.
    unsigned stop_latency_ms, start_latency_ms;
    int64_t pending_until_ms;
    bool stopping, starting;
    bool stuck_running, stuck_stopped, frozen, corrupt_calendar, saved, save_failed;
    bool day_rolls_on_save; // midnight passes while the record is being saved
    bool power_fail_sticks; // a part whose PWRFAIL survives the write that should clear it
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
    const uint8_t stuck = f->power_fail_sticks ? f->regs[3] & 0x10 : 0;
    if (reg <= 3 && reg + length > 3) {
        if (f->regs[3] & 0x10) assert(f->saved); // durable evidence before destructive write
        if (!stuck) memset(f->regs + 0x18, 0, 8);
    }
    // Simulate even a failed transfer having changed some hardware state.
    memcpy(f->regs + reg, data, length);
    if (reg <= 3 && reg + length > 3) f->regs[3] = (f->regs[3] & ~0x10) | stuck;
    f->regs[3] = (f->regs[3] & ~0x20) | running;
    bool wants = f->stuck_running || (!f->stuck_stopped && ((f->regs[0] & 0x80) || (f->regs[7] & 8)));
    if (wants && !running && f->start_latency_ms) {
        f->starting = true; f->stopping = false; f->pending_until_ms = f->clock_ms + f->start_latency_ms;
    } else if (!wants && running && f->stop_latency_ms) {
        f->stopping = true; f->starting = false; f->pending_until_ms = f->clock_ms + f->stop_latency_ms;
    } else {
        f->starting = f->stopping = false;
        if (wants && !running) f->elapsed_ms = 0; // the divider chain starts from zero phase
        if (wants) f->regs[3] |= 0x20; else f->regs[3] &= ~0x20;
    }
    if (reg == 0 && length == 7 && f->corrupt_calendar) f->regs[4] = 0;
    if (++f->operations == f->fail_at) return false;
    return true;
}
static void fake_delay(void *ctx, unsigned ms)
{
    fake_t *f = ctx; int64_t epoch;
    f->clock_ms += ms;
    if ((f->regs[3] & 0x20) && !f->frozen && rtc_decode(f->regs, &epoch)) {
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
    if ((f->stopping || f->starting) && f->clock_ms >= f->pending_until_ms) {
        if (f->starting) { f->regs[3] |= 0x20; f->elapsed_ms = 0; }
        else f->regs[3] &= ~0x20;
        f->stopping = f->starting = false;
    }
}
static int64_t fake_now(void *ctx) { return ((fake_t *)ctx)->clock_ms; }
// A qualified UTC (milliseconds) as of the fake's clock now.
static rtc_time_ref_t utc_at(const fake_t *f, int64_t utc_ms)
{ return (rtc_time_ref_t){.utc_ns = utc_ms * 1000000, .sampled_ms = f->clock_ms}; }
// The running calendar against simulated UTC now: within a second either way.
static bool within_second(const fake_t *f, const rtc_time_ref_t *ref)
{
    int64_t epoch;
    if (!rtc_running(f->regs, &epoch)) return false;
    int64_t diff_ms = epoch * 1000 - (ref->utc_ns / 1000000 + (f->clock_ms - ref->sampled_ms));
    return diff_ms >= -1000 && diff_ms <= 1000;
}
static bool fake_save(void *ctx, const uint8_t calendar[7], const uint8_t stamps[8])
{
    fake_t *f = ctx;
    assert(f->writes == 0); // save failure must leave the RTC entirely untouched
    if (f->save_failed) return false;
    memcpy(f->saved_calendar, calendar, 7); memcpy(f->saved_stamps, stamps, 8);
    if (f->day_rolls_on_save) f->regs[3] = (f->regs[3] & ~7) | ((f->regs[3] & 7) % 7 + 1);
    f->saved = true; return true;
}
static rtc_io_t fake_io(fake_t *f)
{
    memset(f, 0, sizeof *f);
    memset(f->regs + 8, 0xa5, sizeof f->regs - 8); // untouched alarms/SRAM/timestamps
    f->regs[3] = 0x10; // preserve power-fail flag
    f->regs[7] = 0xb7; // preserve unrelated CONTROL bits
    return (rtc_io_t){.ctx=f, .read=fake_read, .write=fake_write, .delay=fake_delay,
        .now_ms=fake_now, .save_power_failure=fake_save};
}
static unsigned audit_calls,audit_fail;
static bool audit(void *ctx,rtc_service_record_t *r)
{
    fake_t *f=ctx;
    if(++audit_calls==1){assert(f->writes==0);assert(r->old_read);r->sequence=42;}
    return audit_calls!=audit_fail;
}
static void service_test(void)
{
    fake_t f;rtc_io_t io=fake_io(&f);
    assert(rtc_encode(1600000000,f.regs));f.regs[0]|=0x80;f.regs[3]|=0x20;
    rtc_service_request_t req={.source=OBS_TIME_FACTORY,.reference=utc_at(&f,1704067200300),.uncertainty_ns=20000000,.reason="battery: powered service",.reference_id="bench_reference",.battery_service=true};
    rtc_service_record_t r;audit_calls=audit_fail=0;
    assert(rtc_service_execute(&io,false,&req,audit,&f,&r)==RTC_SERVICE_OK);
    assert(r.old_valid && r.old_epoch==1600000000 && r.readback_valid && r.readback_epoch==1704067201 && r.sequence==42 && r.audit_complete && audit_calls==2);
    // An unavailable audit store must prevent calendar writes.
    io=fake_io(&f);audit_calls=0;audit_fail=1;
    assert(rtc_service_execute(&io,false,&req,audit,&f,&r)==RTC_SERVICE_AUDIT_FAILED && f.writes==0);
    // A failed final save must not return a completed durable success.
    io=fake_io(&f);audit_calls=0;audit_fail=2;
    assert(rtc_service_execute(&io,false,&req,audit,&f,&r)==RTC_SERVICE_AUDIT_FAILED && !r.audit_complete);
    // Stale, future, unqualified and missing-reason references never write.
    for(unsigned i=0;i<4;i++) {
        io=fake_io(&f);audit_calls=audit_fail=0;rtc_service_request_t bad=req;
        if(i==0)f.clock_ms=2001;if(i==1)bad.reference.sampled_ms=1;if(i==2)bad.source=OBS_TIME_NETWORK;if(i==3)bad.reason[0]=0;
        assert(rtc_service_execute(&io,false,&bad,audit,&f,&r)==RTC_SERVICE_BAD_REFERENCE && f.writes==0);
    }
    io=fake_io(&f);audit_calls=audit_fail=0;f.frozen=true;
    assert(rtc_service_execute(&io,false,&req,audit,&f,&r)==RTC_SERVICE_WRITE_FAILED);
    // The next explicit operation retries a previously failed calendar.
    f.frozen=false;req.reference=utc_at(&f,1704067200300);f.writes=0;audit_calls=0;
    assert(rtc_service_execute(&io,false,&req,audit,&f,&r)==RTC_SERVICE_OK);
    // Powered battery service needs no new absolute time and writes no RTC registers.
    req=(rtc_service_request_t){.source=OBS_TIME_RTC,.verify_only=true,.battery_service=true,.reason="powered cell replacement",.reference_id="retained_calendar"};
    f.writes=0;audit_calls=0;
    assert(rtc_service_execute(&io,false,&req,audit,&f,&r)==RTC_SERVICE_OK && !f.writes && r.old_valid && r.readback_epoch>r.old_epoch);
    f.frozen=true;audit_calls=0;
    assert(rtc_service_execute(&io,false,&req,audit,&f,&r)==RTC_SERVICE_VERIFY_FAILED && !f.writes);
}
static void io_test(void)
{
    fake_t f; rtc_io_t io = fake_io(&f); int64_t epoch, written = 0;
    rtc_time_ref_t ref = utc_at(&f, 1704067200300); // 0.3 s into the second
    assert(rtc_set_verified(&io, &ref, &written) && written == 1704067200);
    unsigned operations = f.operations;
    assert(rtc_running(f.regs, &epoch) && epoch == 1704067201 && within_second(&f, &ref));
    assert((f.regs[3] & 0x18) == 8 && f.regs[7] == 0xb7);
    for (unsigned i = 8; i < 0x18; i++) assert(f.regs[i] == 0xa5);
    assert(f.saved && (f.saved_calendar[3] & 0x10));
    for (unsigned i = 0; i < 8; i++) assert(f.saved_stamps[i] == 0xa5 && f.regs[0x18 + i] == 0);
    // Past the half second the next second is loaded: the clock leads by less than it
    // would otherwise lag.
    io = fake_io(&f); ref = utc_at(&f, 1704067200700);
    assert(rtc_set_verified(&io, &ref, &written) && written == 1704067201 && within_second(&f, &ref));
    // A running oscillator that takes 1.5 s to report stopped, the calendar counting on
    // meanwhile: the second loaded is derived after that wait, so the clock lands within a
    // second of UTC instead of lagging by the wait.
    io = fake_io(&f); assert(rtc_encode(1600000000, f.regs)); f.regs[0] |= 0x80; f.regs[3] |= 0x20;
    f.stop_latency_ms = 1500; ref = utc_at(&f, 1704067200300);
    assert(rtc_set_verified(&io, &ref, &written) && written == 1704067202 && within_second(&f, &ref));
    // A crystal slow to start cannot be compensated for by any ordering, so that lag is
    // reported rather than accepted: 0.4 s is within tolerance, 2 s fails and stops the clock.
    io = fake_io(&f); f.start_latency_ms = 400; ref = utc_at(&f, 1704067200300);
    assert(rtc_set_verified(&io, &ref, &written) && within_second(&f, &ref));
    io = fake_io(&f); f.start_latency_ms = 2000; ref = utc_at(&f, 1704067200300);
    assert(!rtc_set_verified(&io, &ref, &written) && !(f.regs[0] & 0x80));
    // Fault at every read/write boundary must never report initialization success.
    for (unsigned fail = 1; fail <= operations; fail++) {
        io = fake_io(&f); f.fail_at = fail; ref = utc_at(&f, 1704067200300);
        assert(!rtc_set_verified(&io, &ref, NULL));
        assert(!(f.regs[0] & 0x80)); // cleanup stopped an unverified calendar
    }
    io = fake_io(&f); f.stuck_stopped = true; ref = utc_at(&f, 1704067200300);
    assert(!rtc_set_verified(&io, &ref, NULL));
    io = fake_io(&f); f.stuck_running = true; f.regs[3] |= 0x20; ref = utc_at(&f, 1704067200300);
    assert(!rtc_set_verified(&io, &ref, NULL) && f.writes == 3);
    io = fake_io(&f); f.frozen = true; ref = utc_at(&f, 1704067200300);
    assert(!rtc_set_verified(&io, &ref, NULL) && !(f.regs[0] & 0x80));
    io = fake_io(&f); f.corrupt_calendar = true; ref = utc_at(&f, 1704067200300);
    assert(!rtc_set_verified(&io, &ref, NULL));
    io = fake_io(&f); f.regs[7] |= 8; f.regs[3] |= 0x20; // unexpected external clock
    ref = utc_at(&f, 1704067200300);
    assert(rtc_set_verified(&io, &ref, NULL) && !(f.regs[7] & 8));
    io = fake_io(&f); ref = utc_at(&f, 0);
    assert(!rtc_set_verified(&io, &ref, NULL) && f.writes == 0);
    f.save_failed = true; ref = utc_at(&f, 1704067200300);
    assert(!rtc_set_verified(&io, &ref, NULL) && f.writes == 0);
    assert(!rtc_enable_backup(&io) && f.writes == 0);
    io.save_power_failure = NULL;
    assert(!rtc_set_verified(&io, &ref, NULL) && f.writes == 0);
    io = fake_io(&f);
    assert(rtc_encode(1704067200, f.regs));
    f.regs[0] |= 0x80; f.regs[3] = (f.regs[3] | 0x30) & ~8;
    uint8_t before[32]; memcpy(before, f.regs, sizeof before);
    assert(rtc_enable_backup(&io) && f.writes == 1);
    before[3] = (before[3] | 8) & ~0x10; memset(before + 0x18, 0, 8);
    assert(!memcmp(before, f.regs, sizeof before) && f.saved);
    assert(rtc_enable_backup(&io) && f.writes == 1); // retained calendar not rewritten
}
static int64_t at(unsigned y, unsigned m, unsigned d, unsigned h, unsigned min, unsigned s)
{ int64_t epoch; assert(rtc_calendar_epoch(y, m, d, h, min, s, &epoch)); return epoch; }
static uint8_t bcd8(unsigned value) { return value / 10 * 16 + value % 10; }
// One power-fail stamp. The weekday in the month register's top bits is not part of the time.
static void stamp(uint8_t s[4], unsigned month, unsigned day, unsigned hour, unsigned minute)
{ s[0] = bcd8(minute); s[1] = bcd8(hour); s[2] = bcd8(day); s[3] = bcd8(month) | 0xe0; }
// A record as the part holds it at `found`: a running calendar with PWRFAIL and VBATEN set.
static void pfail(uint8_t calendar[7], uint8_t stamps[8], int64_t found,
                  unsigned dm, unsigned dd, unsigned dh, unsigned dmin,
                  unsigned um, unsigned ud, unsigned uh, unsigned umin)
{
    assert(rtc_encode(found, calendar));
    calendar[0] |= 0x80; calendar[3] |= 0x30;
    stamp(stamps, dm, dd, dh, dmin); stamp(stamps + 4, um, ud, uh, umin);
}
static void power_loss_test(void)
{
    uint8_t c[7], s[8]; rtc_power_loss_t loss; char text[80];
    // Three minutes counted without main power, found half a minute after it came back.
    pfail(c, s, at(2026, 11, 21, 22, 43, 32), 11, 21, 22, 40, 11, 21, 22, 43);
    assert(rtc_power_loss(c, s, &loss) && loss.down == at(2026, 11, 21, 22, 40, 0) &&
           loss.up == at(2026, 11, 21, 22, 43, 0) && rtc_backup_proof(c, &loss));
    assert(rtc_power_loss_text(&loss, text, sizeof text) == strlen(text) &&
           !strcmp(text, "2026-11-21 22:40 to 22:43 UTC (3 min)"));
    // Two stamp minutes are more than 60 s counted, and prove it; one may be a single second.
    pfail(c, s, at(2026, 11, 21, 22, 43, 32), 11, 21, 22, 41, 11, 21, 22, 43);
    assert(rtc_power_loss(c, s, &loss) && loss.up - loss.down == 120 && rtc_backup_proof(c, &loss));
    pfail(c, s, at(2026, 11, 21, 22, 33, 32), 11, 21, 22, 32, 11, 21, 22, 33);
    assert(rtc_power_loss(c, s, &loss) && loss.up - loss.down == 60 && !rtc_backup_proof(c, &loss));
    assert(!strcmp((rtc_power_loss_text(&loss, text, sizeof text), text), "2026-11-21 22:32 to 22:33 UTC (1 min)"));
    pfail(c, s, at(2026, 11, 21, 22, 9, 10), 11, 21, 22, 9, 11, 21, 22, 9);
    assert(rtc_power_loss(c, s, &loss) && loss.up == loss.down && !rtc_backup_proof(c, &loss));
    // The stamps hold no year. Across New Year the loss began in the year before the return.
    pfail(c, s, at(2027, 1, 1, 0, 5, 10), 12, 31, 23, 50, 1, 1, 0, 2);
    assert(rtc_power_loss(c, s, &loss) && loss.down == at(2026, 12, 31, 23, 50, 0) &&
           loss.up == at(2027, 1, 1, 0, 2, 0) && rtc_backup_proof(c, &loss));
    assert(!strcmp((rtc_power_loss_text(&loss, text, sizeof text), text), "2026-12-31 23:50 to 2027-01-01 00:02 UTC (12 min)"));
    // A record left on the part since last year is dated to last year, not to the months ahead.
    pfail(c, s, at(2027, 3, 1, 12, 0, 0), 11, 21, 22, 40, 11, 21, 22, 43);
    assert(rtc_power_loss(c, s, &loss) && loss.down == at(2026, 11, 21, 22, 40, 0) &&
           loss.up == at(2026, 11, 21, 22, 43, 0));
    // Hours, and then days: a board that sat on its cell from October to February.
    pfail(c, s, at(2026, 11, 22, 3, 5, 40), 11, 21, 22, 40, 11, 22, 3, 5);
    assert(rtc_power_loss(c, s, &loss) && rtc_backup_proof(c, &loss));
    assert(!strcmp((rtc_power_loss_text(&loss, text, sizeof text), text), "2026-11-21 22:40 to 2026-11-22 03:05 UTC (4 h 25 min)"));
    pfail(c, s, at(2027, 2, 10, 8, 0, 30), 10, 3, 14, 0, 2, 10, 8, 0);
    assert(rtc_power_loss(c, s, &loss) && loss.down == at(2026, 10, 3, 14, 0, 0) && rtc_backup_proof(c, &loss));
    assert(!strcmp((rtc_power_loss_text(&loss, text, sizeof text), text), "2026-10-03 14:00 to 2027-02-10 08:00 UTC (129 d 18 h)"));
    // 29 February: in its own year, the year after, and three years on, when the last one is
    // the nearest date the stamp can mean. Before 2000 there is nothing it can mean.
    pfail(c, s, at(2028, 3, 1, 0, 1, 5), 2, 29, 23, 58, 3, 1, 0, 0);
    assert(rtc_power_loss(c, s, &loss) && loss.down == at(2028, 2, 29, 23, 58, 0) && rtc_backup_proof(c, &loss));
    pfail(c, s, at(2029, 1, 10, 9, 0, 0), 2, 29, 10, 0, 2, 29, 10, 5);
    assert(rtc_power_loss(c, s, &loss) && loss.up == at(2028, 2, 29, 10, 5, 0));
    pfail(c, s, at(2027, 6, 1, 9, 0, 0), 2, 29, 10, 0, 2, 29, 10, 5);
    assert(rtc_power_loss(c, s, &loss) && loss.down == at(2024, 2, 29, 10, 0, 0));
    pfail(c, s, at(2000, 1, 1, 0, 0, 30), 12, 31, 23, 50, 12, 31, 23, 55);
    assert(!rtc_power_loss(c, s, &loss));
    // The stamps follow the calendar's hour format: 10 PM is 22:00.
    pfail(c, s, at(2026, 11, 21, 22, 43, 32), 11, 21, 22, 40, 11, 21, 22, 43);
    s[1] = s[5] = 0x70;
    assert(rtc_power_loss(c, s, &loss) && loss.down == at(2026, 11, 21, 22, 40, 0) && loss.up == at(2026, 11, 21, 22, 43, 0));
    // Stamps that are no date: cleared (month 0), not BCD, minute 60, 31 November.
    pfail(c, s, at(2026, 11, 21, 22, 43, 32), 11, 21, 22, 40, 11, 21, 22, 43);
    memset(s, 0, sizeof s); assert(!rtc_power_loss(c, s, &loss));
    memset(s, 0xa5, sizeof s); assert(!rtc_power_loss(c, s, &loss));
    pfail(c, s, at(2026, 11, 21, 22, 43, 32), 11, 21, 22, 40, 11, 21, 22, 43);
    s[0] = 0x60; assert(!rtc_power_loss(c, s, &loss));
    pfail(c, s, at(2026, 12, 1, 0, 0, 0), 11, 30, 22, 40, 11, 31, 22, 43);
    assert(!rtc_power_loss(c, s, &loss));
    // A calendar that does not decode dates nothing.
    pfail(c, s, at(2026, 11, 21, 22, 43, 32), 11, 21, 22, 40, 11, 21, 22, 43);
    c[4] = 0; assert(!rtc_power_loss(c, s, &loss));
    // Proof needs more than the stamps: the power-fail and backup-enable bits, a running
    // oscillator, and a calendar this firmware could have set.
    static const struct { unsigned reg; uint8_t clear; } needed[] = {{3, 0x10}, {3, 0x08}, {3, 0x20}, {0, 0x80}};
    for (unsigned i = 0; i < sizeof needed / sizeof needed[0]; i++) {
        pfail(c, s, at(2026, 11, 21, 22, 43, 32), 11, 21, 22, 40, 11, 21, 22, 43);
        assert(rtc_power_loss(c, s, &loss) && rtc_backup_proof(c, &loss));
        c[needed[i].reg] &= ~needed[i].clear;
        assert(rtc_power_loss(c, s, &loss) && !rtc_backup_proof(c, &loss));
    }
    pfail(c, s, at(2023, 12, 31, 23, 59, 0), 12, 31, 22, 40, 12, 31, 22, 43);
    assert(rtc_power_loss(c, s, &loss) && !rtc_backup_proof(c, &loss));
    pfail(c, s, at(2024, 1, 1, 0, 0, 0), 12, 31, 22, 40, 12, 31, 22, 43);
    assert(rtc_power_loss(c, s, &loss) && rtc_backup_proof(c, &loss));
    // The text never overruns, and says nothing of a loss it cannot date.
    loss = (rtc_power_loss_t){at(2026, 11, 21, 22, 40, 0), at(2026, 11, 21, 22, 43, 0)};
    assert(rtc_power_loss_text(&loss, text, 0) == 0);
    memset(text, '#', sizeof text);
    assert(rtc_power_loss_text(&loss, text, 11) == 10 && !strcmp(text, "2026-11-21") && text[11] == '#');
    loss.up = loss.down - 60;
    assert(rtc_power_loss_text(&loss, text, sizeof text) == 0 && !text[0]);
    loss = (rtc_power_loss_t){0, 60};
    assert(rtc_power_loss_text(&loss, text, sizeof text) == 0 && !text[0]);
}
static void take_test(void)
{
    fake_t f; rtc_io_t io = fake_io(&f); rtc_power_loss_t loss; uint8_t before[32];
    const int64_t found = at(2026, 11, 21, 22, 43, 32);
    // Nothing recorded: the calendar is read once, and nothing is saved or written.
    assert(rtc_encode(found, f.regs)); f.regs[0] |= 0x80; f.regs[3] |= 0x20;
    assert(rtc_take_power_failure(&io) && !f.saved && !f.writes && f.operations == 1);
    // A record: saved as read, and then RTCWKDAY alone is written back. That clears PWRFAIL
    // and the stamps and leaves the backup enable, the weekday, the running calendar, CONTROL,
    // the trim and the alarms as they were.
    io = fake_io(&f); pfail(f.regs, f.regs + 0x18, found, 11, 21, 22, 40, 11, 21, 22, 43);
    memcpy(before, f.regs, sizeof before);
    assert(rtc_take_power_failure(&io) && f.saved && f.writes == 1);
    const unsigned operations = f.operations;
    assert(!memcmp(f.saved_calendar, before, 7) && !memcmp(f.saved_stamps, before + 0x18, 8));
    assert(rtc_power_loss(f.saved_calendar, f.saved_stamps, &loss) && rtc_backup_proof(f.saved_calendar, &loss));
    before[3] &= ~0x10; memset(before + 0x18, 0, 8);
    assert(!memcmp(before, f.regs, sizeof before) && (f.regs[3] & 0x28) == 0x28);
    assert(rtc_take_power_failure(&io) && f.writes == 1); // taken once
    // A record that cannot be saved stays on the part, untouched.
    io = fake_io(&f); pfail(f.regs, f.regs + 0x18, found, 11, 21, 22, 40, 11, 21, 22, 43);
    memcpy(before, f.regs, sizeof before); f.save_failed = true;
    assert(!rtc_take_power_failure(&io) && !f.writes && !memcmp(before, f.regs, sizeof before));
    f.save_failed = false; io.save_power_failure = NULL;
    assert(!rtc_take_power_failure(&io) && !f.writes && !memcmp(before, f.regs, sizeof before));
    // A transfer that fails at any step is reported, and no step writes before the save.
    for (unsigned fail = 1; fail <= operations; fail++) {
        io = fake_io(&f); pfail(f.regs, f.regs + 0x18, found, 11, 21, 22, 40, 11, 21, 22, 43);
        f.fail_at = fail;
        assert(!rtc_take_power_failure(&io) && (f.saved || !f.writes));
    }
    // A flag that does not clear is read back and reported: the record is saved, and the
    // part will not stamp another loss.
    io = fake_io(&f); pfail(f.regs, f.regs + 0x18, found, 11, 21, 22, 40, 11, 21, 22, 43);
    f.power_fail_sticks = true;
    assert(!rtc_take_power_failure(&io) && f.saved && f.writes == 1 && (f.regs[3] & 0x10));
    // Midnight passing while the record is saved: the weekday written back is the new day's.
    io = fake_io(&f); pfail(f.regs, f.regs + 0x18, found, 11, 21, 22, 40, 11, 21, 22, 43);
    const uint8_t weekday = f.regs[3] & 7; f.day_rolls_on_save = true;
    assert(rtc_take_power_failure(&io) && (f.regs[3] & 7) == weekday % 7 + 1 && (f.regs[3] & 0x18) == 8);
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
    calendar_test(); gnss_test(); trusted_utc_test(); io_test(); square_test();service_test();
    power_loss_test(); take_test();
    puts("RTC calendar, GNSS qualification, trusted UTC, power-loss record and I2C fault tests passed");
    return 0;
}
