#include "rtc_policy.h"
#include <stdio.h>
#include <stdlib.h>
static const unsigned month_days[] = {31,28,31,30,31,30,31,31,30,31,30,31};
static unsigned days_in_month(unsigned y, unsigned m)
{ return month_days[m-1] + (m == 2 && y % 4 == 0); }
bool rtc_calendar_epoch(unsigned y, unsigned m, unsigned d, unsigned h, unsigned min,
                        unsigned sec, int64_t *epoch)
{
    if (!epoch || y < 2000 || y > 2099 || m < 1 || m > 12 || d < 1 ||
        d > days_in_month(y, m) || h > 23 || min > 59 || sec > 59) return false;
    int64_t days = 10957; // 2000-01-01 relative to 1970-01-01, Gregorian UTC
    for (unsigned year = 2000; year < y; year++) days += 365 + (year % 4 == 0);
    for (unsigned month = 1; month < m; month++) days += days_in_month(y, month);
    *epoch = (days + d - 1) * 86400 + h * 3600 + min * 60 + sec;
    return true;
}
static bool unbcd(uint8_t value, unsigned *out)
{
    if ((value & 15) > 9 || (value >> 4) > 9) return false;
    *out = (value >> 4) * 10 + (value & 15); return true;
}
static uint8_t bcd(unsigned value) { return (value / 10) * 16 + value % 10; }
// RTCHOUR's format, which the power-fail stamps share: 24-hour, or 12-hour with AM/PM.
static bool hour24(uint8_t reg, unsigned *h)
{
    if (!unbcd(reg & (reg & 0x40 ? 0x1f : 0x3f), h)) return false;
    if (!(reg & 0x40)) return true;
    if (*h < 1 || *h > 12) return false;
    *h = *h % 12 + ((reg & 0x20) ? 12 : 0);
    return true;
}
bool rtc_decode(const uint8_t r[7], int64_t *epoch)
{
    unsigned s, min, h, d, m, y;
    if (!unbcd(r[0] & 0x7f, &s) || !unbcd(r[1] & 0x7f, &min) || !hour24(r[2], &h) ||
        !unbcd(r[4] & 0x3f, &d) || !unbcd(r[5] & 0x1f, &m) || !unbcd(r[6], &y) ||
        !(r[3] & 7)) return false;
    return rtc_calendar_epoch(2000 + y, m, d, h, min, s, epoch);
}
static bool in_range(int64_t epoch) { return epoch >= 946684800 && epoch < 4102444800LL; }
// The Gregorian date of an epoch in 2000..2099, and the seconds into that day.
static void civil(int64_t epoch, unsigned *y, unsigned *m, unsigned *d, unsigned *seconds)
{
    int64_t days = epoch / 86400 - 10957;
    *y = 2000; *m = 1; *seconds = epoch % 86400;
    while (days >= 365 + (*y % 4 == 0)) { days -= 365 + (*y % 4 == 0); (*y)++; }
    while (days >= days_in_month(*y, *m)) { days -= days_in_month(*y, *m); (*m)++; }
    *d = (unsigned)days + 1;
}
bool rtc_encode(int64_t epoch, uint8_t r[7])
{
    if (!in_range(epoch)) return false;
    unsigned y, m, d, seconds;
    civil(epoch, &y, &m, &d, &seconds);
    r[0] = bcd(seconds % 60); // stopped until the final start write
    r[1] = bcd(seconds / 60 % 60); r[2] = bcd(seconds / 3600); // 24-hour mode
    r[3] = 8 | ((epoch / 86400 + 4) % 7 + 1);
    r[4] = bcd(d); r[5] = bcd(m); r[6] = bcd(y - 2000);
    return true;
}
bool rtc_running(const uint8_t regs[7], int64_t *epoch)
{ return (regs[0] & 0x80) && (regs[3] & 0x20) && rtc_decode(regs, epoch); }
// One power-fail stamp (minute, hour, date, weekday and month) as the last such minute at
// or before `limit`, looking back from `*year`, which is left at the year found. Four years
// back reaches the last 29 February from any date.
static bool stamp_epoch(const uint8_t s[4], unsigned *year, int64_t limit, int64_t *epoch)
{
    unsigned min, h, d, m;
    if (!unbcd(s[0] & 0x7f, &min) || !hour24(s[1], &h) || !unbcd(s[2] & 0x3f, &d) ||
        !unbcd(s[3] & 0x1f, &m)) return false;
    for (unsigned back = 0; back <= 4; back++)
        if (rtc_calendar_epoch(*year - back, m, d, h, min, 0, epoch) && *epoch <= limit) {
            *year -= back; return true;
        }
    return false;
}
bool rtc_power_loss(const uint8_t calendar[7], const uint8_t stamps[8], rtc_power_loss_t *loss)
{
    int64_t now; unsigned year;
    if (!rtc_decode(calendar, &now) || !unbcd(calendar[6], &year)) return false;
    year += 2000;
    return stamp_epoch(stamps + 4, &year, now, &loss->up) && stamp_epoch(stamps, &year, loss->up, &loss->down);
}
bool rtc_backup_proof(const uint8_t calendar[7], const rtc_power_loss_t *loss)
{
    int64_t now;
    return (calendar[3] & 0x18) == 0x18 && rtc_running(calendar, &now) && now >= 1704067200 &&
           loss->up - loss->down >= RTC_BACKUP_PROOF_S;
}
size_t rtc_power_loss_text(const rtc_power_loss_t *loss, char *out, size_t size)
{
    if (!size) return 0;
    out[0] = 0;
    if (!in_range(loss->down) || !in_range(loss->up) || loss->up < loss->down) return 0;
    unsigned y[2], m[2], d[2], s[2];
    civil(loss->down, &y[0], &m[0], &d[0], &s[0]); civil(loss->up, &y[1], &m[1], &d[1], &s[1]);
    char until[36] = "", length[48]; // room for any unsigned, as the compiler checks
    if (y[0] != y[1] || m[0] != m[1] || d[0] != d[1]) snprintf(until, sizeof until, "%04u-%02u-%02u ", y[1], m[1], d[1]);
    const unsigned long long minutes = (unsigned long long)(loss->up - loss->down) / 60;
    if (minutes < 120) snprintf(length, sizeof length, "%llu min", minutes);
    else if (minutes < 2880) snprintf(length, sizeof length, "%llu h %llu min", minutes / 60, minutes % 60);
    else snprintf(length, sizeof length, "%llu d %llu h", minutes / 1440, minutes % 1440 / 60);
    const int n = snprintf(out, size, "%04u-%02u-%02u %02u:%02u to %s%02u:%02u UTC (%s)", y[0], m[0], d[0],
                           s[0] / 3600, s[0] / 60 % 60, until, s[1] / 3600, s[1] / 60 % 60, length);
    return n < 0 ? 0 : (size_t)n < size ? (size_t)n : size - 1;
}
static uint32_t le32(const uint8_t *p)
{ return p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24); }
bool rtc_gnss_candidate(rtc_candidate_t *c, const gnss_status_t *s, int64_t now, int64_t *epoch)
{
    const uint8_t *p = s->pvt_utc;
    int64_t second;
    int32_t nano = (int32_t)le32(p+12);
    if (!s->fix_valid || now < s->fix_ms || now - s->fix_ms > 2000 ||
        (p[7] & 7) != 7 || le32(p+8) > 100000000 || nano <= -1000000000 || nano >= 1000000000 ||
        !rtc_calendar_epoch(p[0] | ((unsigned)p[1] << 8), p[2], p[3], p[4], p[5], p[6], &second)) {
        *c = (rtc_candidate_t){0}; return false;
    }
    int64_t utc_ns = second * 1000000000LL + nano;
    if (c->samples && s->fix_ms == c->last_sample_ms) return false; // same NAV-PVT snapshot
    if (!c->samples || s->fix_ms <= c->last_sample_ms || utc_ns <= c->last_utc_ns ||
        s->fix_ms - c->last_sample_ms > 3000 ||
        llabs((utc_ns - c->last_utc_ns) - (s->fix_ms - c->last_sample_ms) * 1000000) > 250000000) {
        c->samples = 0; c->first_sample_ms = s->fix_ms;
    }
    c->last_sample_ms = s->fix_ms; c->last_utc_ns = utc_ns;
    if (c->samples < 3) c->samples++;
    *epoch = (utc_ns + (now - s->fix_ms) * 1000000) / 1000000000LL;
    return c->samples >= 3 && s->fix_ms - c->first_sample_ms >= 2000;
}
rtc_time_ref_t rtc_candidate_ref(const rtc_candidate_t *c)
{ return (rtc_time_ref_t){.utc_ns = c->last_utc_ns, .sampled_ms = c->last_sample_ms}; }
int64_t rtc_ref_second(const rtc_time_ref_t *ref, int64_t now_ms)
{ return (ref->utc_ns + (now_ms - ref->sampled_ms) * 1000000 + 500000000) / 1000000000; }
unsigned rtc_trusted_utc(rtc_candidate_t *c, const gnss_status_t *g, uint8_t rtc_flags,
                         int64_t rtc_epoch, int64_t rtc_sampled_ms, int64_t now, int64_t *utc)
{
    int64_t epoch;
    (void)rtc_gnss_candidate(c, g, now, &epoch);
    if (c->samples >= 3 && c->last_sample_ms-c->first_sample_ms>=2000 && now >= c->last_sample_ms && now - c->last_sample_ms <= 2000) {
        *utc = (c->last_utc_ns + (now - c->last_sample_ms) * 1000000) / 1000000000; return RTC_UTC_GNSS;
    }
    if ((rtc_flags & 19) == 19 && now >= rtc_sampled_ms && now - rtc_sampled_ms <= 30000) {
        *utc = rtc_epoch + (now - rtc_sampled_ms) / 1000; return RTC_UTC_RTC;
    }
    *utc = 0; return RTC_UTC_UNKNOWN;
}
