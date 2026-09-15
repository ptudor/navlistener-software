#include "hardware_checks.h"
#include <string.h>
uint16_t hardware_crypto_crc(const uint8_t *data, size_t n)
{
    uint16_t crc = 0;
    for (size_t i = 0; i < n; i++) {
        for (unsigned mask = 1; mask < 256; mask <<= 1) {
            bool bit = !!(data[i] & mask) ^ !!(crc & 0x8000);
            crc <<= 1;
            if (bit) crc ^= 0x8005;
        }
    }
    return crc;
}
bool hardware_crypto_response(const uint8_t *data, size_t n)
{
    if (!data || n < 3 || data[0] != n) return false;
    uint16_t crc = hardware_crypto_crc(data, n - 2);
    return data[n - 2] == (uint8_t)crc && data[n - 1] == (uint8_t)(crc >> 8);
}
bool hardware_rng_sample_ok(const uint8_t sample[32], const uint8_t *previous)
{
    if (!sample || (previous && !memcmp(sample, previous, 32))) return false;
    for (size_t period = 1; period <= 8; period++) {
        bool repeats = true;
        for (size_t i = period; i < 32; i++) if (sample[i] != sample[i % period]) repeats = false;
        if (repeats) return false;
    }
    return true;
}
const char *hardware_crypto_lock_state(uint8_t value)
{
    if (value == 0) return "locked";
    if (value == 0x55) return "unlocked";
    return "unknown";
}
