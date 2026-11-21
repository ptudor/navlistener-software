#include "sbf.h"
#include "sbf_build.h"
#include <assert.h>
#include <math.h>
#include <stdio.h>
#include <string.h>

typedef struct {
    unsigned blocks, lines, prompts;
    uint16_t numbers[16];
    char line[8][128];
    char prompt[8];
} sink_t;
static void on_block(const uint8_t *b, size_t n, void *ctx)
{
    sink_t *s = ctx;
    assert(n == sbf_u16(b + 6) && b[0] == '$' && b[1] == '@');
    if (s->blocks < 16) s->numbers[s->blocks] = sbf_number(b);
    s->blocks++;
}
static void on_line(const char *line, void *ctx)
{
    sink_t *s = ctx;
    if (s->lines < 8) snprintf(s->line[s->lines], sizeof s->line[0], "%s", line);
    s->lines++;
}
static void on_prompt(const char *prompt, void *ctx)
{
    sink_t *s = ctx;
    snprintf(s->prompt, sizeof s->prompt, "%s", prompt);
    s->prompts++;
}
static sbf_reader_t reader; // static: the buffer is the largest block

static void crc_test(void)
{
    // CRC-16-CCITT with seed 0 (4.1.1): the CRC-16/XMODEM check value, which Python's
    // binascii.crc_hqx(b"123456789", 0) also gives.
    assert(sbf_crc16((const uint8_t *)"123456789", 9, 0) == 0x31C3);
    assert(sbf_crc16((const uint8_t *)"", 0, 0) == 0);
    // Running the CRC over two pieces is the same as over the whole.
    assert(sbf_crc16((const uint8_t *)"56789", 5, sbf_crc16((const uint8_t *)"1234", 4, 0)) == 0x31C3);
}

