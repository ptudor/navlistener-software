#include "gnss_status.h"
#include "../../../../common/receiver_rf.h"
#include <math.h>
#include <stdio.h>
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
uint8_t gnss_status_counted_jam(const gnss_status_t *s, int64_t now)
{
    return s->jam == 2 && jam_pulse_explains(&s->pulse, now) ? 1 : s->jam;
}
static void set_counted_jam(gnss_status_t *s, uint8_t counted, bool first, int64_t now)
{
    if (first ? counted >= 2 : counted != s->jam_counted) {
        s->event_count++; s->event_ms = now; s->event_flags = 1;
        s->event_states = counted | (s->spoof << 2);
    }
    s->jam_counted = counted;
}
// A crossing is explained for a bounded time even if the next RF report is missing. Ordinary
// receiver traffic advances the cached event state; displays and ATTENTION also calculate it
// directly for the time at which they are read.
static void jam_tick(gnss_status_t *s, int64_t now)
{
    if (!s->rf_valid || !fresh(s->rf_ms, now)) return;
    set_counted_jam(s, gnss_status_counted_jam(s, now), false, now);
}
// One jamming reading, with a signature of the frequencies it flags. The first makes the
// baseline, which is an event only as an alarm. A warning-level crossing of a periodic source
// the tracker knows is no event and no cause for attention: the state those follow leaves it
// out, and takes it in again if the flags outlast a crossing. Critical jamming never trains
// the tracker and always counts at once.
static void jam_reading(gnss_status_t *s, uint8_t jam, uint32_t signature, bool first, int64_t now)
{
    if (jam == 3) {
        // Do not let a transition from warning to critical finish and teach a candidate
        // crossing. The critical report cancels any warning episode without becoming one.
        s->pulse.up = false;
        s->pulse.expected = false;
        s->pulse.mixed = false;
    } else {
        (void)jam_pulse_feed(&s->pulse, jam == 2, signature, now);
    }
    s->rf_valid = true; s->rf_ms = now; s->jam = jam;
    set_counted_jam(s, gnss_status_counted_jam(s, now), first, now);
}
bool gnss_status_position_fresh(const gnss_status_t *s, int64_t now)
{ return s->fix_valid && fresh(s->fix_ms, now); }
void gnss_status_feed(gnss_status_t *s, uint8_t cls, uint8_t id,
                      const uint8_t *p, size_t len, int64_t now)
{
    if (!p) return;
    const bool mon_rf = cls == 0x0a && id == 0x38 && len >= 4 && p[0] == 0 && p[1] && len == 4u + 24u*p[1];
    const bool sec_sig = cls == 0x27 && id == 9 && rf_sec_sig_valid(p, len);
    if (!sec_sig && !(mon_rf && !s->sec_sig)) jam_tick(s, now);
    if (cls == 0x0d && id == 1 && len == 16 && !(p[14] & 0xc0) && le32(p) < 604800000) {
        s->tp_valid=true; s->tp_flags=p[14]; s->tp_ref=p[15]; s->tp_ms=now;
    } else if (mon_rf) {
        uint8_t jam = 0;
        s->rf_blocks = 0;
        for (size_t i = 4; i < len; i += 24) {
            if ((p[i+1] & 3) > jam) jam = p[i+1] & 3;
            if (s->rf_blocks < GNSS_RF_BLOCKS_MAX)
                s->rf_block[s->rf_blocks++] = (gnss_rf_block_t){.id = p[i], .ant_status = p[i+2], .ant_power = p[i+3],
                    .cw_suppression = p[i+16], .noise_per_ms = le16(p+i+12), .agc = le16(p+i+14)};
        }
        s->rf_blocks_valid = true; s->rf_blocks_ms = now;
        if (!s->sec_sig) jam_reading(s, jam, 1, !s->rf_valid, now); // MON-RF names no frequency
    } else if (sec_sig) {
        // SEC-SIG jamState (0 unknown, 1 none, 2 warning) while jamDetEnabled is set.
        const uint8_t jam = (p[1] & 1) ? (p[1] >> 1) & 3 : 0;
        const bool first = !s->sec_sig_known;
        s->sec_sig = true;
        s->centers_assessed = p[3];
        s->centers_jammed = 0;
        // The signature is of the first 32 frequencies flagged: more than any receiver assesses.
        uint32_t flagged[32];
        size_t signed_for = 0;
        for (size_t i = 4; i < len; i += 4) {
            uint32_t word = rf_le32(p + i);
            if (!(word & RF_JAMMED)) continue;
            if (s->centers_jammed < sizeof s->jammed_khz / sizeof *s->jammed_khz)
                s->jammed_khz[s->centers_jammed] = word & RF_CENTER_KHZ;
            if (signed_for < sizeof flagged / sizeof flagged[0]) flagged[signed_for++] = word & RF_CENTER_KHZ;
            s->centers_jammed++;
        }
        if (signed_for) {
            s->flagged_last = (uint8_t)(signed_for < 8 ? signed_for : 8);
            memcpy(s->flagged_last_khz, flagged, s->flagged_last * sizeof flagged[0]);
        }
        if (jam) s->sec_sig_known = true;
        jam_reading(s, jam, jam_pulse_signature(flagged, signed_for), first, now);
    } else if (cls == 1 && id == 3 && len == 16) {
        uint8_t spoof = (p[7] >> 3) & 3;
        if ((s->status_valid && spoof != s->spoof) || (!s->status_valid && spoof >= 2)) {
            s->event_count++; s->event_ms = now; s->event_flags = 2;
            s->event_states = gnss_status_counted_jam(s, now) | (spoof << 2);
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
        s->h_acc_mm = s->fix_valid ? le32(p+40) : 0;
        s->p_dop = s->fix_valid ? le16(p+76) : 0;
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

uint8_t gnss_status_attention(const gnss_status_t *s, int64_t now)
{
    uint8_t flags = 0;
    // fix_ms is the uptime of the last navigation solution, valid or not; 0 before the first.
    const bool reporting = (s->satellites_valid && fresh(s->satellites_ms, now)) || (s->fix_ms > 0 && fresh(s->fix_ms, now));
    if (!reporting && now >= GNSS_ATTENTION_START_MS) flags |= GNSS_ATTENTION_SILENT;
    if (s->rf_blocks_valid && fresh(s->rf_blocks_ms, now))
        for (unsigned b = 0; b < s->rf_blocks; b++) {
            if (s->rf_block[b].ant_status == 3) flags |= GNSS_ATTENTION_ANTENNA_SHORT;
            if (s->rf_block[b].ant_status == 4) flags |= GNSS_ATTENTION_ANTENNA_OPEN;
        }
    if (s->rf_valid && fresh(s->rf_ms, now)) {
        const uint8_t jam = gnss_status_counted_jam(s, now) & 3;
        if (jam == 2) flags |= GNSS_ATTENTION_JAMMING;
        if (jam == 3) flags |= GNSS_ATTENTION_JAMMING_CRITICAL;
    }
    if (s->status_valid && fresh(s->status_ms, now) && (s->spoof & 3) >= 2) flags |= GNSS_ATTENTION_SPOOFING;
    return flags;
}

size_t gnss_status_attention_text(uint8_t flags, char *out, size_t size)
{
    static const struct { uint8_t flag; const char *text; } names[] = {
        {GNSS_ATTENTION_SPOOFING, "spoofing indicated"}, {GNSS_ATTENTION_JAMMING_CRITICAL, "jamming critical"},
        {GNSS_ATTENTION_JAMMING, "jamming warning"}, {GNSS_ATTENTION_ANTENNA_SHORT, "antenna short"},
        {GNSS_ATTENTION_ANTENNA_OPEN, "antenna open"}, {GNSS_ATTENTION_SILENT, "receiver silent"},
    };
    size_t used = 0;
    if (!size) return 0;
    out[0] = 0;
    for (size_t i = 0; i < sizeof names / sizeof names[0]; i++) {
        if (!(flags & names[i].flag)) continue;
        const int n = snprintf(out + used, size - used, "%s%s", used ? ", " : "", names[i].text);
        if (n < 0 || (size_t)n >= size - used) { out[used] = 0; return used; } // what fits whole
        used += (size_t)n;
    }
    if (!used) {
        const int n = snprintf(out, size, "none");
        used = n > 0 && (size_t)n < size ? (size_t)n : 0;
        if (!used) out[0] = 0;
    }
    return used;
}

// RINEX's letter for each UBX gnssId, in the order the lists are written.
static const struct { uint8_t gnss; char letter; } order[] = {
    {0, 'G'}, {6, 'R'}, {2, 'E'}, {3, 'C'}, {5, 'J'}, {7, 'I'}, {1, 'S'},
};
// An elevation the receiver gave: one inside the range elevations have.
static bool elevation_given(int8_t elevation) { return elevation >= -90 && elevation <= 90; }
size_t gnss_status_satellites_text(const gnss_status_t *s, char *out, size_t size)
{
    size_t used = 0;
    if (!size) return 0;
    out[0] = 0;
    if (!s->satellites_valid) return 0;
    for (size_t g = 0; g < sizeof order / sizeof order[0]; g++) {
        // Ascending by satellite number: the smallest one above the last written, until none is left.
        for (int last = -1;;) {
            const gnss_observation_t *next = NULL;
            for (unsigned i = 0; i < s->satellite_count; i++) {
                const gnss_observation_t *o = &s->satellites[i];
                if (o->gnss == order[g].gnss && o->sv > last && (!next || o->sv < next->sv)) next = o;
            }
            if (!next) break;
            last = next->sv;
            char entry[24], elevation[8] = "-";
            if (elevation_given(next->elevation)) snprintf(elevation, sizeof elevation, "%d", (int)next->elevation);
            const int length = snprintf(entry, sizeof entry, "%s%c%02u %s/%u", used ? ", " : "", order[g].letter,
                                        (unsigned)next->sv, elevation, (unsigned)next->cno);
            if (length < 0 || (size_t)length >= sizeof entry || used + (size_t)length >= size) return used;
            memcpy(out + used, entry, (size_t)length + 1);
            used += (size_t)length;
        }
    }
    return used;
}
size_t gnss_status_signals_json(const gnss_status_t *s, char *out, size_t size)
{
    if (!size) return 0;
    out[0] = 0;
    if (!s->signals_valid || size < sizeof "[]") return 0;
    size_t used = 0;
    out[used++] = '[';
    // Ascending by system, satellite and signal: the smallest key above the last written,
    // until none is left. Halves of one signal share a key, and the first of them is kept.
    for (long last = -1;;) {
        const gnss_observation_t *next = NULL;
        long next_key = 0;
        int next_kind = -1;
        char letter = 0;
        for (unsigned i = 0; i < s->signal_count; i++) {
            const gnss_observation_t *o = &s->signals[i];
            size_t g = 0;
            while (g < sizeof order / sizeof order[0] && order[g].gnss != o->gnss) g++;
            if (g == sizeof order / sizeof order[0]) continue;
            const int kind = gnss_signal_kind(o->gnss, o->signal);
            const long key = ((long)g << 16) | ((long)o->sv << 8) | (long)(kind >= 0 ? kind : (int)GNSS_SIGNAL_KINDS + o->signal);
            if (key > last && (!next || key < next_key)) { next = o; next_key = key; next_kind = kind; letter = order[g].letter; }
        }
        if (!next) break;
        last = next_key;
        char entry[40], name[8], elevation[8] = "null";
        if (next_kind >= 0) snprintf(name, sizeof name, "%s", gnss_signal_kinds[next_kind].name);
        else snprintf(name, sizeof name, "sig%u", (unsigned)next->signal);
        if (elevation_given(next->elevation)) snprintf(elevation, sizeof elevation, "%d", (int)next->elevation);
        const int length = snprintf(entry, sizeof entry, "%s[\"%c%02u\",\"%s\",%u,%s]", used > 1 ? "," : "", letter,
                                    (unsigned)next->sv, name, (unsigned)next->cno, elevation);
        // The closing bracket and the NUL keep their room.
        if (length < 0 || (size_t)length >= sizeof entry || used + (size_t)length + sizeof "]" > size) break;
        memcpy(out + used, entry, (size_t)length);
        used += (size_t)length;
    }
    out[used++] = ']';
    out[used] = 0;
    return used;
}
