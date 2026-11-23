// MON-RF's JammingStats record through the real UBX parser, with and without UBX-SEC-SIG:
// from the X20's HPG 2.00 on, MON-RF's per-block jammingState always reads 0 and SEC-SIG
// carries the jamming state, which then stands in every block while it is current. The
// feeder's emit_monrf and the collector's scanUBX apply the same rule.
#include "ubx.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

static uint8_t got[GNF1_RECORD_MAX];
static size_t got_len;
static unsigned emitted;

static void emit(const uint8_t *rec, size_t len, void *ctx)
{
    (void)ctx;
    assert(len <= sizeof got);
    memcpy(got, rec, len);
    got_len = len;
    emitted++;
}

static uint64_t now(void) { return 1700000000000000000ull; }

static void feed(ubx_parser_t *p, uint8_t cls, uint8_t id, const uint8_t *payload, size_t len)
{
    uint8_t buf[8 + UBX_MAX_PAYLOAD];
    assert(len <= UBX_MAX_PAYLOAD);
    buf[0] = 0xb5; buf[1] = 0x62; buf[2] = cls; buf[3] = id;
    buf[4] = (uint8_t)len; buf[5] = (uint8_t)(len >> 8);
    memcpy(buf + 6, payload, len);
    uint8_t a = 0, b = 0;
    for (size_t i = 2; i < 6 + len; i++) { a += buf[i]; b += a; }
    buf[6 + len] = a; buf[7 + len] = b;
    ubx_parser_feed(p, buf, 8 + len);
}

// Two blocks; each block's own jammingState comes from flags.
static void monrf(ubx_parser_t *p, uint8_t flags0, uint8_t flags1)
{
    uint8_t rf[4 + 2 * 24] = {0, 2};
    rf[4] = 0; rf[5] = flags0; rf[6] = 2; rf[18] = 0xa0; rf[19] = 0x0f; rf[20] = 7;
    rf[28] = 1; rf[29] = flags1; rf[30] = 2;
    const unsigned before = emitted;
    feed(p, 0x0a, 0x38, rf, sizeof rf);
    assert(emitted == before + 1 && got[12] == GNF1_T_JAMMING && got_len == GNF1_RECORD_HDR + 2 + 2 * 8);
}
static uint8_t jam(unsigned block) { return got[GNF1_RECORD_HDR + 2 + block * 8 + 6]; }

static void secsig(ubx_parser_t *p, uint8_t version, uint8_t flags, size_t len)
{
    uint8_t sec[4 + 4] = {version, flags, 0, 1, 0x10, 0x0a, 0x18, 0x00};
    feed(p, 0x27, 0x09, sec, len);
}

int main(void)
{
    ubx_parser_t parser;
    ubx_parser_init(&parser, emit, now, NULL);

    // Without SEC-SIG each block keeps its own flag, as on the M9 and M10.
    monrf(&parser, 2, 1);
    assert(jam(0) == 2 && jam(1) == 1);
    assert(got[GNF1_RECORD_HDR + 2 + 1] == 0x0f && got[GNF1_RECORD_HDR + 2 + 2] == 0xa0); // agc, big-endian
    assert(got[GNF1_RECORD_HDR + 2 + 5] == 7 && got[GNF1_RECORD_HDR + 2 + 7] == 2); // CW, antStatus

    // SEC-SIG version 2, detection on, warning: both blocks carry it over the X20's 0 flags.
    secsig(&parser, 2, 1 | 2 << 1, 8);
    const uint8_t assessment[] = {2, 2, 5, 1, 0x10, 0x0a, 0x18, 0};
    assert(emitted == 2 && got_len == GNF1_RECORD_HDR + sizeof assessment);
    assert(!memcmp(got + GNF1_RECORD_HDR, assessment, sizeof assessment));
    for (unsigned i = 0; i < 29; i++) secsig(&parser, 2, 5, 8);
    assert(emitted == 2);
    secsig(&parser, 2, 5, 8);
    assert(emitted == 3);
    monrf(&parser, 0, 0);
    assert(jam(0) == 2 && jam(1) == 2);
    monrf(&parser, 0, 0);
    assert(jam(0) == 2 && jam(1) == 2); // still current one MON-RF later
    monrf(&parser, 0, 0);
    assert(jam(0) == 0 && jam(1) == 0); // stale after two: MON-RF's own flag again

    // Version 3 with no jamming; detection off reads unknown.
    secsig(&parser, 3, 1 | 1 << 1, 8);
    monrf(&parser, 0, 3);
    assert(jam(0) == 1 && jam(1) == 1);
    secsig(&parser, 2, 2 << 1, 8);
    monrf(&parser, 2, 2);
    assert(jam(0) == 0 && jam(1) == 0);

    // Version 1, and a body whose length disagrees with its count, change nothing.
    ubx_parser_init(&parser, emit, now, NULL);
    secsig(&parser, 1, 1 | 2 << 1, 8);
    secsig(&parser, 2, 1 | 2 << 1, 7);
    monrf(&parser, 1, 0);
    assert(jam(0) == 1 && jam(1) == 0 && !parser.sec_sig);
    unsigned before = emitted;
    const uint8_t log[] = {1, 1, 0, 0, 0, 0, 0, 0, 42, 0, 0, 0, 4, 0, 0, 0};
    feed(&parser, 0x27, 0x10, log, sizeof log);
    assert(emitted == before + 1 && got_len == GNF1_RECORD_HDR + 2 + sizeof log);
    assert(got[GNF1_RECORD_HDR+1] == 0x10 && !memcmp(got + GNF1_RECORD_HDR+2, log, sizeof log));
    uint8_t sweep[4 + 2 * RF_SPAN_BLOCK] = {0, 2};
    sweep[4+RF_SPAN_BLOCK] = 217;
    feed(&parser, 0x0a, 0x31, sweep, sizeof sweep);
    assert(emitted == before + 3 && got_len == GNF1_RECORD_HDR + 276);
    assert(!memcmp(got + GNF1_RECORD_HDR, (uint8_t[]){2, 0x31, 1, 2, 217}, 5));
    uint8_t response[] = {1, 0, 0, 0, 0x51, 0, 0xf6, 0x10, 1};
    feed(&parser, 6, 0x8b, response, sizeof response);
    assert(emitted == before + 4 && !memcmp(got + GNF1_RECORD_HDR, (uint8_t[]){2, 0x51, 1}, 3));
    response[1] = 7; // default layer is independent of the active RAM setting
    feed(&parser, 6, 0x8b, response, sizeof response);
    feed(&parser, 0x0a, 0x31, sweep, sizeof sweep - 1);
    feed(&parser, 0x27, 0x10, log, sizeof log - 1);
    assert(emitted == before + 4);
    puts("UBX jamming: MON-RF flags, SEC-SIG in their place while current, and malformed SEC-SIG ignored passed");
    return 0;
}
