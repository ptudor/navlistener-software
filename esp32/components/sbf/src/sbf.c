#include "sbf.h"
#include <math.h>
#include <string.h>

#define PI 3.14159265358979323846

uint16_t sbf_crc16(const uint8_t *p, size_t n, uint16_t crc)
{
    for (size_t i = 0; i < n; i++) {
        crc ^= (uint16_t)(p[i] << 8);
        for (unsigned bit = 0; bit < 8; bit++) crc = crc & 0x8000 ? (uint16_t)((crc << 1) ^ 0x1021) : (uint16_t)(crc << 1);
    }
    return crc;
}

void sbf_reader_init(sbf_reader_t *r, sbf_block_fn block, sbf_line_fn line, sbf_prompt_fn prompt, void *ctx)
{
    memset(r, 0, sizeof *r);
    r->block = block; r->line = line; r->prompt = prompt; r->ctx = ctx;
}
static void line_reset(sbf_reader_t *r) { r->line_used = 0; r->line_overflow = false; r->line_text[0] = 0; }
// A prompt is a connection descriptor and '>' alone on its line: "COM2>", "USB1>", "STOP>".
static bool prompt_shape(const char *text, size_t n)
{
    if (n < 3 || n > 7 || text[n - 1] != '>') return false;
    for (size_t i = 0; i + 1 < n; i++)
        if (!((text[i] >= 'A' && text[i] <= 'Z') || (text[i] >= '0' && text[i] <= '9'))) return false;
    return true;
}
// One byte outside any block. Replies are printable ASCII with CR/LF line ends; anything else
// between blocks is noise and discards the line it interrupts.
static void text_byte(sbf_reader_t *r, uint8_t c)
{
    if (c == '\r' || c == '\n') {
        if (r->line_used && !r->line_overflow && r->line) r->line(r->line_text, r->ctx);
        line_reset(r);
        return;
    }
    if (c < 0x20 || c > 0x7e) { line_reset(r); return; }
    if (r->line_used >= SBF_LINE_MAX) { r->line_overflow = true; return; }
    r->line_text[r->line_used++] = (char)c;
    r->line_text[r->line_used] = 0;
    if (c == '>' && !r->line_overflow && prompt_shape(r->line_text, r->line_used)) {
        if (r->prompt) r->prompt(r->line_text, r->ctx);
        line_reset(r);
    }
}
// Consumes whole blocks and text from the front of the buffer, keeping a candidate block that
// has not fully arrived. The buffer holds the largest block plus a read, so a candidate at its
// front always completes or fails.
static void process(sbf_reader_t *r)
{
    size_t pos = 0;
    while (pos < r->used) {
        const uint8_t *p = r->buffer + pos;
        const size_t left = r->used - pos;
        if (p[0] == '$') {
            if (left < 2) break;
            if (p[1] == '@') {
                if (left < SBF_HEADER) break;
                const uint16_t length = sbf_u16(p + 6);
                if (length < SBF_HEADER || length % 4 || length > SBF_MAX_BLOCK) {
                    r->bad_lengths++;
                } else if (left < length) {
                    break;
                } else if (sbf_crc16(p + 4, length - 4u, 0) != sbf_u16(p + 2)) {
                    r->crc_errors++;
                } else {
                    line_reset(r);
                    r->blocks++;
                    if (r->block) r->block(p, length, r->ctx);
                    pos += length;
                    continue;
                }
            }
        }
        text_byte(r, p[0]);
        pos++;
    }
    memmove(r->buffer, r->buffer + pos, r->used - pos);
    r->used -= pos;
}
void sbf_reader_feed(sbf_reader_t *r, const uint8_t *data, size_t n)
{
    r->bytes += (uint32_t)n;
    while (n) {
        const size_t room = sizeof r->buffer - r->used;
        const size_t take = n < room ? n : room;
        memcpy(r->buffer + r->used, data, take);
        r->used += take; data += take; n -= take;
        process(r);
    }
}

