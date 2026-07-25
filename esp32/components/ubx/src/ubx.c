// ubx — UBX framer + GNF1 record emitters. See include/ubx.h.
// The record-body layouts are byte-for-byte matched to ../../../feeder/navfeeder.c
// (emit_sfrbx/monrf/monhw/navsat) and the collector's scanUBX. For supported signals the
// complete records match; gnf1_frame_type documents the ESP32's broader fallback labels
// for unknown signal IDs.

#include "ubx.h"

#include <string.h>

// UBX protocol constants (u-blox interface description).
#define UBX_SYNC1     0xB5
#define UBX_SYNC2     0x62
#define UBX_CLASS_NAV 0x01
#define UBX_CLASS_RXM 0x02
#define UBX_CLASS_MON 0x0A
#define UBX_ID_SFRBX  0x13
#define UBX_ID_NAVSAT 0x35
#define UBX_ID_MONHW  0x09
#define UBX_ID_MONRF  0x38

#define MAX_TELEM_SATS 200 // matches navfeeder MAX_TELEM_SATS and Go maxTelemSats

// framer states
enum { S_SYNC1 = 0, S_SYNC2, S_CLASS, S_ID, S_LEN1, S_LEN2, S_PAYLOAD, S_CK_A, S_CK_B };

// UBX telemetry fields are little-endian on the wire.
static inline uint16_t rd_le16(const uint8_t *b) { return (uint16_t)b[0] | ((uint16_t)b[1] << 8); }
static inline uint32_t rd_le32(const uint8_t *b) {
    return (uint32_t)b[0] | ((uint32_t)b[1] << 8) | ((uint32_t)b[2] << 16) | ((uint32_t)b[3] << 24);
}

void ubx_parser_init(ubx_parser_t *p, ubx_emit_fn emit, ubx_now_fn now, void *ctx)
{
    memset(p, 0, sizeof *p);
    p->emit = emit;
    p->now = now;
    p->ctx = ctx;
    p->state = S_SYNC1;
}

// --- record emitters (payload is the validated UBX message body) -------------------------

// emit_sfrbx: UBX-RXM-SFRBX -> raw-nav record. Layout (F9/M9): gnssId, svId, sigId, freqId,
// numWords, reserved, version, reserved, then numWords little-endian dwrds each holding one
// native nav word. Re-serialize each word big-endian (the collector reads them back BE).
static void emit_sfrbx(ubx_parser_t *p, const uint8_t *payload, uint16_t len)
{
    if (len < 8) return;
    unsigned gnss_id = payload[0], sv_id = payload[1], sig_id = payload[2],
             freq_id = payload[3], num_words = payload[4];
    if (num_words == 0 || 8u + num_words * 4u > len) return;
    unsigned raw_len = num_words * 4u;
    if (raw_len > GNF1_MAX_RAW) return;

    uint8_t *rec = p->scratch;
    for (unsigned i = 0; i < num_words; i++)
        gnf1_be32(rec + GNF1_RECORD_HDR + i * 4, rd_le32(payload + 8 + i * 4));
    gnf1_be64(rec, p->now());
    rec[8]  = (uint8_t)gnss_id;
    rec[9]  = (uint8_t)sv_id;
    rec[10] = (uint8_t)sig_id;
    rec[11] = (uint8_t)freq_id;
    rec[12] = gnf1_frame_type(gnss_id, sig_id);
    p->emit(rec, GNF1_RECORD_HDR + raw_len, p->ctx);
    atomic_fetch_add_explicit(&p->frames_nav, 1, memory_order_relaxed); /* regression fix */
}

