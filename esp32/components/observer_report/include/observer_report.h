#ifndef OBSERVER_REPORT_H
#define OBSERVER_REPORT_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
// GNF1 ObserverDetails v1; byte layout is specified in docs/OBSERVER-TELEMETRY.md.
#define OBSERVER_REPORT_MAX 256
enum { REPORT_BOOT=1, REPORT_CHANGE=2, REPORT_CHECKIN=4, REPORT_INTERFERENCE=8 };
typedef struct {
    uint8_t valid, ready; // bits 0 MCP9808, 1 HDC2080, 2 BMP388/BMP384
    int16_t mcp_centi_c, hdc_centi_c, bmp_centi_c;
    uint16_t rh_centi_percent;
    uint32_t pressure_pa;
} report_environment_t;
typedef struct { uint8_t flags; uint64_t epoch, sampled_ms; } report_rtc_t;
typedef struct {
    uint64_t checked_ms;
    uint8_t revision_valid, revision[4], config_lock, data_lock, rng;
} report_crypto_t;
typedef struct {
    uint8_t action, eui_valid, eui[8], capabilities_valid, revision, component_count;
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
    report_rtc_t rtc;
    report_crypto_t crypto;
    report_manifest_t manifest;
    report_resources_t resources;
    report_receiver_t receiver;
    char firmware[33];
} observer_report_t;
typedef struct { bool sent; uint64_t sent_ms; observer_report_t last; } report_policy_t;
// Compare with the last successfully queued report, so slow drift accumulates.
uint8_t observer_report_due(const report_policy_t *p, const observer_report_t *r);
void observer_report_sent(report_policy_t *p, const observer_report_t *r);
size_t observer_report_encode(uint8_t *out, size_t cap, const observer_report_t *r);
#endif
