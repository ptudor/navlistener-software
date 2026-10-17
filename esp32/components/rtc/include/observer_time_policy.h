#pragma once
#include <stdbool.h>
#include <stdint.h>

typedef enum { OBS_TIME_UNKNOWN, OBS_TIME_RTC, OBS_TIME_GNSS, OBS_TIME_NETWORK, OBS_TIME_FACTORY } observer_time_source_t;
typedef struct {
    observer_time_source_t source;
    int64_t utc_ns, sampled_ms;
    uint64_t uncertainty_ns;
    bool uncertainty_known;
} observer_time_sample_t;
typedef struct {
    observer_time_sample_t independent, system;
    uint32_t changes;
    int64_t old_utc_ns, new_utc_ns, changed_ms;
} observer_time_state_t;
// Freshness bounds apply to evidence, not to monotonic timers or old records.
bool observer_time_fresh(const observer_time_sample_t *sample, int64_t now_ms);
int64_t observer_time_project(const observer_time_sample_t *sample, int64_t now_ms);
// Bootstrap an unusable system clock, or promote RTC continuity to qualified
// GNSS. Network synchronization remains distinguishable and takes precedence.
bool observer_time_bootstrap(const observer_time_state_t *state, int64_t system_ns,
                              int64_t now_ms, observer_time_sample_t *selected);
void observer_time_changed(observer_time_state_t *state, const observer_time_sample_t *sample,
                            int64_t old_ns, int64_t new_ns, int64_t now_ms);
const char *observer_time_source_name(observer_time_source_t source);
