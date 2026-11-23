#include "oled_gnss.h"
#include <string.h>

static bool fresh(int64_t then, int64_t now) { return now >= then && now - then <= 15000; }

void oled_gnss_from_status(oled_gnss_t *out, const gnss_status_t *s, int64_t now)
{
    *out = (oled_gnss_t){.supported = s->supported};
    const bool satellites = s->satellites_valid && fresh(s->satellites_ms, now);
    // fix_ms is the uptime of the last navigation solution, valid or not; 0 before the first.
    const bool solution = s->fix_ms > 0 && fresh(s->fix_ms, now);
    out->solution_fresh = solution;
    out->receiving = satellites || solution;

    // NAV-PVT bytes 20..23 are pvt_utc[16..19]: fixType, flags, flags2 and numSV. A Septentrio
    // receiver's status leaves them zero and gives the position's validity alone.
    const uint8_t type = s->pvt_utc[16], flags = s->pvt_utc[17];
    if (gnss_status_position_fresh(s, now)) {
        const unsigned carrier = flags >> 6;
        out->fix = carrier == 2 ? OLED_FIX_RTK_FIXED : carrier == 1 ? OLED_FIX_RTK_FLOAT :
                   flags & 2 ? OLED_FIX_DGNSS : type == 2 ? OLED_FIX_2D : type == 3 ? OLED_FIX_3D :
                   type == 4 ? OLED_FIX_DEAD_RECKONING : OLED_FIX_POSITION;
        out->used = s->pvt_utc[19];
        out->accuracy_valid = s->h_acc_mm != 0;
        out->h_acc_mm = s->h_acc_mm;
        out->p_dop_centi = s->p_dop;
    } else if (solution && type == 5 && (flags & 1)) {
        out->fix = OLED_FIX_TIME;
    }

    if (satellites) {
        out->satellites_valid = true;
        memcpy(out->tracked, s->tracked, sizeof out->tracked);
        for (unsigned g = 0; g < 8; g++) out->tracked_total += s->tracked[g];
        // One bar for each satellite: the constellations in the LED panel's column order, and
        // within each by satellite number, the smallest above the last taken until none is left.
        static const uint8_t order[] = {1, 0, 2, 3, 6, 5, 7};
        for (unsigned o = 0; o < sizeof order; o++) {
            for (int last = -1;;) {
                const gnss_observation_t *next = NULL;
                for (unsigned i = 0; i < s->satellite_count; i++) {
                    const gnss_observation_t *seen = &s->satellites[i];
                    if (seen->gnss == order[o] && seen->sv > last && (!next || seen->sv < next->sv)) next = seen;
                }
                if (!next) break;
                last = next->sv;
                if (next->cno > out->cno_max) out->cno_max = next->cno;
                if (out->bars < OLED_BARS) {
                    out->bar[out->bars].gnss = next->gnss;
                    out->bar[out->bars].cno = next->cno;
                    out->bars++;
                }
            }
        }
    }

    if (s->signals_valid && fresh(s->signals_ms, now)) {
        out->signals_valid = true;
        for (unsigned k = 0; k < GNSS_SIGNAL_KINDS; k++) {
            const unsigned band = gnss_signal_kinds[k].band;
            if (band >= OLED_BANDS) continue;
            const unsigned sum = out->band[band] + s->signal_locked[k];
            out->band[band] = (uint8_t)(sum > 255 ? 255 : sum);
        }
    }

    // The antenna supervisor of every RF block (UBX-MON-RF: 2 OK, 3 SHORT, 4 OPEN): a short or
    // an open circuit on any block is the antenna's state, and OK only when every block says so.
    if (s->rf_blocks_valid && s->rf_blocks && fresh(s->rf_blocks_ms, now)) {
        bool ok = true, shorted = false, open = false;
        for (unsigned b = 0; b < s->rf_blocks; b++) {
            ok = ok && s->rf_block[b].ant_status == 2;
            shorted = shorted || s->rf_block[b].ant_status == 3;
            open = open || s->rf_block[b].ant_status == 4;
        }
        out->antenna = shorted ? OLED_ANTENNA_SHORT : open ? OLED_ANTENNA_OPEN : ok ? OLED_ANTENNA_OK : OLED_ANTENNA_UNKNOWN;
    }
    if (s->rf_valid && fresh(s->rf_ms, now)) {
        // The state the ATTENTION lamp follows, and beside it the period of a source whose
        // crossings that state leaves out (jam_pulse.h).
        out->jam = s->jam_counted & 3;
        if (s->pulse.known) {
            const int32_t seconds = (s->pulse.period_ms + 500) / 1000;
            out->pulse_s = (uint8_t)(seconds < 1 ? 1 : seconds > 99 ? 99 : seconds);
        }
        if (s->sec_sig) {
            out->security_fresh = true;
            out->assessed = s->centers_assessed;
            out->jammed = s->centers_jammed;
            _Static_assert(sizeof out->jammed_khz == sizeof s->jammed_khz, "display frequency capacity");
            memcpy(out->jammed_khz, s->jammed_khz, sizeof out->jammed_khz);
        }
    }
    if (s->status_valid && fresh(s->status_ms, now)) out->spoof = s->spoof & 3;
}
