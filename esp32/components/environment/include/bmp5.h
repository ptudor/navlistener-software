#ifndef NVF_BMP5_H
#define NVF_BMP5_H
// Bosch BMP580/BMP581 barometer (the ZED/X20's U2 at I2C 0x46), BST-BMP581-DS004-13. The two
// parts share this register map and CHIP_ID, so the manifest says which is fitted. Forced
// conversions at pressure 16x and temperature 2x oversampling with the IIR filter bypassed;
// the part compensates on chip and reports temperature and pressure directly.
#include <stdbool.h>
#include <stdint.h>
#include "env_sensors.h"

enum { BMP5_ADDRESS = 0x46, BMP5_CHIP_ID = 0x50 };
// The operating range, Table 1: 30-125 kPa and -40..85 C.
enum { BMP5_MIN_PA = 30000, BMP5_MAX_PA = 125000, BMP5_MIN_CENTI_C = -4000, BMP5_MAX_CENTI_C = 8500 };
typedef enum {
    BMP5_ABSENT = 0,   // no answer at 0x46
    BMP5_REJECTED = 1, // it answered, but not as a BMP5 that finished its reset
    BMP5_READY = 2,
} bmp5_state_t;

// Soft reset, then section 4.3.9's checks: CHIP_ID 0x50, the NVM ready without error, and the
// reset reported in INT_STATUS. Then the measurement configuration, read back. The reset
// command is not acknowledged over I2C (section 7.36), so its result is not relied on; the
// checks are. Retrying after a failure is safe.
bmp5_state_t bmp5_init(const env_io_t *io);
// Section 4.5's scaling of a burst read from TEMP_DATA_XLSB: temperature is the signed 24-bit
// value / 2^16 C and pressure the unsigned 24-bit value / 2^6 Pa, both rounded half away from
// zero. False outside the operating range, which includes the 0x7f7f7f of unwritten registers.
bool bmp5_convert(const uint8_t raw[6], int32_t *centi_c, int32_t *pa);
// One forced conversion, waited for through INT_STATUS data ready; requires BMP5_READY.
bool bmp5_read(const env_io_t *io, int32_t *centi_c, int32_t *pa);
#endif
