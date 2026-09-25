#ifndef NVF_RTC_MAX31328_H
#define NVF_RTC_MAX31328_H
#include "rtc_io.h"
// MAX31328 (ZED/X20 U5) at I2C 0x68: DS3231-style registers (ADI 19-100978). Its
// temperature-compensated oscillator switches to the backup cell by itself. The status
// register's oscillator-stop flag (OSF) records any stop, including a failed switch when
// VCC falls faster than tVCCF, and the calendar is untrusted until firmware sets it again
// from qualified GNSS UTC and clears the flag. Calendars outside 2000-2099 (century bit
// set) are never valid; a failed set leaves that bit set so it cannot pass as retained time.
enum {
    MAX31328_ADDRESS = 0x68,
    MAX31328_CONTROL = 0x0e, MAX31328_STATUS = 0x0f, MAX31328_AGING = 0x10,
    MAX31328_EOSC = 0x80, MAX31328_BBSQW = 0x40, MAX31328_CONV = 0x20, MAX31328_RS = 0x18,
    MAX31328_INTCN = 0x04, MAX31328_A2IE = 0x02, MAX31328_A1IE = 0x01,
    MAX31328_OSF = 0x80, MAX31328_EN32KHZ = 0x08, MAX31328_BSY = 0x04, MAX31328_A2F = 0x02,
    MAX31328_A1F = 0x01,
};
// Registers 0x00-0x06: 24-hour or 12-hour BCD with every unused bit clear.
bool max31328_decode(const uint8_t regs[7], int64_t *epoch);
bool max31328_encode(int64_t epoch, uint8_t regs[7]);
// A validated running calendar: oscillator enabled, no stop recorded, calendar valid.
bool max31328_running(const uint8_t regs[7], uint8_t control, uint8_t status, int64_t *epoch);
enum { MAX31328_SET_OK, MAX31328_SET_IO, MAX31328_SET_OSF_STUCK, MAX31328_SET_UNVERIFIED };
// Writes the calendar, clears OSF and proves the clock advances. Anything short of
// MAX31328_SET_OK leaves the century bit set (best effort on a failing bus), so the
// calendar reads as invalid. MAX31328_SET_OSF_STUCK means the flag would not clear.
unsigned max31328_set_verified(const rtc_io_t *io, int64_t epoch);
// 1 Hz on INT/SQW (INTCN and RS clear, no output on battery) with the unused 32 kHz
// output off. Returns rtc_square_wave's codes: 1 enabled, 2 stopped or invalid calendar,
// 3 alarm interrupts in use (left alone), 4 I/O failure. OSF is never cleared here.
unsigned max31328_square_wave(const rtc_io_t *io, uint8_t *control, uint8_t *aging);
#endif
