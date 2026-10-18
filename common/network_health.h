#ifndef NVF_NETWORK_HEALTH_H
#define NVF_NETWORK_HEALTH_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

// ObserverDetails tag 22. All integers on the wire are big endian. Counters and
// event identities belong to the GNF1 boot session; timestamps are boot uptime.
#define NH_TAG 22
#define NH_EVENTS 8
#define NH_FIXED_SIZE 256
#define NH_EVENT_SIZE 24
#define NH_WIRE_SIZE (NH_FIXED_SIZE + NH_EVENTS * NH_EVENT_SIZE)
enum { NH_WIFI_CONFIGURED=1, NH_ETH_PRESENT=2, NH_WIFI_UP=4, NH_WIFI_IP=8,
       NH_ETH_UP=16, NH_ETH_IP=32, NH_CONNECTED=64, NH_TUNNEL=128 };
enum { NH_UNKNOWN, NH_WIFI, NH_ETHERNET, NH_VPN, NH_OTHER };
enum { NH_ASSOCIATED=1, NH_WIFI_DISCONNECTED, NH_WIFI_ADDRESS, NH_WIFI_LOST_IP,
       NH_ETH_CONNECTED, NH_ETH_DISCONNECTED, NH_ETH_ADDRESS, NH_ETH_LOST_IP,
       NH_ROUTE_CHANGED, NH_AP_CHANGED, NH_ETH_MODE_CHANGED, NH_CONNECT_ATTEMPT,
       NH_COLLECTOR_CONNECTED, NH_DNS_FAILED, NH_TCP_FAILED, NH_TLS_FAILED,
       NH_HANDSHAKE_FAILED, NH_AUTH_REJECTED, NH_READ_FAILED, NH_WRITE_FAILED,
       NH_ACK_STALLED, NH_PATH_SWITCH, NH_COLLECTOR_CLOSED, NH_ETH_FAILED,
       NH_ADDRESS_CHANGED };
enum { NH_RSSI_VALID=1, NH_MIN_RSSI_VALID=2, NH_WIFI_IP_VALID=4,
       NH_ETH_IP_VALID=8, NH_DNS_VALID=16 };
enum { NH_WIFI_DROPS, NH_WIFI_JOINS, NH_ETH_LINK_UPS, NH_ETH_LINK_DOWNS,
       NH_WIFI_LEASES, NH_ETH_LEASES, NH_IP_LOSSES, NH_ATTEMPTS, NH_CONNECTS,
       NH_DISCONNECTS, NH_DNS_ERRORS, NH_TCP_ERRORS, NH_TLS_ERRORS,
       NH_HANDSHAKE_ERRORS, NH_ACK_STALLS, NH_READ_ERRORS, NH_WRITE_ERRORS,
       NH_COUNTERS };
typedef struct {
    uint64_t uptime_ms;
    uint8_t kind, interface, flags;
    int8_t rssi;
    int32_t code, detail;
    uint32_t sequence;
} nh_event_t;
typedef struct {
    uint8_t flags, default_interface, socket_interface;
    int8_t rssi, min_rssi;
    uint8_t channel, secondary_channel, duplex, autoneg, bssid[6];
    uint16_t speed_mbps, valid;
    uint64_t sampled_ms;
    uint32_t event_total, events_overwritten, counters[NH_COUNTERS];
    uint16_t wifi_reason;
    uint64_t wifi_disconnect_ms;
    uint8_t error_kind, error_interface;
    int32_t error_code, error_detail;
    uint64_t error_ms, connected_ms, ack_ms;
    uint32_t max_ack_gap_ms, connect_ms, max_connect_ms;
    uint64_t tx_bytes, rx_bytes, sent_records;
    uint8_t wifi_ipv4[12], eth_ipv4[12], dns_ipv4[8], peer_ipv4[4];
    uint32_t max_spool_records, spool_records;
    uint64_t spool_dropped;
    uint32_t weak_samples, rssi_samples, sample_failures;
    uint8_t event_count;
    nh_event_t events[NH_EVENTS]; // oldest first
} nh_report_t;

static inline void nh_put(uint8_t *p, uint64_t v, unsigned n)
{ for (unsigned i=0;i<n;i++) { p[n-1-i]=(uint8_t)v; v>>=8; } }
static inline size_t nh_encode(uint8_t *b, size_t cap, const nh_report_t *r)
{
    if (!b || !r || cap<NH_WIRE_SIZE || r->event_count>NH_EVENTS) return 0;
    for (size_t i=0;i<NH_WIRE_SIZE;i++) b[i]=0;
    b[0]=1;b[1]=r->flags;b[2]=r->default_interface;b[3]=r->socket_interface;
    b[4]=(uint8_t)r->rssi;b[5]=(uint8_t)r->min_rssi;b[6]=r->channel;b[7]=r->secondary_channel;
    nh_put(b+8,r->speed_mbps,2);b[10]=r->duplex;b[11]=r->autoneg;
    for(unsigned i=0;i<6;i++)b[12+i]=r->bssid[i];
    nh_put(b+18,r->valid,2);nh_put(b+20,r->sampled_ms,8);
    nh_put(b+28,r->event_total,4);nh_put(b+32,r->events_overwritten,4);
    for(unsigned i=0;i<NH_COUNTERS;i++)nh_put(b+36+4*i,r->counters[i],4);
    nh_put(b+104,r->wifi_reason,2);nh_put(b+108,r->wifi_disconnect_ms,8);
    b[116]=r->error_kind;b[117]=r->error_interface;
    nh_put(b+120,(uint32_t)r->error_code,4);nh_put(b+124,(uint32_t)r->error_detail,4);
    nh_put(b+128,r->error_ms,8);nh_put(b+136,r->connected_ms,8);nh_put(b+144,r->ack_ms,8);
    nh_put(b+152,r->max_ack_gap_ms,4);nh_put(b+156,r->connect_ms,4);nh_put(b+160,r->max_connect_ms,4);
    nh_put(b+164,r->tx_bytes,8);nh_put(b+172,r->rx_bytes,8);nh_put(b+180,r->sent_records,8);
    for(unsigned i=0;i<12;i++){b[188+i]=r->wifi_ipv4[i];b[200+i]=r->eth_ipv4[i];}
    for(unsigned i=0;i<8;i++)b[212+i]=r->dns_ipv4[i];
    for(unsigned i=0;i<4;i++)b[220+i]=r->peer_ipv4[i];
    nh_put(b+224,r->max_spool_records,4);nh_put(b+228,r->spool_dropped,8);nh_put(b+236,r->spool_records,4);
    nh_put(b+240,r->weak_samples,4);nh_put(b+244,r->rssi_samples,4);nh_put(b+248,r->sample_failures,4);
    b[252]=r->event_count;
    for(unsigned i=0;i<r->event_count;i++) {
        uint8_t *p=b+NH_FIXED_SIZE+NH_EVENT_SIZE*i;const nh_event_t *e=&r->events[i];
        nh_put(p,e->uptime_ms,8);p[8]=e->kind;p[9]=e->interface;p[10]=(uint8_t)e->rssi;p[11]=e->flags;
        nh_put(p+12,(uint32_t)e->code,4);nh_put(p+16,(uint32_t)e->detail,4);nh_put(p+20,e->sequence,4);
    }
    return NH_WIRE_SIZE;
}
#endif
