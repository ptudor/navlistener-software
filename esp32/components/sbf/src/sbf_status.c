#include "sbf_status.h"
#include <math.h>
#include <string.h>

void sbf_status_init(sbf_status_t *a) { memset(a, 0, sizeof *a); }

static void put32(uint8_t *p, uint32_t v) { p[0] = (uint8_t)v; p[1] = (uint8_t)(v >> 8); p[2] = (uint8_t)(v >> 16); p[3] = (uint8_t)(v >> 24); }

// pvt_utc in NAV-PVT bytes 4..23: year U2, month, day, hour, minute, second, valid (date 1,
// time 2, fully resolved 4), tAcc U4 ns, nano I4. The fix fields after them stay zero.
static void compose_utc(gnss_status_t *s, const sbf_time_t *t)
{
    uint8_t *p = s->pvt_utc;
    memset(p, 0, sizeof s->pvt_utc);
    const bool date = t->year >= 0 && t->year <= 99 && t->month >= 1 && t->month <= 12 && t->day >= 1 && t->day <= 31;
    const bool time = t->hour >= 0 && t->hour <= 23 && t->minute >= 0 && t->minute <= 59 && t->second >= 0 && t->second <= 60;
    const bool synced = (t->sync & (SBF_SYNC_WNSET | SBF_SYNC_TOWSET | SBF_SYNC_FINETIME)) ==
                        (SBF_SYNC_WNSET | SBF_SYNC_TOWSET | SBF_SYNC_FINETIME);
    const uint16_t year = date ? (uint16_t)(2000 + t->year) : 0;
    p[0] = (uint8_t)year; p[1] = (uint8_t)(year >> 8);
    if (date) { p[2] = (uint8_t)t->month; p[3] = (uint8_t)t->day; }
    if (time) { p[4] = (uint8_t)t->hour; p[5] = (uint8_t)t->minute; p[6] = (uint8_t)t->second; }
    p[7] = (uint8_t)((date ? 1 : 0) | (time ? 2 : 0) | (date && time && synced && t->delta_ls != -128 ? 4 : 0));
    put32(p + 8, t->sync & SBF_SYNC_FINETIME ? SBF_STATUS_FINETIME_NS :
                 t->sync & SBF_SYNC_TOWSET ? SBF_STATUS_TOWSET_NS : UINT32_MAX);
    put32(p + 12, t->tow == SBF_TOW_UNKNOWN ? 0 : (t->tow % 1000) * 1000000u);
}

typedef struct {
    sbf_status_t *a;
    gnss_status_t *s;
    int64_t now;
    bool overflow;
} epoch_t;
static int8_t elevation_of(const epoch_t *e, uint8_t gnss, uint8_t sv)
{
    const sbf_status_t *a = e->a;
    if (!a->visible_valid || e->now < a->visible_ms || e->now - a->visible_ms > SBF_STATUS_ELEVATION_MS) return 127;
    for (unsigned i = 0; i < a->visible_count; i++)
        if (a->visible[i].gnss == gnss && a->visible[i].sv == sv) return a->visible[i].elevation;
    return 127;
}
// Adds or strengthens an observation; false when the list is full.
static bool observe(gnss_observation_t *list, uint16_t *count, uint8_t gnss, uint8_t sv, uint8_t signal,
                    uint8_t cno, int8_t elevation)
{
    for (unsigned i = 0; i < *count; i++) {
        if (list[i].gnss != gnss || list[i].sv != sv || list[i].signal != signal) continue;
        if (cno > list[i].cno) list[i].cno = cno;
        return true;
    }
    if (*count >= GNSS_OBS_MAX) return false;
    list[(*count)++] = (gnss_observation_t){gnss, sv, signal, cno, elevation};
    return true;
}
static void measurement(const sbf_meas_t *m, void *ctx)
{
    epoch_t *e = ctx;
    uint8_t gnss, sv, signal_gnss, canonical;
    const int centi = sbf_cn0_centi_dbhz(m->cn0, m->signal);
    if (m->antenna != 0 || !m->code_valid || centi < 100 || !sbf_svid(m->svid, &gnss, &sv)) return;
    const uint8_t cno = (uint8_t)(centi / 100 > 255 ? 255 : centi / 100);
    const int8_t elevation = elevation_of(e, gnss, sv);
    gnss_status_t *s = e->s;
    if (!observe(s->satellites, &s->satellite_count, gnss, sv, NR_SATELLITE, cno, elevation)) e->overflow = true;
    if (sbf_signal(m->signal, &signal_gnss, &canonical) && signal_gnss == gnss && canonical != SBF_NO_CANONICAL &&
        !observe(s->signals, &s->signal_count, gnss, sv, canonical, cno, elevation)) e->overflow = true;
}
static bool meas_epoch(sbf_status_t *a, gnss_status_t *s, const uint8_t *b, size_t n, int64_t now)
{
    uint8_t flags = 0;
    if (!sbf_meas_epoch(b, n, &flags, NULL, NULL)) return false;
    if (flags & SBF_MEAS_SCRAMBLED) { a->scrambled_epochs++; return false; }
    s->satellite_count = s->signal_count = 0;
    epoch_t e = {.a = a, .s = s, .now = now};
    (void)sbf_meas_epoch(b, n, NULL, measurement, &e);
    memset(s->tracked, 0, sizeof s->tracked);
    if (e.overflow) {
        s->satellites_valid = s->signals_valid = false;
        s->satellite_count = s->signal_count = 0;
        return true;
    }
    for (unsigned i = 0; i < s->satellite_count; i++)
        if (s->satellites[i].gnss < 8 && s->tracked[s->satellites[i].gnss] < 255) s->tracked[s->satellites[i].gnss]++;
    s->satellites_valid = s->signals_valid = true;
    s->satellites_ms = s->signals_ms = now;
    return true;
}

