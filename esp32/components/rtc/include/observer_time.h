#pragma once
#include "observer_time_policy.h"
#include "gnss_status.h"
#include "observer_report.h"
#include "rtc_service.h"
// Install the SNTP notification before networking starts. No NVS/identity reset.
void observer_time_start(void);
void observer_time_poll(const gnss_status_t *gnss, report_rtc_t rtc, int64_t now_ms);
observer_time_state_t observer_time_status(void);
void observer_time_service_applied(const rtc_service_record_t *record);
// Captures the wall clock and its RS V2 source together. Source 1 is explicitly unknown.
uint64_t observer_time_capture(uint8_t *source);