static double f8(const uint8_t *p)
{
    uint64_t v = 0;
    for (int i = 7; i >= 0; i--) v = v << 8 | p[i];
    double d;
    memcpy(&d, &v, sizeof d);
    return d;
}
static bool block_is(const uint8_t *b, size_t n, size_t minimum, uint16_t number)
{
    return b && n >= minimum && n >= SBF_HEADER && sbf_u16(b + 6) == n && sbf_number(b) == number;
}

bool sbf_pvt_geodetic(const uint8_t *b, size_t n, sbf_pvt_t *o)
{
    // Revision 0 ends with AlertFlag at offset 84; revision 1 adds NrBases to VAccuracy (85-93).
    if (!block_is(b, n, 85, SBF_PVT_GEODETIC)) return false;
    *o = (sbf_pvt_t){.tow = sbf_tow(b), .wnc = sbf_wnc(b), .mode = b[14], .error = b[15], .nr_sv = b[74]};
    const unsigned type = o->mode & 0x0f;
    const bool gnss = type == 1 || type == 2 || (type >= 4 && type <= 8) || type == 10;
    const double lat = f8(b + 16), lon = f8(b + 24), height = f8(b + 32);
    o->position = gnss && o->error == 0 && isfinite(lat) && isfinite(lon) && isfinite(height) &&
                  fabs(lat) <= PI / 2 && fabs(lon) <= PI;
    if (o->position) {
        o->latitude_deg = lat * (180.0 / PI);
        o->longitude_deg = lon * (180.0 / PI);
        o->height_m = height;
    }
    if (sbf_revision(b) >= 1 && n >= 94) {
        o->accuracy = true;
        o->h_accuracy_cm = sbf_u16(b + 90);
        o->v_accuracy_cm = sbf_u16(b + 92);
    }
    return true;
}

bool sbf_receiver_time(const uint8_t *b, size_t n, sbf_time_t *o)
{
    if (!block_is(b, n, 22, SBF_RECEIVER_TIME)) return false;
    *o = (sbf_time_t){.tow = sbf_tow(b), .wnc = sbf_wnc(b), .year = (int8_t)b[14], .month = (int8_t)b[15],
        .day = (int8_t)b[16], .hour = (int8_t)b[17], .minute = (int8_t)b[18], .second = (int8_t)b[19],
        .delta_ls = (int8_t)b[20], .sync = b[21]};
    return true;
}

// Bits 0-4 of Type are the signal number unless 31, when ObsInfo bits 3-7 carry it less 32.
static uint8_t signal_number(uint8_t type, uint8_t obs_info)
{
    const uint8_t low = type & 0x1f;
    return low == 31 ? (uint8_t)(32 + (obs_info >> 3)) : low;
}
bool sbf_meas_epoch(const uint8_t *b, size_t n, uint8_t *common_flags, sbf_meas_fn fn, void *ctx)
{
    if (!block_is(b, n, 20, SBF_MEAS_EPOCH)) return false;
    const unsigned n1 = b[14], sb1 = b[15], sb2 = b[16];
    if (n1 && sb1 < 20) return false;
    size_t off = 20;
    for (unsigned i = 0; i < n1; i++) {
        if (off + sb1 > n) return false;
        const unsigned n2 = b[off + 19];
        off += sb1;
        if (n2 && (sb2 < 12 || off + (size_t)n2 * sb2 > n)) return false;
        off += (size_t)n2 * sb2;
    }
    if (common_flags) *common_flags = b[17];
    off = 20;
    for (unsigned i = 0; i < n1; i++) {
        const uint8_t *t1 = b + off;
        // The pseudorange is absent when CodeMSB and CodeLSB are both zero (note 1).
        const bool code = (t1[3] & 0x0f) || sbf_u32(t1 + 4);
        const sbf_meas_t master = {.svid = t1[2], .signal = signal_number(t1[1], t1[18]), .antenna = t1[1] >> 5,
                                   .cn0 = t1[15], .code_valid = code};
        if (fn) fn(&master, ctx);
        const unsigned n2 = t1[19];
        off += sb1;
        for (unsigned j = 0; j < n2; j++, off += sb2) {
            const uint8_t *t2 = b + off;
            // A slave's pseudorange is the master's plus an offset, absent when CodeOffsetMSB is
            // -4 and CodeOffsetLSB is zero (note 3).
            const bool offset_absent = (t2[3] & 0x07) == 0x04 && sbf_u16(t2 + 6) == 0;
            const sbf_meas_t slave = {.svid = t1[2], .signal = signal_number(t2[0], t2[5]), .antenna = t2[0] >> 5,
                                      .cn0 = t2[2], .code_valid = code && !offset_absent};
            if (fn) fn(&slave, ctx);
        }
    }
    return true;
}
int sbf_cn0_centi_dbhz(uint8_t raw, uint8_t signal)
{
    if (raw == 255) return -1;
    return raw * 25 + (signal == 1 || signal == 2 ? 0 : 1000);
}

