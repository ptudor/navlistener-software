#ifndef NVF_RTC_DISCIPLINE_H
#define NVF_RTC_DISCIPLINE_H
// Rate discipline for an MCP7941X's crystal against the GNSS pulse. I/O-free.
//
// The board times the RTC's 1 Hz output and the receiver's pulse on one counter, so each
// second gives the RTC edge's position within the GNSS second. How far that position moves
// from one RTC second to the next is the RTC's rate error for that second. Most seconds show
// the crystal alone; once a minute the part's digital trim adds or removes clock cycles and
// one second shows that step as well (DS20002266 section 5.6). The median over a window is
// therefore the crystal's own rate, whatever the trim in force, and the trim that cancels it
// can be set in one move and left alone until the crystal drifts.
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define RTC_DISCIPLINE_SAMPLES 600u // one a second: about ten minutes
// One TRIMVAL step adds or removes two cycles of the 32.768 kHz clock each minute: 1.017 ppm.
#define RTC_TRIM_STEP_PPT 1017253 // parts in 10^12
#define RTC_TRIM_STEPS_MAX 127
// A window is used only when its samples agree and the rate is one a crystal can have.
#define RTC_DISCIPLINE_SPREAD_MAX_PPB 1000
#define RTC_DISCIPLINE_RATE_MAX_PPB 150000

// One reading of the board's pulse timing. valid: both pulses are fresh with valid periods,
// the phase was measured, the receiver holds a position, and the RTC is giving 1 Hz in
// fine-trim mode from its crystal.
typedef struct {
    bool valid;
    int32_t phase_ticks;        // RTC edge minus GNSS edge
    uint32_t gnss_period_ticks; // the GNSS second, in the same ticks
    uint64_t gnss_captured, rtc_captured;
    uint64_t gnss_missing, rtc_missing;
    uint32_t gnss_discontinuities, rtc_discontinuities;
} rtc_discipline_sample_t;

typedef struct {
    bool have_last;
    int64_t last_phase_ns;
    uint64_t last_gnss_captured, last_rtc_captured, gnss_missing, rtc_missing;
    uint32_t gnss_discontinuities, rtc_discontinuities;
    unsigned count;
    int32_t rate_ppb[RTC_DISCIPLINE_SAMPLES];
} rtc_discipline_t;

typedef struct {
    int32_t crystal_ppb; // the crystal's rate, fast positive: the samples' median
    int32_t spread_ppb;  // their interquartile range
} rtc_discipline_measure_t;

void rtc_discipline_reset(rtc_discipline_t *d);
// Feed every reading, valid or not. A reading that is not valid, a missed or irregular pulse
// on either input, or a gap between readings starts the window again. True when a window has
// filled: *out is its measurement and the next window begins.
bool rtc_discipline_feed(rtc_discipline_t *d, const rtc_discipline_sample_t *sample, rtc_discipline_measure_t *out);
bool rtc_discipline_usable(const rtc_discipline_measure_t *measure);

// OSCTRIM as signed steps: positive adds clock cycles and runs the clock faster (SIGN set).
int rtc_trim_steps(uint8_t osctrim);
uint8_t rtc_trim_register(int steps);
int32_t rtc_trim_ppb(int steps);
// The steps to hold for a crystal measured at crystal_ppb. The trim in force stays while it
// leaves no more than three quarters of a step, so a crystal sitting between two steps does
// not make the register flap; otherwise the nearest step, within the register's range.
int rtc_discipline_trim(int32_t crystal_ppb, int current_steps);
#endif
