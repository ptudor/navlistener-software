#ifndef OBSERVER_REPORT_H
#define OBSERVER_REPORT_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "timing_report.h"
#include "../../../../common/board_uid.h"
// GNF1 ObserverDetails v1; byte layout is specified in docs/OBSERVER-TELEMETRY.md.
#define OBSERVER_REPORT_MAX 512
enum { REPORT_BOOT=1, REPORT_CHANGE=2, REPORT_CHECKIN=4, REPORT_INTERFERENCE=8 };
typedef struct {
    uint8_t valid, ready; // bits 0 MCP9808, 1 HDC2080/HDC2022, 2 BMP388/BMP384
    int16_t mcp_centi_c, hdc_centi_c, bmp_centi_c;
    uint16_t rh_centi_percent;
    uint32_t pressure_pa;
} report_environment_t;
// HDC humidity-heater condensation recovery; tag 10 in docs/OBSERVER-TELEMETRY.md.
typedef struct {
    bool present; // the HDC is ready and the policy runs
    uint8_t state, flags, runs, stop, valid; // state 0 normal, 1 heating, 2 stopping, 3 recovering
    uint32_t rh95_s, rh98_s, streak_s, on_ms, recovery_ms;
    uint64_t last_utc, start_ms;
    uint16_t rh_before, rh_stop;
    int16_t hdc_before, hdc_peak, hdc_end, mcp_before, mcp_peak, mcp_end;
} report_heater_t;
// The MAX board's MS5607 barometer; tag 11. Invalid measurements stay zero.
typedef struct {
    bool present;          // this board carries the part
    uint8_t state;         // 0 absent, 1 calibration PROM rejected, 2 ready
    uint8_t valid;         // 1: temperature and pressure measured
    uint8_t flags;         // 1: outside the 300-1100 mbar full-accuracy range
    int16_t centi_c;
    uint32_t pressure_pa;
} report_barometer_t;
// The MAX board's MAX31856 thermocouple converter; tag 12.
typedef struct {
    bool present;
    uint8_t state;         // 0 not responding, 1 configured
    uint8_t valid;         // 1 thermocouple, 2 cold junction
    uint8_t flags;         // 1: a new conversion (DRDY_N was low)
    uint8_t fault;         // the fault status register
    uint8_t config;        // bits 3:0 thermocouple type (3 = K), bit 4 the 50 Hz notch
    int32_t tc_centi_c;
    int16_t cj_centi_c;
} report_thermocouple_t;
// The MAX board's ICM-45686 IMU and MMC34160PJ magnetometer; tag 13. Vectors are raw
// counts in each sensor's axes; the ranges carried beside them give the scale.
typedef struct {
    bool present;
    uint8_t imu_state, mag_state; // 0 not responding, 1 ready
    uint8_t valid;                // 1 latest IMU sample, 2 IMU window, 4 magnetometer
    uint8_t profile;              // 0 surface, 1 aerial
    uint8_t moving;               // the governor's moving rate is in force
    uint16_t rate_decihz, gyro_fs_dps;
    uint8_t accel_fs_g;
    int16_t accel[3], gyro[3], imu_centi_c;
    uint32_t packets, overflows, resyncs, rate_changes;
    uint32_t window_samples, window_ms;   // valid samples since the previous queued report
    int16_t accel_mean[3], gyro_mean[3]; // their time-weighted means
    uint16_t accel_min_mg, accel_max_mg, gyro_max_decidps;
    uint64_t imu_ms;
    int16_t mag[3];               // 1/2048 G per count, bridge offset removed
    uint16_t mag_offset[3];
    uint64_t mag_ms;
} report_motion_t;
// The humidity sensor the manifest lists at 0x40; tag 14. HDC2080 and HDC2022 share ID
// registers, so this is what chose the temperature formula, or why none was measured.
enum { HUMIDITY_NOT_LISTED = 0, HUMIDITY_HDC2080 = 1, HUMIDITY_HDC2022 = 2, HUMIDITY_CONFLICT = 3 };
typedef struct { bool present; uint8_t part; } report_humidity_t;
typedef struct { uint8_t flags; uint64_t epoch, sampled_ms; } report_rtc_t;
typedef struct {
    uint64_t checked_ms;
    uint8_t revision_valid, revision[4], config_lock, data_lock, rng;
} report_crypto_t;
// uid_valid: the manifest EEPROM's 128-bit factory serial was read; board_uid is then its
// typed wire form (docs/BOARD-IDENTITY.md), otherwise zero.
typedef struct {
    uint8_t action, uid_valid, board_uid[NVF_BOARD_UID_SIZE], capabilities_valid, revision, component_count;
} report_manifest_t;
typedef struct {
    uint8_t psram;
    uint32_t used, capacity, queued;
    uint64_t dropped;
    uint32_t internal_free, psram_free;
} report_resources_t;
typedef struct {
    uint8_t supported, expected, tracked[8], valid, jam, spoof;
    uint64_t rf_ms, status_ms;
} report_receiver_t;
typedef struct {
    uint64_t uptime_ms, event_ms;
    uint32_t event_count;
    uint8_t reason, event_flags, event_states;
    report_environment_t environment;
    report_heater_t heater;
    report_barometer_t barometer;
    report_thermocouple_t thermocouple;
    report_motion_t motion;
    report_humidity_t humidity;
    report_rtc_t rtc;
    report_crypto_t crypto;
    report_manifest_t manifest;
    report_resources_t resources;
    report_receiver_t receiver;
    char firmware[33];
    report_timing_t timing; // separately paced; omitted by the environmental encoder
} observer_report_t;
typedef struct { bool sent; uint64_t sent_ms; observer_report_t last; } report_policy_t;
// Compare with the last successfully queued report, so slow drift accumulates.
uint8_t observer_report_due(const report_policy_t *p, const observer_report_t *r);
void observer_report_sent(report_policy_t *p, const observer_report_t *r);
size_t observer_report_encode(uint8_t *out, size_t cap, const observer_report_t *r);
size_t observer_timing_encode(uint8_t *out, size_t cap, const report_timing_t *r, uint64_t uptime_ms);
#endif
