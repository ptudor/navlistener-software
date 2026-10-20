#include "hih8121.h"

bool hih8121_read(const env_io_t *io, uint8_t address, int16_t *centi_c, uint16_t *centi_rh,
                  hih8121_result_t *result)
{
    *result = (hih8121_result_t){0};
    uint8_t p[4];
    if (!io->request || !io->receive || !io->request(io->ctx, address)) return false;
    io->delay_ms(io->ctx, HIH8121_MEASURE_MS);
    if (!io->receive(io->ctx, address, p, sizeof p)) return false;
    // Byte 1: status in bits 7:6, humidity bits 13:8; byte 2 humidity bits 7:0; byte 3
    // temperature bits 13:6; byte 4 temperature bits 5:0 in bits 7:2 (technical note Figure 4).
    *result = (hih8121_result_t){.answered = true, .status = (hih8121_status_t)(p[0] >> 6)};
    if (result->status != HIH8121_FRESH) return false;
    const uint32_t humidity = (uint32_t)(p[0] & 0x3f) << 8 | p[1];
    const uint32_t temperature = (uint32_t)p[2] << 6 | p[3] >> 2;
    // RH = counts / (2^14 - 2) * 100 %; T = counts / (2^14 - 2) * 165 - 40 C. The one count
    // past full scale rounds down to 100 %RH; past 125 C a reading is rejected.
    const int32_t t = (int32_t)(temperature * 16500 / HIH8121_FULL_SCALE) - 4000;
    if (t < HIH8121_MIN_CENTI_C || t > HIH8121_MAX_CENTI_C) return false;
    *centi_c = (int16_t)t;
    *centi_rh = (uint16_t)(humidity * 10000 / HIH8121_FULL_SCALE);
    return true;
}
