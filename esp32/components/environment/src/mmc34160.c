#include "mmc34160.h"

enum {
    REG_XOUT = 0x00, REG_STATUS = 0x06, REG_CONTROL0 = 0x07, REG_CONTROL1 = 0x08, REG_PRODUCT_ID = 0x20,
    STATUS_MEAS_DONE = 0x01, STATUS_RD_DONE = 0x02,
    CONTROL0_TM = 0x01, CONTROL0_SET = 0x20, CONTROL0_RESET = 0x40, CONTROL0_REFILL = 0x80,
    CONTROL1_BW_16BIT_7MS = 0x00,
    // Operating timing, Rev C p. 11: refill to SET/RESET 50 ms, SET/RESET 1 ms, and a
    // 16-bit (BW 00) measurement 10 ms; the status is then polled a few more times.
    REFILL_MS = 50, SET_RESET_MS = 1, MEASURE_MS = 10, POLL_MS = 2, POLLS = 5,
};

static bool control0(const env_io_t *io, uint8_t value)
{
    return io->write(io->ctx, MMC34160_ADDRESS, REG_CONTROL0, &value, 1);
}

bool mmc34160_init(const env_io_t *io)
{
    uint8_t id, status, bandwidth = CONTROL1_BW_16BIT_7MS;
    return io->read(io->ctx, MMC34160_ADDRESS, REG_PRODUCT_ID, &id, 1) && id == MMC34160_PRODUCT_ID &&
           io->read(io->ctx, MMC34160_ADDRESS, REG_STATUS, &status, 1) && (status & STATUS_RD_DONE) &&
           io->write(io->ctx, MMC34160_ADDRESS, REG_CONTROL1, &bandwidth, 1);
}

// Refill the capacitor, magnetize in one direction, then measure all three axes.
static bool magnetize_and_measure(const env_io_t *io, uint8_t direction, uint16_t out[3])
{
    if (!control0(io, CONTROL0_REFILL)) return false;
    io->delay_ms(io->ctx, REFILL_MS);
    if (!control0(io, direction)) return false;
    io->delay_ms(io->ctx, SET_RESET_MS);
    if (!control0(io, CONTROL0_TM)) return false;
    io->delay_ms(io->ctx, MEASURE_MS);
    uint8_t status = 0;
    for (unsigned i = 0; ; i++) {
        if (!io->read(io->ctx, MMC34160_ADDRESS, REG_STATUS, &status, 1)) return false;
        if (status & STATUS_MEAS_DONE) break;
        if (i == POLLS) return false;
        io->delay_ms(io->ctx, POLL_MS);
    }
    uint8_t raw[6];
    if (!io->read(io->ctx, MMC34160_ADDRESS, REG_XOUT, raw, sizeof raw)) return false;
    for (unsigned axis = 0; axis < 3; axis++) out[axis] = (uint16_t)(raw[2 * axis] | raw[2 * axis + 1] << 8);
    return true;
}

bool mmc34160_combine(const uint16_t set[3], const uint16_t reset[3], mmc34160_sample_t *s)
{
    for (unsigned axis = 0; axis < 3; axis++) {
        // An output at a rail is saturated, not a measurement.
        if (!set[axis] || set[axis] == 0xffff || !reset[axis] || reset[axis] == 0xffff) return false;
        // SET reads +H + offset and RESET reads -H + offset.
        int32_t difference = (int32_t)set[axis] - (int32_t)reset[axis];
        s->field[axis] = (int16_t)(difference / 2);
        s->offset[axis] = (uint16_t)(((uint32_t)set[axis] + reset[axis]) / 2);
    }
    return true;
}

bool mmc34160_measure(const env_io_t *io, mmc34160_sample_t *s)
{
    uint16_t set[3], reset[3];
    return magnetize_and_measure(io, CONTROL0_SET, set) && magnetize_and_measure(io, CONTROL0_RESET, reset) &&
           mmc34160_combine(set, reset, s);
}
