#include "network_health.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>
int main(int argc,char **argv)
{
    network_health_configure(true,true);
    network_health_event(NH_ASSOCIATED,NH_WIFI,0,0,1000);
    network_health_event(NH_WIFI_ADDRESS,NH_WIFI,0,0,2000);
    nh_report_t r,s={.valid=NH_RSSI_VALID|NH_WIFI_IP_VALID,.rssi=-81,.channel=6,.default_interface=NH_WIFI,
        .wifi_ipv4={192,0,2,2,255,255,255,0,192,0,2,1},.bssid={2,0,0,0,0,1}};
    network_health_read(&r);assert(network_health_sample(&s,r.event_total,3000));
    network_health_event(NH_WIFI_DISCONNECTED,NH_WIFI,200,-85,4000);
    network_health_read(&r);
    assert(!(r.valid&NH_RSSI_VALID) && r.min_rssi==-81 && r.weak_samples==1);
    assert(r.counters[NH_WIFI_DROPS]==1 && r.error_code==200 && r.events[r.event_count-1].rssi==-85);
    assert(!network_health_sample(&s,0,5000)); // an old sample cannot resurrect a disconnected interface
    for(unsigned i=0;i<12;i++)network_health_event(NH_DNS_FAILED,NH_UNKNOWN,0x8001,-1,6000+i);
    network_health_spool(120,3);network_health_spool(20,3);
    network_health_io(1024,32,8);network_health_ack(7000,1200);
    network_health_read(&r);
    assert(r.event_count==8 && r.events_overwritten==r.event_total-8);
    assert(r.max_spool_records==120 && r.spool_records==20 && r.tx_bytes==1024 && r.max_ack_gap_ms==1200);
    assert(r.counters[NH_DNS_ERRORS]==12 && r.events[7].sequence==r.event_total);
    nh_policy_t p={0};assert(!network_health_due(&p,&r,14999));assert(network_health_due(&p,&r,15000));
    network_health_sent(&p,&r,15000);assert(!network_health_due(&p,&r,75000));
    network_health_event(NH_ASSOCIATED,NH_WIFI,0,0,76000);network_health_read(&r);
    assert(network_health_due(&p,&r,76000));network_health_sent(&p,&r,76000);
    assert(!network_health_due(&p,&r,135999));assert(network_health_due(&p,&r,376000));
    uint8_t b[NH_WIRE_SIZE];assert(!nh_encode(b,sizeof(b)-1,&r));assert(nh_encode(b,sizeof b,&r)==sizeof b);
    if(argc==2){FILE *f=fopen(argv[1],"r");assert(f);for(size_t i=0;i<sizeof b;i++){unsigned v;assert(fscanf(f,"%2x",&v)==1);assert(v==b[i]);}fclose(f);}
    puts("network health: outages, races, retention, pacing and wire bounds passed");
}
