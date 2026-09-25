#include "max31856.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated MAX31856: sixteen registers, auto-incrementing transfers and DRDY_N.
typedef struct {
    uint8_t regs[16];
    bool drdy_low, absent, fail;
    unsigned mode_changes_while_running, transfers;
} chip_t;
static bool chip_transfer(void *ctx, const uint8_t *tx, uint8_t *rx, size_t length)
{
    chip_t *c = ctx;
    assert(length >= 2);
    if (c->fail) return false;
    c->transfers++;
    uint8_t address = tx[0] & 0x7f;
    rx[0] = c->absent ? 0xff : 0;
    for (size_t i = 1; i < length; i++, address = (address + 1) & 0x0f) {
        if (tx[0] & 0x80) {
            if (c->absent || address > 0x0b) continue; // 0Ch-0Fh are read-only
            // The notch may change only in normally-off mode (datasheet CR0 bit 0 note).
            if (address == 0 && (c->regs[0] & 0x80) && ((c->regs[0] ^ tx[i]) & 1))
                c->mode_changes_while_running++;
            c->regs[address] = tx[i] & (address == 0 ? 0xfd : 0xff); // FAULTCLR self-clears
            rx[i] = 0;
        } else {
            rx[i] = c->absent ? 0xff : c->regs[address];
            // Reading the cold-junction or linearized temperature returns DRDY_N high.
            if (address >= 0x0a && address <= 0x0e) c->drdy_low = false;
        }
    }
    return true;
}
static bool chip_ready(void *ctx) { return ((chip_t *)ctx)->drdy_low; }
static max31856_io_t chip_io(chip_t *c)
{
    memset(c, 0, sizeof *c);
    static const uint8_t defaults[16] = {0x00, 0x03, 0xff, 0x7f, 0xc0, 0x7f, 0xff, 0x80, 0x00};
    memcpy(c->regs, defaults, sizeof defaults);
    return (max31856_io_t){.ctx = c, .transfer = chip_transfer, .ready = chip_ready};
}
static void set_temperatures(chip_t *c, uint16_t cj, uint32_t tc, uint8_t fault)
{
    c->regs[0x0a] = cj >> 8; c->regs[0x0b] = cj;
    c->regs[0x0c] = tc >> 16; c->regs[0x0d] = tc >> 8; c->regs[0x0e] = tc;
    c->regs[0x0f] = fault;
    c->drdy_low = true;
}

static void decode_test(void)
{
    // Tables 2 and 3 of Maxim 19-7534 Rev 0.
    static const struct { uint16_t code; int16_t centi; } cj[] = {
        {0x7ffc, 12798}, {0x7f00, 12700}, {0x1900, 2500}, {0x0080, 50}, {0x0004, 2}, {0, 0},
        {0xff80, -50}, {0xe700, -2500}, {0xc900, -5500}};
    static const struct { uint32_t code; int32_t centi; } tc[] = {
        {0x640000, 160000}, {0x3e8000, 100000}, {0x064f00, 10094}, {0x019000, 2500}, {0x000100, 6},
        {0, 0}, {0xffff00, -6}, {0xfffc00, -25}, {0xfff000, -100}, {0xf06000, -25000}};
    max31856_sample_t s;
    for (size_t i = 0; i < sizeof cj / sizeof cj[0]; i++) {
        uint8_t r[6] = {cj[i].code >> 8, cj[i].code, 0, 0, 0, 0};
        max31856_decode(r, true, &s);
        assert(s.valid == (MAX31856_CJ_VALID | MAX31856_TC_VALID) && s.cj_centi_c == cj[i].centi);
    }
    for (size_t i = 0; i < sizeof tc / sizeof tc[0]; i++) {
        uint8_t r[6] = {0, 0, tc[i].code >> 16, tc[i].code >> 8, tc[i].code, 0};
        max31856_decode(r, true, &s);
        assert(s.tc_centi_c == tc[i].centi);
    }
    // Any fault rejects the thermocouple; only cold-junction faults reject that reading.
    uint8_t r[6] = {0x19, 0x00, 0x01, 0x90, 0x00, MAX31856_OPEN};
    max31856_decode(r, true, &s);
    assert(s.valid == MAX31856_CJ_VALID && s.cj_centi_c == 2500 && s.tc_centi_c == 0 && s.fault == MAX31856_OPEN);
    r[5] = MAX31856_CJ_RANGE;
    max31856_decode(r, true, &s);
    assert(s.valid == 0 && s.cj_centi_c == 0 && s.fault == MAX31856_CJ_RANGE);
    // A stale conversion is never valid, but its fault byte is still reported.
    r[5] = MAX31856_OVUV;
    max31856_decode(r, false, &s);
    assert(!s.valid && !s.fresh && s.fault == MAX31856_OVUV);
}

static void driver_test(void)
{
    chip_t c; max31856_io_t io = chip_io(&c);
    max31856_sample_t s;
    assert(max31856_cr0(false) == 0x90 && max31856_cr0(true) == 0x91 && max31856_cr1() == 0x23);
    assert(max31856_configure(&io, false) && c.regs[0] == 0x90 && c.regs[1] == 0x23);
    set_temperatures(&c, 0x1900, 0x064f00, 0);
    assert(max31856_read(&io, false, &s) && s.fresh && s.valid == 3 && s.cj_centi_c == 2500 && s.tc_centi_c == 10094);
    assert(!c.drdy_low); // the read consumed the conversion
    assert(max31856_read(&io, false, &s) && !s.fresh && !s.valid);
    // Switching to 50 Hz while converting stops conversions before the notch changes.
    assert(max31856_configure(&io, true) && c.regs[0] == 0x91 && c.mode_changes_while_running == 0);
    // A configuration lost to a reset or power cycle is reported for reconfiguration.
    assert(!max31856_read(&io, false, &s));
    c.regs[0] = 0x00; c.regs[1] = 0x03;
    assert(!max31856_read(&io, true, &s));
    // An absent converter's floating data line fails the readback.
    io = chip_io(&c); c.absent = true;
    assert(!max31856_configure(&io, false) && !max31856_read(&io, false, &s));
    io = chip_io(&c); c.fail = true;
    assert(!max31856_configure(&io, false) && !max31856_read(&io, false, &s));
}

int main(void)
{
    decode_test(); driver_test();
    puts("MAX31856 temperature formats, fault rejection, configuration and DRDY sequencing passed");
    return 0;
}