bool sbf_sat_visibility(const uint8_t *b, size_t n, sbf_sat_fn fn, void *ctx)
{
    if (!block_is(b, n, 16, SBF_SAT_VISIBILITY)) return false;
    const unsigned count = b[14], size = b[15];
    if (count && (size < 8 || 16 + (size_t)count * size > n)) return false;
    for (unsigned i = 0; i < count; i++) {
        const uint8_t *s = b + 16 + (size_t)i * size;
        const sbf_sat_t sat = {.svid = s[0], .azimuth = sbf_u16(s + 2), .elevation = (int16_t)sbf_u16(s + 4)};
        if (fn) fn(&sat, ctx);
    }
    return true;
}

bool sbf_rf_status(const uint8_t *b, size_t n, sbf_rf_t *o)
{
    if (!block_is(b, n, 20, SBF_RF_STATUS)) return false;
    const unsigned count = b[14], size = b[15];
    if (count && (size < 8 || 20 + (size_t)count * size > n)) return false;
    *o = (sbf_rf_t){.flags = b[16]};
    for (unsigned i = 0; i < count; i++) {
        const unsigned mode = b[20 + (size_t)i * size + 6] & 0x0f;
        if (mode == 2 && o->mitigated < 255) o->mitigated++;
        else if (mode == 8 && o->unmitigated < 255) o->unmitigated++;
    }
    return true;
}

bool sbf_receiver_status(const uint8_t *b, size_t n, sbf_rx_status_t *o)
{
    if (!block_is(b, n, 28, SBF_RECEIVER_STATUS)) return false;
    *o = (sbf_rx_status_t){.cpu_load = b[14], .uptime_s = sbf_u32(b + 16), .rx_state = sbf_u32(b + 20),
                           .rx_error = sbf_u32(b + 24)};
    return true;
}

// A c1[] field: up to its size, ending at the first zero, printable, trailing spaces trimmed.
static void field_text(char *out, const uint8_t *in, size_t size)
{
    size_t n = 0;
    while (n < size && in[n]) { out[n] = in[n] >= 0x20 && in[n] <= 0x7e ? (char)in[n] : '?'; n++; }
    while (n && out[n - 1] == ' ') n--;
    out[n] = 0;
}
bool sbf_receiver_setup(const uint8_t *b, size_t n, sbf_setup_t *o)
{
    if (!block_is(b, n, 216, SBF_RECEIVER_SETUP)) return false;
    *o = (sbf_setup_t){0};
    field_text(o->serial, b + 156, 20);
    field_text(o->name, b + 176, 20);
    field_text(o->version, b + 196, 20);
    if (sbf_revision(b) >= 3 && n >= 368) field_text(o->product, b + 328, 40);
    return true;
}

