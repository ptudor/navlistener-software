#ifndef OBSERVER_RTC_H
#define OBSERVER_RTC_H
#include "driver/i2c_master.h"
#include "gnss_status.h"
#include "observer_report.h"
#include "rtc_service.h"
// The RTC the manifest lists; nothing else is probed. Call from the board task before
// the first poll: OBSERVER_RTC_NONE (the default) leaves the RTC unread and unreported.
// The DS3231M shares the MAX31328's register map and driver.
typedef enum { OBSERVER_RTC_NONE, OBSERVER_RTC_MCP79412, OBSERVER_RTC_MAX31328, OBSERVER_RTC_DS3231M } observer_rtc_part_t;
void observer_rtc_select(observer_rtc_part_t part);
// Call only from the board task. Preserves a running clock; initializes only
// from qualified receiver UTC. No system-clock or observation-timestamp changes.
void observer_rtc_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now_ms);
// Snapshot only; call from the same board task. Flags describe the last read.
report_rtc_t observer_rtc_status(void);
void observer_rtc_square_wave_status(uint8_t *state, uint8_t *control, uint8_t *trim);
// USB requests are copied and executed by the board task, the sole RTC I/O owner.
// A request never implicitly re-enrolls the board or changes credentials.
bool observer_rtc_service_request(const rtc_service_request_t *request);
bool observer_rtc_service_result(rtc_service_record_t *record);
bool observer_rtc_service_read(uint32_t sequence, rtc_service_record_t *record);
#endif
