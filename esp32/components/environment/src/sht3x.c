#include "sht3x.h"

// 16-bit commands, sent MSB first: as a register byte followed by one data byte.
enum {
    CMD_MEASURE = 0x2400,    // single shot, high repeatability, clock stretching disabled (Table 8)
    CMD_BREAK = 0x3093,      // stop periodic data acquisition (Table 12)
    CMD_SOFT_RESET = 0x30A2, // Table 13
    CMD_STATUS = 0xF32D,     // read the status register (Table 16)
};
enum { STATUS_HEATER = 1u << 13 }; // Table 17
// tMEAS,h is at most 15 ms and a soft reset at most 1 ms (Table 4); a break finishes the
// ongoing measurement, up to 15 ms (section 4.8).
enum { MEASURE_MS = 16, RESET_MS = 2, BREAK_MS = 15 };

uint8_t sht3x_crc(const uint8_t word[2])
{
    uint8_t crc = 0xff;
    for (unsigned i = 0; i < 2; i++) {
        crc ^= word[i];
        for (unsigned bit = 0; bit < 8; bit++) crc = crc & 0x80 ? (uint8_t)((crc << 1) ^ 0x31) : (uint8_t)(crc << 1);
    }
    return crc;
}
static bool command(const env_io_t *io, uint8_t address, uint16_t code)
{
    const uint8_t lsb = (uint8_t)code;
    return io->write(io->ctx, address, (uint8_t)(code >> 8), &lsb, 1);
}
// Reads count words, each followed by its CRC.
static bool words(const env_io_t *io, uint8_t address, uint16_t *out, unsigned count)
{
    uint8_t p[6];
    if (!io->receive || count > 2 || !io->receive(io->ctx, address, p, 3 * count)) return false;
    for (unsigned i = 0; i < count; i++) {
        if (sht3x_crc(p + 3 * i) != p[3 * i + 2]) return false;
        out[i] = (uint16_t)(p[3 * i] << 8 | p[3 * i + 1]);
    }
    return true;
}

bool sht3x_start(const env_io_t *io, uint8_t address)
{
    // In single-shot mode the part may decline a break; there is then nothing to wait for.
    if (command(io, address, CMD_BREAK)) io->delay_ms(io->ctx, BREAK_MS);
    if (!command(io, address, CMD_SOFT_RESET)) return false;
    io->delay_ms(io->ctx, RESET_MS);
    uint16_t status;
    return command(io, address, CMD_STATUS) && words(io, address, &status, 1) && !(status & STATUS_HEATER);
}

bool sht3x_read(const env_io_t *io, uint8_t address, int16_t *centi_c, uint16_t *centi_rh)
{
    uint16_t w[2];
    if (!command(io, address, CMD_MEASURE)) return false;
    io->delay_ms(io->ctx, MEASURE_MS);
    if (!words(io, address, w, 2)) return false;
    // T = -45 + 175 * S_T / (2^16 - 1) C and RH = 100 * S_RH / (2^16 - 1) %RH (section 4.13).
    const int32_t t = -4500 + (int32_t)(17500L * w[0] / 65535);
    if (t < SHT3X_MIN_CENTI_C || t > SHT3X_MAX_CENTI_C) return false;
    *centi_c = (int16_t)t;
    *centi_rh = (uint16_t)(10000UL * w[1] / 65535);
    return true;
}
