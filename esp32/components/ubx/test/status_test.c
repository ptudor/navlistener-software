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
static void reception_evidence(void)
{
    gnss_status_t s={.supported=1};
    nr_expectation_t e={.id=7,.issued=1800000000,.radius_m=1000,.count=2,.min_expected=1,.min_missing=1,.missing_percent=50,
        .entries={{0,1,3,31},{0,2,6,31}}};
    nr_sample_t sample={.expectation_id=7,.utc=1800000000};
    uint8_t sig[8+4*16]={0};sig[5]=4;
    sig[8]=0;sig[9]=1;sig[10]=3;sig[14]=40;sig[15]=4; // L2 CL, code locked
    memcpy(sig+24,sig+8,16);sig[26]=4; // L2 CM, same group
    memcpy(sig+40,sig+8,16);sig[41]=2;sig[42]=6;sig[47]=3; // acquired, no code lock
    memcpy(sig+56,sig+8,16);sig[57]=2;sig[58]=7;sig[66]=2; // explicitly unhealthy
    gnss_status_feed(&s,1,0x43,sig,sizeof sig,1000);
    assert(s.signals_valid && s.signal_count==1 && s.signals[0].signal==3 && s.signals[0].cno==40);
    gnss_status_compare(&s,&e,&sample,1000);
    assert(nr_counts(&e,&sample)==1 && sample.valid==1 && sample.observed[0]==1 && sample.expected[0]==2);
    gnss_status_compare(&s,&e,&sample,17000);nr_counts(&e,&sample);
    assert(sample.valid==0); // a missing stream is unknown, never measured zero
    uint8_t empty[8]={0};gnss_status_feed(&s,1,0x43,empty,sizeof empty,18000);
    gnss_status_compare(&s,&e,&sample,18000);
    assert(nr_counts(&e,&sample)==1 && sample.valid==1 && sample.observed[0]==0); // a fresh empty stream IS a measured deficit
    gnss_status_t before=s;sig[4]=1;gnss_status_feed(&s,1,0x43,sig,sizeof sig,19000);
    assert(!memcmp(&before,&s,sizeof s)); // unknown version does not refresh coverage
    position(&s,1,1,19000);gnss_status_compare(&s,&e,&sample,19000);
    assert(sample.valid==0); // a moved station cannot reuse the survey forecast
    s.fix_valid=false;gnss_status_compare(&s,&e,&sample,19000);
    assert(sample.valid==1); // loss of position does not erase a fixed-site deficit
}
// MON-RF blocks and antenna supervision, SEC-SIG jamming in place of MON-RF's flag, and the
// per-signal summary of NAV-SIG.
static void rf_and_signals(void)
{
    gnss_status_t s = {0};
    uint8_t rf[4 + 2*24] = {0}; rf[1] = 2;
    rf[4] = 0; rf[6] = 2; rf[7] = 1; rf[16] = 82; rf[18] = 0xa0; rf[19] = 0x0f; rf[20] = 3; // L1: OK, ON
    rf[28] = 1; rf[30] = 3; rf[31] = 1; rf[40] = 0x10; rf[41] = 1; rf[42] = 0xff; rf[43] = 0x1f; // SHORT
    gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 100);
    assert(s.rf_blocks_valid && s.rf_blocks == 2 && s.rf_blocks_ms == 100);
    assert(s.rf_block[0].id == 0 && s.rf_block[0].ant_status == 2 && s.rf_block[0].ant_power == 1);
    assert(s.rf_block[0].noise_per_ms == 82 && s.rf_block[0].agc == 4000 && s.rf_block[0].cw_suppression == 3);
    assert(s.rf_block[1].id == 1 && s.rf_block[1].ant_status == 3 && s.rf_block[1].noise_per_ms == 272 && s.rf_block[1].agc == 8191);
    assert(s.rf_valid && s.jam == 0 && !s.event_count); // the X20's MON-RF flag reads 0

    // SEC-SIG takes over: unknown readings set no baseline, the first state is the baseline.
    uint8_t sec[4 + 4] = {2, 1, 0, 1}; // version 2, detection on, unknown, one centre frequency
    gnss_status_feed(&s, 0x27, 9, sec, sizeof sec, 200);
    gnss_status_feed(&s, 0x27, 9, sec, sizeof sec, 300);
    assert(s.sec_sig && !s.sec_sig_known && s.jam == 0 && s.rf_ms == 300 && !s.event_count);
    sec[1] = 1 | 1 << 1; gnss_status_feed(&s, 0x27, 9, sec, sizeof sec, 400);
    assert(s.sec_sig_known && s.jam == 1 && !s.event_count); // first state, no jamming: baseline
    rf[5] = 0; gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 450);
    assert(s.jam == 1 && s.rf_ms == 400); // MON-RF no longer moves jam
    sec[1] = 1 | 2 << 1; gnss_status_feed(&s, 0x27, 9, sec, sizeof sec, 500);
    assert(s.event_count == 1 && s.event_flags == 1 && s.jam == 2 && s.event_ms == 500);
    sec[0] = 3; sec[1] = 1 | 1 << 1; gnss_status_feed(&s, 0x27, 9, sec, sizeof sec, 600);
    assert(s.event_count == 2 && s.jam == 1); // version 3 too; recovery counts
    sec[1] = 1 << 1; gnss_status_feed(&s, 0x27, 9, sec, sizeof sec, 700);
    assert(s.event_count == 3 && s.jam == 0); // detection disabled reads unknown
    sec[0] = 1; sec[1] = 1 | 2 << 1; gnss_status_feed(&s, 0x27, 9, sec, sizeof sec, 800);
    sec[0] = 2; gnss_status_feed(&s, 0x27, 9, sec, sizeof sec - 1, 800);
    assert(s.event_count == 3 && s.rf_ms == 700); // version 1 and a short body are ignored
    gnss_status_t alarm = {0};
    gnss_status_feed(&alarm, 0x27, 9, sec, sizeof sec, 100);
    assert(alarm.event_count == 1 && alarm.jam == 2); // an alarm at first is an event

    // Signal kinds: every named sigId agrees with the shared pair merging, and with its system.
    for (unsigned g = 0; g < 8; g++)
        for (unsigned id = 0; id < 16; id++) {
            const int kind = gnss_signal_kind(g, id);
            if (kind < 0) continue;
            assert(gnss_signal_kinds[kind].gnss == g && gnss_signal_kinds[kind].name);
            assert(gnss_signal_kind(g, nr_signal(g, id)) == kind);
        }
    for (unsigned k = 0; k < GNSS_SIGNAL_KINDS; k++) assert(gnss_signal_kinds[k].band < GNSS_BANDS);
    assert(gnss_signal_kind(4, 0) < 0 && gnss_signal_kind(0, 1) < 0 && gnss_signal_kind(2, 7) < 0);

    uint8_t sig[8 + 8*16] = {0}; sig[5] = 8;
    const uint8_t rows[8][4] = { // gnssId, svId, sigId, qualityInd
        {0, 1, 0, 7},  // GPS L1C/A
        {0, 1, 6, 5},  // GPS L5 I, unhealthy
        {0, 1, 7, 5},  // GPS L5 Q of the same satellite: one L5 signal
        {2, 3, 4, 7},  // Galileo E5a Q
        {2, 3, 0, 3},  // Galileo E1, not code locked
        {0, 2, 15, 7}, // a sigId the tables do not name
        {3, 60, 10, 4}, // BeiDou B3I D2
        {7, 2, 0, 5},  // NavIC L5
    };
    for (unsigned i = 0; i < 8; i++) {
        uint8_t *r = sig + 8 + 16*i;
        r[0] = rows[i][0]; r[1] = rows[i][1]; r[2] = rows[i][2]; r[6] = 40; r[7] = rows[i][3];
        if (rows[i][0] == 0 && (rows[i][2] == 6 || rows[i][2] == 7)) r[10] = 2;
    }
    gnss_status_feed(&s, 1, 0x43, sig, sizeof sig, 900);
    assert(s.signal_locked[0] == 1 && !s.signal_unhealthy[0]);
    assert(s.signal_locked[2] == 1 && s.signal_unhealthy[2] == 1);
    assert(s.signal_locked[5] == 1 && s.signal_locked[4] == 0 && s.signal_locked[10] == 1 && s.signal_locked[20] == 1);
    assert(s.signal_other == 1);
    unsigned total = 0;
    for (unsigned k = 0; k < GNSS_SIGNAL_KINDS; k++) total += s.signal_locked[k];
    assert(total == 5);
    uint8_t none[8] = {0};
    gnss_status_feed(&s, 1, 0x43, none, sizeof none, 1000);
    assert(!s.signal_locked[0] && !s.signal_other); // each NAV-SIG replaces the last
}
int main(void)
{
    reception_evidence();
    rf_and_signals();
    gnss_status_t timing={0}; uint8_t tp[16]={0}; tp[14]=3; tp[15]=0x10;
    gnss_status_feed(&timing,0x0d,1,tp,sizeof tp,100);
    assert(timing.tp_valid && timing.tp_flags==3 && timing.tp_ref==0x10 && timing.tp_ms==100);
    gnss_status_feed(&timing,0x0d,1,tp,sizeof tp-1,200);
    assert(timing.tp_ms==100);
    tp[14]=0x80; gnss_status_feed(&timing,0x0d,1,tp,sizeof tp,200);
    assert(timing.tp_ms==100);
    tp[14]=3; put32(tp,604800000); gnss_status_feed(&timing,0x0d,1,tp,sizeof tp,200);
    assert(timing.tp_ms==100);
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
    assert(green == 0x80 && yellow == 0x1f); // SBAS GPS GAL BDS GLO expected; QZSS outside its region
    assert(gnss_status_expected(&s, 1000, 1u<<5) & (1u<<5)); // learned reception overrides map
    position(&s, 35.7, 139.7, 1000); assert(gnss_status_expected(&s, 1000, 0) & (1u<<5));
    position(&s, 51.5, -0.1, 1000); assert(!(gnss_status_expected(&s, 1000, 0) & (1u<<5)));
    position(&s, 19, 73, 1000); assert(gnss_status_expected(&s, 1000, 0) & (1u<<1));
    position(&s, 85, 0, 1000); assert(!(gnss_status_expected(&s, 1000, 0) & GNSS_REGIONAL_MASK));
    assert(gnss_status_expected(&s, 20000, 0) == s.supported); // stale position -> unknown
    uint8_t sats[44] = {0}; sats[4]=1; sats[5]=3;
    sats[8]=0; sats[9]=1; sats[10]=30; sats[16]=4; // GPS code lock
    sats[20]=2; sats[22]=30; sats[28]=1; // Galileo searching, not tracked
    sats[32]=6; sats[34]=30; sats[40]=0x24; // unhealthy GLONASS
    gnss_status_feed(&s, 1, 0x35, sats, sizeof sats, 20000);
    assert(s.tracked[0] == 1 && s.tracked[2] == 0 && s.tracked[6] == 0 &&
           s.satellites[0].cno==30 && s.satellites[0].elevation==0);
    gnss_status_leds(&s, 20000, 0, false, &green, &yellow);
    assert(green == 2 && yellow == 0xbd); // GPS tracked lights the second column
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
    puts("GNSS status: tracking, stale data, regional expectations, learned override, movement, RF blocks, SEC-SIG jamming and signal kinds passed");
}
