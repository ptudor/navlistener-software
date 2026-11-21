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
// The MCP7941X's power-fail record (DS20002266K section 5.7.1): the minute, hour, date and
// month at which VCC fell below the trip point and the part went over to its backup input,
// and the same for VCC's return. Both are read from the part's own calendar, so their
// difference is time it counted without main power. They hold no seconds and no year. The
// return is taken as the last such minute at or before `calendar` (RTCSEC..RTCYEAR as read
// when the record was found) and the loss as the last at or before the return, so a loss of
// more than a year reads as what is left over. `stamps` is PWRDNMIN..PWRUPMTH.
typedef struct { int64_t down, up; } rtc_power_loss_t; // UTC seconds, each on a whole minute
bool rtc_power_loss(const uint8_t calendar[7], const uint8_t stamps[8], rtc_power_loss_t *loss);
// What shows that a backup supply is fitted and working: a record found with PWRFAIL and
// VBATEN set, on a running calendar no earlier than 2024, whose stamps are at least this far
// apart. Two stamp minutes mean more than 60 s counted on the backup input. At the part's
// backup current that takes some 30 uF of charge, hundreds of times what a decoupling
// capacitor holds, so only a working backup supply can leave such a record.
#define RTC_BACKUP_PROOF_S 120
bool rtc_backup_proof(const uint8_t calendar[7], const rtc_power_loss_t *loss);
// "2026-11-21 22:40 to 22:43 UTC (3 min)"; the second date is written when it differs.
// Returns the length written, and nothing for a loss outside 2000..2099.
size_t rtc_power_loss_text(const rtc_power_loss_t *loss, char *out, size_t size);
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
