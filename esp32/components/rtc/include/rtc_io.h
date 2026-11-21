#ifndef NVF_RTC_IO_H
#define NVF_RTC_IO_H
#include "rtc_policy.h"
typedef struct {
    void *ctx;
    bool (*read)(void *ctx, uint8_t reg, uint8_t *out, size_t length);
    bool (*write)(void *ctx, uint8_t reg, const uint8_t *data, size_t length);
    void (*delay)(void *ctx, unsigned milliseconds);
    // The monotonic millisecond clock an rtc_time_ref_t is sampled on.
    int64_t (*now_ms)(void *ctx);
    // Must durably save the raw RTC calendar and power-down/up timestamps
    // before a RTCWKDAY write clears the hardware's power-fail evidence.
    bool (*save_power_failure)(void *ctx, const uint8_t calendar[7], const uint8_t stamps[8]);
    // Writes OSCTRIM (0x08) and nothing else. NULL on a part whose rate is not disciplined.
    bool (*write_trim)(void *ctx, uint8_t osctrim);
} rtc_io_t;
bool rtc_enable_backup(const rtc_io_t *io);
// Takes a power failure the part has recorded. With PWRFAIL set, the raw calendar and the
// power-down/up stamps go to save_power_failure, and only once that has saved them is
// RTCWKDAY written back as it stands. The write clears PWRFAIL and the stamps, and until it
// does the part logs no later failure (DS20002266K section 5.7.1, note 1). Nothing else is
// written, and nothing at all with PWRFAIL clear. False when a transfer or the save fails:
// the record then stays on the part.
bool rtc_take_power_failure(const rtc_io_t *io);
// Timekeeping and CONTROL only (0x00..0x07); never SRAM/EEPROM/EUI/alarm data. Stops the
// oscillator, waits for it, loads the second nearest UTC at that moment, starts it, and
// accepts the result only if the calendar reads within a second of UTC and advances;
// *written (optional) receives the second loaded.
bool rtc_set_verified(const rtc_io_t *io, const rtc_time_ref_t *ref, int64_t *written);
// 1=1 Hz enabled, 2=oscillator stopped, 3=alarm/coarse-trim conflict, 4=I/O failure.
// Only CONTROL's SQWEN/SQWFS change; calendar, trim, alarms and identity stay intact.
unsigned rtc_square_wave(const rtc_io_t *io, uint8_t *control, uint8_t *trim);
// The MCP7941X's digital trim (DS20002266 section 5.6). Reads CONTROL and OSCTRIM, and
// writes OSCTRIM only while the part runs from its crystal in fine-trim mode (EXTOSC and
// CRSTRIM clear), where the register means two clock cycles a minute per step; the write is
// read back. *previous (optional) receives the register as it was. False, with OSCTRIM
// untouched, in any other mode or when a transfer fails.
bool rtc_trim_read(const rtc_io_t *io, uint8_t *control, uint8_t *osctrim);
bool rtc_trim_write(const rtc_io_t *io, uint8_t osctrim, uint8_t *previous);
#endif
