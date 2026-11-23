#ifndef NAVLISTEN_RECEIVER_RF_H
#define NAVLISTEN_RECEIVER_RF_H

// JammingStats body version 2 carries an independent receiver observation. Version 1
// continues to carry MON-RF/MON-HW. See docs/OBSERVER-TELEMETRY.md and X20 HPG 2.02
// Interface Description sections 3.14.8, 3.17.2 and 3.17.3.
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

#define RF_SPAN_BLOCK 272u
#define RF_JAMMED (1u << 24)
#define RF_CENTER_KHZ 0xffffffu
#define RF_JAM_SENSITIVITY_KEY 0x10f60051u
#define RF_SENSITIVITY 0x51u

typedef struct {
    uint8_t body[1024], repeats;
    uint16_t length;
} rf_security_cache_t;

// Send the first assessment, each change, and every thirtieth repeat at the
// configured navigation rate. An unchanged receiver-wide flag can hide a change
// of affected frequencies.
static inline bool rf_security_due(rf_security_cache_t *cache, const uint8_t *body, size_t n)
{
    if (n > sizeof cache->body) return false;
    if (n == cache->length && !memcmp(cache->body, body, n) && ++cache->repeats < 30) return false;
    memcpy(cache->body, body, n);
    cache->length = (uint16_t)n;
    cache->repeats = 0;
    return true;
}

static inline uint32_t rf_le32(const uint8_t *p)
{
    return p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 | (uint32_t)p[3] << 24;
}

static inline bool rf_sec_sig_valid(const uint8_t *p, size_t n)
{
    return n >= 4 && (p[0] == 2 || p[0] == 3) && n == 4u + 4u * p[3];
}

// Return one record body, or zero for an unsupported message, bad length or absent
// path. A spectrum gets one record per path so every bin fits even on a multi-path unit.
static inline size_t rf_detail_encode(uint8_t *out, size_t cap, uint8_t cls, uint8_t id,
                                      const uint8_t *p, size_t n, unsigned path)
{
    if (cls == 0x27 && id == 0x09 && path == 0 && rf_sec_sig_valid(p, n) && cap >= n) {
        out[0] = 2;
        out[1] = p[0]; // SEC-SIG version 2 or 3
        out[2] = p[1]; // detector enable bits and states
        memcpy(out + 3, p + 3, n - 3); // count and little-endian frequency words
        return n;
    }
    if (cls == 0x27 && id == 0x10 && path == 0 && n >= 8 && p[0] == 1 &&
        p[1] <= 16 && n == 8u + 8u * p[1] && cap >= 2 + n) {
        out[0] = 2;
        out[1] = id;
        memcpy(out + 2, p, n);
        return 2 + n;
    }
    if (cls == 0x0a && id == 0x31 && n >= 4 && p[0] == 0 && path < p[1] &&
        n == 4u + RF_SPAN_BLOCK * p[1] && cap >= 4 + RF_SPAN_BLOCK) {
        out[0] = 2;
        out[1] = id;
        out[2] = (uint8_t)path;
        out[3] = p[1];
        memcpy(out + 4, p + 4 + RF_SPAN_BLOCK * path, RF_SPAN_BLOCK);
        return 4 + RF_SPAN_BLOCK;
    }
    // Only the single requested RAM key, from a version 1 CFG-VALGET response.
    if (cls == 0x06 && id == 0x8b && path == 0 && n == 9 && p[0] == 1 &&
        p[1] == 0 && rf_le32(p + 4) == RF_JAM_SENSITIVITY_KEY && p[8] <= 1 && cap >= 3) {
        out[0] = 2;
        out[1] = RF_SENSITIVITY;
        out[2] = p[8];
        return 3;
    }
    return 0;
}
#endif
