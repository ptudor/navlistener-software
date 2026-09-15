#pragma once
#include "timing_report.h"
#define PULSE_COUNTER_MODULUS 30000u
typedef struct {
    timing_channel_report_t report;
    uint32_t rise_tick, high_tick, counter_raw;
    uint64_t rise_us, high_us, counter_us;
    bool have_rise, high, counter_started;
} pulse_channel_t;
typedef struct { uint32_t hz; pulse_channel_t channel[2]; } pulse_stats_t;
void pulse_stats_edge(pulse_stats_t *s, unsigned channel, bool rising, uint32_t tick, uint64_t uptime_us);
void pulse_stats_loss(pulse_stats_t *s);
void pulse_stats_counter(pulse_stats_t *s, unsigned channel, uint32_t raw, uint64_t uptime_us);
void pulse_stats_snapshot(const pulse_stats_t *s, uint64_t now_us, report_timing_t *out);