static void reader_test(void)
{
    static uint8_t stream[4 * SBF_MAX_BLOCK], b[SBF_MAX_BLOCK];
    size_t n = 0;
    sink_t sink = {0};
    sbf_reader_init(&reader, on_block, on_line, on_prompt, &sink);
    // Noise, then a reply with its prompt (3.1.3), then a PVT block.
    const char *noise = "\x01\xff\x80garbage";
    memcpy(stream + n, noise, strlen(noise)); n += strlen(noise);
    const char *reply = "\r\n$R: gst\r\n  SatelliteTracking, G01+R01\r\nCOM2>";
    memcpy(stream + n, reply, strlen(reply)); n += strlen(reply);
    n += pvt_block(stream + n, 2, 345600000, 1, 0, 37.5, -122.25, 12.5);
    // A block whose CRC is wrong is dropped; the next one is found right after it.
    size_t bad = pvt_block(stream + n, 2, 345601000, 1, 0, 37.5, -122.25, 12.5);
    stream[n + 20] ^= 0x55;
    n += bad;
    n += time_block(stream + n, 345601000, 26, 10, 19, 18, 30, 5, 18, 7);
    // A sync with a length that is not a multiple of 4, and one claiming more than the maximum.
    memcpy(stream + n, "$@\x00\x00\xa7\x0f\x0a\x00", 8); n += 8;
    memcpy(stream + n, "$@\x00\x00\xa7\x0f\xfc\xff", 8); n += 8;
    // A block whose body contains "$@" (in ReceiverSetup's serial number field).
    n += setup_block(stream + n, 3, "38$@1234", "mosaic", "4.15.1", "mosaic-G5 P3");
    // An error reply and a long line past the line limit, which is dropped whole.
    const char *error = "$R? sso: Invalid command!\r\nCOM2>";
    memcpy(stream + n, error, strlen(error)); n += strlen(error);
    for (unsigned i = 0; i < SBF_LINE_MAX + 10; i++) stream[n++] = 'x';
    stream[n++] = '\n';

    // Fed one byte at a time, then all at once: the same result.
    for (size_t i = 0; i < n; i++) sbf_reader_feed(&reader, stream + i, 1);
    assert(sink.blocks == 3 && sink.numbers[0] == SBF_PVT_GEODETIC && sink.numbers[1] == SBF_RECEIVER_TIME &&
           sink.numbers[2] == SBF_RECEIVER_SETUP);
    // Printable noise ending at a line break is a line too; the control ignores what is not a reply.
    assert(sink.lines == 4 && !strcmp(sink.line[0], "garbage") && !strcmp(sink.line[1], "$R: gst") &&
           !strcmp(sink.line[2], "  SatelliteTracking, G01+R01") && !strcmp(sink.line[3], "$R? sso: Invalid command!"));
    assert(sink.prompts == 2 && !strcmp(sink.prompt, "COM2>"));
    assert(reader.crc_errors == 1 && reader.bad_lengths == 2 && reader.blocks == 3 && reader.bytes == n);
    sink = (sink_t){0};
    sbf_reader_init(&reader, on_block, on_line, on_prompt, &sink);
    sbf_reader_feed(&reader, stream, n);
    assert(sink.blocks == 3 && sink.lines == 4 && sink.prompts == 2 && reader.crc_errors == 1);

    // A truncated block waits for the rest; a block of the maximum size fits the buffer.
    sink = (sink_t){0};
    sbf_reader_init(&reader, on_block, on_line, on_prompt, &sink);
    size_t length = pvt_block(b, 2, 1000, 1, 0, 1, 1, 1);
    sbf_reader_feed(&reader, b, length - 1);
    assert(sink.blocks == 0);
    sbf_reader_feed(&reader, b + length - 1, 1);
    assert(sink.blocks == 1);
    memset(b, 0, sizeof b);
    length = seal(b, 4999, 0, SBF_MAX_BLOCK);
    sbf_reader_feed(&reader, b, length);
    assert(sink.blocks == 2 && sink.numbers[1] == 4999);
    // Prompts are a descriptor and '>' alone: "TOO-LONG-NAME>" and "a>" are just text.
    sink = (sink_t){0};
    sbf_reader_init(&reader, on_block, on_line, on_prompt, &sink);
    const char *notprompt = "AB-CD>\r\nUSB1>STOP>";
    sbf_reader_feed(&reader, (const uint8_t *)notprompt, strlen(notprompt));
    assert(sink.prompts == 2 && !strcmp(sink.prompt, "STOP>") && sink.lines == 1);
}

static void pvt_test(void)
{
    uint8_t b[128];
    sbf_pvt_t p;
    size_t n = pvt_block(b, 2, 345600000, 1, 0, 37.5, -122.25, 12.5);
    assert(sbf_pvt_geodetic(b, n, &p) && p.position && p.tow == 345600000 && p.wnc == 2392 && p.nr_sv == 14);
    assert(fabs(p.latitude_deg - 37.5) < 1e-9 && fabs(p.longitude_deg + 122.25) < 1e-9 && fabs(p.height_m - 12.5) < 1e-9);
    assert(p.accuracy && p.h_accuracy_cm == 123 && p.v_accuracy_cm == 456);
    // GNSS solution types count; no PVT, a fixed location and reserved types do not.
    const struct { uint8_t mode; bool position; } modes[] = {
        {0, false}, {1, true}, {2, true}, {3, false}, {4, true}, {5, true}, {6, true}, {7, true}, {8, true},
        {9, false}, {10, true}, {12, false}, {0x81, true}, {0x40 | 1, true},
    };
    for (size_t i = 0; i < sizeof modes / sizeof modes[0]; i++) {
        n = pvt_block(b, 2, 1000, modes[i].mode, 0, 10, 20, 30);
        assert(sbf_pvt_geodetic(b, n, &p) && p.position == modes[i].position);
    }
    n = pvt_block(b, 2, 1000, 1, 1, 10, 20, 30); // not enough measurements
    assert(sbf_pvt_geodetic(b, n, &p) && !p.position);
    n = pvt_block(b, 2, 1000, 1, 0, 0, 0, 0);
    putf8(b + 16, -2e10); seal(b, SBF_PVT_GEODETIC, 2, n); // Do-Not-Use latitude
    assert(sbf_pvt_geodetic(b, n, &p) && !p.position);
    // Revision 0 has no accuracy fields; a short or mislabelled block is rejected.
    n = pvt_block(b, 0, 1000, 1, 0, 10, 20, 30);
    assert(n == 88 && sbf_pvt_geodetic(b, n, &p) && p.position && !p.accuracy);
    assert(!sbf_pvt_geodetic(b, 84, &p));
    n = time_block(b, 1000, 1, 1, 1, 0, 0, 0, 18, 7);
    assert(!sbf_pvt_geodetic(b, n, &p));
}