// emit_monrf: UBX-MON-RF (F9+) -> JammingStats (0x05). Layout: version U1, nBlocks U1,
// reserved U1[2], then nBlocks x 24-byte blocks (blockId U1, flags X1 [bits0-1=jamState],
// antStatus U1, antPower U1, postStatus U4 @4, reserved U1[4] @8, noisePerMS U2 @12,
// agcCnt U2 @14, jamInd U1 @16 (previously read @14/@16/@20, off by the 2-byte
// antStatus/antPower pair).
static void emit_monrf(ubx_parser_t *p, const uint8_t *payload, uint16_t len)
{
    if (len < 4) return;
    unsigned n_blocks = payload[1];
    if (n_blocks == 0 || 4u + n_blocks * 24u > len) return;
    unsigned body_len = 2u + n_blocks * 8u;
    if (body_len > GNF1_MAX_RAW) return;

    uint8_t *rec = p->scratch;
    uint8_t *body = rec + GNF1_RECORD_HDR;
    body[0] = GNF1_TELEM_VERSION;
    body[1] = (uint8_t)n_blocks;
    for (unsigned i = 0; i < n_blocks; i++) {
        const uint8_t *b = payload + 4 + i * 24;
        unsigned o = 2 + i * 8;
        body[o]     = b[0];                          // blockId
        gnf1_be16(body + o + 1, rd_le16(b + 14));    // agcCnt
        gnf1_be16(body + o + 3, rd_le16(b + 12));    // noisePerMS
        body[o + 5] = b[16];                         // jamInd (CW)
        body[o + 6] = (uint8_t)(b[1] & 0x03);        // jammingState
        body[o + 7] = b[2];                          // antStatus
    }
    gnf1_be64(rec, p->now());
    rec[8] = rec[9] = rec[10] = rec[11] = 0;
    rec[12] = GNF1_T_JAMMING;
    p->emit(rec, GNF1_RECORD_HDR + body_len, p->ctx);
    atomic_fetch_add_explicit(&p->frames_telem, 1, memory_order_relaxed); /* regression fix */
}

// emit_monhw: legacy UBX-MON-HW (60 bytes) -> single-band JammingStats. noisePerMS U2 @16,
// agcCnt U2 @18, aStatus U1 @20, flags X1 @22 (jammingState bits 2-3), jamInd U1 @45.
static void emit_monhw(ubx_parser_t *p, const uint8_t *payload, uint16_t len)
{
    if (len < 60) return;
    uint8_t *rec = p->scratch;
    uint8_t *body = rec + GNF1_RECORD_HDR;
    body[0] = GNF1_TELEM_VERSION;
    body[1] = 1;
    body[2] = 0;                                     // block 0
    gnf1_be16(body + 3, rd_le16(payload + 18));      // agcCnt
    gnf1_be16(body + 5, rd_le16(payload + 16));      // noisePerMS
    body[7] = payload[45];                           // jamInd (CW)
    body[8] = (uint8_t)((payload[22] >> 2) & 0x03);  // jammingState
    body[9] = payload[20];                           // aStatus
    gnf1_be64(rec, p->now());
    rec[8] = rec[9] = rec[10] = rec[11] = 0;
    rec[12] = GNF1_T_JAMMING;
    p->emit(rec, GNF1_RECORD_HDR + 10, p->ctx);
    atomic_fetch_add_explicit(&p->frames_telem, 1, memory_order_relaxed); /* regression fix */
}