static void visible(const sbf_sat_t *sat, void *ctx)
{
    sbf_status_t *a = ctx;
    uint8_t gnss, sv;
    if (sat->elevation == -32768 || a->visible_count >= SBF_STATUS_VISIBLE_MAX || !sbf_svid(sat->svid, &gnss, &sv)) return;
    // 0.01 degree to whole degrees, rounded half away from zero; elevations stay within +-90.
    const int degrees = sat->elevation >= 0 ? (sat->elevation + 50) / 100 : -((-sat->elevation + 50) / 100);
    if (degrees < -90 || degrees > 90) return;
    a->visible[a->visible_count++] = (sbf_visible_t){gnss, sv, (int8_t)degrees};
}

static void interference(gnss_status_t *s, const sbf_rf_t *rf, int64_t now)
{
    const uint8_t jam = rf->mitigated || rf->unmitigated ? (gnss_status_position_fresh(s, now) ? 2 : 3) : 1;
    const uint8_t spoof = rf->flags & 0x03 ? 2 : 1;
    // As on the u-blox path: a change of either state, or a first report already raised, is an event.
    if ((s->rf_valid && jam != s->jam) || (!s->rf_valid && jam >= 2)) {
        s->event_count++; s->event_ms = now; s->event_flags = 1; s->event_states = (uint8_t)(jam | (s->spoof << 2));
    }
    s->rf_valid = true; s->rf_ms = now; s->jam = jam;
    if ((s->status_valid && spoof != s->spoof) || (!s->status_valid && spoof >= 2)) {
        s->event_count++; s->event_ms = now; s->event_flags = 2; s->event_states = (uint8_t)(s->jam | (spoof << 2));
    }
    s->status_valid = true; s->status_ms = now; s->spoof = spoof;
}

bool sbf_status_feed(sbf_status_t *a, gnss_status_t *s, const uint8_t *b, size_t n, int64_t now)
{
    if (!b || n < SBF_HEADER) return false;
    switch (sbf_number(b)) {
    case SBF_MEAS_EPOCH:
        return meas_epoch(a, s, b, n, now);
    case SBF_SAT_VISIBILITY: {
        const unsigned before = a->visible_count;
        a->visible_count = 0;
        if (!sbf_sat_visibility(b, n, visible, a)) { a->visible_count = before; return false; }
        a->visible_valid = true;
        a->visible_ms = now;
        return false;
    }
    case SBF_PVT_GEODETIC: {
        sbf_pvt_t pvt;
        if (!sbf_pvt_geodetic(b, n, &pvt)) return false;
        s->fix_valid = pvt.position;
        s->fix_ms = now;
        if (pvt.position) {
            s->latitude = (int32_t)llround(pvt.latitude_deg * 1e7);
            s->longitude = (int32_t)llround(pvt.longitude_deg * 1e7);
        }
        // The calendar follows the fix: this epoch's ReceiverTime, if it came first.
        a->pvt_seen = true;
        a->pvt_tow = pvt.tow;
        if (a->time_seen && a->time.tow == pvt.tow && pvt.tow != SBF_TOW_UNKNOWN) compose_utc(s, &a->time);
        else memset(s->pvt_utc, 0, sizeof s->pvt_utc);
        return true;
    }
    case SBF_RECEIVER_TIME: {
        sbf_time_t t;
        if (!sbf_receiver_time(b, n, &t)) return false;
        a->time = t;
        a->time_seen = true;
        if (a->pvt_seen && a->pvt_tow == t.tow && t.tow != SBF_TOW_UNKNOWN) { compose_utc(s, &t); return true; }
        return false;
    }
    case SBF_RF_STATUS: {
        sbf_rf_t rf;
        if (!sbf_rf_status(b, n, &rf)) return false;
        interference(s, &rf, now);
        return true;
    }
    default:
        return false;
    }
}