static void time_test(void)
{
    uint8_t b[32];
    sbf_time_t t;
    size_t n = time_block(b, 345601000, 26, 10, 19, 18, 30, 5, 18, 7);
    assert(sbf_receiver_time(b, n, &t) && t.tow == 345601000 && t.year == 26 && t.month == 10 && t.day == 19 &&
           t.hour == 18 && t.minute == 30 && t.second == 5 && t.delta_ls == 18 && t.sync == 7);
    n = time_block(b, SBF_TOW_UNKNOWN, -128, -128, -128, -128, -128, -128, -128, 0);
    assert(sbf_receiver_time(b, n, &t) && t.year == -128 && t.delta_ls == -128 && t.sync == 0);
}

typedef struct { unsigned n; sbf_meas_t m[16]; } meas_sink_t;
static void on_meas(const sbf_meas_t *m, void *ctx)
{
    meas_sink_t *s = ctx;
    assert(s->n < 16);
    s->m[s->n++] = *m;
}
static void meas_test(void)
{
    static uint8_t b[SBF_MAX_BLOCK];
    const test_master_t masters[] = {
        {5, 0, 140, true, 2},     // G05 L1C/A, with L2C and L5 slaves
        {82, 17, 120, true, 0},   // E12 E1
        {183, 33, 100, true, 0},  // J03 L1S: a signal number above 31, carried in ObsInfo
        {9, 0, 90, false, 0},     // G09 without a pseudorange
    };
    const test_slave_t slaves[] = {{3, 110, true}, {4, 130, false}};
    size_t n = meas_block(b, 345600000, 0x08, masters, 4, slaves);
    meas_sink_t s = {0};
    uint8_t flags = 0;
    assert(sbf_meas_epoch(b, n, &flags, on_meas, &s) && flags == 0x08 && s.n == 6);
    assert(s.m[0].svid == 5 && s.m[0].signal == 0 && s.m[0].cn0 == 140 && s.m[0].code_valid && s.m[0].antenna == 0);
    assert(s.m[1].svid == 5 && s.m[1].signal == 3 && s.m[1].cn0 == 110 && s.m[1].code_valid);
    assert(s.m[2].svid == 5 && s.m[2].signal == 4 && !s.m[2].code_valid);
    assert(s.m[3].svid == 82 && s.m[3].signal == 17);
    assert(s.m[4].svid == 183 && s.m[4].signal == 33);
    assert(s.m[5].svid == 9 && !s.m[5].code_valid);
    // C/N0: raw x 0.25 + 10 dB-Hz, except the P(Y) signals; 255 is unknown.
    assert(sbf_cn0_centi_dbhz(140, 0) == 4500 && sbf_cn0_centi_dbhz(140, 1) == 3500 && sbf_cn0_centi_dbhz(140, 2) == 3500);
    assert(sbf_cn0_centi_dbhz(255, 0) == -1 && sbf_cn0_centi_dbhz(0, 17) == 1000);
    // Sub-blocks that run past the block, or a master shorter than its fields, reject it whole.
    s.n = 0;
    b[14] = 5; seal(b, SBF_MEAS_EPOCH, 1, n);
    assert(!sbf_meas_epoch(b, n, NULL, on_meas, &s) && s.n == 0);
    b[14] = 4; b[15] = 16; seal(b, SBF_MEAS_EPOCH, 1, n);
    assert(!sbf_meas_epoch(b, n, NULL, on_meas, &s) && s.n == 0);
    b[15] = 20; b[16] = 8; seal(b, SBF_MEAS_EPOCH, 1, n);
    assert(!sbf_meas_epoch(b, n, NULL, on_meas, &s) && s.n == 0);
    // An empty epoch is valid.
    n = meas_block(b, 1000, 0, NULL, 0, NULL);
    assert(sbf_meas_epoch(b, n, NULL, on_meas, &s) && s.n == 0);
}

