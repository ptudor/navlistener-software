#include "gnss_status.h"
#include <math.h>
#include <string.h>
static uint32_t le32(const uint8_t *p)
{ return p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24); }
static bool fresh(int64_t then, int64_t now) { return now >= then && now - then <= 15000; }
bool gnss_status_position_fresh(const gnss_status_t *s, int64_t now)
{ return s->fix_valid && fresh(s->fix_ms, now); }
void gnss_status_feed(gnss_status_t *s, uint8_t cls, uint8_t id,
                      const uint8_t *p, size_t len, int64_t now)
{
    if (!p) return;
    if (cls == 0x0a && id == 4 && len >= 40 && (len - 40) % 30 == 0) {
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
        for (size_t i = 8; i < len; i += 12) {
            unsigned g = p[i], quality = p[i+8] & 7, health = (p[i+8] >> 4) & 3;
            // Acquired signals alone do not prove usable tracking. Require code
            // lock, nonzero C/N0, and no explicit unhealthy indication.
            if (g < 8 && p[i+2] && quality >= 4 && health != 2) s->tracked[g]++;
        }
        s->satellites_ms = now; s->satellites_valid = true;
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
    const unsigned gnss[] = {0, 1, 2, 3, 5, 6, 7};
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
