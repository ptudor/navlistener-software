/* Typed board UID wire contract. See docs/BOARD-IDENTITY.md.
 * Shared portable code; no hardware access and no native integer serialization. */
#ifndef NVF_BOARD_UID_H
#define NVF_BOARD_UID_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

#define NVF_BOARD_UID_SIZE 35
#define NVF_BOARD_UID_MAX 32
#define NVF_BOARD_OBSERVER_SIZE 76
enum { NVF_UID_MICROCHIP_EUI64 = 1, NVF_UID_MICROCHIP_CS128 = 3 };

static inline uint16_t nvf_uid_kind(const uint8_t uid[NVF_BOARD_UID_SIZE]) { return (uint16_t)((uint16_t)uid[0] << 8 | uid[1]); }
static inline const char *nvf_uid_kind_name(uint16_t kind) {
    switch (kind) { case NVF_UID_MICROCHIP_EUI64: return "microchip_eui64"; case NVF_UID_MICROCHIP_CS128: return "microchip_cs128"; default: return NULL; }
}
static inline bool nvf_uid_valid(const uint8_t uid[NVF_BOARD_UID_SIZE]) {
    if (!uid) return false;
    uint16_t kind = nvf_uid_kind(uid);
    size_t n = kind == NVF_UID_MICROCHIP_CS128 ? 16 : 8;
    if (!nvf_uid_kind_name(kind) || uid[2] != n) return false;
    bool zero = true, erased = true;
    for (size_t i = 0; i < n; i++) { zero &= uid[3+i] == 0; erased &= uid[3+i] == 0xff; }
    if (zero || erased) return false;
    for (size_t i = 3+n; i < NVF_BOARD_UID_SIZE; i++) if (uid[i]) return false;
    return true;
}
static inline bool nvf_uid_pack(uint16_t kind, const uint8_t *value, size_t n, uint8_t out[NVF_BOARD_UID_SIZE]) {
    if (!out || !value || n > NVF_BOARD_UID_MAX) return false;
    memset(out, 0, NVF_BOARD_UID_SIZE); out[0] = (uint8_t)(kind >> 8); out[1] = (uint8_t)kind; out[2] = (uint8_t)n; memcpy(out+3, value, n);
    return nvf_uid_valid(out);
}
static inline bool nvf_uid_observer(const uint8_t uid[NVF_BOARD_UID_SIZE], char out[NVF_BOARD_OBSERVER_SIZE]) {
    static const char hex[] = "0123456789abcdef";
    if (!out) return false;
    out[0] = 0;
    if (!nvf_uid_valid(uid)) return false;
    memcpy(out, "board-", 6);
    for (unsigned i = 0; i < 2; i++) { out[6+2*i] = hex[uid[i] >> 4]; out[7+2*i] = hex[uid[i] & 15]; }
    out[10] = '-';
    for (unsigned i = 0; i < uid[2]; i++) { out[11+2*i] = hex[uid[3+i] >> 4]; out[12+2*i] = hex[uid[3+i] & 15]; }
    out[11+2*uid[2]] = 0; return true;
}
#endif
