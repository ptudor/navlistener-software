// The reception record (telemetry 0x01, body version 2) through the real UBX parser,
// checked against the golden case shared with the collector and the C feeder.
#include "ubx.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifndef GOLDEN
#error GOLDEN must name testdata/reception_data_v2.txt
#endif

typedef struct { uint8_t bytes[2048]; size_t len; } blob_t;
static blob_t navsat, body;

static void read_golden(void)
{
    FILE *f = fopen(GOLDEN, "r");
    assert(f);
    char line[4200], name[32], hex[4100];
    while (fgets(line, sizeof line, f)) {
        if (line[0] == '#' || line[0] == '\n') continue;
        assert(sscanf(line, "%31s %4099s", name, hex) == 2);
        blob_t *b = !strcmp(name, "nav-sat") ? &navsat : !strcmp(name, "body") ? &body : NULL;
        assert(b && strlen(hex) % 2 == 0 && strlen(hex) / 2 <= sizeof b->bytes);
        b->len = strlen(hex) / 2;
        for (size_t i = 0; i < b->len; i++) {
            unsigned v;
            assert(sscanf(hex + 2 * i, "%2x", &v) == 1);
            b->bytes[i] = (uint8_t)v;
        }
    }
    fclose(f);
    assert(navsat.len == RD_UBX_SAT_HDR + 5 * RD_UBX_SAT_BLOCK && body.len == 3 + 5 * RD_ENTRY_LEN);
}

static size_t frame(uint8_t *out, const uint8_t *payload, size_t len)
{
    out[0] = 0xb5; out[1] = 0x62; out[2] = RD_UBX_CLASS_NAV; out[3] = RD_UBX_ID_SAT;
    out[4] = (uint8_t)len; out[5] = (uint8_t)(len >> 8);
    memcpy(out + 6, payload, len);
    uint8_t a = 0, b = 0;
    for (size_t i = 2; i < len + 6; i++) { a += out[i]; b += a; }
    out[len + 6] = a; out[len + 7] = b;
    return len + 8;
}

static uint8_t record[GNF1_RECORD_MAX];
static size_t record_len, n_records;
static void emit(const uint8_t *rec, size_t len, void *ctx)
{
    (void)ctx;
    memcpy(record, rec, len);
    record_len = len;
    n_records++;
}

static uint64_t now(void) { return 1234567890ull; }

static void feed(ubx_parser_t *p, const uint8_t *payload, size_t len)
{
    static uint8_t buf[UBX_MAX_PAYLOAD + 8];
    ubx_parser_feed(p, buf, frame(buf, payload, len));
}

int main(void)
{
    read_golden();
    static ubx_parser_t parser;
    ubx_parser_init(&parser, emit, now, NULL);

    // The golden NAV-SAT is the golden body: tracked satellites first, the extended
    // fields big-endian, dropped flag bits gone and reserved health sent as unknown.
    feed(&parser, navsat.bytes, navsat.len);
    assert(n_records == 1 && record_len == GNF1_RECORD_HDR + body.len);
    assert(record[12] == GNF1_T_RECEPTION && GNF1_T_RECEPTION == RD_TELEM_TYPE);
    assert(!memcmp(record + GNF1_RECORD_HDR, body.bytes, body.len));
    assert(parser.frames_telem == 1);

    // Another message version, a length other than the declared count, and an empty
    // list are not sent.
    blob_t bad = navsat;
    bad.bytes[4] = 2;
    feed(&parser, bad.bytes, bad.len);
    feed(&parser, navsat.bytes, navsat.len - 1);
    bad = navsat;
    bad.bytes[5] = 0;
    feed(&parser, bad.bytes, RD_UBX_SAT_HDR);
    assert(n_records == 1);

    // More satellites than the record holds: the tracked ones are kept, in order.
    static uint8_t many[RD_UBX_SAT_HDR + 150 * RD_UBX_SAT_BLOCK];
    memset(many, 0, sizeof many);
    many[4] = RD_UBX_SAT_VERSION;
    many[5] = 150;
    for (unsigned i = 0; i < 150; i++) {
        uint8_t *s = many + RD_UBX_SAT_HDR + i * RD_UBX_SAT_BLOCK;
        s[1] = (uint8_t)i;
        s[2] = (uint8_t)(i % 2 ? 30 + i % 10 : 0);
    }
    feed(&parser, many, sizeof many);
    assert(n_records == 2 && record_len == GNF1_RECORD_HDR + RD_BODY_MAX && RD_BODY_MAX <= GNF1_MAX_RAW);
    const uint8_t *out = record + GNF1_RECORD_HDR;
    assert(out[0] == RD_VERSION && (out[1] << 8 | out[2]) == RD_MAX_SATS);
    for (unsigned i = 0; i < RD_MAX_SATS; i++) {
        const uint8_t *e = out + 3 + i * RD_ENTRY_LEN;
        if (i < 75) assert(e[1] == 2 * i + 1 && e[2] > 0);
        else assert(e[2] == 0);
    }
    puts("UBX reception: golden 0x01 version 2 body, rejection and tracked-first cap passed");
    return 0;
}
