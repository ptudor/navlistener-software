#pragma once
#include "../../../../common/network_health.h"

// Short critical sections only. No driver calls, allocation, logging or flash
// writes here; the event loop and pusher may call these without blocking on I/O.
void network_health_configure(bool wifi, bool ethernet);
void network_health_event(uint8_t kind, uint8_t interface, int32_t code, int32_t detail, uint64_t now);
void network_health_read(nh_report_t *out);
// Discard a driver sample if a link event raced its collection.
bool network_health_sample(const nh_report_t *sample, uint32_t generation, uint64_t now);
void network_health_io(uint64_t written, uint64_t read, uint64_t records);
void network_health_ack(uint64_t now, uint32_t gap_ms);
void network_health_path(uint8_t interface, const uint8_t peer[4]);
void network_health_spool(uint32_t queued, uint64_t dropped);
typedef struct { bool sent; uint64_t sent_ms; uint32_t events; int8_t rssi; uint16_t valid; } nh_policy_t;
bool network_health_due(const nh_policy_t *p, const nh_report_t *r, uint64_t now);
void network_health_sent(nh_policy_t *p, const nh_report_t *r, uint64_t now);
