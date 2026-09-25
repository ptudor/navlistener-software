#ifndef NVF_MMC34160_H
#define NVF_MMC34160_H
// MMC34160PJ magnetometer (the MAX board's U38 at I2C 0x30), MEMSIC MMC3416xPJ Rev C.
// Each measurement is a SET and a RESET reading, each after refilling the SET/RESET
// capacitor. Half their difference is the field with the bridge offset removed; their
// mean is that offset. Mounting (hard- and soft-iron) calibration is not applied here.
#include <stdbool.h>
#include <stdint.h>
#include "env_sensors.h"

enum {
    MMC34160_ADDRESS = 0x30, MMC34160_PRODUCT_ID = 0x06,
    MMC34160_COUNTS_PER_GAUSS = 2048, // 16-bit mode
    MMC34160_NULL = 32768,            // output with no applied field
};
typedef struct {
    int16_t field[3];   // X, Y, Z in the sensor's axes, counts of 1/2048 G
    uint16_t offset[3]; // bridge offset per axis, raw counts around MMC34160_NULL
} mmc34160_sample_t;

// Product ID 06h, the OTP read completed, and 16-bit output selected.
bool mmc34160_init(const env_io_t *io);
// The SET/RESET pair: false on a transfer failure, a measurement that does not finish,
// or an output at either rail (beyond the part's 16 G range).
bool mmc34160_measure(const env_io_t *io, mmc34160_sample_t *sample);
// The arithmetic of one SET/RESET pair, raw little-endian outputs per axis.
bool mmc34160_combine(const uint16_t set[3], const uint16_t reset[3], mmc34160_sample_t *sample);
#endif
