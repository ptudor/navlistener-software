#include "icm45686_mock.h"

static void configure_test(void)
{
    chip_t c; icm45686_io_t io = chip_io(&c);
    assert(icm45686_configure(&io, &ICM45686_SURFACE) && c.resets == 1 && c.rule_violations == 0);
    // Still at 12.5 Hz, +/-8 g and +/-1000 dps, both written at once.
    assert(c.regs[0x10] == 0x0f && c.regs[0x1b] == 0x2c && c.regs[0x1c] == 0x2c && c.rate_writes == 1);
    // Both low-pass filters at ODR/4, the gyroscope register's other bits kept.
    assert(c.gyro_lpf == 0x81 && c.accel_lpf == 0x01);
    assert(c.regs[0x1d] == 0x47 && c.regs[0x21] == 0x07 && c.regs[0x22] == 0);
    assert(c.regs[0x1e] == 16 && c.regs[0x1f] == 0 && (c.regs[0x20] & 0x08));
    // Both interrupt outputs push-pull and active high, as the board wires them.
    assert(c.regs[0x18] == 0x03 && c.regs[0x58] == 0x01 && c.regs[0x16] == 0x03 && c.regs[0x56] == 0);
    assert(c.flushes == 1);
    // Aerial ranges, +/-16 g and +/-2000 dps, also start still; the same filters.
    io = chip_io(&c); c.ireg_busy = 3; // a slow indirect access is waited for
    assert(icm45686_configure(&io, &ICM45686_AERIAL) && c.regs[0x1b] == 0x1c && c.regs[0x1c] == 0x1c);
    assert(c.gyro_lpf == 0x81 && c.accel_lpf == 0x01 && !c.ireg_busy);
    // A filter that does not take is a failed configuration, not an unfiltered one.
    io = chip_io(&c); c.ireg_ignored = true;
    assert(!icm45686_configure(&io, &ICM45686_SURFACE));
    io = chip_io(&c); c.ireg_busy = 100;
    assert(!icm45686_configure(&io, &ICM45686_SURFACE));
    io = chip_io(&c); c.regs[0x72] = 0x47;
    assert(!icm45686_configure(&io, &ICM45686_SURFACE) && c.resets == 0);
    io = chip_io(&c); c.reset_stuck = true;
    assert(!icm45686_configure(&io, &ICM45686_SURFACE));
    io = chip_io(&c); c.flush_stuck = true;
    assert(!icm45686_configure(&io, &ICM45686_SURFACE));
    io = chip_io(&c); c.absent = true;
    assert(!icm45686_configure(&io, &ICM45686_SURFACE) && !icm45686_configure(&io, NULL));
}

// Isolated batches below end with an invalid sentinel that the streaming FIFO must
// retain. Remove that sentinel between batches so parser/governor assertions describe
// only the samples supplied by each test. streaming_test exercises carry-over directly.
static bool drain(const icm45686_io_t *io, icm45686_stats_t *s)
{
    chip_t *c = io->ctx;
    const int16_t missing[3] = {INT16_MIN, INT16_MIN, INT16_MIN};
    push_packet(c, 0x68, missing, missing, 0);
    bool ok = icm45686_service(io, s);
    if (ok) {
        assert(c->fifo_bytes == 0 || c->fifo_bytes == ICM45686_PACKET);
        c->fifo_bytes = 0;
    }
    return ok;
}

static void streaming_test(void)
{
    chip_t c; icm45686_io_t io = chip_io(&c);
    icm45686_stats_t s = {0};
    assert(icm45686_configure(&io, &ICM45686_SURFACE));
    icm45686_stats_start(&s, &ICM45686_SURFACE);
    const int16_t level[3] = {0, 0, 4096}, zero[3] = {0};
    // Even an empty FIFO must be read twice; a stale first count cannot cause over-read.
    c.first_count = 127;
    assert(icm45686_service(&io, &s) && c.count_reads == 2 && s.packets == 0);
    push_packet(&c, 0x68, level, zero, 0);
    assert(icm45686_service(&io, &s) && s.packets == 0 && c.fifo_bytes == 16);
    push_packet(&c, 0x68, level, zero, 0);
    assert(icm45686_service(&io, &s) && s.packets == 1 && c.fifo_bytes == 16);
    for (unsigned i = 0; i < 20; i++) push_packet(&c, 0x68, level, zero, 0);
    assert(icm45686_service(&io, &s) && s.packets == 21 && c.fifo_bytes == 16 && c.count_reads == 8);
    // Do not silently clamp impossible counts into an apparently valid transfer.
    c.fifo_bytes = 129 * ICM45686_PACKET;
    assert(!icm45686_service(&io, &s) && s.packets == 21);
    // A reset invalidates the latest reading, but preserves the report window/counters.
    assert(s.latest_valid);
    icm45686_stats_start(&s, &ICM45686_SURFACE);
    assert(!s.latest_valid && s.packets == 21 && s.window[ICM45686_WINDOW_REPORT].samples == 21);
}

