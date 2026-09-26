#ifndef NVF_ENVIRONMENT_H
#define NVF_ENVIRONMENT_H
#include <stdbool.h>
#include <stdint.h>
#include "env_heater.h"
#include "env_sensors.h"
#include "mmc34160.h"
#include "ms5607.h"
#include "driver/i2c_master.h"
// The parts the manifest lists; call before the first sample. Nothing unlisted is probed
// or measured. hdc is the variant listed at 0x40, or ENV_HDC_NONE, which measures no
// humidity (see env_hdc_heater_off_unlisted).
typedef struct {
    bool mcp9808, bmp388, ms5607, mmc34160;
    env_hdc_variant_t hdc;
} env_parts_t;
void environment_configure(const env_parts_t *parts);
// Call from the board task, which owns sensor/RTC transactions. utc_valid means utc is time the
// firmware trusts (GNSS-qualified or a validated running RTC), never SNTP. HDC values are
// withheld (invalid) while the humidity heater runs or recovers; heater->active is false
// without an HDC sensor.
void environment_sample(i2c_master_bus_handle_t bus, int64_t now_ms, bool utc_valid, int64_t utc,
                        env_sample_t *sample, uint8_t *ready, env_heater_status_t *heater);
// Call on every board-task pass: converts at the heater step cadence while the heater is on,
// and retries clearing HEAT_EN until the sensor confirms it is off. Idle otherwise.
void environment_poll(int64_t now_ms);

// The MS5607 barometer and MMC34160PJ magnetometer (the MAX's), when listed, sampled
// from the board task after environment_sample. A listed part that is absent or rejected
// at one sample is identified again at the next.
typedef struct {
    ms5607_state_t state;
    bool valid;
    int32_t centi_c, pressure_pa;
} env_barometer_t;
typedef struct {
    bool ready, valid;
    mmc34160_sample_t sample;
} env_magnetometer_t;
void environment_sample_max(env_barometer_t *barometer, env_magnetometer_t *magnetometer);
#endif
