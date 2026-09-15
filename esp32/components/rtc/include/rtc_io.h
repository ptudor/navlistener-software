#ifndef NVF_RTC_IO_H
#define NVF_RTC_IO_H
#include "rtc_policy.h"
typedef struct {
    void *ctx;
    bool (*read)(void *ctx, uint8_t reg, uint8_t *out, size_t length);
    bool (*write)(void *ctx, uint8_t reg, const uint8_t *data, size_t length);
    void (*delay)(void *ctx, unsigned milliseconds);
    // Must durably save the raw RTC calendar and power-down/up timestamps
    // before a RTCWKDAY write clears the hardware's power-fail evidence.
    bool (*save_power_failure)(void *ctx, const uint8_t calendar[7], const uint8_t stamps[8]);
} rtc_io_t;
bool rtc_enable_backup(const rtc_io_t *io);
// Timekeeping and CONTROL only (0x00..0x07); never SRAM/EEPROM/EUI/alarm data.
bool rtc_set_verified(const rtc_io_t *io, int64_t epoch);
#endif
