#include "ina3221.h"

enum {
    REG_CONFIG = 0x00, REG_SHUNT1 = 0x01, REG_CRITICAL1 = 0x07, REG_MASK_ENABLE = 0x0f,
    REG_MANUFACTURER_ID = 0xfe, REG_DIE_ID = 0xff,
    // Table 5: channels 1-3 enabled, 64 averages, 1.1 ms bus and shunt conversions,
    // continuous shunt and bus.
    CONFIG = 0x7000 | 3 << 9 | 4 << 6 | 4 << 3 | 7,
    // One cycle is 3 channels x 2 conversions x 64 averages at 1.1 ms, 0.42 s, or 0.47 s at
    // the 1.21 ms maximum conversion time (Electrical Characteristics); polled to 0.6 s.
    POLL_MS = 50, POLLS = 12,
    // Mask/Enable bits a write sets: the reserved bit 15, SCC1-3, WEN and CEN.
    MASK_SETTINGS = 0x8000 | INA3221_MASK_SCC | INA3221_MASK_WEN | INA3221_MASK_CEN,
    LIMIT_MAX_STEPS = 4095, LIMIT_MIN_STEPS = -4096,
};

static bool read16(const env_io_t *io, uint8_t reg, uint16_t *value)
{
    uint8_t b[2];
    if (!io->read(io->ctx, INA3221_ADDRESS, reg, b, 2)) return false;
    *value = (uint16_t)(b[0] << 8 | b[1]); // MSB first
    return true;
}
static bool write16(const env_io_t *io, uint8_t reg, uint16_t value)
{
    const uint8_t b[2] = {value >> 8, value & 0xff};
    return io->write(io->ctx, INA3221_ADDRESS, reg, b, 2);
}

int32_t ina3221_shunt_uv(uint16_t reg) { return (int32_t)(int16_t)(reg & 0xfff8) / 8 * 40; }
int32_t ina3221_bus_mv(uint16_t reg) { return (int32_t)(int16_t)(reg & 0xfff8) / 8 * 8; }

bool ina3221_init(const env_io_t *io)
{
    uint16_t manufacturer, die, config;
    return read16(io, REG_MANUFACTURER_ID, &manufacturer) && manufacturer == INA3221_MANUFACTURER_ID &&
           read16(io, REG_DIE_ID, &die) && die == INA3221_DIE_ID &&
           write16(io, REG_CONFIG, CONFIG) && read16(io, REG_CONFIG, &config) && config == CONFIG;
}

bool ina3221_read(const env_io_t *io, ina3221_sample_t *s)
{
    *s = (ina3221_sample_t){0};
    uint16_t config, mask;
    if (!read16(io, REG_CONFIG, &config) || config != CONFIG) return false;
    for (unsigned i = 0; ; i++) {
        if (!read16(io, REG_MASK_ENABLE, &mask)) return false;
        s->alert_flags |= mask & INA3221_MASK_ALERTS; // this read cleared them
        if (mask & INA3221_MASK_CVRF) break;
        if (i == POLLS) return false;
        io->delay_ms(io->ctx, POLL_MS);
    }
    ina3221_sample_t out = {.alert_flags = s->alert_flags};
    for (unsigned c = 0; c < INA3221_CHANNELS; c++) {
        uint16_t shunt, bus;
        if (!read16(io, REG_SHUNT1 + 2 * c, &shunt) || !read16(io, REG_SHUNT1 + 2 * c + 1, &bus)) return false;
        out.shunt_uv[c] = ina3221_shunt_uv(shunt);
        out.bus_mv[c] = ina3221_bus_mv(bus);
    }
    *s = out;
    return true;
}

uint16_t ina3221_limit(int32_t uv)
{
    const int64_t v = uv; // -INT32_MIN would overflow int32_t
    int64_t steps = v >= 0 ? (v + 20) / 40 : -((-v + 20) / 40);
    if (steps > LIMIT_MAX_STEPS) steps = LIMIT_MAX_STEPS;
    if (steps < LIMIT_MIN_STEPS) steps = LIMIT_MIN_STEPS;
    return (uint16_t)(steps * 8); // a negative limit wraps to its two's complement
}

bool ina3221_set_alerts(const env_io_t *io, const int32_t warn_uv[INA3221_CHANNELS],
                        const int32_t crit_uv[INA3221_CHANNELS], uint16_t *cleared)
{
    // Channel c's Critical-Alert Limit is register 0x07 + 2c and its Warning-Alert Limit the
    // next (Table 8-1).
    for (unsigned c = 0; c < INA3221_CHANNELS; c++)
        for (unsigned warning = 0; warning < 2; warning++) {
            const int32_t uv = warning ? warn_uv[c] : crit_uv[c];
            if (uv && !write16(io, REG_CRITICAL1 + 2 * c + warning, ina3221_limit(uv))) return false;
        }
    uint16_t mask;
    if (!read16(io, REG_MASK_ENABLE, &mask)) return false;
    *cleared |= mask & INA3221_MASK_ALERTS;
    const uint16_t settings = (mask & (MASK_SETTINGS & ~(INA3221_MASK_WEN | INA3221_MASK_CEN))) |
                              INA3221_MASK_WEN | INA3221_MASK_CEN;
    if (!write16(io, REG_MASK_ENABLE, settings)) return false;
    for (unsigned c = 0; c < INA3221_CHANNELS; c++)
        for (unsigned warning = 0; warning < 2; warning++) {
            const int32_t uv = warning ? warn_uv[c] : crit_uv[c];
            uint16_t limit;
            if (uv && (!read16(io, REG_CRITICAL1 + 2 * c + warning, &limit) || limit != ina3221_limit(uv)))
                return false;
        }
    if (!read16(io, REG_MASK_ENABLE, &mask)) return false;
    *cleared |= mask & INA3221_MASK_ALERTS;
    return (mask & MASK_SETTINGS) == settings;
}

void ina3221_decode_alerts(uint16_t mask, ina3221_alerts_t *out)
{
    *out = (ina3221_alerts_t){.mask_enable = mask,
        .critical_latch = mask & INA3221_MASK_CEN, .warning_latch = mask & INA3221_MASK_WEN,
        .summation = mask & INA3221_MASK_SF, .power_valid = mask & INA3221_MASK_PVF,
        .timing_control = mask & INA3221_MASK_TCF, .conversion_ready = mask & INA3221_MASK_CVRF};
    // CF1 and WF1, channel 1's flags, are the higher bits of each three-bit field.
    for (unsigned c = 0; c < INA3221_CHANNELS; c++) {
        if (mask & (0x0200 >> c)) out->critical |= (uint8_t)(1u << c);
        if (mask & (0x0020 >> c)) out->warning |= (uint8_t)(1u << c);
    }
}

bool ina3221_read_alerts(const env_io_t *io, ina3221_alerts_t *out)
{
    uint16_t mask;
    *out = (ina3221_alerts_t){0};
    if (!read16(io, REG_MASK_ENABLE, &mask)) return false;
    ina3221_decode_alerts(mask, out);
    return true;
}
