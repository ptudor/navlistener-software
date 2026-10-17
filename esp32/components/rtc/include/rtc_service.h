#pragma once
#include "rtc_io.h"
#include "observer_time_policy.h"
#define RTC_SERVICE_TEXT 64
typedef struct {
    observer_time_source_t source; // GNSS or explicit factory reference only
    rtc_time_ref_t reference;
    uint64_t uncertainty_ns;
    char reference_id[RTC_SERVICE_TEXT], reason[RTC_SERVICE_TEXT];
    bool battery_service; // operator declaration; never automatic detection
    bool verify_only; // observe advancing continuity without changing the calendar
} rtc_service_request_t;
enum { RTC_SERVICE_PENDING, RTC_SERVICE_OK, RTC_SERVICE_BAD_REFERENCE,
       RTC_SERVICE_READ_FAILED, RTC_SERVICE_AUDIT_FAILED, RTC_SERVICE_WRITE_FAILED,
       RTC_SERVICE_OSF_STUCK, RTC_SERVICE_NO_DEVICE, RTC_SERVICE_VERIFY_FAILED };
typedef struct {
    uint32_t version, sequence, result;
    uint8_t rtc_part; // fitted observer_rtc_part_t, recorded by the board adapter
    rtc_service_request_t request;
    int64_t started_ms, completed_ms, old_epoch, written_epoch, readback_epoch;
    uint8_t old_calendar[7], old_control, old_status;
    uint8_t readback_calendar[7], readback_control, readback_status;
    bool old_read, old_valid, readback_read, readback_valid, audit_complete;
} rtc_service_record_t;
// Save must durably record the pre-write attempt before any calendar write.
// The same sequence is updated with the final result; failed final persistence
// leaves a pending attempt, not a fictitious success.
typedef bool (*rtc_service_save_t)(void *ctx, rtc_service_record_t *record);
unsigned rtc_service_execute(const rtc_io_t *io, bool tcxo, const rtc_service_request_t *request,
                             rtc_service_save_t save, void *save_ctx, rtc_service_record_t *record);
