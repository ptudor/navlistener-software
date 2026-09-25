#include "ms5607.h"

enum {
    CMD_RESET = 0x1e, CMD_D1_4096 = 0x48, CMD_D2_4096 = 0x58, CMD_ADC_READ = 0x00, CMD_PROM = 0xa0,
    RESET_MS = 3,       // 2.8 ms PROM reload
    CONVERSION_MS = 10, // 9.04 ms maximum at OSR 4096
};

uint8_t ms5607_crc4(const uint16_t prom[8])
{
    // AN520 computes in a 16-bit unsigned int; only bits 15:12 survive to the result.
    uint16_t rem = 0;
    for (unsigned i = 0; i < 16; i++) {
        uint16_t word = i / 2 == 7 ? prom[7] & 0xff00 : prom[i / 2];
        rem ^= i % 2 ? word & 0xff : word >> 8;
        for (unsigned bit = 0; bit < 8; bit++)
            rem = rem & 0x8000 ? (uint16_t)((rem << 1) ^ 0x3000) : (uint16_t)(rem << 1);
    }
    return (rem >> 12) & 0xf;
}

bool ms5607_compensate(const uint16_t prom[8], uint32_t d1, uint32_t d2, int32_t *centi_c, int32_t *pa)
{
    if (!d1 || !d2 || d1 >= 1u << 24 || d2 >= 1u << 24) return false;
    // The datasheet's integer arithmetic, divisions truncating toward zero as in AN520.
    int64_t dt = (int64_t)d2 - (int64_t)prom[5] * 256;
    int64_t temp = 2000 + dt * prom[6] / 8388608;
    int64_t off = (int64_t)prom[2] * 131072 + (int64_t)prom[4] * dt / 64;
    int64_t sens = (int64_t)prom[1] * 65536 + (int64_t)prom[3] * dt / 128;
    if (temp < 2000) {
        int64_t low = (temp - 2000) * (temp - 2000);
        int64_t t2 = dt * dt / 2147483648LL, off2 = 61 * low / 16, sens2 = 2 * low;
        if (temp < -1500) {
            int64_t very_low = (temp + 1500) * (temp + 1500);
            off2 += 15 * very_low;
            sens2 += 8 * very_low;
        }
        temp -= t2; off -= off2; sens -= sens2;
    }
    int64_t pressure = ((int64_t)d1 * sens / 2097152 - off) / 32768;
    if (temp < -4000 || temp > 8500 || pressure < MS5607_MIN_PA || pressure > MS5607_MAX_PA) return false;
    *centi_c = (int32_t)temp; *pa = (int32_t)pressure;
    return true;
}

ms5607_state_t ms5607_init(ms5607_t *m, const env_io_t *io)
{
    m->state = MS5607_ABSENT;
    if (!io->write(io->ctx, MS5607_ADDRESS, CMD_RESET, NULL, 0)) return m->state;
    io->delay_ms(io->ctx, RESET_MS);
    for (unsigned i = 0; i < 8; i++) {
        uint8_t word[2];
        if (!io->read(io->ctx, MS5607_ADDRESS, CMD_PROM + 2 * i, word, 2)) return m->state;
        m->prom[i] = (uint16_t)(word[0] << 8 | word[1]);
    }
    // A bus that returns constant bytes can satisfy a 4-bit CRC; no real part has a
    // coefficient of 0 or 65535.
    bool plausible = true;
    for (unsigned i = 1; i <= 6; i++) plausible &= m->prom[i] != 0 && m->prom[i] != 0xffff;
    m->state = plausible && ms5607_crc4(m->prom) == (m->prom[7] & 0xf) ? MS5607_READY : MS5607_PROM_REJECTED;
    return m->state;
}

static bool convert(const env_io_t *io, uint8_t command, uint32_t *value)
{
    uint8_t adc[3];
    if (!io->write(io->ctx, MS5607_ADDRESS, command, NULL, 0)) return false;
    io->delay_ms(io->ctx, CONVERSION_MS);
    if (!io->read(io->ctx, MS5607_ADDRESS, CMD_ADC_READ, adc, 3)) return false;
    *value = (uint32_t)adc[0] << 16 | (uint32_t)adc[1] << 8 | adc[2];
    return true;
}

bool ms5607_read(const ms5607_t *m, const env_io_t *io, int32_t *centi_c, int32_t *pa)
{
    uint32_t d1, d2;
    return m->state == MS5607_READY && convert(io, CMD_D1_4096, &d1) && convert(io, CMD_D2_4096, &d2) &&
           ms5607_compensate(m->prom, d1, d2, centi_c, pa);
}