static void fifo_test(void)
{
    chip_t c; icm45686_io_t io = chip_io(&c);
    icm45686_stats_t s = {0};
    assert(icm45686_configure(&io, &ICM45686_SURFACE));
    icm45686_stats_start(&s, &ICM45686_SURFACE);
    // At rest on its back: +1 g on Z (4096 counts), small gyro noise, 27.5 C.
    const int16_t rest[3] = {10, -20, 4096}, still[3] = {3, -2, 1};
    const int16_t bump[3] = {0, 0, 8192}, fall[3] = {0, 0, 100}, turn[3] = {0, 3280, 0};
    const int16_t missing[3] = {INT16_MIN, 0, 0};
    for (unsigned i = 0; i < 40; i++) push_packet(&c, 0x68, rest, still, 5);
    push_packet(&c, 0x68, bump, still, 5);
    push_packet(&c, 0x68, fall, turn, 5);
    push_packet(&c, 0x68, missing, still, 5); // counted but never the latest or an extreme
    push_packet(&c, 0x6b, rest, still, 5);    // the ODR-change flags are informational
    assert(drain(&io, &s) && s.packets == 44 && c.fifo_bytes == 0 && !s.overflows);
    assert(s.latest_packet == 44);
    push_packet(&c, 0x68, missing, still, 5);
    assert(drain(&io, &s) && s.packets == 45 && s.latest_packet == 44);
    assert(s.latest_valid && s.accel[2] == 4096 && s.gyro[0] == 3 && s.temp == 5);
    const icm45686_window_t *w = &s.window[ICM45686_WINDOW_REPORT];
    assert(w->valid && w->accel_max_sq == 8192u * 8192u && w->accel_min_sq == 100u * 100u);
    assert(w->gyro_max_sq == 3280u * 3280u);
    // A report is built; a bump arrives before it is sent; the next report still has it.
    icm45686_window_snapshot(&s);
    push_packet(&c, 0x68, bump, still, 5);
    assert(drain(&io, &s));
    icm45686_window_sent(&s);
    assert(w->valid && w->accel_max_sq == 8192u * 8192u && w->accel_min_sq == 8192u * 8192u);
    // A report that is not sent leaves its window open.
    icm45686_window_snapshot(&s);
    push_packet(&c, 0x68, rest, still, 5);
    assert(drain(&io, &s) && w->accel_max_sq == 8192u * 8192u && w->accel_min_sq < 8192u * 8192u);
    icm45686_window_snapshot(&s);
    icm45686_window_sent(&s);
    assert(!w->valid && s.latest_valid && s.packets == 47);

    // A FIFO left unread fills; stream mode keeps the newest packets and flags it.
    for (unsigned i = 0; i < 200; i++) push_packet(&c, 0x68, rest, still, 6);
    assert(drain(&io, &s) && s.overflows == 1 && s.packets == 47 + 127);
    assert(w->valid && w->accel_max_sq == w->accel_min_sq);
    assert(drain(&io, &s) && s.overflows == 1 && s.packets == 174);

    // An undecodable header stops decoding and flushes the FIFO to realign.
    push_packet(&c, 0x68, rest, still, 6);
    push_packet(&c, 0xe8, rest, still, 6); // extended header: not this configuration
    push_packet(&c, 0x68, rest, still, 6);
    unsigned flushes = c.flushes;
    assert(drain(&io, &s) && s.resyncs == 1 && c.flushes == flushes + 1 && s.packets == 175);
    assert(c.fifo_bytes == 0);
    c.fail_fifo = true;
    push_packet(&c, 0x68, rest, still, 6);
    assert(!drain(&io, &s));

    // Accelerometer-only, gyroscope-only and high-resolution packets are not this format.
    icm45686_stats_t t = {0};
    uint8_t raw[16] = {0x40};
    assert(icm45686_parse(raw, sizeof raw, &t) == 0);
    raw[0] = 0x78;
    assert(icm45686_parse(raw, sizeof raw, &t) == 0);
    raw[0] = 0x68;
    assert(icm45686_parse(raw, 15, &t) == 0 && icm45686_parse(raw, 16, &t) == 16 && t.packets == 1);
}