typedef struct { unsigned n; sbf_sat_t s[8]; } sat_sink_t;
static void on_sat(const sbf_sat_t *sat, void *ctx) { sat_sink_t *s = ctx; s->s[s->n++] = *sat; }
// ReceiverStatus's AGCState sub-blocks and Temperature (reference guide 4.2), and the log text.
static void front_ends_test(void)
{
    uint8_t b[512];
    sbf_rx_status_t st;
    char text[SBF_FRONT_ENDS_TEXT];
    // Nine front ends in a receiver's own order, 27 C, with an L1/L5 antenna in a quiet place.
    static const agc_build_t nine[] = {
        {5, 0, 37, 98, 1}, {6, 0, 46, 104, 1}, {3, 0, 51, 107, 1}, {4, 0, 50, 100, 0}, {12, 0, 51, 98, 0},
        {2, 0, 51, 93, 0}, {0, 0, 40, 96, 0}, {1, 0, 44, 96, 0}, {11, 0, 42, 98, 0},
    };
    size_t n = status_block_agc(b, 607, 0x08, 127, nine, 9, 9, 4);
    assert(n == 68 && sbf_receiver_status(b, n, &st));
    assert(st.uptime_s == 607 && st.cpu_load == 4 && st.rx_state == 0x30 && st.ext_error == 0x08);
    assert(st.temperature_valid && st.temperature_c == 27 && st.front_ends == 9);
    for (unsigned i = 0; i < 9; i++)
        assert(st.front_end[i].code == nine[i].code && st.front_end[i].antenna == 0 && st.front_end[i].locked &&
               st.front_end[i].gain_db == nine[i].gain && st.front_end[i].sample_variance == nine[i].variance &&
               st.front_end[i].blanked_percent == nine[i].blanked);
    static const char expected[] = "L5/E5a/B2a 37/98/1, E5b/B2b 46/104/1, GPSL2 51/107/1, GLOL2 50/100/0, B3 51/98/0, "
                                   "E6 51/93/0, GPSL1/E1 40/96/0, GLOL1 44/96/0, B1 42/98/0";
    assert(sbf_front_ends_text(&st, text, sizeof text) == strlen(expected) && !strcmp(text, expected));
    // Too little room: the text ends after the last front end that fits whole, and stays terminated.
    char small[40];
    memset(small, 'x', sizeof small);
    static const char two[] = "L5/E5a/B2a 37/98/1, E5b/B2b 46/104/1", one[] = "L5/E5a/B2a 37/98/1";
    assert(sbf_front_ends_text(&st, small, sizeof small) == strlen(two) && !strcmp(small, two));
    memset(small, 'x', sizeof small);
    assert(sbf_front_ends_text(&st, small, sizeof one) == strlen(one) && !strcmp(small, one));
    assert(sbf_front_ends_text(&st, small, sizeof one - 1) == 0 && small[0] == 0);
    assert(sbf_front_ends_text(&st, small, 1) == 0 && small[0] == 0 && sbf_front_ends_text(&st, small, 0) == 0);

    // Longer sub-blocks are stepped over by SBLength; -128 dB is an unlocked PLL; an auxiliary
    // antenna and a code the guide does not define are named as such; negative gains are read.
    static const agc_build_t odd[] = {{0, 0, -128, 0, 0}, {5, 1, -3, 255, 100}, {21, 2, 15, 100, 7}, {14, 3, 50, 100, 0}};
    n = status_block_agc(b, 5, 0, 60, odd, 4, 4, 8);
    assert(n == 64 && sbf_receiver_status(b, n, &st) && st.front_ends == 4 && st.temperature_c == -40);
    assert(!st.front_end[0].locked && st.front_end[1].locked && st.front_end[1].gain_db == -3 &&
           st.front_end[1].antenna == 1 && st.front_end[2].code == 21 && st.front_end[2].antenna == 2);
    assert(sbf_front_ends_text(&st, text, sizeof text) &&
           !strcmp(text, "GPSL1/E1 unlocked, aux1 L5/E5a/B2a -3/255/100, aux2 fe21 15/100/7, ant3 B3/E6 combined 50/100/0"));
    assert(!sbf_front_end_name(15) && !sbf_front_end_name(31) && !strcmp(sbf_front_end_name(10), "MSS/L-band"));

    // A Temperature of zero is the Do-Not-Use value. A block that states more sub-blocks than
    // it carries gives those that fit; one whose SBLength is under four gives none.
    n = status_block_agc(b, 5, 0, 0, nine, 2, 9, 4);
    assert(n == 40 && sbf_receiver_status(b, n, &st) && !st.temperature_valid && st.front_ends == 2 &&
           st.front_end[1].code == 6 && st.uptime_s == 5);
    n = status_block_agc(b, 5, 0, 127, nine, 3, 3, 3);
    assert(sbf_receiver_status(b, n, &st) && st.front_ends == 0 && st.temperature_c == 27);
    assert(sbf_front_ends_text(&st, text, sizeof text) == 0 && text[0] == 0);
    // More sub-blocks than the list holds: the first SBF_FRONT_ENDS_MAX are kept, and their text fits.
    agc_build_t many[20];
    for (unsigned i = 0; i < 20; i++) many[i] = (agc_build_t){5, 2, -127, 255, 100};
    n = status_block_agc(b, 5, 0, 127, many, 20, 20, 4);
    assert(sbf_receiver_status(b, n, &st) && st.front_ends == SBF_FRONT_ENDS_MAX);
    assert(sbf_front_ends_text(&st, text, sizeof text) == 16 * strlen("aux2 L5/E5a/B2a -127/255/100") + 15 * 2);
    // A block that ends after RxError still gives the fields before the sub-blocks.
    memset(b, 0, 28);
    b[14] = 255; put32(b + 16, 9); put32(b + 20, 2); put32(b + 24, 0x20);
    n = seal(b, SBF_RECEIVER_STATUS, 0, 28);
    assert(sbf_receiver_status(b, n, &st) && st.uptime_s == 9 && st.cpu_load == 255 && st.rx_error == 0x20 &&
           !st.temperature_valid && st.front_ends == 0);
    // UpTime alone, under the same block checks.
    uint32_t uptime = 0;
    assert(sbf_receiver_uptime(b, n, &uptime) && uptime == 9 && !sbf_receiver_uptime(b, n - 4, &uptime));
    n = pvt_block(b, 2, 1000, 1, 0, 0, 0, 0);
    assert(!sbf_receiver_uptime(b, n, &uptime) && uptime == 9);
}

