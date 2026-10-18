#include "network_health.h"
#include <limits.h>
#include <string.h>
#ifdef ESP_PLATFORM
#include "freertos/FreeRTOS.h"
static portMUX_TYPE mux=portMUX_INITIALIZER_UNLOCKED;
#define LOCK() portENTER_CRITICAL(&mux)
#define UNLOCK() portEXIT_CRITICAL(&mux)
#else
#include <stdatomic.h>
static atomic_flag mux=ATOMIC_FLAG_INIT;
#define LOCK() while(atomic_flag_test_and_set(&mux)) {}
#define UNLOCK() atomic_flag_clear(&mux)
#endif
static nh_report_t state;
static void increment(uint32_t *v) { if(*v<UINT32_MAX)++*v; }
static void add(uint64_t *v,uint64_t n) { *v=UINT64_MAX-*v<n ? UINT64_MAX : *v+n; }
void network_health_configure(bool wifi,bool eth)
{
    LOCK();state.flags=(state.flags & ~(NH_WIFI_CONFIGURED|NH_ETH_PRESENT)) |
        (wifi?NH_WIFI_CONFIGURED:0) | (eth?NH_ETH_PRESENT:0);UNLOCK();
}
// Called under the lock. Event chronology follows the order events enter the
// accumulator even if a caller captured its clock just before being preempted.
static void event(uint8_t kind,uint8_t interface,int32_t code,int32_t detail,uint64_t now)
{
    if(state.event_count && now<state.events[state.event_count-1].uptime_ms)
        now=state.events[state.event_count-1].uptime_ms;
    bool error=false;
    switch(kind) {
    case NH_ASSOCIATED: state.flags|=NH_WIFI_UP;increment(&state.counters[NH_WIFI_JOINS]);break;
    case NH_WIFI_DISCONNECTED:
        state.flags&=~(NH_WIFI_UP|NH_WIFI_IP);state.valid&=~(NH_RSSI_VALID|NH_WIFI_IP_VALID);
        increment(&state.counters[NH_WIFI_DROPS]);state.wifi_reason=(uint16_t)code;state.wifi_disconnect_ms=now;error=true;break;
    case NH_WIFI_ADDRESS:state.flags|=NH_WIFI_IP;increment(&state.counters[NH_WIFI_LEASES]);break;
    case NH_WIFI_LOST_IP:state.flags&=~NH_WIFI_IP;state.valid&=~NH_WIFI_IP_VALID;increment(&state.counters[NH_IP_LOSSES]);error=true;break;
    case NH_ETH_CONNECTED:state.flags|=NH_ETH_UP;increment(&state.counters[NH_ETH_LINK_UPS]);break;
    case NH_ETH_DISCONNECTED:
        state.flags&=~(NH_ETH_UP|NH_ETH_IP);state.valid&=~NH_ETH_IP_VALID;state.speed_mbps=0;state.duplex=0;state.autoneg=0;
        increment(&state.counters[NH_ETH_LINK_DOWNS]);error=true;break;
    case NH_ETH_ADDRESS:state.flags|=NH_ETH_IP;increment(&state.counters[NH_ETH_LEASES]);break;
    case NH_ETH_LOST_IP:state.flags&=~NH_ETH_IP;state.valid&=~NH_ETH_IP_VALID;increment(&state.counters[NH_IP_LOSSES]);error=true;break;
    case NH_CONNECT_ATTEMPT:increment(&state.counters[NH_ATTEMPTS]);break;
    case NH_COLLECTOR_CONNECTED:
        state.flags|=NH_CONNECTED;state.flags=detail ? state.flags|NH_TUNNEL : state.flags&~NH_TUNNEL;
        state.connected_ms=now;state.connect_ms=(uint32_t)code;
        if(state.connect_ms>state.max_connect_ms)state.max_connect_ms=state.connect_ms;
        increment(&state.counters[NH_CONNECTS]);break;
    case NH_COLLECTOR_CLOSED:
        if(state.flags&NH_CONNECTED)increment(&state.counters[NH_DISCONNECTS]);
        state.flags&=~(NH_CONNECTED|NH_TUNNEL);state.socket_interface=0;break;
    case NH_DNS_FAILED:increment(&state.counters[NH_DNS_ERRORS]);error=true;break;
    case NH_TCP_FAILED:increment(&state.counters[NH_TCP_ERRORS]);error=true;break;
    case NH_TLS_FAILED:increment(&state.counters[NH_TLS_ERRORS]);error=true;break;
    case NH_HANDSHAKE_FAILED:case NH_AUTH_REJECTED:increment(&state.counters[NH_HANDSHAKE_ERRORS]);error=true;break;
    case NH_READ_FAILED:increment(&state.counters[NH_READ_ERRORS]);error=true;break;
    case NH_WRITE_FAILED:increment(&state.counters[NH_WRITE_ERRORS]);error=true;break;
    case NH_ACK_STALLED:
        increment(&state.counters[NH_ACK_STALLS]);error=true;
        if(code>0 && (uint32_t)code>state.max_ack_gap_ms)state.max_ack_gap_ms=(uint32_t)code;
        break;
    case NH_ETH_FAILED:error=true;break;
    default:break;
    }
    if(error){state.error_kind=kind;state.error_interface=interface;state.error_code=code;state.error_detail=detail;state.error_ms=now;}
    // Saturate rather than wrap event identities. Counters remain useful after
    // saturation; at normal event rates this takes decades.
    if(state.event_total==UINT32_MAX)return;
    increment(&state.event_total);
    if(state.event_count==NH_EVENTS){memmove(state.events,state.events+1,sizeof(state.events[0])*(NH_EVENTS-1));state.event_count--;increment(&state.events_overwritten);}
    int8_t rssi=(state.valid&NH_RSSI_VALID)?state.rssi:127;
    if(kind==NH_WIFI_DISCONNECTED && detail>=-127 && detail<127)rssi=(int8_t)detail;
    state.events[state.event_count++]=(nh_event_t){.uptime_ms=now,.kind=kind,.interface=interface,.rssi=rssi,
        .flags=state.flags,.code=code,.detail=detail,.sequence=state.event_total};
}
void network_health_event(uint8_t kind,uint8_t interface,int32_t code,int32_t detail,uint64_t now)
{ LOCK();event(kind,interface,code,detail,now);UNLOCK(); }
void network_health_read(nh_report_t *out) { LOCK();*out=state;UNLOCK(); }
bool network_health_sample(const nh_report_t *s,uint32_t generation,uint64_t now)
{
    LOCK();
    if(state.event_total!=generation){increment(&state.sample_failures);UNLOCK();return false;}
    if(s->default_interface!=state.default_interface)event(NH_ROUTE_CHANGED,s->default_interface,state.default_interface,0,now);
    if((s->valid&NH_RSSI_VALID) && (state.valid&NH_RSSI_VALID) &&
       (memcmp(s->bssid,state.bssid,6) || s->channel!=state.channel))event(NH_AP_CHANGED,NH_WIFI,s->channel,state.channel,now);
    if((state.flags&NH_ETH_UP) && (s->speed_mbps!=state.speed_mbps || s->duplex!=state.duplex || s->autoneg!=state.autoneg))
        event(NH_ETH_MODE_CHANGED,NH_ETHERNET,s->speed_mbps,s->duplex,now);
    if((state.valid&NH_WIFI_IP_VALID) && (s->valid&NH_WIFI_IP_VALID) && memcmp(state.wifi_ipv4,s->wifi_ipv4,12))event(NH_ADDRESS_CHANGED,NH_WIFI,0,0,now);
    if((state.valid&NH_ETH_IP_VALID) && (s->valid&NH_ETH_IP_VALID) && memcmp(state.eth_ipv4,s->eth_ipv4,12))event(NH_ADDRESS_CHANGED,NH_ETHERNET,0,0,now);
    state.sampled_ms=now;state.default_interface=s->default_interface;
    state.valid=(state.valid&NH_MIN_RSSI_VALID)|(s->valid&~NH_MIN_RSSI_VALID);
    state.rssi=s->rssi;state.channel=s->channel;state.secondary_channel=s->secondary_channel;memcpy(state.bssid,s->bssid,6);
    state.speed_mbps=s->speed_mbps;state.duplex=s->duplex;state.autoneg=s->autoneg;
    memcpy(state.wifi_ipv4,s->wifi_ipv4,12);memcpy(state.eth_ipv4,s->eth_ipv4,12);memcpy(state.dns_ipv4,s->dns_ipv4,8);
    if(s->sample_failures)increment(&state.sample_failures);
    if(s->valid&NH_RSSI_VALID){
        if(!(state.valid&NH_MIN_RSSI_VALID) || s->rssi<state.min_rssi)state.min_rssi=s->rssi;
        state.valid|=NH_MIN_RSSI_VALID;increment(&state.rssi_samples);
        if(s->rssi<=-75)increment(&state.weak_samples);
    }
    UNLOCK();return true;
}
void network_health_io(uint64_t tx,uint64_t rx,uint64_t records)
{ LOCK();add(&state.tx_bytes,tx);add(&state.rx_bytes,rx);add(&state.sent_records,records);UNLOCK(); }
void network_health_ack(uint64_t now,uint32_t gap)
{ LOCK();state.ack_ms=now;if(gap>state.max_ack_gap_ms)state.max_ack_gap_ms=gap;UNLOCK(); }
void network_health_path(uint8_t interface,const uint8_t peer[4])
{ LOCK();state.socket_interface=interface;memcpy(state.peer_ipv4,peer,4);UNLOCK(); }
void network_health_spool(uint32_t queued,uint64_t dropped)
{ LOCK();state.spool_records=queued;state.spool_dropped=dropped;if(queued>state.max_spool_records)state.max_spool_records=queued;UNLOCK(); }
bool network_health_due(const nh_policy_t *p,const nh_report_t *r,uint64_t now)
{
    if(!p->sent)return now>=15000;
    if(now<p->sent_ms || now-p->sent_ms<60000)return false;
    int delta=(int)r->rssi-p->rssi;
    return now-p->sent_ms>=300000 || r->event_total!=p->events ||
        ((r->valid^p->valid)&NH_RSSI_VALID) || ((r->valid&NH_RSSI_VALID) && (delta>=10 || delta<=-10));
}
void network_health_sent(nh_policy_t *p,const nh_report_t *r,uint64_t now)
{ *p=(nh_policy_t){.sent=true,.sent_ms=now,.events=r->event_total,.rssi=r->rssi,.valid=r->valid}; }
