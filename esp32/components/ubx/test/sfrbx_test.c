// The SFRBX message version byte through the real UBX parser: a version 1 (M8) payload
// carries a reserved byte where version 2 (F9/M9) carries sigId, so the record's sigId must
// be 0 for version 1 whatever that byte holds, and byte 2 for version 2. The feeder's
// emit_sfrbx and the collector's parseSFRBX apply the same rule, so the three stay a
// cross-oracle.
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

static size_t frame(uint8_t *out, const uint8_t *payload, size_t len)
{
    out[0] = 0xb5; out[1] = 0x62; out[2] = 0x02; out[3] = 0x13;
    out[4] = (uint8_t)len; out[5] = (uint8_t)(len >> 8);
    memcpy(out + 6, payload, len);
    uint8_t a = 0, b = 0;
    for (size_t i = 2; i < 6 + len; i++) { a += out[i]; b += a; }
    out[6 + len] = a; out[7 + len] = b;
    return 8 + len;
}

static void feed(ubx_parser_t *p, uint8_t version, uint8_t byte2)
{
    uint8_t payload[8 + 4 * 10] = {0}, buf[8 + sizeof payload];
    payload[0] = 0;      // GPS
    payload[1] = 7;      // svId
    payload[2] = byte2;  // sigId (version 2) or reserved (version 1)
    payload[3] = 0;      // freqId
    payload[4] = 10;     // numWords
    payload[6] = version;
    for (unsigned i = 0; i < 10; i++) payload[8 + i * 4] = (uint8_t)(0x10 + i);
    ubx_parser_feed(p, buf, frame(buf, payload, sizeof payload));
}

int main(void)
{
    ubx_parser_t parser;
    ubx_parser_init(&parser, emit, now, NULL);

    feed(&parser, 1, 0x5a); // M8 layout: byte 2 is reserved and must not become sigId
    assert(emitted == 1 && got_len == GNF1_RECORD_HDR + 40);
    assert(got[8] == 0 && got[9] == 7 && got[10] == 0 && got[11] == 0 && got[12] == 0x10);
    assert(got[GNF1_RECORD_HDR + 3] == 0x10); // first word, re-serialized big-endian

    feed(&parser, 2, 3); // F9/M9 layout: byte 2 is sigId (GPS L2C -> CNAV)
    assert(emitted == 2 && got[10] == 3 && got[12] == 0x11);

    feed(&parser, 2, 0);
    assert(emitted == 3 && got[10] == 0 && got[12] == 0x10);
    assert(parser.frames_nav == 3);
    puts("UBX SFRBX: version 1 reserved byte ignored, version 2 sigId honoured passed");
    return 0;
}