// The governor: 12.5 Hz while still, the profile's moving rate from the first sample
// that shows motion until 30 s pass without any, and time-weighted window means.
static void governor_test(void)
{
    chip_t c; icm45686_io_t io = chip_io(&c);
    icm45686_stats_t s = {0};
    bool moving = false;
    assert(icm45686_configure(&io, &ICM45686_SURFACE));
    icm45686_stats_start(&s, &ICM45686_SURFACE);
    assert(s.period_ms == 80 && !s.moving && icm45686_period_ms(500) == 20 && icm45686_period_ms(1000) == 10);
    // Parked: gravity alone and no rotation.
    const int16_t level[3] = {0, 0, 4096}, tilted[3] = {0, 1416, 3843}, noise[3] = {20, -30, 10};
    for (unsigned i = 0; i < 10; i++) push_packet(&c, 0x68, level, noise, 5);
    assert(drain(&io, &s) && !s.motion_seen);
    assert(!icm45686_rate_due(&s, 1000, &moving));
    // Pulling away: 0.2 g horizontally changes the magnitude by only 2%, but the vector
    // is 0.2 g from gravity.
    const int16_t pulling[3] = {819, 0, 4096};
    push_packet(&c, 0x68, level, noise, 5);
    push_packet(&c, 0x68, pulling, noise, 5);
    assert(drain(&io, &s) && s.motion_seen);
    assert(icm45686_rate_due(&s, 2000, &moving) && moving);
    assert(icm45686_set_rate(&io, &ICM45686_SURFACE, true, &s) && s.moving && s.rate_changes == 1);
    assert(c.regs[0x1b] == 0x2a && c.regs[0x1c] == 0x2a); // 50 Hz, same ranges
    assert(!icm45686_rate_due(&s, 2000, &moving));
    // Packets queued before the change keep the still period; the flagged one starts 50 Hz.
    icm45686_window_snapshot(&s);
    icm45686_window_sent(&s); // an empty window
    push_packet(&c, 0x68, level, noise, 5);                // 80 ms
    for (unsigned i = 0; i < 4; i++) push_packet(&c, i ? 0x68 : 0x6b, pulling, noise, 5); // 4 x 20 ms
    assert(drain(&io, &s) && s.period_ms == 20 && !s.pending_period_ms);
    const icm45686_window_t *w = &s.window[ICM45686_WINDOW_REPORT];
    assert(w->samples == 5 && w->span_ms == 160);
    // Time-weighted: half the time level (0 g), half at 0.2 g, so the mean X is 0.1 g.
    assert(w->accel_sum[0] / (int64_t)w->span_ms == 409 && w->accel_sum[2] / (int64_t)w->span_ms == 4096);
    // Turning at 6 dps with gravity alone is motion too; the quiet time restarts.
    const int16_t turning[3] = {0, 0, 197};
    push_packet(&c, 0x68, level, turning, 5);
    assert(drain(&io, &s) && s.motion_seen && !icm45686_rate_due(&s, 20000, &moving));
    // Cruising smoothly: nothing crosses a threshold. Still again 30 s after the last motion.
    push_packet(&c, 0x68, level, noise, 5);
    assert(drain(&io, &s) && !s.motion_seen);
    assert(!icm45686_rate_due(&s, 49999, &moving));
    assert(icm45686_rate_due(&s, 50000, &moving) && !moving);
    assert(icm45686_set_rate(&io, &ICM45686_SURFACE, false, &s) && !s.moving && s.rate_changes == 2);
    assert(c.regs[0x1b] == 0x2c && s.pending_period_ms == 80);
    // A flush for realignment puts the new rate in force at once.
    push_packet(&c, 0xe8, level, noise, 5);
    assert(drain(&io, &s) && s.period_ms == 80 && !s.pending_period_ms);
    // The aerial profile moves to 100 Hz with its wider ranges; its 1 g is 2048 counts.
    icm45686_stats_start(&s, &ICM45686_AERIAL);
    assert(icm45686_set_rate(&io, &ICM45686_AERIAL, true, &s) && c.regs[0x1b] == 0x19 && c.regs[0x1c] == 0x19);
    assert(s.pending_period_ms == 10);
    const int16_t aerial_level[3] = {0, 0, 2048};
    push_packet(&c, 0x6a, aerial_level, noise, 5);
    assert(drain(&io, &s) && !s.motion_seen && s.period_ms == 10);
    // A rate write that does not read back is reported, and nothing changes.
    unsigned changes = s.rate_changes;
    c.absent = true;
    assert(!icm45686_set_rate(&io, &ICM45686_AERIAL, false, &s) && s.moving && s.rate_changes == changes);

    // Parked on a slope from the start: never motion.
    io = chip_io(&c);
    assert(icm45686_configure(&io, &ICM45686_SURFACE));
    icm45686_stats_start(&s, &ICM45686_SURFACE);
    for (unsigned i = 0; i < 10; i++) push_packet(&c, 0x68, tilted, noise, 5);
    assert(drain(&io, &s) && !s.motion_seen);
    // Stopping on a different slope (20 degrees) is motion until the reference settles,
    // within about 2.5 s at 12.5 Hz; then the unit is quiet and can step down.
    for (unsigned i = 0; i < 10; i++) push_packet(&c, 0x68, level, noise, 5);
    assert(drain(&io, &s) && s.motion_seen);
    for (unsigned i = 0; i < 32; i++) push_packet(&c, 0x68, level, noise, 5);
    assert(drain(&io, &s));
    for (unsigned i = 0; i < 10; i++) push_packet(&c, 0x68, level, noise, 5);
    assert(drain(&io, &s) && !s.motion_seen);
}

int main(void)
{
    configure_test(); streaming_test(); fifo_test(); governor_test();
    puts("ICM-45686 configuration order, filters, FIFO decoding, overflow, realignment and the rate governor passed");
    return 0;
}
