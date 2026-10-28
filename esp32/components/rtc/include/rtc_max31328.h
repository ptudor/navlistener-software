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
    MAX31328_CONTROL = 0x0e, MAX31328_STATUS = 0x0f, MAX31328_AGING = 0x10, MAX31328_TEMPERATURE = 0x11,
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
// Writes the calendar (the second nearest UTC at the moment of the write, which restarts
// the countdown chain), clears OSF and proves the clock reads within a second of UTC and
// advances. Anything short of MAX31328_SET_OK leaves the century bit set (best effort on a
// failing bus), so the calendar reads as invalid. MAX31328_SET_OSF_STUCK means the flag
// would not clear. *written (optional) receives the second loaded.
unsigned max31328_set_verified(const rtc_io_t *io, const rtc_time_ref_t *ref, int64_t *written);
// 1 Hz on INT/SQW (INTCN and RS clear, no output on battery) with the unused 32 kHz
// output off. Returns rtc_square_wave's codes: 1 enabled, 2 stopped or invalid calendar,
// 3 alarm interrupts in use (left alone), 4 I/O failure. OSF is never cleared here.
unsigned max31328_square_wave(const rtc_io_t *io, uint8_t *control, uint8_t *aging);
// The temperature the oscillator compensation measures (DS3231M datasheet p16, MAX31328 p14):
// a 10-bit two's-complement code in 0.25 C steps, the integer part at 0x11 and the fraction
// in bits 7:6 of 0x12, whose other bits read zero. A power-on reset loads 0 C until the first
// conversion, about 2 s after VCC (DS3231M p10, MAX31328 p9), and powers up with OSF set. So
// a zero is withheld as possibly that default unless OSF is clear (no power loss since the
// calendar was last set) or the previous reading, NULL when there is none, lay within 0.5 C.
enum { MAX31328_TEMPERATURE_OK, MAX31328_TEMPERATURE_DEFAULT, MAX31328_TEMPERATURE_INVALID };
// regs: status 0x0f through 0x12, read in one burst. *centi_c (0.01 C) is set only when OK.
unsigned max31328_temperature(const uint8_t regs[4], const int16_t *previous_centi_c, int16_t *centi_c);
#endif