// emit_navsat: UBX-NAV-SAT -> ReceptionData (0x01) for the C/N0-vs-elevation spoof gate.
// Layout: iTOW U4, version U1, numSvs U1 @5, reserved U1[2], then numSvs x 12-byte blocks
// (gnssId U1, svId U1, cno U1 @2, elev I1 @3, ..., flags X4 @8 [bit3=svUsed]).
static void emit_navsat(ubx_parser_t *p, const uint8_t *payload, uint16_t len)
{
    if (len < 8) return;
    unsigned num_svs = payload[5];
    if (num_svs == 0 || 8u + num_svs * 12u > len) return;
    unsigned n = num_svs > MAX_TELEM_SATS ? MAX_TELEM_SATS : num_svs;

    uint8_t *rec = p->scratch;
    uint8_t *body = rec + GNF1_RECORD_HDR;
    body[0] = GNF1_TELEM_VERSION;
    gnf1_be16(body + 1, (uint16_t)n);
    for (unsigned i = 0; i < n; i++) {
        const uint8_t *s = payload + 8 + i * 12;
        unsigned o = 3 + i * 5;
        body[o]     = s[0];                          // gnssId
        body[o + 1] = s[1];                          // svId
        body[o + 2] = s[2];                          // cno
        body[o + 3] = s[3];                          // elev (I1, verbatim)
        body[o + 4] = (rd_le32(s + 8) & 0x08) ? 0x01 : 0x00; // svUsed
    }
    gnf1_be64(rec, p->now());
    rec[8] = rec[9] = rec[10] = rec[11] = 0;
    rec[12] = GNF1_T_RECEPTION;
    p->emit(rec, GNF1_RECORD_HDR + 3u + n * 5u, p->ctx);
    atomic_fetch_add_explicit(&p->frames_telem, 1, memory_order_relaxed); /* regression fix */
}

// dispatch routes a checksum-valid message to its emitter.
static void dispatch(ubx_parser_t *p)
{
    if (p->cls == UBX_CLASS_RXM && p->id == UBX_ID_SFRBX)      emit_sfrbx(p, p->payload, p->len);
    else if (p->cls == UBX_CLASS_MON && p->id == UBX_ID_MONRF) emit_monrf(p, p->payload, p->len);
    else if (p->cls == UBX_CLASS_MON && p->id == UBX_ID_MONHW) emit_monhw(p, p->payload, p->len);
    else if (p->cls == UBX_CLASS_NAV && p->id == UBX_ID_NAVSAT) emit_navsat(p, p->payload, p->len);
}

// ck accumulates one byte into the running Fletcher-8 checksum.
static inline void ck(ubx_parser_t *p, uint8_t b) { p->ck_a += b; p->ck_b += p->ck_a; }

void ubx_parser_feed(ubx_parser_t *p, const uint8_t *data, size_t len)
{
    for (size_t i = 0; i < len; i++) {
        uint8_t b = data[i];
        switch (p->state) {
        case S_SYNC1:
            if (b == UBX_SYNC1) p->state = S_SYNC2;
            break;
        case S_SYNC2:
            // A lone 0xB5 followed by another 0xB5 keeps the second as a fresh candidate.
            if (b == UBX_SYNC2) p->state = S_CLASS;
            else if (b != UBX_SYNC1) p->state = S_SYNC1;
            break;
        case S_CLASS:
            p->cls = b; p->ck_a = p->ck_b = 0; ck(p, b); p->state = S_ID;
            break;
        case S_ID:
            p->id = b; ck(p, b); p->state = S_LEN1;
            break;
        case S_LEN1:
            p->len = b; ck(p, b); p->state = S_LEN2;
            break;
        case S_LEN2:
            p->len |= (uint16_t)b << 8; ck(p, b);
            if (p->len > UBX_MAX_PAYLOAD) { atomic_fetch_add_explicit(&p->oversize, 1, memory_order_relaxed); p->state = S_SYNC1; break; }
            p->idx = 0;
            p->state = p->len ? S_PAYLOAD : S_CK_A;
            break;
        case S_PAYLOAD:
            p->payload[p->idx++] = b; ck(p, b);
            if (p->idx >= p->len) p->state = S_CK_A;
            break;
        case S_CK_A:
            p->exp_a = b; p->state = S_CK_B;
            break;
        case S_CK_B:
            p->exp_b = b;
            if (p->ck_a == p->exp_a && p->ck_b == p->exp_b) dispatch(p);
            else atomic_fetch_add_explicit(&p->bad_checksum, 1, memory_order_relaxed); /* regression fix */
            p->state = S_SYNC1;
            break;
        default:
            p->state = S_SYNC1;
            break;
        }
    }
}
