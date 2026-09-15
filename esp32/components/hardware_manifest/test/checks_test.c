#include "hardware_checks.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>
int main(void)
{
    uint8_t wake[] = {4,0x11,0x33,0x43};
    assert(hardware_crypto_response(wake, sizeof wake));
    wake[3] ^= 1; assert(!hardware_crypto_response(wake, sizeof wake));
    uint8_t sample[32] = {0}, previous[32];
    assert(!hardware_rng_sample_ok(sample, NULL));
    memset(sample, 255, sizeof sample); assert(!hardware_rng_sample_ok(sample, NULL));
    for (size_t i=0;i<32;i++) sample[i] = i % 2 ? 0 : 255;
    assert(!hardware_rng_sample_ok(sample, NULL)); // ff00ff00
    for (size_t i=0;i<32;i++) sample[i] = i % 8;
    assert(!hardware_rng_sample_ok(sample, NULL));
    for (size_t i=0;i<32;i++) sample[i] = i*i + 17*i + 39;
    assert(hardware_rng_sample_ok(sample, NULL));
    memcpy(previous, sample, 32); assert(!hardware_rng_sample_ok(sample, previous));
    sample[0] ^= 1; assert(hardware_rng_sample_ok(sample, previous));
    assert(!strcmp(hardware_crypto_lock_state(0x55), "unlocked"));
    assert(!strcmp(hardware_crypto_lock_state(0), "locked"));
    assert(!strcmp(hardware_crypto_lock_state(0xff), "unknown"));
    puts("Hardware checks: response CRC, repeated RNG output and independent lock status passed");
}
