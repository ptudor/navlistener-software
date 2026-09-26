#include "bmp5.h"

enum {
    REG_CHIP_ID = 0x01, REG_INT_CONFIG = 0x14, REG_INT_SOURCE = 0x15, REG_TEMP_XLSB = 0x1d,
    REG_INT_STATUS = 0x27, REG_STATUS = 0x28, REG_OSR_CONFIG = 0x36, REG_ODR_CONFIG = 0x37, REG_CMD = 0x7e,
    CMD_SOFT_RESET = 0xb6,
    STATUS_NVM_RDY = 0x02, STATUS_NVM_ERR = 0x04,
    INT_STATUS_DRDY = 0x01, INT_STATUS_POR = 0x10,
    // INT_CONFIG's reset value leaves the pin disabled, open-drain and active low, as R15's
    // pull-up expects; data ready is enabled as a status bit only.
    INT_CONFIG_RESET = 0x35, INT_SOURCE_DRDY = 0x01,
    OSR_PRESS_EN = 0x40, OSR_P_16X = 4 << 3, OSR_T_2X = 1,
    // The reset ODR field (1 Hz) is kept; deep standby is disabled so every register write
    // lands between conversions, and pwr_mode selects standby or one forced conversion.
    ODR_RESET = 0x70, ODR_DEEP_DIS = 0x80, ODR_STANDBY = 0, ODR_FORCED = 2,
    // Table 4: t_powup and tsoft_res are 2 ms. A forced conversion takes tconv_p at 16x,
    // 10.4 ms, plus tconv_t at 2x, 1.1 ms, each within 5%; data ready is then polled.
    RESET_MS = 2, CONVERSION_MS = 13, POLL_MS = 2, POLLS = 10,
};
static const uint8_t OSR_CONFIG = OSR_PRESS_EN | OSR_P_16X | OSR_T_2X;
static const uint8_t ODR_CONFIG = ODR_RESET | ODR_DEEP_DIS | ODR_STANDBY;

static bool read8(const env_io_t *io, uint8_t reg, uint8_t *value)
{
    return io->read(io->ctx, BMP5_ADDRESS, reg, value, 1);
}

static bool write_checked(const env_io_t *io, uint8_t reg, uint8_t value)
{
    uint8_t back;
    return io->write(io->ctx, BMP5_ADDRESS, reg, &value, 1) && read8(io, reg, &back) && back == value;
}

bmp5_state_t bmp5_init(const env_io_t *io)
{
    const uint8_t reset = CMD_SOFT_RESET;
    (void)io->write(io->ctx, BMP5_ADDRESS, REG_CMD, &reset, 1);
    io->delay_ms(io->ctx, RESET_MS);
    uint8_t id, status, int_status, int_config;
    if (!read8(io, REG_CHIP_ID, &id)) return BMP5_ABSENT;
    // INT_STATUS clears on read, so a POR bit set here comes from this reset.
    if (id != BMP5_CHIP_ID || !read8(io, REG_STATUS, &status) || (status & (STATUS_NVM_RDY | STATUS_NVM_ERR)) != STATUS_NVM_RDY ||
        !read8(io, REG_INT_STATUS, &int_status) || !(int_status & INT_STATUS_POR) ||
        !read8(io, REG_INT_CONFIG, &int_config) || int_config != INT_CONFIG_RESET)
        return BMP5_REJECTED;
    return write_checked(io, REG_OSR_CONFIG, OSR_CONFIG) && write_checked(io, REG_INT_SOURCE, INT_SOURCE_DRDY) &&
           write_checked(io, REG_ODR_CONFIG, ODR_CONFIG) ? BMP5_READY : BMP5_REJECTED;
}

// Rounds value / 2^shift half away from zero.
static int32_t scaled(int64_t value, unsigned shift)
{
    int64_t half = (int64_t)1 << (shift - 1);
    return (int32_t)(value >= 0 ? (value + half) >> shift : -((-value + half) >> shift));
}

bool bmp5_convert(const uint8_t raw[6], int32_t *centi_c, int32_t *pa)
{
    int32_t t = (int32_t)((uint32_t)raw[0] | (uint32_t)raw[1] << 8 | (uint32_t)raw[2] << 16);
    if (t & 0x800000) t -= 0x1000000;
    uint32_t p = (uint32_t)raw[3] | (uint32_t)raw[4] << 8 | (uint32_t)raw[5] << 16;
    int32_t c = scaled((int64_t)t * 100, 16), pascal = scaled(p, 6);
    if (c < BMP5_MIN_CENTI_C || c > BMP5_MAX_CENTI_C || pascal < BMP5_MIN_PA || pascal > BMP5_MAX_PA) return false;
    *centi_c = c; *pa = pascal;
    return true;
}

bool bmp5_read(const env_io_t *io, int32_t *centi_c, int32_t *pa)
{
    // A power cycle or changed configuration requires identification again, even if
    // old data registers still contain plausible values. Only trigger from standby.
    uint8_t int_status, osr, odr, forced = ODR_CONFIG | ODR_FORCED;
    if (!read8(io, REG_OSR_CONFIG, &osr) || osr != OSR_CONFIG ||
        !read8(io, REG_ODR_CONFIG, &odr) || odr != ODR_CONFIG ||
        !read8(io, REG_INT_STATUS, &int_status) || (int_status & INT_STATUS_POR) ||
        !io->write(io->ctx, BMP5_ADDRESS, REG_ODR_CONFIG, &forced, 1))
        return false;
    io->delay_ms(io->ctx, CONVERSION_MS);
    for (unsigned i = 0; ; i++) {
        if (!read8(io, REG_INT_STATUS, &int_status)) return false;
        if (int_status & INT_STATUS_POR) return false;
        if (int_status & INT_STATUS_DRDY) break;
        if (i == POLLS) return false;
        io->delay_ms(io->ctx, POLL_MS);
    }
    uint8_t raw[6];
    return io->read(io->ctx, BMP5_ADDRESS, REG_TEMP_XLSB, raw, sizeof raw) && bmp5_convert(raw, centi_c, pa);
}
