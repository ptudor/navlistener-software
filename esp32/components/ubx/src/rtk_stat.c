#include "rtk_stat.h"
bool rtk_stat_sample(rtk_stat_t *s, bool high, int64_t now_ms)
{
    if (!s->started) {
        *s = (rtk_stat_t){.started = true, .level = high, .window_ms = now_ms};
        return false;
    }
    if (high != s->level) { s->level = high; s->changes++; }
    if (now_ms - s->window_ms < RTK_STAT_WINDOW_MS) return false;
    const rtk_stat_state_t state = s->changes >= 2 ? RTK_STAT_TOGGLING : high ? RTK_STAT_HIGH : RTK_STAT_LOW;
    const bool changed = state != s->state;
    s->state = state;
    s->window_changes = s->changes;
    s->changes = 0;
    s->window_ms = now_ms;
    return changed;
}
const char *rtk_stat_name(rtk_stat_state_t state)
{
    switch (state) {
    case RTK_STAT_HIGH: return "high";
    case RTK_STAT_LOW: return "low";
    case RTK_STAT_TOGGLING: return "toggling";
    case RTK_STAT_UNKNOWN: break;
    }
    return "unknown";
}
const char *rtk_stat_meaning(rtk_stat_state_t state)
{
    switch (state) {
    case RTK_STAT_HIGH: return "no carrier-phase solution";
    case RTK_STAT_LOW: return "RTK fixed";
    case RTK_STAT_TOGGLING: return "corrections received and used, RTK not fixed";
    case RTK_STAT_UNKNOWN: break;
    }
    return "not sampled yet";
}
