// record_test — host unit test for the DATA and SBF record encoders.
//
// An SBF block travels whole (GNF1_SBF_FORMAT), so its record may exceed GNF1_RECORD_MAX; the
// pusher then writes gnf1_data_header and the record itself. gnf1_encode_data must refuse such
// a record outright: it used to clamp it, which would have sent a cut-short block under a
// valid frame length.
//
//   make -C esp32/components/gnf1/test

#include <stdio.h>
#include <string.h>

#include "gnf1.h"

static int failures;

#define CHECK(cond, ...)                           \
    do {                                           \
        if (!(cond)) {                             \
            fprintf(stderr, "FAIL: " __VA_ARGS__); \
            fputc('\n', stderr);                   \
            failures++;                            \
        }                                          \
    } while (0)

// A block shaped as SBF frames it: sync, CRC (left zero: gnf1 does not check it, the reader
// already did), ID, Length, then body bytes. Not a receiver capture.
static void make_block(uint8_t *b, size_t len)
{
    memset(b, 0, len);
    b[0] = '$';
    b[1] = '@';
    b[4] = 4027 & 0xff;
    b[5] = 4027 >> 8;
    b[6] = (uint8_t)len;
    b[7] = (uint8_t)(len >> 8);
    for (size_t i = 8; i < len; i++) b[i] = (uint8_t)(i * 7);
}

static uint8_t block[GNF1_SBF_RAW_MAX + 8];
static uint8_t record[GNF1_SBF_RECORD_MAX + 8];

static void test_encode_sbf(void)
{
    make_block(block, 24);
    size_t n = gnf1_encode_sbf(record, 0x0102030405060708ull, block, 24);
    CHECK(n == GNF1_RECORD_HDR + 24, "24-byte block record length %zu", n);
    CHECK(record[0] == 0x01 && record[7] == 0x08, "receive stamp not big-endian");
    CHECK(!record[8] && !record[9] && !record[10] && !record[11], "navigation header bytes not zero");
    CHECK(record[12] == GNF1_T_SBF_BLOCK, "frame_type 0x%02x", record[12]);
    CHECK(!memcmp(record + GNF1_RECORD_HDR, block, 24), "block not copied whole");

    make_block(block, GNF1_SBF_RAW_MAX);
    n = gnf1_encode_sbf(record, 1, block, GNF1_SBF_RAW_MAX);
    CHECK(n == GNF1_SBF_RECORD_MAX, "largest block record length %zu", n);

    make_block(block, GNF1_SBF_RAW_MAX + 4);
    CHECK(!gnf1_encode_sbf(record, 1, block, GNF1_SBF_RAW_MAX + 4), "oversized block encoded");
    make_block(block, 24);
    CHECK(!gnf1_encode_sbf(record, 1, block, 28), "block whose Length differs from its size encoded");
    CHECK(!gnf1_encode_sbf(record, 1, block, 4), "short block encoded");
    block[1] = '#';
    CHECK(!gnf1_encode_sbf(record, 1, block, 24), "block without its sync encoded");
    make_block(block, 26);
    CHECK(!gnf1_encode_sbf(record, 1, block, 26), "block not a multiple of four encoded");
    CHECK(!gnf1_encode_sbf(record, 1, NULL, 24), "NULL block encoded");
}

static void test_data_frames(void)
{
    static uint8_t frame[GNF1_DATA_MAX];
    uint8_t rec[GNF1_RECORD_MAX];
    memset(rec, 0x5a, sizeof rec);
    size_t n = gnf1_encode_data(frame, 7, rec, GNF1_RECORD_MAX);
    CHECK(n == GNF1_DATA_MAX, "largest inline DATA frame length %zu", n);
    CHECK(frame[0] == GNF1_F_DATA && gnf1_rd_be32(frame + 1) == 8 + GNF1_RECORD_MAX &&
              gnf1_rd_be64(frame + GNF1_FRAME_HDR) == 7,
          "inline DATA header wrong");
    CHECK(!gnf1_encode_data(frame, 7, record, GNF1_RECORD_MAX + 1), "oversized record clamped into a frame");

    uint8_t hdr[GNF1_DATA_HDR];
    gnf1_data_header(hdr, 9, GNF1_SBF_RECORD_MAX);
    CHECK(hdr[0] == GNF1_F_DATA && gnf1_rd_be32(hdr + 1) == 8 + GNF1_SBF_RECORD_MAX &&
              gnf1_rd_be64(hdr + GNF1_FRAME_HDR) == 9,
          "DATA header for a large record wrong");
    gnf1_data_header(hdr, 7, GNF1_RECORD_MAX);
    CHECK(!memcmp(hdr, frame, GNF1_DATA_HDR), "the two DATA encodings disagree");
}

int main(void)
{
    test_encode_sbf();
    test_data_frames();
    if (failures) {
        fprintf(stderr, "record_test: %d failure(s)\n", failures);
        return 1;
    }
    printf("record_test: OK\n");
    return 0;
}
