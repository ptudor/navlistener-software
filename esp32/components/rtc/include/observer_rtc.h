#ifndef OBSERVER_RTC_H
#define OBSERVER_RTC_H
#include "driver/i2c_master.h"
#include "gnss_status.h"
#include "observer_report.h"
// Call only from the board task. Preserves a running clock; initializes only
// from qualified receiver UTC. No system-clock or observation-timestamp changes.
void observer_rtc_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now_ms);
// Snapshot only; call from the same board task. Flags describe the last read.
report_rtc_t observer_rtc_status(void);
#endif
