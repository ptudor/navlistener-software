#ifndef NVF_ENVIRONMENT_H
#define NVF_ENVIRONMENT_H
#include <stdbool.h>
#include <stdint.h>
#include "env_heater.h"
#include "env_sensors.h"
#include "mmc34160.h"
#include "ms5607.h"
#include "driver/i2c_master.h"
// Call from the board task, which owns sensor/RTC transactions. utc_valid means utc is time the
// firmware trusts (GNSS-qualified or a validated running RTC), never SNTP. HDC values are
// withheld (invalid) while the humidity heater runs or recovers; heater->active is false
// without an HDC sensor.
void environment_sample(i2c_master_bus_handle_t bus, int64_t now_ms, bool utc_valid, int64_t utc,
                        env_sample_t *sample, uint8_t *ready, env_heater_status_t *heater);
// Call on every board-task pass: converts at the heater step cadence while the heater is on,
// and retries clearing HEAT_EN until the sensor confirms it is off. Idle otherwise.
void environment_poll(int64_t now_ms);

// The MAX board's MS5607 barometer and MMC34160PJ magnetometer, sampled from the board
// task after environment_sample. A part that is absent or rejected at one sample is
// identified again at the next.
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
