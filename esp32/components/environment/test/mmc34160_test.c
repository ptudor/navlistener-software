#include "mmc34160.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated MMC34160PJ: the sensing element's magnetization follows the last SET or
// RESET made after a full capacitor refill; a measurement reads +H or -H plus offset.
typedef struct {
    int32_t field[3], offset[3];
    int magnetization;        // +1 after SET, -1 after RESET, 0 unknown
    unsigned since_refill, since_measure, elapsed;
    bool refilled, measuring, done, rd_done, absent, stuck, fail_write;
    uint8_t id, control1, out[6];
    unsigned measurements;
} chip_t;
static bool chip_read(void *ctx, uint8_t address, uint8_t reg, uint8_t *p, size_t n)
{
    chip_t *c = ctx;
    assert(address == MMC34160_ADDRESS);
    if (c->absent) return false;
    if (reg == 0x20) { assert(n == 1); *p = c->id; return true; }
    if (reg == 0x06) {
        assert(n == 1);
        if (c->measuring && c->since_measure >= 8 && !c->stuck) { c->measuring = false; c->done = true; }
        *p = (c->done ? 1 : 0) | (c->rd_done ? 2 : 0);
        return true;
    }
    assert(reg == 0x00 && n == 6 && c->done); // Meas Done is checked before the output
    memcpy(p, c->out, 6);
    return true;
}
static bool chip_write(void *ctx, uint8_t address, uint8_t reg, const uint8_t *p, size_t n)
{
    chip_t *c = ctx;
    assert(address == MMC34160_ADDRESS && n == 1);
    if (c->absent || c->fail_write) return false;
    if (reg == 0x08) { c->control1 = *p; return true; }
    assert(reg == 0x07);
    if (*p == 0x80) { c->refilled = true; c->since_refill = 0; return true; }
    if (*p == 0x20 || *p == 0x40) {
        // SET and RESET need the capacitor refilled at least 50 ms earlier.
        if (c->refilled && c->since_refill >= 50) c->magnetization = *p == 0x20 ? 1 : -1;
        c->refilled = false;
        return true;
    }
    assert(*p == 0x01 && c->control1 == 0); // TM, 16-bit mode
    for (unsigned axis = 0; axis < 3; axis++) {
        int32_t value = c->offset[axis] + c->magnetization * c->field[axis];
        if (value < 0) value = 0;
        if (value > 0xffff) value = 0xffff;
        c->out[2 * axis] = value; c->out[2 * axis + 1] = value >> 8;
    }
    c->measuring = true; c->done = false; c->since_measure = 0; c->measurements++;
    return true;
}
static void chip_delay(void *ctx, unsigned ms)
{
    chip_t *c = ctx;
    c->since_refill += ms; c->since_measure += ms; c->elapsed += ms;
}
static env_io_t chip_io(chip_t *c)
{
    memset(c, 0, sizeof *c);
    c->id = 0x06; c->rd_done = true;
    // Roughly an Earth-strength field (0.25, -0.1, 0.45 G) and a small bridge offset.
    c->field[0] = 512; c->field[1] = -205; c->field[2] = 922;
    c->offset[0] = 32768 + 40; c->offset[1] = 32768 - 17; c->offset[2] = 32768 + 3;
    return (env_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay_ms = chip_delay};
}

int main(void)
{
    chip_t c; env_io_t io = chip_io(&c);
    mmc34160_sample_t s;
    c.control1 = 0x03;
    assert(mmc34160_init(&io) && c.control1 == 0);
    assert(mmc34160_measure(&io, &s) && c.measurements == 2);
    assert(s.field[0] == 512 && s.field[1] == -205 && s.field[2] == 922);
    assert(s.offset[0] == 32808 && s.offset[1] == 32751 && s.offset[2] == 32771);
    // Both refills get their full 50 ms; a measurement takes at least 10 ms.
    assert(c.elapsed >= 2 * (50 + 1 + 10));
    // A changed offset does not change the field: SET/RESET removes it.
    c.offset[0] = 30000;
    assert(mmc34160_measure(&io, &s) && s.field[0] == 512 && s.offset[0] == 30000);

    // A field beyond 16 G saturates an output; that pair is rejected.
    c.field[2] = 40000;
    assert(!mmc34160_measure(&io, &s));
    uint16_t set[3] = {1, 2, 3}, reset[3] = {3, 2, 0};
    assert(!mmc34160_combine(set, reset, &s));
    set[2] = 65534; reset[2] = 1;
    assert(mmc34160_combine(set, reset, &s) && s.field[2] == 32766);

    io = chip_io(&c); c.stuck = true;
    assert(!mmc34160_measure(&io, &s));
    io = chip_io(&c); c.fail_write = true;
    assert(!mmc34160_measure(&io, &s));
    io = chip_io(&c); c.id = 0x07;
    assert(!mmc34160_init(&io));
    io = chip_io(&c); c.rd_done = false;
    assert(!mmc34160_init(&io));
    io = chip_io(&c); c.absent = true;
    assert(!mmc34160_init(&io) && !mmc34160_measure(&io, &s));
    puts("MMC34160PJ SET/RESET offset removal, timing, saturation and identification passed");
    return 0;
}
