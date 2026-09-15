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
#endif
