#ifndef NVF_RTC_POLICY_H
#define NVF_RTC_POLICY_H
#include "gnss_status.h"
typedef struct {
    unsigned samples;
    int64_t first_sample_ms, last_sample_ms, last_utc_ns;
} rtc_candidate_t;
bool rtc_calendar_epoch(unsigned year, unsigned month, unsigned day,
                        unsigned hour, unsigned minute, unsigned second, int64_t *epoch);
bool rtc_decode(const uint8_t regs[7], int64_t *epoch);
bool rtc_encode(int64_t epoch, uint8_t regs[7]);
bool rtc_running(const uint8_t regs[7], int64_t *epoch);
// At least three fresh, advancing UTC solutions spanning two seconds, with valid position/date/time, resolved
// seconds and <=100 ms reported uncertainty. Not authenticated or PPS-qualified.
bool rtc_gnss_candidate(rtc_candidate_t *candidate, const gnss_status_t *gnss,
                        int64_t now_ms, int64_t *epoch);
// Wall-clock UTC this firmware trusts: GNSS UTC once the candidate has three advancing samples,
// the newest no more than 2 s old; otherwise a validated running RTC calendar (readable, oscillator
// running, validated calendar) read no more than 30 s ago. Never SNTP. Feed on every poll: the
// candidate tracks NAV-PVT progression. Returns the source; *utc is 0 when unknown.
enum { RTC_UTC_UNKNOWN, RTC_UTC_RTC, RTC_UTC_GNSS };
unsigned rtc_trusted_utc(rtc_candidate_t *candidate, const gnss_status_t *gnss, uint8_t rtc_flags,
                         int64_t rtc_epoch, int64_t rtc_sampled_ms, int64_t now_ms, int64_t *utc);
#endif
