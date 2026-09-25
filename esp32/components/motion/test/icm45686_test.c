#include "icm45686.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated ICM-45686: bank-0 registers, a packet FIFO in stream mode, and the
// ordering rules of DS-000577 sections 17.23, 17.28 and 17.32.
typedef struct {
    uint8_t regs[128];
    uint8_t fifo[4096];
    size_t fifo_bytes;
    bool full, absent, reset_stuck, fail_fifo;
    unsigned resets, flushes, rule_violations;
} chip_t;
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
    for (size_t i = 0; i < length; i++) {
        uint8_t r = reg + i;
        if (r == 0x12 || r == 0x13) { // FIFO count in packets, little-endian by default
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
    assert(length == 1);
    if (c->absent) return false;
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
    return (icm45686_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay_ms = chip_delay};
}

static void configure_test(void)
{
    chip_t c; icm45686_io_t io = chip_io(&c);
    assert(icm45686_configure(&io) && c.resets == 1 && c.rule_violations == 0);
    assert(c.regs[0x10] == 0x0f && c.regs[0x1b] == 0x29 && c.regs[0x1c] == 0x29);
    assert(c.regs[0x1d] == 0x47 && c.regs[0x21] == 0x07 && c.regs[0x22] == 0);
    assert(c.regs[0x1e] == 32 && c.regs[0x1f] == 0 && (c.regs[0x20] & 0x08));
    // Both interrupt outputs push-pull and active high, as the board wires them.
    assert(c.regs[0x18] == 0x03 && c.regs[0x58] == 0x01 && c.regs[0x16] == 0x03 && c.regs[0x56] == 0);
    assert(c.flushes == 1);
    io = chip_io(&c); c.regs[0x72] = 0x47;
    assert(!icm45686_configure(&io) && c.resets == 0);
    io = chip_io(&c); c.reset_stuck = true;
    assert(!icm45686_configure(&io));
    io = chip_io(&c); c.absent = true;
    assert(!icm45686_configure(&io));
}

static void fifo_test(void)
{
    chip_t c; icm45686_io_t io = chip_io(&c);
    icm45686_stats_t s = {0};
    assert(icm45686_configure(&io));
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

int main(void)
{
    configure_test(); fifo_test();
    puts("ICM-45686 configuration order, FIFO decoding, overflow, invalid samples and realignment passed");
    return 0;
}
