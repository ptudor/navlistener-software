#include "sbf_status.h"
#include "sbf_build.h"
#include "rtc_policy.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

static gnss_status_t status;
static sbf_status_t adapter;
static uint8_t b[SBF_MAX_BLOCK];

static const gnss_observation_t *find(const gnss_observation_t *list, unsigned n, uint8_t g, uint8_t sv, uint8_t sig)
{
    for (unsigned i = 0; i < n; i++)
        if (list[i].gnss == g && list[i].sv == sv && list[i].signal == sig) return &list[i];
    return NULL;
}
static const test_master_t masters[] = {
    {5, 0, 140, true, 2},     // G05 L1C/A 45 dB-Hz, L2C 37.5, L5 without a pseudorange
    {82, 17, 120, true, 0},   // E12 E1 40 dB-Hz
    {183, 33, 100, true, 0},  // J03 L1S 35 dB-Hz
    {9, 0, 90, false, 0},     // G09 without a pseudorange: not tracked
};
static const test_slave_t slaves[] = {{3, 110, true}, {4, 130, false}};

static void tracking_test(void)
{
    memset(&status, 0, sizeof status); status.supported = 0x6f;
    sbf_status_init(&adapter);
    const uint8_t svid[] = {5, 82};
    const int16_t elevation[] = {4550, -250};
    size_t n = visibility_block(b, svid, elevation, 2);
    assert(!sbf_status_feed(&adapter, &status, b, n, 1000) && adapter.visible_count == 2);
    n = meas_block(b, 345600000, 0, masters, 4, slaves);
    assert(sbf_status_feed(&adapter, &status, b, n, 1500));
    assert(status.satellites_valid && status.signals_valid && status.satellites_ms == 1500 && status.signals_ms == 1500);
    assert(status.tracked[0] == 1 && status.tracked[2] == 1 && status.tracked[5] == 1 && status.satellite_count == 3);
    const gnss_observation_t *o = find(status.satellites, status.satellite_count, 0, 5, NR_SATELLITE);
    assert(o && o->cno == 45 && o->elevation == 46); // the strongest signal; 45.50 degrees rounds up
    o = find(status.satellites, status.satellite_count, 2, 12, NR_SATELLITE);
    assert(o && o->cno == 40 && o->elevation == -3);
    o = find(status.satellites, status.satellite_count, 5, 3, NR_SATELLITE);
    assert(o && o->cno == 35 && o->elevation == 127); // not in SatVisibility
    assert(!find(status.satellites, status.satellite_count, 0, 9, NR_SATELLITE));
    assert(status.signal_count == 4);
    assert((o = find(status.signals, status.signal_count, 0, 5, 0)) && o->cno == 45);
    assert((o = find(status.signals, status.signal_count, 0, 5, 3)) && o->cno == 37);
    assert(!find(status.signals, status.signal_count, 0, 5, 6)); // L5 had no pseudorange
    assert(find(status.signals, status.signal_count, 2, 12, 0) && find(status.signals, status.signal_count, 5, 3, 1));

    // Elevations older than 5 s are unknown; tracking still counts.
    n = meas_block(b, 345601000, 0, masters, 4, slaves);
    assert(sbf_status_feed(&adapter, &status, b, n, 1000 + SBF_STATUS_ELEVATION_MS + 1));
    assert(find(status.satellites, status.satellite_count, 0, 5, NR_SATELLITE)->elevation == 127);

    // A scrambled epoch is not used; the last status stands to go stale on its own.
    n = meas_block(b, 345602000, SBF_MEAS_SCRAMBLED, masters, 4, slaves);
    assert(!sbf_status_feed(&adapter, &status, b, n, 9000) && adapter.scrambled_epochs == 1 && status.satellites_ms != 9000);

    // An auxiliary antenna's signal is not the observer's antenna.
    const test_master_t aux[] = {{7, 0, 140, true, 0}};
    n = meas_block(b, 345603000, 0, aux, 1, NULL);
    b[20 + 1] |= 0x20; seal(b, SBF_MEAS_EPOCH, 1, n);
    assert(sbf_status_feed(&adapter, &status, b, n, 10000) && status.satellites_valid && !status.satellite_count &&
           !status.tracked[0]);

    // More satellites than the status holds: the epoch is invalid, not truncated.
    static test_master_t many[201];
    unsigned count = 0;
    for (unsigned svid_value = 1; svid_value <= 245 && count < 201; svid_value++) {
        uint8_t g, sv;
        if (!sbf_svid((uint8_t)svid_value, &g, &sv)) continue;
        many[count++] = (test_master_t){(uint8_t)svid_value, 0, 140, true, 0};
    }
    assert(count == 201);
    n = meas_block(b, 345604000, 0, many, 201, NULL);
    assert(sbf_status_feed(&adapter, &status, b, n, 11000) && !status.satellites_valid && !status.signals_valid &&
           !status.satellite_count && !status.tracked[0]);
}

