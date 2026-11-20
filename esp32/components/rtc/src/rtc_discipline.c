#include "rtc_discipline.h"
#include <stdlib.h>
#include <string.h>

#define SECOND_NS INT64_C(1000000000)

void rtc_discipline_reset(rtc_discipline_t *d) { memset(d, 0, sizeof *d); }

static int by_value(const void *a, const void *b)
{
    const int32_t x = *(const int32_t *)a, y = *(const int32_t *)b;
    return (x > y) - (x < y);
}

bool rtc_discipline_feed(rtc_discipline_t *d, const rtc_discipline_sample_t *s, rtc_discipline_measure_t *out)
{
    if (!s->valid || !s->gnss_period_ticks) { rtc_discipline_reset(d); return false; }
    // The phase in nanoseconds of GNSS time: the measured GNSS second takes the ESP32's own
    // clock error out of it.
    const int64_t phase_ns = (int64_t)s->phase_ticks * SECOND_NS / (int64_t)s->gnss_period_ticks;
    const bool continuous = d->have_last && s->gnss_missing == d->gnss_missing && s->rtc_missing == d->rtc_missing &&
        s->gnss_discontinuities == d->gnss_discontinuities && s->rtc_discontinuities == d->rtc_discontinuities &&
        s->gnss_captured >= d->last_gnss_captured && s->gnss_captured - d->last_gnss_captured <= 3 &&
        s->rtc_captured >= d->last_rtc_captured && s->rtc_captured - d->last_rtc_captured <= 3;
    if (!continuous) {
        rtc_discipline_reset(d);
        d->gnss_missing = s->gnss_missing; d->rtc_missing = s->rtc_missing;
        d->gnss_discontinuities = s->gnss_discontinuities; d->rtc_discontinuities = s->rtc_discontinuities;
    } else if (s->rtc_captured == d->last_rtc_captured) {
        // No new RTC edge since the last reading: nothing has moved yet.
        d->last_gnss_captured = s->gnss_captured;
        return false;
    } else {
        // The phase is kept within half a second either way, so it wraps as the RTC gains or
        // loses a whole half second on GNSS. It moves once for each RTC second that has passed.
        int64_t moved = phase_ns - d->last_phase_ns;
        if (moved > SECOND_NS / 2) moved -= SECOND_NS;
        if (moved < -SECOND_NS / 2) moved += SECOND_NS;
        // An RTC running fast has its edge arrive earlier each second: the phase falls.
        d->rate_ppb[d->count++] = (int32_t)(-moved / (int64_t)(s->rtc_captured - d->last_rtc_captured));
    }
    d->have_last = true;
    d->last_phase_ns = phase_ns;
    d->last_gnss_captured = s->gnss_captured;
    d->last_rtc_captured = s->rtc_captured;
    if (d->count < RTC_DISCIPLINE_SAMPLES) return false;
    qsort(d->rate_ppb, d->count, sizeof d->rate_ppb[0], by_value);
    const unsigned n = d->count;
    *out = (rtc_discipline_measure_t){
        .crystal_ppb = (int32_t)(((int64_t)d->rate_ppb[(n - 1) / 2] + d->rate_ppb[n / 2]) / 2),
        .spread_ppb = (int32_t)((int64_t)d->rate_ppb[n - 1 - n / 4] - d->rate_ppb[n / 4]),
    };
    d->count = 0;
    return true;
}

bool rtc_discipline_usable(const rtc_discipline_measure_t *m)
{
    return m->spread_ppb <= RTC_DISCIPLINE_SPREAD_MAX_PPB && m->crystal_ppb <= RTC_DISCIPLINE_RATE_MAX_PPB &&
           m->crystal_ppb >= -RTC_DISCIPLINE_RATE_MAX_PPB;
}

int rtc_trim_steps(uint8_t osctrim)
{
    const int value = osctrim & 0x7f;
    return osctrim & 0x80 ? value : -value;
}
uint8_t rtc_trim_register(int steps)
{
    if (steps > RTC_TRIM_STEPS_MAX) steps = RTC_TRIM_STEPS_MAX;
    if (steps < -RTC_TRIM_STEPS_MAX) steps = -RTC_TRIM_STEPS_MAX;
    return steps > 0 ? (uint8_t)(0x80 | steps) : (uint8_t)-steps;
}
static int64_t rounded(int64_t numerator, int64_t denominator)
{
    return numerator >= 0 ? (numerator + denominator / 2) / denominator : -((-numerator + denominator / 2) / denominator);
}
int32_t rtc_trim_ppb(int steps) { return (int32_t)rounded((int64_t)steps * RTC_TRIM_STEP_PPT, 1000); }

int rtc_discipline_trim(int32_t crystal_ppb, int current_steps)
{
    const int64_t crystal_ppt = (int64_t)crystal_ppb * 1000;
    int64_t left = crystal_ppt + (int64_t)current_steps * RTC_TRIM_STEP_PPT;
    if (left < 0) left = -left;
    if (current_steps >= -RTC_TRIM_STEPS_MAX && current_steps <= RTC_TRIM_STEPS_MAX && 4 * left <= 3 * (int64_t)RTC_TRIM_STEP_PPT)
        return current_steps;
    int64_t steps = rounded(-crystal_ppt, RTC_TRIM_STEP_PPT);
    if (steps > RTC_TRIM_STEPS_MAX) steps = RTC_TRIM_STEPS_MAX;
    if (steps < -RTC_TRIM_STEPS_MAX) steps = -RTC_TRIM_STEPS_MAX;
    return (int)steps;
}
