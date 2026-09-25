#ifndef NVF_MS5607_H
#define NVF_MS5607_H
// MS5607-02BA03 barometer (the MAX board's U2 at I2C 0x77). TE datasheet 06/2017 and
// application note AN520: PROM coefficients with a CRC-4, 24-bit conversions at
// OSR 4096, first- and second-order temperature compensation.
#include <stdbool.h>
#include <stdint.h>
#include "env_sensors.h"

enum { MS5607_ADDRESS = 0x77 };
// Full accuracy is specified from 300 to 1100 mbar; 10 to 1200 mbar is the extended
// range, where readings stay linear but are not full-accuracy.
enum {
    MS5607_MIN_PA = 1000, MS5607_MAX_PA = 120000,
    MS5607_FULL_MIN_PA = 30000, MS5607_FULL_MAX_PA = 110000,
};
typedef enum {
    MS5607_ABSENT = 0,       // no answer at 0x77
    MS5607_PROM_REJECTED = 1, // the calibration PROM failed its CRC-4
    MS5607_READY = 2,
} ms5607_state_t;
typedef struct {
    ms5607_state_t state;
    uint16_t prom[8]; // word 0 factory data, 1-6 C1-C6, 7 serial and CRC (bits 3:0)
} ms5607_t;

// AN520's CRC-4 over the eight PROM words, with the low byte of word 7 zeroed.
uint8_t ms5607_crc4(const uint16_t prom[8]);
// The datasheet's compensation, second order included. False for a zero conversion
// (read before it finished) or a result outside -40..85 C or 10..1200 mbar.
bool ms5607_compensate(const uint16_t prom[8], uint32_t d1, uint32_t d2, int32_t *centi_c, int32_t *pa);
// Reset, read the PROM and check its CRC. Retrying after a failure is safe.
ms5607_state_t ms5607_init(ms5607_t *m, const env_io_t *io);
// One pressure and one temperature conversion; requires MS5607_READY.
bool ms5607_read(const ms5607_t *m, const env_io_t *io, int32_t *centi_c, int32_t *pa);
#endif
