#ifndef NVF_SHT3X_H
#define NVF_SHT3X_H
// Sensirion SHT3x-DIS humidity and temperature sensor (datasheet version 4, March 2017): the
// P3's SHT35 (U45) at 0x44, its ADDR pin grounded. Single-shot, high-repeatability conversions
// without clock stretching. The heater stays off and the Alert output unused. The part has no
// identity register; a status word that passes its CRC with the heater off stands for one.
#include <stdbool.h>
#include <stdint.h>
#include "env_sensors.h"

// The part's operating range in 0.01 C (Table 5); a conversion outside it is rejected. The
// transfer function keeps humidity within 0-100 %RH.
enum { SHT3X_MIN_CENTI_C = -4000, SHT3X_MAX_CENTI_C = 12500 };
// The CRC sent after each 16-bit word: CRC-8, polynomial 0x31, initial value 0xFF (Table 19).
uint8_t sht3x_crc(const uint8_t word[2]);
// Stops a periodic acquisition another firmware may have left running, soft-resets the part
// (heater off, single-shot mode) and reads its status word. True when the status passes its
// CRC and shows the heater off.
bool sht3x_start(const env_io_t *io, uint8_t address);
// One conversion: temperature in 0.01 C and relative humidity in 0.01 %RH, rounded down. False
// on a failed transfer, a CRC mismatch or a temperature outside the operating range.
bool sht3x_read(const env_io_t *io, uint8_t address, int16_t *centi_c, uint16_t *centi_rh);
#endif