static void other_blocks_test(void)
{
    static uint8_t b[512];
    const uint8_t svid[] = {5, 82};
    const int16_t elevation[] = {4523, -250};
    size_t n = visibility_block(b, svid, elevation, 2);
    sat_sink_t sats = {0};
    assert(sbf_sat_visibility(b, n, on_sat, &sats) && sats.n == 2 && sats.s[0].svid == 5 && sats.s[0].elevation == 4523 &&
           sats.s[0].azimuth == 9000 && sats.s[1].elevation == -250);
    b[14] = 9; seal(b, SBF_SAT_VISIBILITY, 0, n);
    assert(!sbf_sat_visibility(b, n, on_sat, &sats));

    const uint8_t modes[] = {1, 2, 8, 8, 0x42};
    sbf_rf_t rf;
    n = rf_block(b, 0x01, modes, 5);
    assert(sbf_rf_status(b, n, &rf) && rf.flags == 1 && rf.mitigated == 2 && rf.unmitigated == 2);
    n = rf_block(b, 0, NULL, 0);
    assert(sbf_rf_status(b, n, &rf) && !rf.flags && !rf.mitigated && !rf.unmitigated);

    sbf_rx_status_t st;
    n = status_block(b, 86400, SBF_RXSTATE_ACTIVE_ANTENNA, SBF_RXERROR_CONGESTION);
    assert(sbf_receiver_status(b, n, &st) && st.uptime_s == 86400 && st.cpu_load == 12 &&
           st.rx_state == SBF_RXSTATE_ACTIVE_ANTENNA && st.rx_error == SBF_RXERROR_CONGESTION);
    // Its Temperature byte is 140, and it carries no AGCState sub-block.
    assert(st.temperature_valid && st.temperature_c == 40 && st.front_ends == 0 && st.ext_error == 0);
    front_ends_test();

    sbf_setup_t setup;
    n = setup_block(b, 3, "3802345", "mosaic-G5", "4.15.1", "mosaic-G5 P3");
    assert(sbf_receiver_setup(b, n, &setup) && !strcmp(setup.serial, "3802345") && !strcmp(setup.name, "mosaic-G5") &&
           !strcmp(setup.version, "4.15.1") && !strcmp(setup.product, "mosaic-G5 P3"));
    b[160] = 0x07; seal(b, SBF_RECEIVER_SETUP, 3, n); // a control character is not passed on
    assert(sbf_receiver_setup(b, n, &setup) && setup.serial[4] == '?');
    n = setup_block(b, 0, "1", "mosaic", "4.0", "ignored before revision 3");
    assert(sbf_receiver_setup(b, n, &setup) && !strcmp(setup.version, "4.0") && setup.product[0] == 0);
    assert(!sbf_receiver_setup(b, 212, &setup));
}

