#ifndef NVF_ENVIRONMENT_H
#define NVF_ENVIRONMENT_H
#include <stdbool.h>
#include <stdint.h>
#include "env_heater.h"
#include "bmp5.h"
#include "env_sensors.h"
#include "ina3221.h"
#include "mmc34160.h"
#include "ms5607.h"
#include "driver/i2c_master.h"
// The BMP5 part the manifest lists at 0x46. BMP580 and BMP581 share their register map and
// CHIP_ID, so this only names it.
typedef enum { ENV_BMP5_NONE = 0, ENV_BMP580, ENV_BMP581 } env_bmp5_t;
// The parts the manifest lists; call before the first sample. Nothing unlisted is probed
// or measured. hdc is the variant listed at 0x40, or ENV_HDC_NONE, which measures no
// humidity (see env_hdc_heater_off_unlisted). The BMP388 and a BMP5 fill the same
// barometer slot of the sample, so at most one of them is listed.
typedef struct {
    bool mcp9808, bmp388, ms5607, mmc34160, ina3221;
    env_bmp5_t bmp5;
    env_hdc_variant_t hdc;
    // The INA3221's alert limits per channel in shunt microvolts, from the board's rails;
    // 0 leaves that limit unset (ina3221_set_alerts).
    int32_t rail_warn_uv[INA3221_CHANNELS], rail_crit_uv[INA3221_CHANNELS];
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

// The INA3221 rail monitor (the ZED/X20's), when listed, sampled from the board task after
// environment_sample. A part that is absent, or whose configuration was lost to a 3V3_SENS
// power cycle, is configured again at the next sample, and its alert limits with it.
typedef struct {
    bool ready, valid;
    ina3221_sample_t sample;
    // The alert flags (CF, SF, WF) this sample's Mask/Enable reads cleared. A flag is
    // reported once: here, or by environment_rail_alerts if it reads it first.
    uint16_t alert_flags;
} env_rails_t;
void environment_sample_rails(env_rails_t *rails);
// Reads and decodes the INA3221's Mask/Enable, which releases a latched alert; from the
// board task, as on the PWR_ALERT_N interrupt. False when the monitor is not listed, not
// configured, or does not answer.
bool environment_rail_alerts(ina3221_alerts_t *alerts);
#endif
