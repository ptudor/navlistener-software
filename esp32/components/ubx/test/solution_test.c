// The receiver-solution record (telemetry 0x03) through the real UBX parser, checked
// against the golden case shared with the collector and the C feeder.
#include "ubx.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifndef GOLDEN
#error GOLDEN must name testdata/receiver_solution_v1.txt
#endif

typedef struct { uint8_t bytes[128]; size_t len; } blob_t;
static blob_t pvt, clk, status, body;

static void read_golden(void)
{
    FILE *f = fopen(GOLDEN, "r");
    assert(f);
    char line[512], name[32], hex[400];
    while (fgets(line, sizeof line, f)) {
        if (line[0] == '#' || line[0] == '\n') continue;
        assert(sscanf(line, "%31s %399s", name, hex) == 2);
        blob_t *b = !strcmp(name, "nav-pvt") ? &pvt : !strcmp(name, "nav-clock") ? &clk :
                    !strcmp(name, "nav-status") ? &status : !strcmp(name, "body") ? &body : NULL;
        assert(b && strlen(hex) % 2 == 0 && strlen(hex) / 2 <= sizeof b->bytes);
        b->len = strlen(hex) / 2;
        for (size_t i = 0; i < b->len; i++) {
            unsigned v;
            assert(sscanf(hex + 2 * i, "%2x", &v) == 1);
            b->bytes[i] = (uint8_t)v;
        }
    }
    fclose(f);
    assert(pvt.len == 92 && clk.len == 20 && status.len == 16 && body.len == 102);
}

static size_t frame(uint8_t *out, uint8_t cls, uint8_t id, const uint8_t *payload, size_t len)
{
    out[0] = 0xb5; out[1] = 0x62; out[2] = cls; out[3] = id; out[4] = (uint8_t)len; out[5] = (uint8_t)(len >> 8);
    memcpy(out + 6, payload, len);
    uint8_t a = 0, b = 0;
    for (size_t i = 2; i < len + 6; i++) { a += out[i]; b += a; }
    out[len + 6] = a; out[len + 7] = b;
    return len + 8;
}

static void feed(ubx_parser_t *p, uint8_t id, const uint8_t *payload, size_t len)
{
    uint8_t buf[200];
    ubx_parser_feed(p, buf, frame(buf, 0x01, id, payload, len));
}

static uint64_t clock_ns;
static uint64_t now(void) { return clock_ns += 100000000ull; }

static uint8_t records[4][GNF1_RECORD_MAX];
static size_t record_len[4], n_records;
static void emit(const uint8_t *rec, size_t len, void *ctx)
{
    (void)ctx;
    assert(n_records < 4);
    memcpy(records[n_records], rec, len);
    record_len[n_records++] = len;
}

static uint64_t be64(const uint8_t *b)
{
    uint64_t v = 0;
    for (int i = 0; i < 8; i++) v = v << 8 | b[i];
    return v;
}

static void eoe(ubx_parser_t *p, uint32_t tow)
{
    uint8_t e[4] = {(uint8_t)tow, (uint8_t)(tow >> 8), (uint8_t)(tow >> 16), (uint8_t)(tow >> 24)};
    feed(p, 0x61, e, sizeof e);
}

static void with_tow(blob_t *b, uint32_t tow)
{
    b->bytes[0] = (uint8_t)tow; b->bytes[1] = (uint8_t)(tow >> 8); b->bytes[2] = (uint8_t)(tow >> 16); b->bytes[3] = (uint8_t)(tow >> 24);
}

int main(void)
{
    read_golden();
    static ubx_parser_t parser;

    // The golden epoch, completed by NAV-EOE, is the golden record body, stamped
    // with its first message's arrival.
    clock_ns = 0; n_records = 0;
    ubx_parser_init(&parser, emit, now, NULL);
    feed(&parser, 0x07, pvt.bytes, pvt.len);
    feed(&parser, 0x22, clk.bytes, clk.len);
    feed(&parser, 0x03, status.bytes, status.len);
    assert(n_records == 0);
    uint32_t tow = (uint32_t)pvt.bytes[0] | (uint32_t)pvt.bytes[1] << 8 | (uint32_t)pvt.bytes[2] << 16 | (uint32_t)pvt.bytes[3] << 24;
    eoe(&parser, tow);
    assert(n_records == 1 && record_len[0] == GNF1_RECORD_HDR + body.len);
    assert(be64(records[0]) == 100000000ull);
    assert(records[0][8] == 0 && records[0][9] == 0 && records[0][10] == 0 && records[0][11] == 0);
    assert(records[0][12] == GNF1_T_SOLUTION);
    assert(!memcmp(records[0] + GNF1_RECORD_HDR, body.bytes, body.len));
    assert(parser.frames_telem == 1 && parser.solution_rejected == 0);

    // A polled repeat of the completed epoch is not sent again.
    feed(&parser, 0x07, pvt.bytes, pvt.len);
    eoe(&parser, tow);
    assert(n_records == 1);

    // Without NAV-EOE the next epoch completes the previous one, still stamped with
    // its own first arrival.
    blob_t next = pvt;
    with_tow(&next, tow + 1000);
    feed(&parser, 0x07, next.bytes, next.len);
    uint64_t next_arrival = clock_ns;
    blob_t later = pvt;
    with_tow(&later, tow + 2000);
    feed(&parser, 0x07, later.bytes, later.len);
    assert(n_records == 2 && be64(records[1]) == next_arrival);
    assert(records[1][GNF1_RECORD_HDR + 1] == RS_HAS_PVT && record_len[1] == GNF1_RECORD_HDR + 2 + RS_PVT_LEN);

    // Messages the record cannot carry are counted, never sent.
    blob_t bad = pvt;
    bad.bytes[31] = 0x7f; // latitude far beyond 90 degrees
    feed(&parser, 0x07, bad.bytes, bad.len);
    feed(&parser, 0x07, pvt.bytes, 84);       // the old, shorter NAV-PVT
    feed(&parser, 0x03, status.bytes, 15);
    feed(&parser, 0x61, status.bytes, 3);
    assert(parser.solution_rejected == 4 && n_records == 2);

    // Another NAV message is neither a solution block nor a rejection.
    feed(&parser, 0x01, pvt.bytes, 20);
    assert(parser.solution_rejected == 4);

    rs_epoch_t empty = {0};
    uint8_t out[RS_BODY_MAX];
    assert(rs_encode(&empty, out, sizeof out) == 0);
    rs_epoch_t full = {.present = RS_HAS_PVT | RS_HAS_CLOCK | RS_HAS_STATUS};
    assert(rs_encode(&full, out, RS_BODY_MAX - 1) == 0 && rs_encode(&full, out, sizeof out) == RS_BODY_MAX);
    puts("UBX solution: golden 0x03 body, first-arrival stamp, repeat drop and rejection passed");
}
