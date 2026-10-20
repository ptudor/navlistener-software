#include "regulator_thermal.h"
#include <string.h>

static const int8_t caps[REGULATOR_THERMAL_LEVELS] = {0, 66, 52, 44, 34};

bool regulator_thermal_init(regulator_thermal_t *t, const regulator_thermal_limits_t *limits, int64_t now_ms)
{
    memset(t, 0, sizeof *t);
    if (limits->release_centi_c >= limits->throttle_centi_c || limits->throttle_centi_c > limits->full_centi_c)
        return false;
    t->limits = *limits;
    // Both dwell times count as served at boot, so a hot first reading throttles at once.
    t->changed_ms = INT64_MIN / 2;
    t->accounted_ms = now_ms;
    return true;
}

static void set_level(regulator_thermal_t *t, uint8_t level, int64_t now_ms)
{
    t->level = level;
    t->changed_ms = now_ms;
    t->changes++;
}

bool regulator_thermal_sample(regulator_thermal_t *t, int64_t now_ms, bool valid, int16_t centi_c)
{
    if (t->level && now_ms > t->accounted_ms) t->throttled_ms += (uint64_t)(now_ms - t->accounted_ms);
    t->accounted_ms = now_ms;
    if (!valid) return false;
    t->seen = true;
    t->centi_c = centi_c;
    t->sample_ms = now_ms;
    if (!t->peak_valid || centi_c > t->peak_centi_c) { t->peak_centi_c = centi_c; t->peak_valid = true; }
    const uint8_t last = REGULATOR_THERMAL_LEVELS - 1, before = t->level;
    const int64_t held = now_ms - t->changed_ms;
    if (centi_c >= t->limits.full_centi_c && t->level < last) set_level(t, last, now_ms);
    else if (centi_c >= t->limits.throttle_centi_c && t->level < last && held >= REGULATOR_THERMAL_STEP_UP_MS)
        set_level(t, t->level + 1, now_ms);
    else if (centi_c <= t->limits.release_centi_c && t->level > 0 && held >= REGULATOR_THERMAL_STEP_DOWN_MS)
        set_level(t, t->level - 1, now_ms);
    return t->level != before;
}

bool regulator_thermal_responding(const regulator_thermal_t *t, int64_t now_ms)
{
    return t->seen && now_ms - t->sample_ms < REGULATOR_THERMAL_LOST_MS;
}

int8_t regulator_thermal_tx_cap(uint8_t level)
{
    return caps[level < REGULATOR_THERMAL_LEVELS ? level : REGULATOR_THERMAL_LEVELS - 1];
}

void regulator_thermal_report_sent(regulator_thermal_t *t)
{
    t->peak_centi_c = t->centi_c;
    t->peak_valid = t->seen;
}
