#ifndef GNSS_STATUS_H
#define GNSS_STATUS_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "../../../../common/reception.h"
#define GNSS_OBS_MAX 200u
typedef struct {
    uint8_t gnss,sv,signal,cno;
    int8_t elevation; // NAV-SAT degrees; 127 when a signal has no fresh satellite geometry
} gnss_observation_t;
#define GNSS_REGIONAL_MASK ((1u << 1) | (1u << 5) | (1u << 7))
// One UBX-MON-RF block, one per band the receiver supports; which band a block ID stands for
// is the product's configuration. The antenna supervisor: 0 INIT, 1 DONTKNOW, 2 OK, 3 SHORT,
// 4 OPEN; its power: 0 OFF, 1 ON, 2 DONTKNOW. agc runs 0..8191 for 0..100 % of maximum gain.
#define GNSS_RF_BLOCKS_MAX 8u
typedef struct {
    uint8_t id, ant_status, ant_power, cw_suppression; // CW suppression 0 none .. 255 strong
    uint16_t noise_per_ms, agc;
} gnss_rf_block_t;
// The named signals of the u-blox signal identifier tables (X20 HPG 2.11 Table 4), each
// with its data/pilot halves as one, and the band it is received in.
enum { GNSS_BAND_L1, GNSS_BAND_L2, GNSS_BAND_L5, GNSS_BAND_E5B, GNSS_BAND_E6, GNSS_BANDS };
typedef struct { uint8_t gnss, band; const char *name; } gnss_signal_kind_t;
#define GNSS_SIGNAL_KINDS 21u
extern const gnss_signal_kind_t gnss_signal_kinds[GNSS_SIGNAL_KINDS];
extern const char *const gnss_band_names[GNSS_BANDS];
// The kind a NAV-SIG gnssId and sigId belong to, or -1 for a signal the tables do not name.
int gnss_signal_kind(uint8_t gnss, uint8_t sig_id);
typedef struct {
    uint8_t supported, tracked[8];
    bool satellites_valid, fix_valid;
    int32_t latitude, longitude; // degrees * 1e7, from a valid NAV-PVT fix
    int64_t satellites_ms, fix_ms;
    bool signals_valid;
    int64_t signals_ms;
    uint16_t satellite_count, signal_count;
    gnss_observation_t satellites[GNSS_OBS_MAX], signals[GNSS_OBS_MAX];
    // Code-locked signals with C/N0 in the latest NAV-SIG, one per satellite and kind, the
    // unhealthy ones (such as pre-operational GPS L5) included and counted again apart.
    uint8_t signal_locked[GNSS_SIGNAL_KINDS], signal_unhealthy[GNSS_SIGNAL_KINDS], signal_other;
    bool rf_blocks_valid;
    uint8_t rf_blocks;
    gnss_rf_block_t rf_block[GNSS_RF_BLOCKS_MAX];
    int64_t rf_blocks_ms;
    // jam is MON-RF's flag until the receiver sends UBX-SEC-SIG (version 2 or 3), which
    // replaces it: from the ZED-X20P's HPG 2.00 on, MON-RF reports 0 there. Until SEC-SIG
    // first reports a state, its unknown readings set no baseline.
    bool sec_sig, sec_sig_known;
    bool rf_valid, status_valid;
    uint8_t jam, spoof, event_flags, event_states;
    uint32_t event_count;
    int64_t rf_ms, status_ms, event_ms;
    uint8_t pvt_utc[20]; // NAV-PVT bytes 4..23; RTC validates UTC independently
    bool tp_valid;
    uint8_t tp_flags, tp_ref;
    int64_t tp_ms; // TIM-TP describes the NEXT pulse, not an associated capture
} gnss_status_t;
void gnss_status_feed(gnss_status_t *s, uint8_t cls, uint8_t id,
                      const uint8_t *body, size_t len, int64_t now_ms);
bool gnss_status_position_fresh(const gnss_status_t *s, int64_t now_ms);
bool gnss_status_same_place(int32_t lat_a, int32_t lon_a, int32_t lat_b, int32_t lon_b);
uint8_t gnss_status_expected(const gnss_status_t *s, int64_t now_ms, uint8_t learned);
void gnss_status_leds(const gnss_status_t *s, int64_t now_ms, uint8_t learned,
                      bool uplink, uint8_t *green, uint8_t *yellow);
// Build fresh match evidence; an incomplete report invalidates its comparison.
void gnss_status_compare(const gnss_status_t *s, const nr_expectation_t *e,
                         nr_sample_t *sample, int64_t now_ms);
#endif