static void identity_test(void)
{
    uint8_t g, sv, c;
    const struct { uint8_t svid; bool ok; uint8_t gnss, sv; } svids[] = {
        {0, false, 0, 0}, {1, true, 0, 1}, {37, true, 0, 37}, {38, true, 6, 1}, {61, true, 6, 24},
        {62, false, 0, 0}, {63, true, 6, 25}, {68, true, 6, 30}, {69, false, 0, 0}, {71, true, 2, 1},
        {106, true, 2, 36}, {107, false, 0, 0}, {119, false, 0, 0}, {120, true, 1, 120}, {140, true, 1, 140},
        {141, true, 3, 1}, {180, true, 3, 40}, {181, true, 5, 1}, {190, true, 5, 10}, {191, true, 7, 1},
        {197, true, 7, 7}, {198, true, 1, 141}, {215, true, 1, 158}, {216, true, 7, 8}, {222, true, 7, 14},
        {223, true, 3, 41}, {245, true, 3, 63}, {246, false, 0, 0}, {255, false, 0, 0},
    };
    for (size_t i = 0; i < sizeof svids / sizeof svids[0]; i++) {
        bool ok = sbf_svid(svids[i].svid, &g, &sv);
        assert(ok == svids[i].ok);
        if (ok) assert(g == svids[i].gnss && sv == svids[i].sv);
    }
    // Signals with a reception-space counterpart, the data/pilot pairs merged as nr_signal does.
    const struct { uint8_t number, gnss, canonical; } signals[] = {
        {0, 0, 0}, {3, 0, 3}, {4, 0, 6}, {6, 5, 0}, {7, 5, 4}, {8, 6, 0}, {11, 6, 2}, {13, 3, 5},
        {14, 3, 8}, {15, 7, 0}, {17, 2, 0}, {20, 2, 3}, {21, 2, 5}, {24, 1, 0}, {26, 5, 8}, {28, 3, 0},
        {29, 3, 2}, {33, 5, 1},
        {1, 0, SBF_NO_CANONICAL}, {19, 2, SBF_NO_CANONICAL}, {30, 3, SBF_NO_CANONICAL}, {34, 3, SBF_NO_CANONICAL},
        {37, 7, SBF_NO_CANONICAL}, {25, 1, SBF_NO_CANONICAL}, {39, 5, SBF_NO_CANONICAL},
    };
    for (size_t i = 0; i < sizeof signals / sizeof signals[0]; i++)
        assert(sbf_signal(signals[i].number, &g, &c) && g == signals[i].gnss && c == signals[i].canonical);
    const uint8_t reserved[] = {16, 18, 23, 31, 35, 36, 40, 63};
    for (size_t i = 0; i < sizeof reserved; i++) assert(!sbf_signal(reserved[i], &g, &c));
}

