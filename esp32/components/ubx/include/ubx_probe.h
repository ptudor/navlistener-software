// Receiver discovery and volatile configuration packets, UBX-21022436.
#ifndef UBX_PROBE_H
#define UBX_PROBE_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
size_t ubx_poll_version(uint8_t out[8]);
size_t ubx_set_ram(uint8_t out[20], uint32_t key, uint32_t value, unsigned width);
bool ubx_version_is_m9n(const uint8_t *body, size_t len);
typedef struct { unsigned state, length; uint8_t checksum, expected; } nmea_probe_t;
bool nmea_probe_feed(nmea_probe_t *p, uint8_t byte);
#endif
