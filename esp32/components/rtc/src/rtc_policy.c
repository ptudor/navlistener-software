#include "rtc_policy.h"
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
bool rtc_decode(const uint8_t r[7], int64_t *epoch)
{
    unsigned s, min, h, d, m, y;
    if (!unbcd(r[0] & 0x7f, &s) || !unbcd(r[1] & 0x7f, &min) ||
        !unbcd(r[2] & (r[2] & 0x40 ? 0x1f : 0x3f), &h) ||
        !unbcd(r[4] & 0x3f, &d) || !unbcd(r[5] & 0x1f, &m) || !unbcd(r[6], &y) ||
        !(r[3] & 7)) return false;
    if (r[2] & 0x40) {
        if (h < 1 || h > 12) return false;
        h = h % 12 + ((r[2] & 0x20) ? 12 : 0);
    }
    return rtc_calendar_epoch(2000 + y, m, d, h, min, s, epoch);
}
bool rtc_encode(int64_t epoch, uint8_t r[7])
{
    if (epoch < 946684800 || epoch >= 4102444800LL) return false;
    int64_t days = epoch / 86400 - 10957;
    unsigned y = 2000, m = 1, seconds = epoch % 86400;
    while (days >= 365 + (y % 4 == 0)) { days -= 365 + (y % 4 == 0); y++; }
    while (days >= days_in_month(y, m)) { days -= days_in_month(y, m); m++; }
    r[0] = bcd(seconds % 60); // stopped until the final start write
    r[1] = bcd(seconds / 60 % 60); r[2] = bcd(seconds / 3600); // 24-hour mode
    r[3] = 8 | ((epoch / 86400 + 4) % 7 + 1);
    r[4] = bcd((unsigned)days + 1); r[5] = bcd(m); r[6] = bcd(y - 2000);
    return true;
}
bool rtc_running(const uint8_t regs[7], int64_t *epoch)
{ return (regs[0] & 0x80) && (regs[3] & 0x20) && rtc_decode(regs, epoch); }
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
unsigned rtc_trusted_utc(rtc_candidate_t *c, const gnss_status_t *g, uint8_t rtc_flags,
                         int64_t rtc_epoch, int64_t rtc_sampled_ms, int64_t now, int64_t *utc)
{
    int64_t epoch;
    (void)rtc_gnss_candidate(c, g, now, &epoch);
    if (c->samples >= 3 && now >= c->last_sample_ms && now - c->last_sample_ms <= 2000) {
        *utc = (c->last_utc_ns + (now - c->last_sample_ms) * 1000000) / 1000000000; return RTC_UTC_GNSS;
    }
    if ((rtc_flags & 19) == 19 && now >= rtc_sampled_ms && now - rtc_sampled_ms <= 30000) {
        *utc = rtc_epoch + (now - rtc_sampled_ms) / 1000; return RTC_UTC_RTC;
    }
    *utc = 0; return RTC_UTC_UNKNOWN;
}