// Random streams and random CRC-valid blocks of every configured number: the reader and every
// decoder must stay within their bounds (run under the sanitizers to check).
static uint32_t rng = 0x9e3779b9u;
static uint32_t next_random(void) { rng ^= rng << 13; rng ^= rng >> 17; rng ^= rng << 5; return rng; }
static void ignore_meas(const sbf_meas_t *m, void *ctx) { (void)m; (void)ctx; }
static void ignore_sat(const sbf_sat_t *s, void *ctx) { (void)s; (void)ctx; }
static void robustness_test(void)
{
    static uint8_t stream[SBF_MAX_BLOCK * 2], b[SBF_MAX_BLOCK];
    sink_t sink = {0};
    sbf_reader_init(&reader, on_block, on_line, on_prompt, &sink);
    for (unsigned round = 0; round < 200; round++) {
        const size_t n = next_random() % sizeof stream;
        for (size_t i = 0; i < n; i++) {
            const uint32_t r = next_random();
            stream[i] = (r & 0xff) < 8 ? (uint8_t)"$@\r\n>$@R"[r % 8] : (uint8_t)(r >> 8);
        }
        size_t at = 0;
        while (at < n) { const size_t piece = 1 + next_random() % 700; sbf_reader_feed(&reader, stream + at, piece > n - at ? n - at : piece); at += piece; }
    }
    static const uint16_t numbers[] = {SBF_MEAS_EPOCH, SBF_PVT_GEODETIC, SBF_RECEIVER_TIME, SBF_SAT_VISIBILITY,
                                       SBF_RECEIVER_STATUS, SBF_RF_STATUS, SBF_RECEIVER_SETUP};
    for (unsigned round = 0; round < 20000; round++) {
        const size_t length = 8 + 4 * (next_random() % 128);
        for (size_t i = 8; i < length; i++) b[i] = (uint8_t)next_random();
        if (round % 3 == 0) { b[14] = (uint8_t)(next_random() % 8); b[15] = (uint8_t)(next_random() % 32); b[16] = (uint8_t)(next_random() % 16); }
        seal(b, numbers[round % 7], (uint8_t)(next_random() % 8), length);
        sbf_pvt_t pvt; sbf_time_t t; sbf_rf_t rf; sbf_rx_status_t st; sbf_setup_t setup;
        (void)sbf_meas_epoch(b, length, NULL, ignore_meas, NULL);
        (void)sbf_sat_visibility(b, length, ignore_sat, NULL);
        (void)sbf_pvt_geodetic(b, length, &pvt);
        (void)sbf_receiver_time(b, length, &t);
        (void)sbf_rf_status(b, length, &rf);
        (void)sbf_receiver_status(b, length, &st);
        (void)sbf_receiver_setup(b, length, &setup);
        // A Length field that disagrees with the length given is refused by every decoder.
        assert(!sbf_receiver_time(b, length + 4, &t) && !sbf_pvt_geodetic(b, length - 4, &pvt));
    }
}

