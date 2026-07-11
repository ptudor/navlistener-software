// gnf1 — GNF1 framing + record encoding. See include/gnf1.h.
// Byte-for-byte matched to ../../../go/internal/wire/wire.go and ../../../feeder/navfeeder.c.

#include "gnf1.h"

#include <stdio.h>
#include <string.h>

uint8_t gnf1_frame_type(unsigned gnss_id, unsigned sig_id)
{
    switch (gnss_id) {
    case 0: return sig_id == 0 ? 0x10 : 0x11;                        // GPS: LNAV / CNAV
    case 5:                                                         // QZSS: shipped LNAV/CNAV only
        if (sig_id == 0) return 0x50;
        if (sig_id == 4 || sig_id == 5 || sig_id == 8 || sig_id == 9) return 0x51;
        return 0;
    case 2: return (sig_id == 3 || sig_id == 4) ? 0x21 : 0x20;       // Galileo: F/NAV / I/NAV
    case 3:                                                          // BeiDou: D2 / B-CNAV2 / D1
        if (sig_id == 1 || sig_id == 3) return 0x31;
        if (sig_id == 7 || sig_id == 8) return 0x33;
        return 0x30;
    case 6: return 0x40;                                             // GLONASS
    case 7: return 0;                                                // NavIC planned; no collector decoder
    case 1: return 0x70;                                             // SBAS
    default: return 0;
    }
}

size_t gnf1_encode_record(uint8_t *out, uint64_t recv_ns, uint8_t gnss_id, uint8_t sv_id,
                          uint8_t sig_id, uint8_t freq_id, uint8_t frame_type,
                          const uint8_t *raw, size_t raw_len)
{
    if (raw_len > GNF1_MAX_RAW) return 0;
    gnf1_be64(out, recv_ns);
    out[8]  = gnss_id;
    out[9]  = sv_id;
    out[10] = sig_id;
    out[11] = freq_id;
    out[12] = frame_type;
    if (raw_len) memcpy(out + GNF1_RECORD_HDR, raw, raw_len);
    return GNF1_RECORD_HDR + raw_len;
}

size_t gnf1_encode_telem(uint8_t *out, uint64_t recv_ns, uint8_t type, const uint8_t *body,
                         size_t body_len)
{
    if (body_len > GNF1_MAX_RAW) return 0;
    gnf1_be64(out, recv_ns);
    out[8] = out[9] = out[10] = out[11] = 0; // gnssId/svId/sigId/freqId unused for telemetry
    out[12] = type;
    if (body_len) memcpy(out + GNF1_RECORD_HDR, body, body_len);
    return GNF1_RECORD_HDR + body_len;
}

void gnf1_frame_header(uint8_t hdr[GNF1_FRAME_HDR], uint8_t type, uint32_t len)
{
    hdr[0] = type;
    gnf1_be32(hdr + 1, len);
}

size_t gnf1_encode_data(uint8_t *out, uint64_t seq, const uint8_t *record, size_t record_len)
{
    if (record_len > GNF1_RECORD_MAX) record_len = GNF1_RECORD_MAX;
    out[0] = GNF1_F_DATA;
    gnf1_be32(out + 1, (uint32_t)(8 + record_len)); // seq + record
    gnf1_be64(out + 5, seq);
    if (record_len) memcpy(out + GNF1_FRAME_HDR + 8, record, record_len);
    return GNF1_FRAME_HDR + 8 + record_len;
}

// gnf1_json_escape copies src into dst as a JSON string body (no surrounding quotes),
// escaping '"', '\', and control characters : an operator-supplied token/station
// containing '"' or '\' would otherwise produce invalid JSON, and the collector's
// ParseHello failing on it manifests as a confusing permanent auth-reject/reconnect loop
// rather than a clear error at provisioning time. Truncates cleanly (never overruns) if
// the escaped form would not fit dstcap; dst is always NUL-terminated when dstcap > 0.
// Well-formed inputs (no '"', '\', or control chars) are copied byte-identical. Mirrors
// json_escape() in ../../../feeder/navfeeder.c.
static void gnf1_json_escape(char *dst, size_t dstcap, const char *src)
{
    if (dstcap == 0) return;
    size_t di = 0;
    for (const unsigned char *s = (const unsigned char *)src; *s; s++) {
        unsigned char c = *s;
        char ubuf[7];
        const char *esc = NULL;
        switch (c) {
        case '"':  esc = "\\\""; break;
        case '\\': esc = "\\\\"; break;
        case '\n': esc = "\\n"; break;
        case '\r': esc = "\\r"; break;
        case '\t': esc = "\\t"; break;
        default:
            if (c < 0x20) {
                snprintf(ubuf, sizeof ubuf, "\\u%04x", c);
                esc = ubuf;
            }
        }
        size_t elen = esc ? strlen(esc) : 1;
        if (di + elen + 1 > dstcap) break; // would overflow: truncate cleanly
        if (esc) { memcpy(dst + di, esc, elen); } else { dst[di] = (char)c; }
        di += elen;
    }
    dst[di] = 0;
}

int gnf1_build_hello(char *out, size_t cap, const char *token, const char *station,
                     const char *feed, bool zstd)
{
    char token_esc[512], station_esc[256], feed_esc[128];
    gnf1_json_escape(token_esc, sizeof token_esc, token ? token : "");
    gnf1_json_escape(station_esc, sizeof station_esc, station ? station : "");
    gnf1_json_escape(feed_esc, sizeof feed_esc, feed ? feed : "ubx");
    int n = snprintf(out, cap,
                     "{\"token\":\"%s\",\"station\":\"%s\",\"feed\":\"%s\",\"sw\":\"navfeeder-esp/1\"%s}",
                     token_esc, station_esc, feed_esc,
                     zstd ? ",\"zstd\":true" : "");
    if (n < 0 || (size_t)n >= cap) return -1;
    return n;
}

bool gnf1_welcome_ok(const char *welcome, size_t len)
{
    // The payload is short, bounded JSON; a substring match mirrors navfeeder.c's handshake
    // acceptance without pulling a JSON parser onto the node.
    (void)len;
    return strstr(welcome, "\"ok\":true") != NULL;
}

bool gnf1_welcome_zstd(const char *welcome, size_t len)
{
    (void)len;
    return strstr(welcome, "\"zstd\":true") != NULL;
}

bool gnf1_decode_ack(const uint8_t *payload, size_t len, uint64_t *seq_out)
{
    if (len < 8) return false;
    *seq_out = gnf1_rd_be64(payload);
    return true;
}
