#ifndef GNSS_STATUS_H
#define GNSS_STATUS_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#define GNSS_REGIONAL_MASK ((1u << 1) | (1u << 5) | (1u << 7))
typedef struct {
    uint8_t supported, tracked[8];
    bool satellites_valid, fix_valid;
    int32_t latitude, longitude; // degrees * 1e7, from a valid NAV-PVT fix
    int64_t satellites_ms, fix_ms;
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
#endif