static void time_test(void)
{
    memset(&status, 0, sizeof status);
    sbf_status_init(&adapter);
    rtc_candidate_t candidate = {0};
    int64_t epoch = 0;
    // Three synchronized epochs, the ReceiverTime before or after its PVT: GNSS UTC qualifies.
    for (unsigned i = 0; i < 3; i++) {
        const uint32_t tow = 345600000 + 1000 * i;
        const int64_t now = 10000 + 1000 * i;
        size_t pn = pvt_block(b, 2, tow, 1, 0, 37.5, -122.25, 12.5);
        uint8_t t[32];
        size_t tn = time_block(t, tow, 26, 10, 19, 18, 30, (int8_t)i, 18, 7);
        if (i % 2) { sbf_status_feed(&adapter, &status, t, tn, now); sbf_status_feed(&adapter, &status, b, pn, now); }
        else { sbf_status_feed(&adapter, &status, b, pn, now); sbf_status_feed(&adapter, &status, t, tn, now); }
        const uint8_t *p = status.pvt_utc;
        assert(status.fix_valid && status.fix_ms == now && (p[0] | p[1] << 8) == 2026 && p[2] == 10 && p[3] == 19);
        assert(p[4] == 18 && p[5] == 30 && p[6] == i && (p[7] & 7) == 7);
        assert(sbf_u32(p + 8) == SBF_STATUS_FINETIME_NS && sbf_u32(p + 12) == 0);
        bool qualified = rtc_gnss_candidate(&candidate, &status, now, &epoch);
        assert(qualified == (i == 2));
    }
    assert(status.latitude == 375000000 && status.longitude == -1222500000);
    int64_t expect;
    assert(rtc_calendar_epoch(2026, 10, 19, 18, 30, 2, &expect) && epoch == expect);

    // A PVT whose epoch's ReceiverTime has not come leaves the calendar unusable.
    size_t pn = pvt_block(b, 2, 345603000, 1, 0, 37.5, -122.25, 12.5);
    sbf_status_feed(&adapter, &status, b, pn, 13000);
    assert(status.pvt_utc[7] == 0);
    // Not FINETIME, or the leap seconds unknown: date and time, but not fully resolved.
    uint8_t t[32];
    size_t tn = time_block(t, 345603000, 26, 10, 19, 18, 30, 3, 18, SBF_SYNC_WNSET | SBF_SYNC_TOWSET);
    assert(sbf_status_feed(&adapter, &status, t, tn, 13000));
    assert((status.pvt_utc[7] & 7) == 3 && sbf_u32(status.pvt_utc + 8) == SBF_STATUS_TOWSET_NS);
    tn = time_block(t, 345604000, 26, 10, 19, 18, 30, 4, -128, 7);
    pn = pvt_block(b, 2, 345604000, 1, 0, 37.5, -122.25, 12.5);
    sbf_status_feed(&adapter, &status, t, tn, 14000);
    sbf_status_feed(&adapter, &status, b, pn, 14000);
    assert((status.pvt_utc[7] & 7) == 3 && !rtc_gnss_candidate(&candidate, &status, 14000, &epoch));
    // No UTC yet: nothing valid, accuracy unknown.
    tn = time_block(t, 345605000, -128, -128, -128, -128, -128, -128, -128, 0);
    pn = pvt_block(b, 2, 345605000, 1, 0, 37.5, -122.25, 12.5);
    sbf_status_feed(&adapter, &status, b, pn, 15000);
    sbf_status_feed(&adapter, &status, t, tn, 15000);
    assert(status.pvt_utc[7] == 0 && sbf_u32(status.pvt_utc + 8) == UINT32_MAX);
    // No position: no fix, and the last good position stays for the history check.
    pn = pvt_block(b, 2, 345606000, 0, 1, 0, 0, 0);
    sbf_status_feed(&adapter, &status, b, pn, 16000);
    assert(!status.fix_valid && status.latitude == 375000000 && !gnss_status_position_fresh(&status, 16000));
}

static void interference_test(void)
{
    memset(&status, 0, sizeof status);
    sbf_status_init(&adapter);
    size_t n = rf_block(b, 0, NULL, 0);
    assert(sbf_status_feed(&adapter, &status, b, n, 1000));
    assert(status.rf_valid && status.jam == 1 && status.status_valid && status.spoof == 1 && !status.event_count);
    // Interference detected while a fix holds: warning, an event.
    size_t pn = pvt_block(b, 2, 345600000, 1, 0, 37.5, -122.25, 12.5);
    sbf_status_feed(&adapter, &status, b, pn, 2000);
    const uint8_t mitigated[] = {2};
    n = rf_block(b, 0, mitigated, 1);
    assert(sbf_status_feed(&adapter, &status, b, n, 2500));
    assert(status.jam == 2 && status.event_count == 1 && status.event_flags == 1 && status.event_states == (2 | 1 << 2));
    // An authenticity flag: spoofing indicated, a second event.
    n = rf_block(b, 0x02, mitigated, 1);
    sbf_status_feed(&adapter, &status, b, n, 3000);
    assert(status.spoof == 2 && status.event_count == 2 && status.event_flags == 2 && status.event_states == (2 | 2 << 2));
    // Interference without a fix: critical. A user's notch filter alone is not interference.
    pn = pvt_block(b, 2, 345601000, 0, 1, 0, 0, 0);
    sbf_status_feed(&adapter, &status, b, pn, 4000);
    const uint8_t unmitigated[] = {8};
    n = rf_block(b, 0x02, unmitigated, 1);
    sbf_status_feed(&adapter, &status, b, n, 4500);
    assert(status.jam == 3 && status.event_count == 3);
    const uint8_t notch[] = {1};
    n = rf_block(b, 0, notch, 1);
    sbf_status_feed(&adapter, &status, b, n, 5000);
    assert(status.jam == 1 && status.spoof == 1 && status.event_count == 5);
    // A first report that is already raised counts as an event.
    memset(&status, 0, sizeof status);
    n = rf_block(b, 0x01, NULL, 0);
    sbf_status_feed(&adapter, &status, b, n, 1000);
    assert(status.spoof == 2 && status.event_count == 1);
}

int main(void)
{
    tracking_test();
    time_test();
    interference_test();
    puts("SBF status: tracking, C/N0 and elevations, scrambling, antenna, capacity, PVT and ReceiverTime pairing "
         "into a qualifying RTC candidate, and interference and spoofing events passed");
    return 0;
}
