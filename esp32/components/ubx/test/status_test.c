#include "gnss_status.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>
static void put32(uint8_t *p, int32_t value)
{ for (unsigned i = 0; i < 4; i++) p[i] = (uint32_t)value >> (8 * i); }
static void position(gnss_status_t *s, double lat, double lon, int64_t now)
{
    uint8_t p[92] = {0}; p[20] = 3; p[21] = 1;
    put32(p+24, (int32_t)(lon*1e7)); put32(p+28, (int32_t)(lat*1e7));
    gnss_status_feed(s, 1, 7, p, sizeof p, now);
    assert(gnss_status_position_fresh(s, now));
}
int main(void)
{
    // NAV-STATUS spoofing and MON-RF jamming transitions survive between board polls.
    gnss_status_t security = {0}; uint8_t rf[28] = {0}, ns[16] = {0};
    rf[1]=1; rf[5]=1; ns[7]=8;
    gnss_status_feed(&security,0x0a,0x38,rf,sizeof rf,100);
    gnss_status_feed(&security,1,3,ns,sizeof ns,100);
    assert(security.rf_valid && security.status_valid && security.event_count==0);
    rf[5]=2; gnss_status_feed(&security,0x0a,0x38,rf,sizeof rf,200);
    assert(security.event_count==1 && security.event_flags==1 && security.jam==2);
    gnss_status_feed(&security,0x0a,0x38,rf,sizeof rf,250);
    assert(security.event_count==1); // sustained alarm is not an event storm
    ns[7]=16; gnss_status_feed(&security,1,3,ns,sizeof ns,300);
    assert(security.event_count==2 && security.event_flags==2 && security.spoof==2);
    ns[7]=8; gnss_status_feed(&security,1,3,ns,sizeof ns,350);
    assert(security.event_count==3 && security.event_ms==350);
    ns[7]=24; gnss_status_feed(&security,1,3,ns,sizeof ns-1,400);
    assert(security.event_count==3 && security.spoof==1); // malformed input ignored

    gnss_status_t s = {.supported = 0x6f}; uint8_t green, yellow;
    gnss_status_leds(&s, 100, 0, false, &green, &yellow);
    assert(green == 0 && yellow == 0xbf); // no fix: six expected, NavIC unsupported
    position(&s, 37.7, -122.4, 1000); // WAAS region, outside QZSS
    uint8_t expected = gnss_status_expected(&s, 1000, 0);
    assert((expected & (1u<<1)) && !(expected & (1u<<5)));
    gnss_status_leds(&s, 1000, 0, true, &green, &yellow);
    assert(green == 0x80 && yellow == 0x2f);
    assert(gnss_status_expected(&s, 1000, 1u<<5) & (1u<<5)); // learned reception overrides map
    position(&s, 35.7, 139.7, 1000); assert(gnss_status_expected(&s, 1000, 0) & (1u<<5));
    position(&s, 51.5, -0.1, 1000); assert(!(gnss_status_expected(&s, 1000, 0) & (1u<<5)));
    position(&s, 19, 73, 1000); assert(gnss_status_expected(&s, 1000, 0) & (1u<<1));
    position(&s, 85, 0, 1000); assert(!(gnss_status_expected(&s, 1000, 0) & GNSS_REGIONAL_MASK));
    assert(gnss_status_expected(&s, 20000, 0) == s.supported); // stale position -> unknown
    uint8_t sats[44] = {0}; sats[4]=1; sats[5]=3;
    sats[8]=0; sats[10]=30; sats[16]=4; // GPS code lock
    sats[20]=2; sats[22]=30; sats[28]=1; // Galileo searching, not tracked
    sats[32]=6; sats[34]=30; sats[40]=0x24; // unhealthy GLONASS
    gnss_status_feed(&s, 1, 0x35, sats, sizeof sats, 20000);
    assert(s.tracked[0] == 1 && s.tracked[2] == 0 && s.tracked[6] == 0);
    gnss_status_leds(&s, 20000, 0, false, &green, &yellow);
    assert(green == 1 && yellow == 0xbe);
    gnss_status_leds(&s, 40000, 0, false, &green, &yellow);
    assert(green == 0 && yellow == 0xbf); // receiver disappearance cannot leave stale green
    gnss_status_t previous = s;
    gnss_status_feed(&s, 1, 0x35, sats, sizeof sats-1, 30000);
    assert(!memcmp(&s, &previous, sizeof s));
    uint8_t version[100] = {0}; strcpy((char*)version+40, "GPS;GLO;GAL;BDS");
    strcpy((char*)version+70, "SBAS;QZSS");
    s.supported = 0; gnss_status_feed(&s, 10, 4, version, sizeof version, 40000);
    assert(s.supported == 0x6f);
    assert(gnss_status_same_place(377000000,-1224000000,377010000,-1224010000));
    assert(!gnss_status_same_place(377000000,-1224000000,357000000,1397000000));
    assert(gnss_status_same_place(0,1799999000,0,-1799999000));
    puts("GNSS status: tracking, stale data, regional expectations, learned override and movement passed");
}
