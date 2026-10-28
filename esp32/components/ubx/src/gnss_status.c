#include "gnss_status.h"
#include <math.h>
#include <string.h>
static uint32_t le32(const uint8_t *p)
{ return p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24); }
static uint16_t le16(const uint8_t *p) { return p[0] | (uint16_t)(p[1] << 8); }
static bool fresh(int64_t then, int64_t now) { return now >= then && now - then <= 15000; }
const char *const gnss_band_names[GNSS_BANDS] = {"L1", "L2", "L5", "E5b", "E6"};
const gnss_signal_kind_t gnss_signal_kinds[GNSS_SIGNAL_KINDS] = {
    {0, GNSS_BAND_L1, "L1C/A"}, {0, GNSS_BAND_L2, "L2C"}, {0, GNSS_BAND_L5, "L5"},
    {1, GNSS_BAND_L1, "L1C/A"},
    {2, GNSS_BAND_L1, "E1"}, {2, GNSS_BAND_L5, "E5a"}, {2, GNSS_BAND_E5B, "E5b"}, {2, GNSS_BAND_E6, "E6"},
    {3, GNSS_BAND_L1, "B1I"}, {3, GNSS_BAND_E5B, "B2I"}, {3, GNSS_BAND_E6, "B3I"}, {3, GNSS_BAND_L1, "B1C"},
    {3, GNSS_BAND_L5, "B2a"},
    {5, GNSS_BAND_L1, "L1C/A"}, {5, GNSS_BAND_L1, "L1S"}, {5, GNSS_BAND_L2, "L2C"}, {5, GNSS_BAND_L5, "L5"},
    {5, GNSS_BAND_L1, "L1C/B"},
    {6, GNSS_BAND_L1, "L1OF"}, {6, GNSS_BAND_L2, "L2OF"},
    {7, GNSS_BAND_L5, "L5"},
};
int gnss_signal_kind(uint8_t g, uint8_t sig)
{
    // Rows in gnss_signal_kinds order; each pair of halves (I/Q, data/pilot, B/C/A) is one kind.
    static const struct { uint8_t gnss, sig, kind; } ids[] = {
        {0,0,0}, {0,3,1}, {0,4,1}, {0,6,2}, {0,7,2},
        {1,0,3},
        {2,0,4}, {2,1,4}, {2,3,5}, {2,4,5}, {2,5,6}, {2,6,6}, {2,8,7}, {2,9,7}, {2,10,7},
        {3,0,8}, {3,1,8}, {3,2,9}, {3,3,9}, {3,4,10}, {3,10,10}, {3,5,11}, {3,6,11}, {3,7,12}, {3,8,12},
        {5,0,13}, {5,1,14}, {5,4,15}, {5,5,15}, {5,8,16}, {5,9,16}, {5,12,17},
        {6,0,18}, {6,2,19},
        {7,0,20},
    };
    for (size_t i = 0; i < sizeof ids / sizeof ids[0]; i++)
        if (ids[i].gnss == g && ids[i].sig == sig) return ids[i].kind;
    return -1;
}
// A NAV-SIG row for a code-locked signal (qualityInd 4-7) with C/N0, health aside.
static bool locked_row(const uint8_t *r) { return r[0] < 8 && r[0] != 4 && r[1] && r[6] && r[7] >= 4 && r[7] <= 7; }
// One jamming reading. The first makes the baseline, which is an event only as an alarm.
static void jam_reading(gnss_status_t *s, uint8_t jam, bool first, int64_t now)
{
    if (first ? jam >= 2 : jam != s->jam) {
        s->event_count++; s->event_ms = now; s->event_flags = 1;
        s->event_states = jam | (s->spoof << 2);
    }
    s->rf_valid = true; s->rf_ms = now; s->jam = jam;
}
bool gnss_status_position_fresh(const gnss_status_t *s, int64_t now)
{ return s->fix_valid && fresh(s->fix_ms, now); }
void gnss_status_feed(gnss_status_t *s, uint8_t cls, uint8_t id,
                      const uint8_t *p, size_t len, int64_t now)
{
    if (!p) return;
    if (cls == 0x0d && id == 1 && len == 16 && !(p[14] & 0xc0) && le32(p) < 604800000) {
        s->tp_valid=true; s->tp_flags=p[14]; s->tp_ref=p[15]; s->tp_ms=now;
    } else if (cls == 0x0a && id == 0x38 && len >= 4 && p[0] == 0 && p[1] && len == 4u + 24u*p[1]) {
        uint8_t jam = 0;
        s->rf_blocks = 0;
        for (size_t i = 4; i < len; i += 24) {
            if ((p[i+1] & 3) > jam) jam = p[i+1] & 3;
            if (s->rf_blocks < GNSS_RF_BLOCKS_MAX)
                s->rf_block[s->rf_blocks++] = (gnss_rf_block_t){.id = p[i], .ant_status = p[i+2], .ant_power = p[i+3],
                    .cw_suppression = p[i+16], .noise_per_ms = le16(p+i+12), .agc = le16(p+i+14)};
        }
        s->rf_blocks_valid = true; s->rf_blocks_ms = now;
        if (!s->sec_sig) jam_reading(s, jam, !s->rf_valid, now);
    } else if (cls == 0x27 && id == 9 && len >= 4 && (p[0] == 2 || p[0] == 3) && len == 4u + 4u*p[3]) {
        // SEC-SIG jamState (0 unknown, 1 none, 2 warning) while jamDetEnabled is set.
        const uint8_t jam = (p[1] & 1) ? (p[1] >> 1) & 3 : 0;
        const bool first = !s->sec_sig_known;
        s->sec_sig = true;
        if (jam) s->sec_sig_known = true;
        jam_reading(s, jam, first, now);
    } else if (cls == 1 && id == 3 && len == 16) {
        uint8_t spoof = (p[7] >> 3) & 3;
        if ((s->status_valid && spoof != s->spoof) || (!s->status_valid && spoof >= 2)) {
            s->event_count++; s->event_ms = now; s->event_flags = 2;
            s->event_states = s->jam | (spoof << 2);
        }
        s->status_valid = true; s->status_ms = now; s->spoof = spoof;
    } else if (cls == 0x0a && id == 4 && len >= 40 && (len - 40) % 30 == 0) {
        const char *names[] = {"GPS", "SBAS", "GAL", "BDS", "", "QZSS", "GLO", "NAVIC"};
        uint8_t supported = 0;
        for (size_t i = 40; i + 30 <= len; i += 30) {
            // Capability extensions are semicolon-separated exact tokens.
            size_t start = 0;
            for (size_t j = 0; j < 30; j++) {
                if (p[i+j] != ';' && p[i+j] != 0) continue;
                for (unsigned g = 0; g < 8; g++)
                    if (names[g][0] && strlen(names[g]) == j - start &&
                        !memcmp(p+i+start, names[g], j-start)) supported |= 1u << g;
                if (!p[i+j]) break;
                start = j + 1;
            }
        }
        if (supported) s->supported = supported;
    } else if (cls == 1 && id == 0x35 && len >= 8 && p[4] == 1 && len == 8u + 12u * p[5]) {
        memset(s->tracked, 0, sizeof s->tracked);
        s->satellite_count=0;
        if(p[5]>GNSS_OBS_MAX) {s->satellites_valid=false;return;}
        for (size_t i = 8; i < len; i += 12) {
            unsigned g = p[i], quality = p[i+8] & 7, health = (p[i+8] >> 4) & 3;
            // Acquired signals alone do not prove usable tracking. Require code
            // lock, nonzero C/N0, and no explicit unhealthy indication.
            if (g < 8 && g!=4 && p[i+1] && p[i+2] && quality >= 4 && health != 2) {
                s->tracked[g]++;
                s->satellites[s->satellite_count++]=(gnss_observation_t){g,p[i+1],NR_SATELLITE,p[i+2],(int8_t)p[i+3]};
            }
        }
        s->satellites_ms = now; s->satellites_valid = true;
    } else if(cls==1 && id==0x43 && len>=8 && p[4]==0 && len==8u+16u*p[5]) {
        s->signal_count=0;
        memset(s->signal_locked,0,sizeof s->signal_locked);memset(s->signal_unhealthy,0,sizeof s->signal_unhealthy);
        s->signal_other=0;
        if(p[5]>GNSS_OBS_MAX) {s->signals_valid=false;return;}
        for(size_t i=8;i<len;i+=16) {
            if(!locked_row(p+i)) continue;
            const int kind=gnss_signal_kind(p[i],p[i+2]);
            bool repeat=false; // the other half of a pair, from the same satellite
            for(size_t j=8;j<i && !repeat;j+=16)
                repeat=locked_row(p+j) && p[j]==p[i] && p[j+1]==p[i+1] && gnss_signal_kind(p[j],p[j+2])==kind && kind>=0;
            if(repeat) continue;
            if(kind<0) {if(s->signal_other<255)s->signal_other++;continue;}
            if(s->signal_locked[kind]<255)s->signal_locked[kind]++;
            if((p[i+10]&3)==2 && s->signal_unhealthy[kind]<255)s->signal_unhealthy[kind]++;
        }
        for(size_t i=8;i<len;i+=16) {
            uint8_t g=p[i],sv=p[i+1],sig=nr_signal(g,p[i+2]);
            if(g>7 || g==4 || !sv || sig>31 || !p[i+6] || p[i+7]<4 || p[i+7]>7 || (p[i+10]&3)==2) continue;
            bool duplicate=false;
            for(unsigned j=0;j<s->signal_count;j++) if(s->signals[j].gnss==g && s->signals[j].sv==sv && s->signals[j].signal==sig) duplicate=true;
            if(!duplicate) {
                int8_t elevation=127;
                if(s->satellites_valid && fresh(s->satellites_ms,now))
                    for(unsigned j=0;j<s->satellite_count;j++) if(s->satellites[j].gnss==g && s->satellites[j].sv==sv) {
                        elevation=s->satellites[j].elevation;break;
                    }
                s->signals[s->signal_count++]=(gnss_observation_t){g,sv,sig,p[i+6],elevation};
            }
        }
        s->signals_ms=now;s->signals_valid=true;
    } else if (cls == 1 && id == 7 && len == 92) {
        memcpy(s->pvt_utc, p + 4, sizeof s->pvt_utc);
        int32_t lon = (int32_t)le32(p+24), lat = (int32_t)le32(p+28);
        s->fix_valid = (p[21] & 1) && p[20] >= 2 && p[20] <= 4 && !(p[78] & 1) &&
                       lat >= -900000000 && lat <= 900000000 &&
                       lon >= -1800000000 && lon <= 1800000000;
        s->fix_ms = now;
        if (s->fix_valid) { s->latitude = lat; s->longitude = lon; }
    }
}
void gnss_status_compare(const gnss_status_t *s,const nr_expectation_t *e,nr_sample_t *r,int64_t now)
{
    memset(r->matched,0,sizeof r->matched);r->valid=s->supported;
    if(gnss_status_position_fresh(s,now)) {
        double dy=(s->latitude/1e7-e->latitude/1e7)*111320.0;
        double dx=s->longitude/1e7-e->longitude/1e7;
        if(dx>180)dx-=360;
        if(dx < -180)dx+=360;
        dx*=111320.0*cos(e->latitude/1e7*0.017453292519943295);
        if(dx*dx+dy*dy>(double)e->radius_m*e->radius_m) {r->valid=0;return;}
    }
    for(unsigned i=0;i<e->count;i++) {
        const nr_entry_t *v=&e->entries[i];bool sat=v->signal==NR_SATELLITE;
        if(!(sat ? s->satellites_valid && fresh(s->satellites_ms,now) : s->signals_valid && fresh(s->signals_ms,now))) {
            r->valid&=~(1u<<v->gnss);continue;
        }
        const gnss_observation_t *obs=sat ? s->satellites : s->signals;
        unsigned n=sat ? s->satellite_count : s->signal_count;
        for(unsigned j=0;j<n;j++) if(obs[j].gnss==v->gnss && obs[j].sv==v->sv && obs[j].signal==v->signal) {
            r->matched[i/8]|=1u<<(i%8);break;
        }
    }
}
bool gnss_status_same_place(int32_t lat_a, int32_t lon_a, int32_t lat_b, int32_t lon_b)
{
    const double radians = 0.017453292519943295;
    double dy = ((double)lat_a - lat_b) / 1e7;
    double dx = ((double)lon_a - lon_b) / 1e7;
    if (dx > 180) dx -= 360;
    if (dx < -180) dx += 360;
    dx *= cos(((double)lat_a + lat_b) / 2e7 * radians);
    return dx * dx + dy * dy <= 0.0405; // approximately 22 km; fixed installation history
}
static bool box(double lat, double lon, double south, double north, double west, double east)
{ return lat >= south && lat <= north && lon >= west && lon <= east; }
uint8_t gnss_status_expected(const gnss_status_t *s, int64_t now, uint8_t learned)
{
    uint8_t expected = s->supported;
    if (!gnss_status_position_fresh(s, now)) return expected;
    double lat = s->latitude / 1e7, lon = s->longitude / 1e7;
    // Broad service-region envelopes are a panel hint, not RF footprints or
    // navigation integrity limits. Actual reception overrides every envelope.
    // SBAS represents any regional service, not WAAS alone.
    bool sbas = box(lat, lon, 5, 75, -170, -45) ||     // North America
                box(lat, lon, 15, 75, -40, 65) ||     // Europe and surrounding region
                box(lat, lon, -10, 45, 35, 115) ||    // India and surrounding region
                box(lat, lon, -10, 65, 105, 175) ||   // East Asia
                box(lat, lon, -55, 5, 100, 180);      // Australia / New Zealand
    double east_lon = lon < 0 ? lon + 360 : lon;
    bool qzss = box(lat, east_lon, -60, 70, 70, 220);
    bool navic = box(lat, lon, -20, 50, 40, 115);
    if (!sbas) expected &= ~(1u << 1);
    if (!qzss) expected &= ~(1u << 5);
    if (!navic) expected &= ~(1u << 7);
    return expected | (learned & s->supported & GNSS_REGIONAL_MASK);
}
void gnss_status_leds(const gnss_status_t *s, int64_t now, uint8_t learned,
                      bool uplink, uint8_t *green, uint8_t *yellow)
{
    const unsigned gnss[] = NR_PANEL_GNSS; // column i shows this gnssId; bit 7 is the uplink
    uint8_t expected = gnss_status_expected(s, now, learned);
    *green = uplink ? 0x80 : 0; *yellow = uplink ? 0 : 0x80;
    bool current = s->satellites_valid && fresh(s->satellites_ms, now);
    for (unsigned i = 0; i < 7; i++) {
        unsigned g = gnss[i];
        if (!(s->supported & (1u << g))) continue;
        if (current && s->tracked[g]) *green |= 1u << i;
        else if (expected & (1u << g)) *yellow |= 1u << i;
    }
}
