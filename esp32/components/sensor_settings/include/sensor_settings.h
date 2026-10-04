#ifndef NVF_SENSOR_SETTINGS_H
#define NVF_SENSOR_SETTINGS_H
// Per-unit sensor settings. They describe where a unit is deployed and how it moves,
// not its firmware image or its assembly, so one image serves every unit of a board.
// The setup page writes them to the `nvf_sensor` NVS namespace, which the network
// configuration reset leaves alone, and telemetry reports the values in use.
#include <stdbool.h>
#include <stdint.h>
#include "esp_err.h"

// The IMU samples at 12.5 Hz while still and steps up while moving: to 50 Hz for
// vehicles and vessels, to 100 Hz with wider ranges for aircraft and drones.
typedef enum {
    SENSOR_MOTION_SURFACE = 0,
    SENSOR_MOTION_AERIAL = 1,
} sensor_motion_t;

typedef struct {
    uint8_t mains_hz;       // 50 or 60: the thermocouple converter's notch
    sensor_motion_t motion; // the IMU's moving rate and ranges
} sensor_settings_t;

#define SENSOR_SETTINGS_DEFAULT ((sensor_settings_t){.mains_hz = 60, .motion = SENSOR_MOTION_SURFACE})

bool sensor_settings_valid(const sensor_settings_t *settings);
// Setup-page values: "50" or "60"; "surface" or "aerial".
bool sensor_settings_parse_mains(const char *text, uint8_t *hz);
bool sensor_settings_parse_motion(const char *text, sensor_motion_t *motion);
const char *sensor_motion_name(sensor_motion_t motion);
// Missing settings are the defaults and loading never writes flash. A stored value
// out of range returns ESP_ERR_INVALID_ARG; any error leaves the defaults in out.
esp_err_t sensor_settings_load(sensor_settings_t *out);
// Both values in one commit.
esp_err_t sensor_settings_save(const sensor_settings_t *settings);
#endif
