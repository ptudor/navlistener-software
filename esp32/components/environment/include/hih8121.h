#ifndef NVF_HIH8121_H
#define NVF_HIH8121_H
// Honeywell HumidIcon HIH8121 humidity and temperature sensor (HIH8000 series datasheet; I2C
// technical note 009061-2-EN): the P3's U44 at its default address 0x27. The part sleeps until
// a measurement request, the address with the write bit and no data, then converts
// temperature and humidity and holds both for a data fetch whose first two bits are its status.
#include <stdbool.h>
#include <stdint.h>
#include "env_sensors.h"

// The status bits of a data fetch (technical note Table 1).
typedef enum {
    HIH8121_FRESH = 0,        // normal operation: data not fetched since the last measurement
    HIH8121_STALE = 1,        // already fetched, or fetched before the measurement completed
    HIH8121_COMMAND_MODE = 2, // the programming mode, never seen in normal operation
    HIH8121_UNUSED = 3,
} hih8121_status_t;
// Full scale is 2^14 - 2 counts: 100 %RH, and -40 C to 125 C (technical note sections 4, 5).
enum { HIH8121_FULL_SCALE = 16382, HIH8121_MIN_CENTI_C = -4000, HIH8121_MAX_CENTI_C = 12500 };
// The time from a request to data ready: the datasheet's power-on-to-data-ready maximum
// (Table 6), above the technical note's typical 36.65 ms measurement cycle.
enum { HIH8121_MEASURE_MS = 60 };

typedef struct {
    bool answered;           // the request and the fetch were acknowledged
    hih8121_status_t status; // the fetched status bits; meaningful only when answered
} hih8121_result_t;
// Requests a measurement and fetches it once the conversion is done: temperature in 0.01 C and
// relative humidity in 0.01 %RH, rounded down. True only for fresh data within full scale.
bool hih8121_read(const env_io_t *io, uint8_t address, int16_t *centi_c, uint16_t *centi_rh,
                  hih8121_result_t *result);
#endif
