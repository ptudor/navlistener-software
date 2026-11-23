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
static void satellites_by_name(void)
{
    static gnss_status_t s;
    char text[160];
    memset(&s, 0, sizeof s);
    // Nothing is written for a list that is not valid.
    s.satellite_count = 1; s.satellites[0] = (gnss_observation_t){0, 5, NR_SATELLITE, 45, 61};
    assert(gnss_status_satellites_text(&s, text, sizeof text) == 0 && !text[0]);
    // GPS, GLONASS, Galileo, BeiDou, QZSS, NavIC and SBAS in that order, by number within each,
    // whatever order the receiver reported them in; an unknown elevation is a dash.
    const gnss_observation_t reported[] = {
        {1, 131, NR_SATELLITE, 41, 42}, {0, 12, NR_SATELLITE, 38, 23}, {3, 36, NR_SATELLITE, 40, 9},
        {0, 5, NR_SATELLITE, 45, 61}, {6, 3, NR_SATELLITE, 31, 127}, {2, 7, NR_SATELLITE, 44, -2},
        {5, 2, NR_SATELLITE, 33, 4}, {7, 14, NR_SATELLITE, 30, 3},
    };
    s.satellite_count = sizeof reported / sizeof reported[0];
    memcpy(s.satellites, reported, sizeof reported);
    s.satellites_valid = true;
    const char *expected = "G05 61/45, G12 23/38, R03 -/31, E07 -2/44, C36 9/40, J02 4/33, I14 3/30, S131 42/41";
    assert(gnss_status_satellites_text(&s, text, sizeof text) == strlen(expected) && !strcmp(text, expected));
    // A list too long for the text ends after the last satellite that fits whole.
    char two[24];
    assert(gnss_status_satellites_text(&s, two, sizeof two) == strlen("G05 61/45, G12 23/38") && !strcmp(two, "G05 61/45, G12 23/38"));
    assert(gnss_status_satellites_text(&s, two, 0) == 0);
    // No satellite: an empty text, for the caller to word.
    s.satellite_count = 0;
    assert(gnss_status_satellites_text(&s, text, sizeof text) == 0 && !text[0]);
}
// What lights the panel's ATTENTION lamp: the receiver's own reports while they are fresh,
// and its silence.
static void attention(void)
{
    static gnss_status_t s;
    char text[96];
    memset(&s, 0, sizeof s);
    // A receiver that has said nothing is given a minute from the start before that counts.
    assert(gnss_status_attention(&s, 0) == 0 && gnss_status_attention(&s, GNSS_ATTENTION_START_MS - 1) == 0);
    assert(gnss_status_attention(&s, GNSS_ATTENTION_START_MS) == GNSS_ATTENTION_SILENT);
    // Reporting: a navigation solution, valid or not, or a satellite list. Fifteen seconds
    // without either is silence again.
    uint8_t pvt[92] = {0};
    gnss_status_feed(&s, 1, 7, pvt, sizeof pvt, 100000);
    assert(!s.fix_valid && gnss_status_attention(&s, 100000) == 0 && gnss_status_attention(&s, 115000) == 0);
    assert(gnss_status_attention(&s, 115001) == GNSS_ATTENTION_SILENT);
    uint8_t sat[8] = {0}; sat[4] = 1;
    gnss_status_feed(&s, 1, 0x35, sat, sizeof sat, 120000);
    assert(gnss_status_attention(&s, 130000) == 0);

    // MON-RF: the antenna supervisor of any block, and the jamming flag where SEC-SIG is not used.
    uint8_t rf[4 + 2*24] = {0}; rf[1] = 2;
    rf[4+1] = 1; rf[4+2] = 2; rf[28+1] = 1; rf[28+2] = 2;              // both OK, no jamming
    gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 130000);
    assert(gnss_status_attention(&s, 130000) == 0);
    rf[28+2] = 3; gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 130100); // second block SHORT
    assert(gnss_status_attention(&s, 130100) == GNSS_ATTENTION_ANTENNA_SHORT);
    rf[4+2] = 4; gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 130200);  // and the first OPEN
    assert(gnss_status_attention(&s, 130200) == (GNSS_ATTENTION_ANTENNA_SHORT | GNSS_ATTENTION_ANTENNA_OPEN));
    rf[4+2] = rf[28+2] = 2; rf[28+1] = 2; gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 130300);
    assert(gnss_status_attention(&s, 130300) == GNSS_ATTENTION_JAMMING);
    rf[4+1] = 3; gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 130400);
    assert(gnss_status_attention(&s, 130400) == GNSS_ATTENTION_JAMMING_CRITICAL);
    // A report older than 15 s lights nothing: the satellites and the solution are stale by
    // then too, so what is left is the silence.
    assert(gnss_status_attention(&s, 130400 + 15001) == GNSS_ATTENTION_SILENT);
    // NAV-STATUS: spoofing indicated (2) or several indications (3); none (1) and unknown (0) are not.
    uint8_t ns[16] = {0};
    for (unsigned state = 0; state < 4; state++) {
        ns[7] = (uint8_t)(state << 3);
        gnss_status_feed(&s, 1, 3, ns, sizeof ns, 200000 + state);
        gnss_status_feed(&s, 1, 7, pvt, sizeof pvt, 200000 + state);
        assert(gnss_status_attention(&s, 200000 + state) == (state >= 2 ? GNSS_ATTENTION_SPOOFING : 0));
    }
    // SEC-SIG's state where the receiver sends it: warning, then none.
    uint8_t sec[4] = {2, 1 | 2 << 1, 0, 0};
    gnss_status_feed(&s, 0x27, 9, sec, sizeof sec, 200010);
    assert(gnss_status_attention(&s, 200010) == (GNSS_ATTENTION_SPOOFING | GNSS_ATTENTION_JAMMING));
    sec[1] = 1 | 1 << 1; gnss_status_feed(&s, 0x27, 9, sec, sizeof sec, 200020);
    assert(gnss_status_attention(&s, 200020) == GNSS_ATTENTION_SPOOFING);
    assert((GNSS_ATTENTION_FLASHING & GNSS_ATTENTION_SPOOFING) && (GNSS_ATTENTION_FLASHING & GNSS_ATTENTION_JAMMING_CRITICAL) &&
           !(GNSS_ATTENTION_FLASHING & GNSS_ATTENTION_JAMMING));

    // In words, most urgent first; what does not fit whole is left out.
    assert(gnss_status_attention_text(0, text, sizeof text) == 4 && !strcmp(text, "none"));
    assert(gnss_status_attention_text(GNSS_ATTENTION_JAMMING, text, sizeof text) == 15 && !strcmp(text, "jamming warning"));
    const uint8_t all = GNSS_ATTENTION_SILENT | GNSS_ATTENTION_ANTENNA_SHORT | GNSS_ATTENTION_ANTENNA_OPEN |
                        GNSS_ATTENTION_JAMMING | GNSS_ATTENTION_JAMMING_CRITICAL | GNSS_ATTENTION_SPOOFING;
    char every[128];
    gnss_status_attention_text(all, every, sizeof every);
    assert(!strcmp(every, "spoofing indicated, jamming critical, jamming warning, antenna short, antenna open, receiver silent"));
    assert(gnss_status_attention_text(all, text, 30) == strlen("spoofing indicated") && !strcmp(text, "spoofing indicated"));
    assert(gnss_status_attention_text(all, text, 3) == 0 && !text[0] && gnss_status_attention_text(all, text, 0) == 0);
    assert(gnss_status_attention_text(0, text, 3) == 0 && !text[0]);
}
// One second of an X20 for the periodic source: a satellite list, and a SEC-SIG report of its
// seven frequencies with those in `flagged` (bit n for the nth) marked jammed.
static void second(gnss_status_t *s, int64_t at_ms, unsigned flagged)
{
    static const uint32_t khz[7] = {1575420, 1176450, 1227600, 1268520, 1278750, 1246000, 1602000};
    uint8_t sat[8] = {0}, sec[4 + 4 * 7] = {2, 0, 0, 7};
    sat[4] = 1;
    sec[1] = (uint8_t)(1 | (flagged ? 2 : 1) << 1);
    for (unsigned i = 0; i < 7; i++) {
        const uint32_t word = khz[i] | (flagged >> i & 1 ? 1u << 24 : 0);
        for (unsigned b = 0; b < 4; b++) sec[4 + 4 * i + b] = (uint8_t)(word >> (8 * b));
    }
    gnss_status_feed(s, 1, 0x35, sat, sizeof sat, at_ms);
    gnss_status_feed(s, 0x27, 9, sec, sizeof sec, at_ms);
}
// A rotating radar on the path that carries L2, B3, E6 and GLONASS L2: one flagged report
// every 12 s. Its first four crossings are jamming like any other; once the source is known
// its crossings are neither events nor causes for attention, while the receiver's own state
// is kept as reported. Flags that outlast a crossing, or name other frequencies, count.
static void periodic_source(void)
{
    static gnss_status_t s;
    const unsigned path2 = 1u << 2 | 1u << 3 | 1u << 4 | 1u << 5, every = 0x7f;
    int64_t t = 1000;
    for (; t < 20000; t += 1000) second(&s, t, 0);
    assert(s.sec_sig_known && s.jam == 1 && s.jam_counted == 1 && !s.event_count);
    unsigned events = 0;
    for (unsigned turn = 0; turn < 4; turn++) {
        second(&s, t, path2);
        assert(s.jam == 2 && s.jam_counted == 2 && s.centers_jammed == 4 && s.event_count == ++events);
        assert(gnss_status_attention(&s, t + 400) == GNSS_ATTENTION_JAMMING);
        for (unsigned i = 1; i < 12; i++) second(&s, t + 1000 * i, 0);
        assert(s.jam == 1 && s.event_count == ++events && gnss_status_attention(&s, t + 11400) == 0);
        t += 12000;
    }
    assert(s.pulse.known && s.pulse.period_ms == 12000);
    for (unsigned turn = 0; turn < 30; turn++) {
        second(&s, t, path2);
        assert(s.jam == 2 && s.jam_counted == 1 && s.centers_jammed == 4); // the receiver's state is kept
        assert(s.event_count == events && gnss_status_attention(&s, t + 400) == 0);
        for (unsigned i = 1; i < 12; i++) {
            second(&s, t + 1000 * i, 0);
            assert(s.event_count == events && gnss_status_attention(&s, t + 1000 * i) == 0);
        }
        t += 12000;
    }
    // Flags on every frequency when a crossing is due: jamming, at once.
    second(&s, t, every);
    assert(s.jam_counted == 2 && s.event_count == ++events && gnss_status_attention(&s, t) == GNSS_ATTENTION_JAMMING);
    second(&s, t + 1000, 0);
    assert(s.event_count == ++events);
    for (unsigned i = 2; i < 12; i++) second(&s, t + 1000 * i, 0);
    t += 12000;
    // The source's own frequencies, due, but staying up: a crossing for two reports, jamming
    // from the third, and over when they clear.
    second(&s, t, path2);
    second(&s, t + 1000, path2);
    assert(s.jam_counted == 1 && s.event_count == events && gnss_status_attention(&s, t + 1400) == 0);
    second(&s, t + 2000, path2);
    assert(s.jam_counted == 2 && s.event_count == ++events && gnss_status_attention(&s, t + 2000) == GNSS_ATTENTION_JAMMING);
    for (unsigned i = 3; i < 9; i++) second(&s, t + 1000 * i, path2);
    assert(s.event_count == events && gnss_status_attention(&s, t + 8000) == GNSS_ATTENTION_JAMMING);
    second(&s, t + 9000, 0);
    assert(s.jam_counted == 1 && s.event_count == ++events && gnss_status_attention(&s, t + 9000) == 0);
    for (unsigned i = 10; i < 12; i++) second(&s, t + 1000 * i, 0);
    t += 12000;
    // The source is still known, and its next crossing still explained.
    second(&s, t, path2);
    assert(s.pulse.known && s.jam_counted == 1 && s.event_count == events && gnss_status_attention(&s, t) == 0);
    second(&s, t + 1000, 0);
    // A spoofing report during a crossing records the counted state, not the crossing.
    for (unsigned i = 2; i < 12; i++) second(&s, t + 1000 * i, 0);
    t += 12000;
    second(&s, t, path2);
    uint8_t ns[16] = {0}; ns[7] = 2 << 3;
    gnss_status_feed(&s, 1, 3, ns, sizeof ns, t + 100);
    assert(s.event_count == ++events && s.event_flags == 2 && s.event_states == (1 | 2 << 2));

    // A receiver whose jamming flag is MON-RF's names no frequency, and is tracked the same.
    static gnss_status_t m;
    uint8_t rf[4 + 24] = {0}, sat[8] = {0};
    rf[1] = 1; rf[4 + 2] = 2; sat[4] = 1;
    unsigned counted = 0;
    for (int64_t at = 1000; at < 200000; at += 1000) {
        const bool crossing = at >= 20000 && (at - 20000) % 10000 == 0;
        rf[4 + 1] = crossing ? 2 : 1;
        gnss_status_feed(&m, 1, 0x35, sat, sizeof sat, at);
        gnss_status_feed(&m, 0x0a, 0x38, rf, sizeof rf, at);
        if (crossing && gnss_status_attention(&m, at) == GNSS_ATTENTION_JAMMING) counted++;
        if (!crossing) assert(gnss_status_attention(&m, at) == 0);
    }
    assert(counted == 4 && m.pulse.known && m.pulse.period_ms == 10000 && m.event_count == 8);
}
int main(void)
{
    reception_evidence();
    rf_and_signals();
    satellites_by_name();
    attention();
    periodic_source();
    // NAV-PVT's horizontal accuracy estimate and position DOP are kept with a valid fix and
    // cleared without one.
    gnss_status_t fix = {0}; uint8_t pvt[92] = {0};
    pvt[20] = 3; pvt[21] = 1; put32(pvt + 40, 1234); pvt[76] = 125; // 1.234 m, PDOP 1.25
    gnss_status_feed(&fix, 1, 7, pvt, sizeof pvt, 100);
    assert(fix.fix_valid && fix.h_acc_mm == 1234 && fix.p_dop == 125);
    pvt[21] = 0; gnss_status_feed(&fix, 1, 7, pvt, sizeof pvt, 200); // gnssFixOK cleared
    assert(!fix.fix_valid && fix.h_acc_mm == 0 && fix.p_dop == 0);
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
    puts("GNSS status: tracking, stale data, regional expectations, learned override, movement, RF blocks, SEC-SIG jamming, signal kinds, satellites by name, fix accuracy, the attention flags and a periodic source left out of them passed");
}
