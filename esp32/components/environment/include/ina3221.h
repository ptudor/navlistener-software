#ifndef NVF_INA3221_H
#define NVF_INA3221_H
// TI INA3221 three-channel shunt and bus-voltage monitor (the ZED/X20's U37 at I2C 0x41),
// SBOS576C. Continuous shunt and bus conversions of 1.1 ms each, averaged over 64: each
// result register holds a mean over one 0.42 s cycle of all three channels. The board
// supplies the shunt resistances; current is shunt voltage / resistance (uV / mOhm = mA).
// The Critical and Warning alert outputs, wired together to the board's PWR_ALERT_N, latch
// once limits are set: Critical compares each conversion, Warning the averaged value.
#include <stdbool.h>
#include <stdint.h>
#include "env_sensors.h"

enum { INA3221_ADDRESS = 0x41, INA3221_CHANNELS = 3 };
enum { INA3221_MANUFACTURER_ID = 0x5449, INA3221_DIE_ID = 0x3220 };
// The Mask/Enable register (0x0F), Tables 8-33 and 8-34. Reading it clears CF1-3, SF and
// WF1-3 (and CVRF); writing it does not.
enum {
    INA3221_MASK_CVRF = 0x0001, INA3221_MASK_TCF = 0x0002, INA3221_MASK_PVF = 0x0004,
    INA3221_MASK_WF = 0x0038,   // WF1 bit 5, WF2 bit 4, WF3 bit 3
    INA3221_MASK_SF = 0x0040,
    INA3221_MASK_CF = 0x0380,   // CF1 bit 9, CF2 bit 8, CF3 bit 7
    INA3221_MASK_CEN = 0x0400, INA3221_MASK_WEN = 0x0800,
    INA3221_MASK_SCC = 0x7000,  // SCC1 bit 14, SCC2 bit 13, SCC3 bit 12
    INA3221_MASK_ALERTS = INA3221_MASK_CF | INA3221_MASK_SF | INA3221_MASK_WF,
};
typedef struct {
    int32_t bus_mv[INA3221_CHANNELS];   // IN- to ground, 8 mV steps
    int32_t shunt_uv[INA3221_CHANNELS]; // IN+ to IN-, 40 uV steps, signed
    uint16_t alert_flags;               // CF, SF and WF bits the Mask/Enable reads cleared
} ina3221_sample_t;
// Mask/Enable decoded. Channel bits: bit 0 is channel 1, bit 2 channel 3.
typedef struct {
    uint16_t mask_enable;               // the register value decoded
    bool critical_latch, warning_latch; // CEN, WEN: the alert holds until Mask/Enable is read
    uint8_t critical, warning;          // CF1-3, WF1-3: the channels whose limit was exceeded
    bool summation;                     // SF: the shunt-voltage sum exceeded its limit
    bool power_valid, timing_control, conversion_ready; // PVF, TCF, CVRF
} ina3221_alerts_t;

// The manufacturer and die IDs (Table 3), then the configuration, read back.
bool ina3221_init(const env_io_t *io);
// The results of a completed averaging cycle, waiting up to one cycle for the first after
// configuration. False on a transfer failure, no completed cycle, or a configuration that no
// longer reads back, as after a 3V3_SENS power cycle; the caller then initializes it again.
// Its Mask/Enable reads clear the alert flags, so sample->alert_flags holds the ones they
// returned even when it fails.
bool ina3221_read(const env_io_t *io, ina3221_sample_t *sample);
// A shunt or bus register's two's-complement value over its three reserved low bits.
int32_t ina3221_shunt_uv(uint16_t reg);
int32_t ina3221_bus_mv(uint16_t reg);

// A limit register value in the shunt-voltage format: 40 uV steps in bits 15-3, two's
// complement, the low three bits zero; rounded to the nearest step and clamped to the
// register's range, -163.84 mV (0x8000) to 163.80 mV (0x7FF8).
uint16_t ina3221_limit(int32_t uv);
// Writes each channel's Critical-Alert Limit (0x07, 0x09, 0x0B) and Warning-Alert Limit (0x08,
// 0x0A, 0x0C) from crit_uv and warn_uv, leaving a channel whose limit is 0 as it is, then sets
// CEN and WEN in Mask/Enable so either alert latches until Mask/Enable is read. The reserved
// bit and SCC1-3 are kept as read; the flags are status, which a write does not change, and
// are written as 0. Reads every written limit and Mask/Enable back. *cleared gains the alert
// flags its Mask/Enable reads returned. False on a transfer failure or a readback mismatch.
bool ina3221_set_alerts(const env_io_t *io, const int32_t warn_uv[INA3221_CHANNELS],
                        const int32_t crit_uv[INA3221_CHANNELS], uint16_t *cleared);
// Reads Mask/Enable once, which clears CF, SF and WF and so releases a latched alert, and
// decodes it.
bool ina3221_read_alerts(const env_io_t *io, ina3221_alerts_t *out);
void ina3221_decode_alerts(uint16_t mask_enable, ina3221_alerts_t *out);
#endif
