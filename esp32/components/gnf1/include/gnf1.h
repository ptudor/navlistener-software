// gnf1 — GNF1 framing + the raw-record envelope for navfeeder-esp.
//
// GNF1 is navlistener's length-prefixed feeder<->collector framing over TLS. It carries
// raw broadcast nav frames; the collector decodes. This component is the byte-for-byte
// counterpart of ../../../../go/internal/wire/wire.go and ../../../../feeder/navfeeder.c, authored
// fresh (clean-room; NOT galmon's navmon.proto). Pure buffer functions — no I/O, so it is
// host-testable and the pusher does the actual esp-tls writes.
//
// Wire (must match wire.go):
//   stream = "GNF1" then frames [1B type][4B BE len][payload]
//   DATA(0x03)  [8B BE seq][record];  ACK(0x04) [8B BE seq]
//   record = [8B BE recv_unix_ns][gnssId][svId][sigId][freqId][frame_type][raw...]
//   raw    = native nav words serialized BIG-ENDIAN (the collector reads them back BE).

#ifndef GNF1_H
#define GNF1_H

#include <stddef.h>
#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

#define GNF1_MAGIC "GNF1" // 4 bytes, sent once after the TLS handshake

// Frame type discriminator (1 byte). Mirrors wire.FrameType.
#define GNF1_F_HELLO       0x01 // feeder->collector: JSON hello
#define GNF1_F_WELCOME     0x02 // collector->feeder: JSON welcome
#define GNF1_F_DATA        0x03 // feeder->collector: [8B seq][record]
#define GNF1_F_ACK 0x04 // collector->feeder: [8B seq] highest sequence received this connection (regression fix/regression fix; NOT a durability guarantee)
#define GNF1_F_PING        0x05 // keepalive
#define GNF1_F_PONG        0x06 // keepalive
#define GNF1_F_SIGNED_DATA 0x07 // P-hw: ATECC ECDSA batch (reserved)

// Telemetry record types (frame_type < 0x10, CONSTELLATIONS.md §6.2). Body layouts must
// match ../../../go/internal/ingest/telemetry.go.
#define GNF1_T_RECEPTION 0x01 // NAV-SAT per-SV C/N0 + elevation
#define GNF1_T_JAMMING   0x05 // MON-RF / MON-HW AGC/jamming/antenna
#define GNF1_TELEM_VERSION 1

#define GNF1_MAX_FRAME    (1u << 20) // wire.MaxFrameLen — validate every length prefix
#define GNF1_RECORD_HDR   13         // recv_ns(8) + gnssId + svId + sigId + freqId + frame_type
#define GNF1_MAX_RAW      1024       // numWords is u8 => <=1020 raw bytes; round up
#define GNF1_RECORD_MAX   (GNF1_RECORD_HDR + GNF1_MAX_RAW)
#define GNF1_FRAME_HDR    5          // [1B type][4B BE len]
#define GNF1_DATA_MAX     (GNF1_FRAME_HDR + 8 + GNF1_RECORD_MAX)

// gnf1_frame_type maps (gnssId, sigId) to the GNF1 nav message type byte (CONSTELLATIONS.md
// §6). It is a forensic label — the collector dispatches decode on (gnssId, sigId), so an
// unmapped type (0) still decodes. It is an exact allow-list matching navfeeder.c and Go's
// RawFrame.NavType row-for-row; the shared authority is the golden matrix at
// ../../../../testdata/gnf1_frame_type.tsv, which test/frame_type_matrix_test.c checks this
// implementation against. Anything without a shipped, capture-verified decoder
// maps to 0 — never to a constellation-family default.
uint8_t gnf1_frame_type(unsigned gnss_id, unsigned sig_id);

// gnf1_encode_record builds a raw-nav record (the payload the spool stores, without the seq)
// into out, which must be at least GNF1_RECORD_HDR + raw_len bytes. `raw` is already
// big-endian nav words. Returns the record length, or 0 if raw_len exceeds GNF1_MAX_RAW.
size_t gnf1_encode_record(uint8_t *out, uint64_t recv_ns, uint8_t gnss_id, uint8_t sv_id,
                          uint8_t sig_id, uint8_t freq_id, uint8_t frame_type,
                          const uint8_t *raw, size_t raw_len);

// gnf1_encode_telem builds a telemetry record (zeroed gnssId/svId/sigId/freqId, a typed
// body). Returns the record length, or 0 if body_len exceeds GNF1_MAX_RAW.
size_t gnf1_encode_telem(uint8_t *out, uint64_t recv_ns, uint8_t type, const uint8_t *body,
                         size_t body_len);

