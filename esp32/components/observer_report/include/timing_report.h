#pragma once
#include <stdbool.h>
#include <stdint.h>
#define TIMING_WIRE_SIZE 196u
enum { TIMING_ENABLED=1, TIMING_COUNT_VALID=2, TIMING_FRESH=4,
       TIMING_PERIOD_VALID=8, TIMING_WIDTH_VALID=16, TIMING_SPAN_VALID=32 };
typedef struct {
    uint32_t flags, period_ticks, width_ticks, min_ticks, max_ticks, discontinuities;
    uint64_t missing_estimate, captured, physical, span_ticks, span_intervals, last_rise_ms;
    uint32_t counter_discontinuities;
} timing_channel_report_t;
typedef struct {
    bool present;
    uint8_t clock, rtc_state, rtc_control, rtc_trim, flags, tp_flags, tp_ref;
    uint32_t resolution_hz, queue_dropped;
    uint64_t started_ms, tp_ms;
    int32_t rtc_minus_gnss_ticks;
    timing_channel_report_t channel[2]; // GNSS GPIO10, RTC GPIO15
} report_timing_t;
