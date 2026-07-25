// frame_type_matrix_test — host unit test for gnf1_frame_type().
//
// Differential test against the shared golden matrix : the same
// testdata/gnf1_frame_type.tsv that Go's TestNavTypeMatchesGolden generates from the
// canonical RawFrame.NavType, and that the C feeder is checked against end-to-end. Three
// implementations of one mapping drifted pairwise before this fixture existed; this binary
// is the ESP32 leg of the check.
//
// The gnf1 component is pure buffer code with no ESP-IDF dependency, so it compiles and runs
// on the host — no board, no flash, no idf.py:
//
//   make -C esp32/components/gnf1/test        # build + run
//
// go/internal/ingest's TestESP32Gnf1FrameTypeMatchesGolden runs this same source
// automatically as part of `make -C go check`, so a drifting ESP32 mapper fails the normal
// Go gate even on a machine with no ESP-IDF installed at all.

#include <stdarg.h>
#include <stdio.h>

#include "gnf1.h"

#define MATRIX_GNSS_IDS 8  // u-blox gnssId 0..7 (docs/CONSTELLATIONS.md §0)
#define MATRIX_SIG_IDS  16 // sigId 0..15; nothing assigned above that on any current receiver

static int fail_count;

__attribute__((format(printf, 1, 2))) static void fail(const char *fmt, ...)
{
    va_list ap;
    va_start(ap, fmt);
    vfprintf(stderr, fmt, ap);
    va_end(ap);
    fail_count++;
}

int main(int argc, char **argv)
{
    const char *path = (argc > 1) ? argv[1] : "../../../../testdata/gnf1_frame_type.tsv";
    FILE *f = fopen(path, "r");
    if (!f) {
        fprintf(stderr, "FAIL: cannot open golden matrix %s (pass the path as argv[1])\n", path);
        return 2;
    }

    // seen[][] proves the fixture covers the whole matrix — a truncated or partially
    // regenerated file must not pass by simply asserting fewer rows.
    static int seen[MATRIX_GNSS_IDS][MATRIX_SIG_IDS];
    char line[512];
    int rows = 0, lineno = 0;
    while (fgets(line, sizeof line, f)) {
        lineno++;
        char *p = line;
        while (*p == ' ' || *p == '\t') p++;
        if (*p == '#' || *p == '\n' || *p == '\r' || *p == 0) continue;

        unsigned gnss_id, sig_id, want;
        // Columns: gnssId, sigId, frame_type (0xNN), name. The name column is for human
        // readers and is deliberately not parsed here.
        if (sscanf(p, "%u %u 0x%x", &gnss_id, &sig_id, &want) != 3) {
            fail("FAIL: %s:%d: unparsable row: %s", path, lineno, p);
            continue;
        }
        if (gnss_id >= MATRIX_GNSS_IDS || sig_id >= MATRIX_SIG_IDS) {
            fail("FAIL: %s:%d: row (gnssId=%u, sigId=%u) is outside the matrix\n", path, lineno,
                 gnss_id, sig_id);
            continue;
        }
        if (seen[gnss_id][sig_id]) {
            fail("FAIL: %s:%d: duplicate row (gnssId=%u, sigId=%u)\n", path, lineno, gnss_id, sig_id);
        }
        seen[gnss_id][sig_id] = 1;
        rows++;

        uint8_t got = gnf1_frame_type(gnss_id, sig_id);
        if (got != (uint8_t)want) {
            fail("FAIL: gnf1_frame_type(%u, %u) = 0x%02x, golden says 0x%02x\n", gnss_id, sig_id,
                 got, want);
        }
    }
    fclose(f);

    for (unsigned g = 0; g < MATRIX_GNSS_IDS; g++) {
        for (unsigned s = 0; s < MATRIX_SIG_IDS; s++) {
            if (!seen[g][s]) fail("FAIL: golden is missing row (gnssId=%u, sigId=%u)\n", g, s);
        }
    }

    // Covered by rule, not by row: everything outside the enumerated matrix must map to 0.
    // A constellation-family fallback (the bug this test was written for) shows up here even
    // if the in-matrix rows happen to agree.
    for (unsigned g = 0; g < MATRIX_GNSS_IDS; g++) {
        for (unsigned s = MATRIX_SIG_IDS; s <= 255; s++) {
            if (gnf1_frame_type(g, s) != 0) {
                fail("FAIL: gnf1_frame_type(%u, %u) = 0x%02x, want 0 (unassigned signal)\n", g, s,
                     gnf1_frame_type(g, s));
            }
        }
    }
    for (unsigned g = MATRIX_GNSS_IDS; g <= 255; g++) {
        for (unsigned s = 0; s < MATRIX_SIG_IDS; s++) {
            if (gnf1_frame_type(g, s) != 0) {
                fail("FAIL: gnf1_frame_type(%u, %u) = 0x%02x, want 0 (unassigned constellation)\n",
                     g, s, gnf1_frame_type(g, s));
            }
        }
    }

    if (fail_count) {
        fprintf(stderr, "frame_type_matrix_test: %d failure(s)\n", fail_count);
        return 1;
    }
    printf("frame_type_matrix_test: OK (%d golden rows + out-of-matrix rule)\n", rows);
    return 0;
}
