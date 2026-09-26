#include "icm45686.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated ICM-45686: bank-0 registers, the two indirect low-pass registers, a
// packet FIFO in stream mode, and the ordering rules of DS-000577 sections 14, 17.23,
// 17.28 and 17.32.
typedef struct {
    uint8_t regs[128];
    uint8_t fifo[4096];
    size_t fifo_bytes;
    uint16_t ireg_address;
    uint8_t gyro_lpf, accel_lpf; // IPREG_SYS1 0xAC and IPREG_SYS2 0x83
    bool full, absent, reset_stuck, fail_fifo, ireg_ignored;
    unsigned resets, flushes, rule_violations, ireg_busy, rate_writes;
} chip_t;
static uint8_t *ireg(chip_t *c, uint16_t address)
{
    if (address == 0xa4ac) return &c->gyro_lpf;
    if (address == 0xa583) return &c->accel_lpf;
    assert(!"unexpected indirect register"); return NULL;
}
static void push_packet(chip_t *c, uint8_t header, const int16_t a[3], const int16_t g[3], int8_t temp)
{
    uint8_t p[16] = {header};
    for (unsigned i = 0; i < 3; i++) {
        p[1 + 2 * i] = (uint8_t)a[i]; p[2 + 2 * i] = (uint8_t)((uint16_t)a[i] >> 8);
        p[7 + 2 * i] = (uint8_t)g[i]; p[8 + 2 * i] = (uint8_t)((uint16_t)g[i] >> 8);
    }
    p[13] = (uint8_t)temp;
    if (c->fifo_bytes + 16 > 2048) { // stream mode: the oldest packet is overwritten
        memmove(c->fifo, c->fifo + 16, c->fifo_bytes - 16);
        c->fifo_bytes -= 16;
        c->full = true;
    }
    memcpy(c->fifo + c->fifo_bytes, p, 16);
    c->fifo_bytes += 16;
}
static bool chip_read(void *ctx, uint8_t reg, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    if (c->absent) return false;
    if (reg == 0x14) { // FIFO_DATA streams without advancing the register address
        if (c->fail_fifo) return false;
        assert(length <= c->fifo_bytes);
        memcpy(out, c->fifo, length);
        memmove(c->fifo, c->fifo + length, c->fifo_bytes - length);
        c->fifo_bytes -= length;
        return true;
    }
    if (reg == 0x7e) { // IREG_DATA: the pre-fetched indirect register, then the next address
        assert(length == 1);
        out[0] = *ireg(c, c->ireg_address);
        return true;
    }
    for (size_t i = 0; i < length; i++) {
        uint8_t r = reg + i;
        if (r == 0x7f) {
            // IREG_DONE reads 0 while a previous indirect access is still in progress.
            out[i] = (uint8_t)((c->regs[0x7f] & ~1) | (c->ireg_busy ? 0 : 1));
            if (c->ireg_busy) c->ireg_busy--;
        } else if (r == 0x12 || r == 0x13) { // FIFO count in packets, little-endian by default
            unsigned packets = c->fifo_bytes / 16;
            out[i] = r == 0x12 ? packets & 0xff : packets >> 8;
        } else if (r == 0x19) {
            out[i] = c->regs[0x16] & 1 && c->full ? 1 : 0;
            c->full = false; // read to clear
        } else {
            out[i] = c->regs[r];
        }
    }
    return true;
}
static bool chip_write(void *ctx, uint8_t reg, const uint8_t *data, size_t length)
{
    chip_t *c = ctx;
    if (c->absent) return false;
    if (reg == 0x7c) {
        // Section 14: an address alone starts a read pre-fetch; a write carries its
        // address and data in one burst. The low-pass registers are in the main-clock
        // domain, which runs only with a sensor on.
        assert(length == 2 || length == 3);
        c->ireg_address = (uint16_t)(data[0] << 8 | data[1]);
        if (length == 3 && c->regs[0x10] && !c->ireg_ignored) *ireg(c, c->ireg_address) = data[2];
        return true;
    }
    if (length > 1) { // bank 0 auto-increments: the two rate registers are written together
        assert(reg == 0x1b && length == 2);
        c->rate_writes++;
        c->regs[0x1b] = data[0]; c->regs[0x1c] = data[1];
        return true;
    }
    uint8_t v = data[0];
    if ((reg == 0x18 || reg == 0x58) && (c->regs[0x16] || c->regs[0x56])) c->rule_violations++;
    if (reg == 0x1d && (c->regs[0x1d] & 0xc0) && ((c->regs[0x1d] ^ v) & 0x3f)) c->rule_violations++;
    if (reg == 0x20 && (c->regs[0x1d] & 0xc0) && ((c->regs[0x20] ^ v) & 0x08)) c->rule_violations++;
    if (reg == 0x21 && (v & 1) && !(c->regs[0x1d] & 0xc0)) c->rule_violations++;
    if (reg == 0x7f && (v & 2)) {
        c->resets++;
        memset(c->regs, 0, sizeof c->regs);
        c->regs[0x72] = 0xe9; c->regs[0x16] = 0x80; c->regs[0x56] = 0x80;
        c->regs[0x18] = 0x04; c->regs[0x58] = 0x04; c->regs[0x1b] = 0x06; c->regs[0x1c] = 0x06;
        c->regs[0x20] = 0x20; c->regs[0x7f] = c->reset_stuck ? 0x03 : 0x01;
        c->fifo_bytes = 0;
        c->gyro_lpf = 0x80; c->accel_lpf = 0x00; // reset values: gyro OIS HPF bypass set
        return true;
    }
    if (reg == 0x20 && (v & 0x80)) { c->flushes++; c->fifo_bytes = 0; v &= 0x7f; }
    c->regs[reg] = v;
    return true;
}
static void chip_delay(void *ctx, unsigned ms) { (void)ctx; (void)ms; }
static icm45686_io_t chip_io(chip_t *c)
{
    memset(c, 0, sizeof *c);
    c->regs[0x72] = 0xe9; c->regs[0x16] = 0x80; c->regs[0x18] = 0x04; c->regs[0x7f] = 0x01;
    c->gyro_lpf = 0x80;
    return (icm45686_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay_ms = chip_delay};
}

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
    io = chip_io(&c); c.absent = true;
    assert(!icm45686_configure(&io, &ICM45686_SURFACE) && !icm45686_configure(&io, NULL));
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
    assert(icm45686_service(&io, &s) && s.packets == 44 && c.fifo_bytes == 0 && !s.overflows);
    assert(s.latest_packet == 44);
    push_packet(&c, 0x68, missing, still, 5);
    assert(icm45686_service(&io, &s) && s.packets == 45 && s.latest_packet == 44);
    assert(s.latest_valid && s.accel[2] == 4096 && s.gyro[0] == 3 && s.temp == 5);
    const icm45686_window_t *w = &s.window[ICM45686_WINDOW_REPORT];
    assert(w->valid && w->accel_max_sq == 8192u * 8192u && w->accel_min_sq == 100u * 100u);
    assert(w->gyro_max_sq == 3280u * 3280u);
    // A report is built; a bump arrives before it is sent; the next report still has it.
    icm45686_window_snapshot(&s);
    push_packet(&c, 0x68, bump, still, 5);
    assert(icm45686_service(&io, &s));
    icm45686_window_sent(&s);
    assert(w->valid && w->accel_max_sq == 8192u * 8192u && w->accel_min_sq == 8192u * 8192u);
    // A report that is not sent leaves its window open.
    icm45686_window_snapshot(&s);
    push_packet(&c, 0x68, rest, still, 5);
    assert(icm45686_service(&io, &s) && w->accel_max_sq == 8192u * 8192u && w->accel_min_sq < 8192u * 8192u);
    icm45686_window_snapshot(&s);
    icm45686_window_sent(&s);
    assert(!w->valid && s.latest_valid && s.packets == 47);

    // A FIFO left unread fills; stream mode keeps the newest packets and flags it.
    for (unsigned i = 0; i < 200; i++) push_packet(&c, 0x68, rest, still, 6);
    assert(icm45686_service(&io, &s) && s.overflows == 1 && s.packets == 47 + 128);
    assert(w->valid && w->accel_max_sq == w->accel_min_sq);
    assert(icm45686_service(&io, &s) && s.overflows == 1 && s.packets == 175);

    // An undecodable header stops decoding and flushes the FIFO to realign.
    push_packet(&c, 0x68, rest, still, 6);
    push_packet(&c, 0xe8, rest, still, 6); // extended header: not this configuration
    push_packet(&c, 0x68, rest, still, 6);
    unsigned flushes = c.flushes;
    assert(icm45686_service(&io, &s) && s.resyncs == 1 && c.flushes == flushes + 1 && s.packets == 176);
    assert(c.fifo_bytes == 0);
    c.fail_fifo = true;
    push_packet(&c, 0x68, rest, still, 6);
    assert(!icm45686_service(&io, &s));

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
    assert(icm45686_service(&io, &s) && !s.motion_seen);
    assert(!icm45686_rate_due(&s, 1000, &moving));
    // Pulling away: 0.2 g horizontally changes the magnitude by only 2%, but the vector
    // is 0.2 g from gravity.
    const int16_t pulling[3] = {819, 0, 4096};
    push_packet(&c, 0x68, level, noise, 5);
    push_packet(&c, 0x68, pulling, noise, 5);
    assert(icm45686_service(&io, &s) && s.motion_seen);
    assert(icm45686_rate_due(&s, 2000, &moving) && moving);
    assert(icm45686_set_rate(&io, &ICM45686_SURFACE, true, &s) && s.moving && s.rate_changes == 1);
    assert(c.regs[0x1b] == 0x2a && c.regs[0x1c] == 0x2a); // 50 Hz, same ranges
    assert(!icm45686_rate_due(&s, 2000, &moving));
    // Packets queued before the change keep the still period; the flagged one starts 50 Hz.
    icm45686_window_snapshot(&s);
    icm45686_window_sent(&s); // an empty window
    push_packet(&c, 0x68, level, noise, 5);                // 80 ms
    for (unsigned i = 0; i < 4; i++) push_packet(&c, i ? 0x68 : 0x6b, pulling, noise, 5); // 4 x 20 ms
    assert(icm45686_service(&io, &s) && s.period_ms == 20 && !s.pending_period_ms);
    const icm45686_window_t *w = &s.window[ICM45686_WINDOW_REPORT];
    assert(w->samples == 5 && w->span_ms == 160);
    // Time-weighted: half the time level (0 g), half at 0.2 g, so the mean X is 0.1 g.
    assert(w->accel_sum[0] / (int64_t)w->span_ms == 409 && w->accel_sum[2] / (int64_t)w->span_ms == 4096);
    // Turning at 6 dps with gravity alone is motion too; the quiet time restarts.
    const int16_t turning[3] = {0, 0, 197};
    push_packet(&c, 0x68, level, turning, 5);
    assert(icm45686_service(&io, &s) && s.motion_seen && !icm45686_rate_due(&s, 20000, &moving));
    // Cruising smoothly: nothing crosses a threshold. Still again 30 s after the last motion.
    push_packet(&c, 0x68, level, noise, 5);
    assert(icm45686_service(&io, &s) && !s.motion_seen);
    assert(!icm45686_rate_due(&s, 49999, &moving));
    assert(icm45686_rate_due(&s, 50000, &moving) && !moving);
    assert(icm45686_set_rate(&io, &ICM45686_SURFACE, false, &s) && !s.moving && s.rate_changes == 2);
    assert(c.regs[0x1b] == 0x2c && s.pending_period_ms == 80);
    // A flush for realignment puts the new rate in force at once.
    push_packet(&c, 0xe8, level, noise, 5);
    assert(icm45686_service(&io, &s) && s.period_ms == 80 && !s.pending_period_ms);
    // The aerial profile moves to 100 Hz with its wider ranges; its 1 g is 2048 counts.
    icm45686_stats_start(&s, &ICM45686_AERIAL);
    assert(icm45686_set_rate(&io, &ICM45686_AERIAL, true, &s) && c.regs[0x1b] == 0x19 && c.regs[0x1c] == 0x19);
    assert(s.pending_period_ms == 10);
    const int16_t aerial_level[3] = {0, 0, 2048};
    push_packet(&c, 0x6a, aerial_level, noise, 5);
    assert(icm45686_service(&io, &s) && !s.motion_seen && s.period_ms == 10);
    // A rate write that does not read back is reported, and nothing changes.
    unsigned changes = s.rate_changes;
    c.absent = true;
    assert(!icm45686_set_rate(&io, &ICM45686_AERIAL, false, &s) && s.moving && s.rate_changes == changes);

    // Parked on a slope from the start: never motion.
    io = chip_io(&c);
    assert(icm45686_configure(&io, &ICM45686_SURFACE));
    icm45686_stats_start(&s, &ICM45686_SURFACE);
    for (unsigned i = 0; i < 10; i++) push_packet(&c, 0x68, tilted, noise, 5);
    assert(icm45686_service(&io, &s) && !s.motion_seen);
    // Stopping on a different slope (20 degrees) is motion until the reference settles,
    // within about 2.5 s at 12.5 Hz; then the unit is quiet and can step down.
    for (unsigned i = 0; i < 10; i++) push_packet(&c, 0x68, level, noise, 5);
    assert(icm45686_service(&io, &s) && s.motion_seen);
    for (unsigned i = 0; i < 32; i++) push_packet(&c, 0x68, level, noise, 5);
    assert(icm45686_service(&io, &s));
    for (unsigned i = 0; i < 10; i++) push_packet(&c, 0x68, level, noise, 5);
    assert(icm45686_service(&io, &s) && !s.motion_seen);
}

int main(void)
{
    configure_test(); fifo_test(); governor_test();
    puts("ICM-45686 configuration order, filters, FIFO decoding, overflow, realignment and the rate governor passed");
    return 0;
}