static void signal_names_test(void)
{
    // Every name setSignalTracking takes on a mosaic-G5 (3.2), with its signal number (4.1.10).
    static const struct { const char *name; uint8_t number; } named[] = {
        {"GPSL1CA", 0}, {"GPSL2PY", 2}, {"GPSL2C", 3}, {"GPSL5", 4}, {"GPSL1C", 5}, {"GLOL1CA", 8}, {"GLOL2P", 10},
        {"GLOL2CA", 11}, {"GLOL3", 12}, {"GALE1BC", 17}, {"GALE6BC", 19}, {"GALE5a", 20}, {"GALE5b", 21},
        {"GALE5", 22}, {"GEOL1", 24}, {"GEOL5", 25}, {"BDSB1I", 28}, {"BDSB2I", 29}, {"BDSB3I", 30}, {"BDSB1C", 13},
        {"BDSB2a", 14}, {"BDSB2b", 34}, {"QZSL1CA", 6}, {"QZSL2C", 7}, {"QZSL5", 26}, {"QZSL6", 27}, {"QZSL1C", 32},
        {"QZSL1S", 33}, {"QZSL1CB", 38}, {"NAVICL5", 15},
    };
    for (size_t i = 0; i < sizeof named / sizeof named[0]; i++) {
        assert(sbf_signal_number(named[i].name, strlen(named[i].name)) == named[i].number);
        assert(!strcmp(sbf_signal_name(named[i].number), named[i].name));
    }
    // A name is matched whole and exactly: GALE5 is not GALE5a's start, and case counts.
    assert(sbf_signal_number("GALE5a", 5) == 22 && sbf_signal_number("GALE5A", 6) == -1);
    assert(sbf_signal_number("GPSL1", 5) == -1 && sbf_signal_number("GPSL1CAX", 8) == -1 && sbf_signal_number("", 0) == -1);
    // A number has a name exactly when it is a signal: not the reserved ones, L-band or beyond the table.
    for (unsigned n = 0; n < 256; n++) {
        uint8_t gnss, canonical;
        assert(sbf_signal((uint8_t)n, &gnss, &canonical) == (sbf_signal_name((uint8_t)n) != NULL));
        assert(!sbf_signal_name((uint8_t)n) || n < SBF_SIGNALS);
    }
    assert(!sbf_signal_name(16) && !sbf_signal_name(23) && !sbf_signal_name(31) && !sbf_signal_name(40));
    // A mask's names in the reference guide's order, whatever the numbers' order.
    const uint64_t mask = 1ull << 0 | 1ull << 34 | 1ull << 5 | 1ull << 17;
    char text[64], small[12];
    assert(sbf_signal_names(mask, text, sizeof text) == 29 && !strcmp(text, "GPSL1CA GPSL1C GALE1BC BDSB2b"));
    assert(sbf_signal_names(0, text, sizeof text) == 0 && !text[0]);
    assert(sbf_signal_names(mask, small, sizeof small) == 7 && !strcmp(small, "GPSL1CA")); // the last that fits whole
    assert(sbf_signal_names(mask, small, 0) == 0);
    // Counts per signal number, those counting nothing left out.
    uint8_t counts[SBF_SIGNALS] = {0};
    counts[0] = 7; counts[22] = 6; counts[38] = 1;
    assert(sbf_signal_counts(counts, text, sizeof text) == 29 && !strcmp(text, "GPSL1CA 7, GALE5 6, QZSL1CB 1"));
    assert(sbf_signal_counts(counts, small, sizeof small) == 9 && !strcmp(small, "GPSL1CA 7"));
    memset(counts, 0, sizeof counts);
    assert(sbf_signal_counts(counts, text, sizeof text) == 0 && !text[0]);
}
int main(void)
{
    crc_test();
    signal_names_test();
    reader_test();
    pvt_test();
    time_test();
    meas_test();
    other_blocks_test();
    identity_test();
    robustness_test();
    puts("SBF CRC, reader resynchronization and replies, PVTGeodetic, ReceiverTime, MeasEpoch, SatVisibility, "
         "RFStatus, ReceiverStatus with its front ends and temperature, ReceiverSetup, identity maps, signal "
         "names and random-input bounds passed");
    return 0;
}
