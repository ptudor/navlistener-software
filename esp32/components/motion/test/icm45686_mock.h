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
    bool full, absent, reset_stuck, fail_fifo, ireg_ignored, flush_stuck;
    unsigned flush_busy, count_reads;
    uint16_t first_count; // stale first read, AN-000364
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
            if (r == 0x12) c->count_reads++;
            unsigned packets = c->first_count && c->count_reads % 2 ? c->first_count : c->fifo_bytes / 16;
            out[i] = r == 0x12 ? packets & 0xff : packets >> 8;
        } else if (r == 0x20 && (c->regs[r] & 0x80)) {
            if (c->flush_busy) c->flush_busy--;
            else if (!c->flush_stuck) { c->regs[r] &= ~0x80; c->fifo_bytes = 0; }
            out[i] = c->regs[r];
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
    if (reg == 0x20 && (v & 0x80)) { c->flushes++; c->flush_busy = 2; }
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
