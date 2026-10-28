#include "rtk_stat.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// Feeds 250 ms samples, as the panel task takes them, from start_ms up to (not including)
// end_ms; level(t) gives the pin. Returns how many samples reported a change.
static unsigned run(rtk_stat_t *s, int64_t start_ms, int64_t end_ms, bool (*level)(int64_t))
{
    unsigned reported = 0;
    for (int64_t t = start_ms; t < end_ms; t += 250) reported += rtk_stat_sample(s, level(t), t);
    return reported;
}
static bool always_high(int64_t t) { (void)t; return true; }
static bool always_low(int64_t t) { (void)t; return false; }
static bool toggling(int64_t t) { return (t / 500) % 2 == 0; } // 1 Hz
static bool fixed_at_25s(int64_t t) { return t < 25000; }

int main(void)
{
    assert(!strcmp(rtk_stat_name(RTK_STAT_UNKNOWN), "unknown"));
    assert(!strcmp(rtk_stat_meaning(RTK_STAT_UNKNOWN), "not sampled yet"));

    // No carrier-phase solution: the first window reports high once, and then nothing.
    rtk_stat_t s = {0};
    assert(!rtk_stat_sample(&s, true, 0) && s.started && s.state == RTK_STAT_UNKNOWN);
    assert(run(&s, 250, 10000, always_high) == 0 && s.state == RTK_STAT_UNKNOWN);
    assert(rtk_stat_sample(&s, true, 10000) && s.state == RTK_STAT_HIGH && s.window_changes == 0);
    assert(run(&s, 10250, 60000, always_high) == 0 && s.state == RTK_STAT_HIGH);
    assert(!strcmp(rtk_stat_name(s.state), "high") && !strcmp(rtk_stat_meaning(s.state), "no carrier-phase solution"));

    // A fix: one change, then a steady low, reported once at the window's end.
    s = (rtk_stat_t){0};
    assert(run(&s, 0, 20250, fixed_at_25s) == 1 && s.state == RTK_STAT_HIGH); // two high windows, one report
    assert(run(&s, 20250, 30250, fixed_at_25s) == 1 && s.state == RTK_STAT_LOW && s.window_changes == 1);
    assert(run(&s, 30250, 90000, fixed_at_25s) == 0);
    assert(!strcmp(rtk_stat_name(s.state), "low") && !strcmp(rtk_stat_meaning(s.state), "RTK fixed"));

    // Corrections in use: a 1 Hz toggle is two changes a second, about 20 in a window.
    s = (rtk_stat_t){0};
    assert(run(&s, 0, 10250, toggling) == 1 && s.state == RTK_STAT_TOGGLING);
    assert(s.window_changes >= 19 && s.window_changes <= 21);
    assert(run(&s, 10250, 60000, toggling) == 0 && s.state == RTK_STAT_TOGGLING);
    assert(!strcmp(rtk_stat_meaning(s.state), "corrections received and used, RTK not fixed"));
    // Toggling stops at a fix: reported once, as low.
    assert(run(&s, 60000, 80000, always_low) == 1 && s.state == RTK_STAT_LOW);

    // Two changes inside one window are toggling, even when the pin ends where it began.
    s = (rtk_stat_t){0};
    rtk_stat_sample(&s, true, 0);
    assert(!rtk_stat_sample(&s, false, 3000) && !rtk_stat_sample(&s, true, 6000));
    assert(rtk_stat_sample(&s, true, 10000) && s.state == RTK_STAT_TOGGLING && s.window_changes == 2);
    assert(rtk_stat_sample(&s, true, 20000) && s.state == RTK_STAT_HIGH);

    // A stalled sampler closes its window at the next sample, however late.
    s = (rtk_stat_t){0};
    rtk_stat_sample(&s, false, 0);
    assert(rtk_stat_sample(&s, false, 45000) && s.state == RTK_STAT_LOW && s.window_ms == 45000);
    puts("RTK_STAT classification tests passed");
}
