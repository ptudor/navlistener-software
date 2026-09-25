#include "max31856.h"
#include <string.h>

enum {
    REG_CR0 = 0x00, REG_CR1 = 0x01, REG_CJTH = 0x0a, WRITE = 0x80,
    CR0_CMODE = 0x80, CR0_OCFAULT_UNDER_5K = 0x10, CR0_50HZ = 0x01,
    CR1_AVERAGE_4 = 0x20,
};

uint8_t max31856_cr0(bool filter_50hz) { return CR0_CMODE | CR0_OCFAULT_UNDER_5K | (filter_50hz ? CR0_50HZ : 0); }
uint8_t max31856_cr1(void) { return CR1_AVERAGE_4 | MAX31856_TYPE_K; }

static bool write_register(const max31856_io_t *io, uint8_t reg, uint8_t value)
{
    uint8_t tx[2] = {WRITE | reg, value}, rx[2];
    return io->transfer(io->ctx, tx, rx, sizeof tx);
}

bool max31856_configure(const max31856_io_t *io, bool filter_50hz)
{
    // The notch may change only in normally-off mode: stop conversions with the current
    // notch, then change it.
    uint8_t tx[3] = {REG_CR0, 0, 0}, rx[3];
    if (!io->transfer(io->ctx, tx, rx, 2)) return false;
    uint8_t desired = max31856_cr0(filter_50hz);
    return write_register(io, REG_CR0, rx[1] & ~CR0_CMODE) &&
           write_register(io, REG_CR0, desired & ~CR0_CMODE) &&
           write_register(io, REG_CR1, max31856_cr1()) && write_register(io, REG_CR0, desired) &&
           io->transfer(io->ctx, tx, rx, sizeof tx) && rx[1] == desired && rx[2] == max31856_cr1();
}

void max31856_decode(const uint8_t r[6], bool fresh, max31856_sample_t *s)
{
    memset(s, 0, sizeof *s);
    s->fault = r[5];
    s->fresh = fresh;
    // CJTH:CJTL is degrees Celsius times 256 (CJTL bits 1:0 read zero).
    int32_t cj = (int16_t)(r[0] << 8 | r[1]);
    // LTCBH:LTCBM:LTCBL is a 24-bit two's-complement value in 1/4096 C (LTCBL bits 4:0 unused).
    int32_t tc = (int32_t)((uint32_t)r[2] << 16 | (uint32_t)r[3] << 8 | r[4]);
    if (tc & 0x800000) tc -= 0x1000000;
    if (fresh && !(s->fault & MAX31856_CJ_FAULTS)) {
        s->valid |= MAX31856_CJ_VALID;
        s->cj_centi_c = (int16_t)((cj * 100 + (cj >= 0 ? 128 : -128)) / 256);
    }
    if (fresh && !s->fault) {
        s->valid |= MAX31856_TC_VALID;
        s->tc_centi_c = (int32_t)(((int64_t)tc * 100 + (tc >= 0 ? 2048 : -2048)) / 4096);
    }
}

bool max31856_read(const max31856_io_t *io, bool filter_50hz, max31856_sample_t *s)
{
    memset(s, 0, sizeof *s);
    // DRDY_N first: the register read below is what returns it high.
    bool fresh = io->ready(io->ctx);
    uint8_t tx[17] = {REG_CR0}, rx[17];
    if (!io->transfer(io->ctx, tx, rx, sizeof tx)) return false;
    const uint8_t *regs = rx + 1;
    if (regs[REG_CR0] != max31856_cr0(filter_50hz) || regs[REG_CR1] != max31856_cr1()) return false;
    max31856_decode(regs + REG_CJTH, fresh, s);
    return true;
}