// gnf1_frame_header fills a 5-byte frame header [type][4B BE len] into hdr.
void gnf1_frame_header(uint8_t hdr[GNF1_FRAME_HDR], uint8_t type, uint32_t len);

// gnf1_encode_data builds a full DATA frame [F_DATA][4B BE len][8B BE seq][record] into out,
// which must be at least GNF1_FRAME_HDR + 8 + record_len bytes. Returns the total frame
// length. An oversized record_len is silently clamped to GNF1_RECORD_MAX, matching
// navfeeder.c send_data(); normal callers pass records produced by the bounded encoders.
size_t gnf1_encode_data(uint8_t *out, uint64_t seq, const uint8_t *record, size_t record_len);

// GNF1_SESSION_MAX bounds the HELLO session identity, matching wire.SessionMaxLen (Go) and
// SPOOL_SESSION_CAP (navfeeder.c). Both reference feeders mint 32 hex characters.
#define GNF1_SESSION_MAX 64

// gnf1_session_valid reports whether s is an acceptable GNF1 session identity: 1..
// GNF1_SESSION_MAX bytes of [A-Za-z0-9._-], the same domain as the collector's
// wire.ValidSession. Exposed so a caller can check a session before it reaches the wire (and
// so the host tests can pin the domain).
bool gnf1_session_valid(const char *s);

// gnf1_build_hello writes the HELLO JSON payload into out (capacity cap). Returns the length
// written, or -1 on truncation or an invalid/missing session. It carries the bearer token,
// the station/feed identity, and the regression fix session; TLS certificate configuration, if
// added, belongs to the connection layer.
//
// session (regression fix, REQUIRED since the 2026-07-31 contract revision) is the feeder's
// boot/session identity: the collector's replay-dedup key is (observer, session, seq), so the
// value must be fresh whenever the DATA sequence space restarts from zero and stable while it
// continues. navfeeder-esp's spool is RAM-only, so its sequence space ALWAYS
// restarts at boot and app_main mints a new session every boot — that is exactly what keeps
// post-reboot frames from colliding with the previous boot's ledger rows. Must satisfy
// gnf1_session_valid; it is emitted unescaped.
int gnf1_build_hello(char *out, size_t cap, const char *token, const char *station,
                     const char *feed, const char *session, bool zstd);

// gnf1_welcome_ok reports whether a WELCOME JSON payload accepted the handshake.
// welcome must be NUL-terminated in addition to supplying len. The tiny parser
// recognizes the compact `"ok":true` spelling emitted by Go encoding/json.
bool gnf1_welcome_ok(const char *welcome, size_t len);

// gnf1_welcome_zstd reports whether the collector confirmed zstd (only then compress).
// It has the same NUL-termination and compact-JSON requirements as gnf1_welcome_ok.
bool gnf1_welcome_zstd(const char *welcome, size_t len);

// gnf1_decode_ack reads the first 8 bytes of an ACK payload as the sequence. Returns
// false if short and ignores any trailing extension bytes.
bool gnf1_decode_ack(const uint8_t *payload, size_t len, uint64_t *seq_out);

// --- byte order (big-endian on the wire) -----------------------------------------------

static inline void gnf1_be16(uint8_t *b, uint16_t v) { b[0] = (uint8_t)(v >> 8); b[1] = (uint8_t)v; }
static inline void gnf1_be32(uint8_t *b, uint32_t v) {
    b[0] = (uint8_t)(v >> 24); b[1] = (uint8_t)(v >> 16); b[2] = (uint8_t)(v >> 8); b[3] = (uint8_t)v;
}
static inline void gnf1_be64(uint8_t *b, uint64_t v) {
    for (int i = 7; i >= 0; i--) { b[i] = (uint8_t)(v & 0xff); v >>= 8; }
}
static inline uint32_t gnf1_rd_be32(const uint8_t *b) {
    return ((uint32_t)b[0] << 24) | ((uint32_t)b[1] << 16) | ((uint32_t)b[2] << 8) | b[3];
}
static inline uint64_t gnf1_rd_be64(const uint8_t *b) {
    uint64_t v = 0; for (int i = 0; i < 8; i++) v = (v << 8) | b[i]; return v;
}

#ifdef __cplusplus
}
#endif

#endif // GNF1_H
