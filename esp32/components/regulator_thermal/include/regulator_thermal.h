#ifndef NVF_REGULATOR_THERMAL_H
#define NVF_REGULATOR_THERMAL_H
// Wi-Fi throttling from the system regulator's heat-spreader temperature (the P3's MCP9808
// U47 beside the LDL1117's tab). The LDO dissipates (VIN - 3.3 V) times the 3V3_SYS
// current, and Wi-Fi transmission dominates that current, so while the spreader is hot the
// throttle lowers the radio's maximum transmit power a step at a time and raises it again
// once the spreader has cooled. The reading is the regulator's, not the air's: it raises no
// alarm and stays out of the environmental report's temperatures.
//
// Pure policy: the board samples the sensor every few seconds, feeds each result here and
// applies regulator_thermal_tx_cap() to the radio. A failed read never relaxes the throttle:
// without a reading there is no evidence the spreader has cooled.
#include <stdbool.h>
#include <stdint.h>

enum {
    REGULATOR_THERMAL_LEVELS = 5,              // 0 unthrottled; 4 is the lowest transmit cap
    REGULATOR_THERMAL_STEP_UP_MS = 30000,      // time at a level before the next hotter one
    REGULATOR_THERMAL_STEP_DOWN_MS = 120000,   // time at a level before the next cooler one
    REGULATOR_THERMAL_LOST_MS = 60000,         // no valid reading for this long: not responding
};
// The board's limits, in 0.01 C at the sensor. They must satisfy release < throttle <= full.
typedef struct {
    int16_t throttle_centi_c;   // at or above: one level hotter every STEP_UP_MS
    int16_t full_centi_c;       // at or above: straight to the last level
    int16_t release_centi_c;    // at or below: one level cooler every STEP_DOWN_MS
} regulator_thermal_limits_t;
typedef struct {
    regulator_thermal_limits_t limits;
    uint8_t level;
    bool seen;                  // a valid reading since boot
    bool peak_valid;
    int16_t centi_c;            // the latest valid reading
    int16_t peak_centi_c;       // the highest since regulator_thermal_report_sent
    int64_t sample_ms;          // when centi_c was read
    int64_t changed_ms;         // when the level last changed
    int64_t accounted_ms;       // throttled_ms counts up to here
    uint32_t changes;           // level changes since boot
    uint64_t throttled_ms;      // time spent above level 0 since boot
} regulator_thermal_t;

// Unthrottled, with no reading yet; false, leaving the state unusable, for disordered limits.
bool regulator_thermal_init(regulator_thermal_t *t, const regulator_thermal_limits_t *limits, int64_t now_ms);
// One sample: a valid reading, or a failed read (valid false). True when the level changed.
bool regulator_thermal_sample(regulator_thermal_t *t, int64_t now_ms, bool valid, int16_t centi_c);
// A valid reading within REGULATOR_THERMAL_LOST_MS.
bool regulator_thermal_responding(const regulator_thermal_t *t, int64_t now_ms);
// The radio's maximum transmit power at a level, in ESP-IDF's 0.25 dBm units: 16.5, 13, 11
// and 8.5 dBm, each a step of esp_wifi_set_max_tx_power()'s mapping table. Level 0 returns
// 0, which leaves the radio's own maximum (its PHY and country limits) in force.
int8_t regulator_thermal_tx_cap(uint8_t level);
// The report queued: the next peak starts from the latest reading.
void regulator_thermal_report_sent(regulator_thermal_t *t);
#endif
