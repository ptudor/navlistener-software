#ifndef NVF_INA3221_H
#define NVF_INA3221_H
// TI INA3221 three-channel shunt and bus-voltage monitor (the ZED/X20's U37 at I2C 0x41),
// SBOS576B. Continuous shunt and bus conversions of 1.1 ms each, averaged over 64: each
// result register holds a mean over one 0.42 s cycle of all three channels. The alert
// outputs are not used. The board supplies the shunt resistances; current is
// shunt voltage / resistance (uV / mOhm = mA).
#include <stdbool.h>
#include <stdint.h>
#include "env_sensors.h"

enum { INA3221_ADDRESS = 0x41, INA3221_CHANNELS = 3 };
enum { INA3221_MANUFACTURER_ID = 0x5449, INA3221_DIE_ID = 0x3220 };
typedef struct {
    int32_t bus_mv[INA3221_CHANNELS];   // IN- to ground, 8 mV steps
    int32_t shunt_uv[INA3221_CHANNELS]; // IN+ to IN-, 40 uV steps, signed
} ina3221_sample_t;

// The manufacturer and die IDs (Table 3), then the configuration, read back.
bool ina3221_init(const env_io_t *io);
// The results of a completed averaging cycle, waiting up to one cycle for the first after
// configuration. False on a transfer failure, no completed cycle, or a configuration that no
// longer reads back, as after a 3V3_SENS power cycle; the caller then initializes it again.
bool ina3221_read(const env_io_t *io, ina3221_sample_t *sample);
// A shunt or bus register's two's-complement value over its three reserved low bits.
int32_t ina3221_shunt_uv(uint16_t reg);
int32_t ina3221_bus_mv(uint16_t reg);
#endif