bool sbf_svid(uint8_t svid, uint8_t *gnss, uint8_t *sv)
{
    static const struct { uint8_t first, last, gnss, offset; } ranges[] = {
        {1, 37, 0, 0},       // GPS
        {38, 61, 6, 37},     // GLONASS R01-R24
        {63, 68, 6, 38},     // GLONASS R25-R30
        {71, 106, 2, 70},    // Galileo
        {120, 140, 1, 0},    // SBAS S120-S140, by PRN
        {141, 180, 3, 140},  // BeiDou C01-C40
        {181, 190, 5, 180},  // QZSS J01-J10
        {191, 197, 7, 190},  // NavIC I01-I07
        {198, 215, 1, 57},   // SBAS S141-S158
        {216, 222, 7, 208},  // NavIC I08-I14
        {223, 245, 3, 182},  // BeiDou C41-C63
    };
    for (size_t i = 0; i < sizeof ranges / sizeof ranges[0]; i++) {
        if (svid < ranges[i].first || svid > ranges[i].last) continue;
        *gnss = ranges[i].gnss;
        *sv = (uint8_t)(svid - ranges[i].offset);
        return true;
    }
    return false;
}

bool sbf_signal(uint8_t number, uint8_t *gnss, uint8_t *canonical)
{
    // Each signal number's constellation and its reception-space ID (the u-blox sigId, data and
    // pilot components merged as nr_signal does); NONE where that space has no counterpart.
    enum { NONE = SBF_NO_CANONICAL, RESERVED = 254 };
    static const uint8_t table[][2] = {
        {0, 0},          // 0  GPS L1C/A
        {0, NONE},       // 1  GPS L1P
        {0, NONE},       // 2  GPS L2P
        {0, 3},          // 3  GPS L2C (L2 CL/CM)
        {0, 6},          // 4  GPS L5 (L5 I/Q)
        {0, NONE},       // 5  GPS L1C
        {5, 0},          // 6  QZSS L1C/A
        {5, 4},          // 7  QZSS L2C (L2 CM/CL)
        {6, 0},          // 8  GLONASS L1C/A (L1 OF)
        {6, NONE},       // 9  GLONASS L1P
        {6, NONE},       // 10 GLONASS L2P
        {6, 2},          // 11 GLONASS L2C/A (L2 OF)
        {6, NONE},       // 12 GLONASS L3
        {3, 5},          // 13 BeiDou B1C (B1 Cp/Cd)
        {3, 8},          // 14 BeiDou B2a (B2 ap/ad)
        {7, 0},          // 15 NavIC L5 (L5 A)
        {RESERVED, 0},   // 16
        {2, 0},          // 17 Galileo E1 (E1 C/B)
        {RESERVED, 0},   // 18
        {2, NONE},       // 19 Galileo E6
        {2, 3},          // 20 Galileo E5a (E5 aI/aQ)
        {2, 5},          // 21 Galileo E5b (E5 bI/bQ)
        {2, NONE},       // 22 Galileo E5 AltBOC
        {RESERVED, 0},   // 23 L-band (MSS)
        {1, 0},          // 24 SBAS L1C/A
        {1, NONE},       // 25 SBAS L5
        {5, 8},          // 26 QZSS L5 (L5 I/Q)
        {5, NONE},       // 27 QZSS L6
        {3, 0},          // 28 BeiDou B1I
        {3, 2},          // 29 BeiDou B2I
        {3, NONE},       // 30 BeiDou B3I
        {RESERVED, 0},   // 31
        {5, NONE},       // 32 QZSS L1C
        {5, 1},          // 33 QZSS L1S
        {3, NONE},       // 34 BeiDou B2b
        {RESERVED, 0},   // 35
        {RESERVED, 0},   // 36
        {7, NONE},       // 37 NavIC L1
        {5, NONE},       // 38 QZSS L1CB
        {5, NONE},       // 39 QZSS L5S
    };
    if (number >= sizeof table / sizeof table[0] || table[number][0] == RESERVED) return false;
    *gnss = table[number][0];
    *canonical = table[number][1];
    return true;
}
