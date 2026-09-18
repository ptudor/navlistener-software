// gnf1 — GNF1 framing + record encoding. See include/gnf1.h.
// Byte-for-byte matched to ../../../../go/internal/wire/wire.go and
// ../../../../feeder/navfeeder.c. The (gnssId,sigId) → frame_type map below is checked
// against the shared golden matrix by test/frame_type_matrix_test.c — that claim is now
// enforced, not asserted.

#include "gnf1.h"

#include <stdio.h>
#include <string.h>

uint8_t gnf1_frame_type(unsigned gnss_id, unsigned sig_id)
{
    // This byte is a forensic label, not the collector's decode key — but the historian's
    // provenance depends on one input population never being split by which feeder saw it,
    // so this is an EXACT allow-list matching ../../../../testdata/gnf1_frame_type.tsv
    // row-for-row (the golden matrix generated from Go's RawFrame.NavType, regression fix).
    //
    // It previously applied constellation-family fallbacks on the GPS/Galileo/GLONASS/SBAS
    // arms — any unknown GPS sigId became CNAV, any Galileo sigId became I/NAV, every
    // GLONASS and SBAS sigId got its family's byte. That both contradicted this file's own
    // "byte-for-byte matched to navfeeder.c" header and labelled unverified signals as
    // supported message families: a future L5/DFMC or B-CNAV frame persisted under an L1
    // type is re-decoded through the wrong layout on replay. The
    // correct label for anything without a shipped, capture-verified decoder is 0 —
    // unmapped still decodes, because dispatch is on (gnssId, sigId).
    switch (gnss_id) {
    case 0:                                                          // GPS: L1 C/A LNAV, L2C/L5 CNAV
        if (sig_id == 0) return 0x10;
        if (sig_id == 3 || sig_id == 4 || sig_id == 6 || sig_id == 7) return 0x11;
        return 0;
    case 5:                                                          // QZSS: shipped LNAV/CNAV only
        if (sig_id == 0) return 0x50;
        if (sig_id == 4 || sig_id == 5 || sig_id == 8 || sig_id == 9) return 0x51;
        return 0;                                                    // L1S/L1C-CNAV2/L6 planned
    case 2: // Galileo: 0x20 = I/NAV page layout (E1-B; E5b-I same layout, collector dispatch deferred regression fix), E5a F/NAV
        if (sig_id == 0 || sig_id == 1 || sig_id == 5 || sig_id == 6) return 0x20;
        if (sig_id == 3 || sig_id == 4) return 0x21;
        return 0;
    case 3:                                                          // BeiDou: shipped B1I D1 + B2a B-CNAV2 only
        if (sig_id == 0) return 0x30;
        if (sig_id == 8) return 0x33;
        return 0; // D2/B2I/B-CNAV1/B2a-companion planned: no verified decoder
    case 6: return (sig_id == 0 || sig_id == 2) ? 0x40 : 0;          // GLONASS L1OF/L2OF
    case 7: return 0;                                                // NavIC planned; no collector decoder
    case 1: return (sig_id == 0) ? 0x70 : 0;                         // SBAS L1 C/A; L5 DFMC (0x71) reserved
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

bool gnf1_session_valid(const char *s)
{
    // Mirrors the collector's wire.ValidSession and navfeeder.c's session_charset_ok:
    // 1..GNF1_SESSION_MAX bytes of [A-Za-z0-9._-]. The session lands verbatim inside the
    // HELLO JSON (it is deliberately NOT escaped — see gnf1_build_hello) and then in the
    // collector's logs and historian ledger, so the charset is the thing that keeps it from
    // smuggling quotes, control bytes, or SQL/JSON metacharacters across the boundary.
    if (!s) return false;
    size_t n = 0;
    for (; s[n]; n++) {
        char c = s[n];
        if (!((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
              c == '.' || c == '_' || c == '-'))
            return false;
    }
    return n >= 1 && n <= GNF1_SESSION_MAX;
}

int gnf1_build_hello(char *out, size_t cap, const char *token, const char *station,
                     const char *feed, const char *session, bool zstd, bool evidence)
{
    // session : the boot-identity half of the collector's replay-dedup key
    // (observer, session, seq) — REQUIRED since the 2026-07-31 GNF1 contract revision. A
    // HELLO without a valid session is rejected before WELCOME
    // (`{"ok":false,"error":"missing or invalid session"}`), and there is no legacy tier to
    // fall back on, so refusing to build the frame here is strictly better than emitting one
    // the collector will certainly reject. Field ORDER and spelling match navfeeder.c's
    // handshake() byte-for-byte (after "sw", before the optional ",\"zstd\":true"): the JSON
    // is order-insensitive to Go's decoder, but a byte-identical HELLO across the two feeders
    // keeps the wire diffable in a packet capture.
    if (!gnf1_session_valid(session)) return -1;
    char token_esc[512], station_esc[256], feed_esc[128];
    gnf1_json_escape(token_esc, sizeof token_esc, token ? token : "");
    gnf1_json_escape(station_esc, sizeof station_esc, station ? station : "");
    gnf1_json_escape(feed_esc, sizeof feed_esc, feed ? feed : "ubx");
    // session needs no escaping: gnf1_session_valid just proved it holds no '"', '\', or
    // control characters.
    int n = snprintf(out, cap,
                     "{\"token\":\"%s\",\"station\":\"%s\",\"feed\":\"%s\",\"sw\":\"navfeeder-esp/1\","
                     "\"session\":\"%s\"%s%s}",
                     token_esc, station_esc, feed_esc, session,
                     zstd ? ",\"zstd\":true" : "",
                     evidence ? ",\"evidence\":true" : "");
    if (n < 0 || (size_t)n >= cap) return -1;
    return n;
}

bool gnf1_welcome_ok(const char *welcome, size_t len)
{
    // The payload is short, bounded, NUL-terminated JSON (the pusher adds the
    // terminator after read_frame). Substring matching avoids carrying a JSON
    // parser, but couples us to Go's compact `"ok":true` spelling: an
    // independently formatted WELCOME containing `"ok": true` is rejected.
    // that coupling is NORMATIVE in the wire contract (docs/DESIGN.md §GNF1)
    // and restated at the collector's write site (wire.MarshalWelcome) — a GNF1 server
    // must emit the compact spelling, so this match is spec-conformant, not a shortcut
    // that a future server is free to invalidate.
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
