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
// A qualified UTC instant: utc_ns at sampled_ms on the monotonic millisecond clock every
// *_ms argument here is on. A set derives the calendar second from it at the moment the
// seconds register is written, not from the second the candidate qualified with: stopping
// an oscillator and waiting for it can take seconds, and the clock starts counting from the
// value loaded.
typedef struct { int64_t utc_ns, sampled_ms; } rtc_time_ref_t;
// The reference a qualified candidate carries: its newest NAV-PVT UTC at that report's
// receive time.
rtc_time_ref_t rtc_candidate_ref(const rtc_candidate_t *candidate);
// The calendar second nearest UTC at now_ms. A clock started from it at that moment is
// within half a second of UTC either way, instead of lagging by the dropped fraction.
int64_t rtc_ref_second(const rtc_time_ref_t *ref, int64_t now_ms);
// Wall-clock UTC this firmware trusts: GNSS UTC once the candidate has three advancing samples,
// the newest no more than 2 s old; otherwise a validated running RTC calendar (readable, oscillator
// running, validated calendar) read no more than 30 s ago. Never SNTP. Feed on every poll: the
// candidate tracks NAV-PVT progression. Returns the source; *utc is 0 when unknown.
enum { RTC_UTC_UNKNOWN, RTC_UTC_RTC, RTC_UTC_GNSS };
unsigned rtc_trusted_utc(rtc_candidate_t *candidate, const gnss_status_t *gnss, uint8_t rtc_flags,
                         int64_t rtc_epoch, int64_t rtc_sampled_ms, int64_t now_ms, int64_t *utc);
#endif
