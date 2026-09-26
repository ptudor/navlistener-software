#include "ina3221.h"

enum {
    REG_CONFIG = 0x00, REG_SHUNT1 = 0x01, REG_MASK_ENABLE = 0x0f, REG_MANUFACTURER_ID = 0xfe, REG_DIE_ID = 0xff,
    // Table 5: channels 1-3 enabled, 64 averages, 1.1 ms bus and shunt conversions,
    // continuous shunt and bus.
    CONFIG = 0x7000 | 3 << 9 | 4 << 6 | 4 << 3 | 7,
    MASK_CVRF = 0x0001, // conversion ready; cleared by reading the register
    // One cycle is 3 channels x 2 conversions x 64 averages at 1.1 ms, 0.42 s, or 0.47 s at
    // the 1.21 ms maximum conversion time (Electrical Characteristics); polled to 0.6 s.
    POLL_MS = 50, POLLS = 12,
};

static bool read16(const env_io_t *io, uint8_t reg, uint16_t *value)
{
    uint8_t b[2];
    if (!io->read(io->ctx, INA3221_ADDRESS, reg, b, 2)) return false;
    *value = (uint16_t)(b[0] << 8 | b[1]); // MSB first
    return true;
}

int32_t ina3221_shunt_uv(uint16_t reg) { return (int32_t)(int16_t)(reg & 0xfff8) / 8 * 40; }
int32_t ina3221_bus_mv(uint16_t reg) { return (int32_t)(int16_t)(reg & 0xfff8) / 8 * 8; }

bool ina3221_init(const env_io_t *io)
{
    uint16_t manufacturer, die, config;
    const uint8_t b[2] = {CONFIG >> 8, CONFIG & 0xff};
    return read16(io, REG_MANUFACTURER_ID, &manufacturer) && manufacturer == INA3221_MANUFACTURER_ID &&
           read16(io, REG_DIE_ID, &die) && die == INA3221_DIE_ID &&
           io->write(io->ctx, INA3221_ADDRESS, REG_CONFIG, b, 2) && read16(io, REG_CONFIG, &config) && config == CONFIG;
}

bool ina3221_read(const env_io_t *io, ina3221_sample_t *s)
{
    *s = (ina3221_sample_t){0};
    uint16_t config, mask;
    if (!read16(io, REG_CONFIG, &config) || config != CONFIG) return false;
    for (unsigned i = 0; ; i++) {
        if (!read16(io, REG_MASK_ENABLE, &mask)) return false;
        if (mask & MASK_CVRF) break;
        if (i == POLLS) return false;
        io->delay_ms(io->ctx, POLL_MS);
    }
    ina3221_sample_t out;
    for (unsigned c = 0; c < INA3221_CHANNELS; c++) {
        uint16_t shunt, bus;
        if (!read16(io, REG_SHUNT1 + 2 * c, &shunt) || !read16(io, REG_SHUNT1 + 2 * c + 1, &bus)) return false;
        out.shunt_uv[c] = ina3221_shunt_uv(shunt);
        out.bus_mv[c] = ina3221_bus_mv(bus);
    }
    *s = out;
    return true;
}
