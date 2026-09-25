// Receiver discovery and volatile configuration packets. MON-VER, CFG-VALSET and every
// key the firmware sets are identical in the M9 SPG 4.04 (UBX-21022436), M10 SPG 5.10
// (UBX-21035062) and X20 HPG 2.11 (UBXDOC-304424225-21617) interface descriptions.
#ifndef UBX_PROBE_H
#define UBX_PROBE_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
size_t ubx_poll_version(uint8_t out[8]);
size_t ubx_set_ram(uint8_t out[20], uint32_t key, uint32_t value, unsigned width);
// A MON-VER body whose extensions include exactly "MOD=<module>", such as "NEO-M9N".
bool ubx_version_is_module(const uint8_t *body, size_t len, const char *module);
typedef struct { unsigned state, length; uint8_t checksum, expected; } nmea_probe_t;
bool nmea_probe_feed(nmea_probe_t *p, uint8_t byte);
#endif
