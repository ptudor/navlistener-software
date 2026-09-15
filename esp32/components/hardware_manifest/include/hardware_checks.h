#ifndef HARDWARE_CHECKS_H
#define HARDWARE_CHECKS_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
uint16_t hardware_crypto_crc(const uint8_t *data, size_t length);
bool hardware_crypto_response(const uint8_t *data, size_t length);
// Diagnostic screening only; a pass does not certify entropy or provisioning.
bool hardware_rng_sample_ok(const uint8_t sample[32], const uint8_t *previous);
const char *hardware_crypto_lock_state(uint8_t value);
#endif
