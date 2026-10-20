// Test blocks built from the mosaic-G5 reference guide's layouts (chapter 4): constructed
// field by field with the CRC computed, not captured from a receiver.
#ifndef SBF_BUILD_H
#define SBF_BUILD_H
#include "sbf.h"
#include <string.h>

static inline void put16(uint8_t *p, uint16_t v) { p[0] = (uint8_t)v; p[1] = (uint8_t)(v >> 8); }
static inline void put32(uint8_t *p, uint32_t v) { put16(p, (uint16_t)v); put16(p + 2, (uint16_t)(v >> 16)); }
static inline void putf8(uint8_t *p, double d)
{
    uint64_t v;
    memcpy(&v, &d, sizeof v);
    for (int i = 0; i < 8; i++) p[i] = (uint8_t)(v >> (8 * i));
}
// A block of `length` bytes (a multiple of 4) whose body, from offset 8, is already in out.
static inline size_t seal(uint8_t *out, uint16_t number, uint8_t revision, size_t length)
{
    out[0] = '$'; out[1] = '@';
    put16(out + 4, (uint16_t)(number | revision << 13));
    put16(out + 6, (uint16_t)length);
    put16(out + 2, sbf_crc16(out + 4, length - 4, 0));
    return length;
}
static inline size_t padded(size_t n) { return (n + 3) & ~(size_t)3; }

#define PI_TEST 3.14159265358979323846
// PVTGeodetic revision 2 (96 bytes) or revision 0 (88): TOW, WNc, Mode, Error, position.
static inline size_t pvt_block(uint8_t *out, uint8_t revision, uint32_t tow, uint8_t mode, uint8_t error,
                               double lat_deg, double lon_deg, double height)
{
    const size_t length = revision >= 1 ? 96 : 88;
    memset(out, 0, length);
    put32(out + 8, tow); put16(out + 12, 2392);
    out[14] = mode; out[15] = error;
    putf8(out + 16, lat_deg * PI_TEST / 180); putf8(out + 24, lon_deg * PI_TEST / 180); putf8(out + 32, height);
    out[74] = 14; // NrSV
    if (revision >= 1) { put16(out + 90, 123); put16(out + 92, 456); }
    return seal(out, SBF_PVT_GEODETIC, revision, length);
}
// ReceiverTime: UTC two-digit year, month, day, hour, minute, second, DeltaLS, SyncLevel.
static inline size_t time_block(uint8_t *out, uint32_t tow, int8_t year, int8_t month, int8_t day, int8_t hour,
                                int8_t minute, int8_t second, int8_t delta_ls, uint8_t sync)
{
    memset(out, 0, 24);
    put32(out + 8, tow); put16(out + 12, 2392);
    out[14] = (uint8_t)year; out[15] = (uint8_t)month; out[16] = (uint8_t)day; out[17] = (uint8_t)hour;
    out[18] = (uint8_t)minute; out[19] = (uint8_t)second; out[20] = (uint8_t)delta_ls; out[21] = sync;
    return seal(out, SBF_RECEIVER_TIME, 0, 24);
}
// MeasEpoch built from master/slave signal lists. A Type field of 31 puts the signal number,
// less 32, in ObsInfo bits 3-7 (4.2.1).
typedef struct { uint8_t svid, signal, cn0; bool code; uint8_t slaves; } test_master_t;
typedef struct { uint8_t signal, cn0; bool code; } test_slave_t;
static inline void type_field(uint8_t signal, uint8_t *type, uint8_t *obs)
{
    if (signal >= 32) { *type = 31; *obs = (uint8_t)((signal - 32) << 3); }
    else { *type = signal; *obs = 0; }
}
static inline size_t meas_block(uint8_t *out, uint32_t tow, uint8_t flags, const test_master_t *m, unsigned n,
                                const test_slave_t *s)
{
    size_t off = 20;
    unsigned slave = 0;
    memset(out, 0, SBF_MAX_BLOCK);
    put32(out + 8, tow); put16(out + 12, 2392);
    out[14] = (uint8_t)n; out[15] = 20; out[16] = 12; out[17] = flags;
    for (unsigned i = 0; i < n; i++) {
        uint8_t *t = out + off;
        t[0] = (uint8_t)(i + 1);
        type_field(m[i].signal, &t[1], &t[18]);
        t[2] = m[i].svid;
        if (m[i].code) put32(t + 4, 21000000u + i); // CodeLSB: about 21 000 km
        t[15] = m[i].cn0;
        t[19] = m[i].slaves;
        off += 20;
        for (unsigned j = 0; j < m[i].slaves; j++, slave++) {
            uint8_t *u = out + off;
            uint8_t obs;
            type_field(s[slave].signal, &u[0], &obs);
            u[5] = obs;
            u[2] = s[slave].cn0;
            if (s[slave].code) put16(u + 6, 1234); else u[3] = 0x04; // CodeOffsetMSB -4, LSB 0: absent
            off += 12;
        }
    }
    return seal(out, SBF_MEAS_EPOCH, 1, padded(off));
}
// SatVisibility: SVID and elevation in 0.01 degree.
static inline size_t visibility_block(uint8_t *out, const uint8_t *svid, const int16_t *elevation, unsigned n)
{
    memset(out, 0, 16 + 8 * n + 4);
    put32(out + 8, 1000); put16(out + 12, 2392);
    out[14] = (uint8_t)n; out[15] = 8;
    for (unsigned i = 0; i < n; i++) {
        uint8_t *s = out + 16 + 8 * i;
        s[0] = svid[i]; put16(s + 2, 9000); put16(s + 4, (uint16_t)elevation[i]); s[6] = 1; s[7] = 2;
    }
    return seal(out, SBF_SAT_VISIBILITY, 0, padded(16 + 8 * n));
}
// RFStatus: Flags and bands given by their Info modes.
static inline size_t rf_block(uint8_t *out, uint8_t flags, const uint8_t *modes, unsigned n)
{
    memset(out, 0, 20 + 8 * n + 4);
    put32(out + 8, 1000); put16(out + 12, 2392);
    out[14] = (uint8_t)n; out[15] = 8; out[16] = flags;
    for (unsigned i = 0; i < n; i++) {
        uint8_t *b = out + 20 + 8 * i;
        put32(b, 1575420000u); put16(b + 4, 2000); b[6] = modes[i]; b[7] = (uint8_t)-90;
    }
    return seal(out, SBF_RF_STATUS, 0, padded(20 + 8 * n));
}
// ReceiverStatus with no AGC sub-blocks.
static inline size_t status_block(uint8_t *out, uint32_t uptime, uint32_t rx_state, uint32_t rx_error)
{
    memset(out, 0, 32);
    put32(out + 8, 1000); put16(out + 12, 2392);
    out[14] = 12; put32(out + 16, uptime); put32(out + 20, rx_state); put32(out + 24, rx_error);
    out[29] = 4; out[31] = 140;
    return seal(out, SBF_RECEIVER_STATUS, 1, 32);
}
// ReceiverSetup, revision 3 with ProductName (400 bytes) or revision 0 (272).
static inline size_t setup_block(uint8_t *out, uint8_t revision, const char *serial, const char *name,
                                 const char *version, const char *product)
{
    const size_t length = revision >= 3 ? 400 : 272;
    memset(out, 0, length);
    put32(out + 8, 1000); put16(out + 12, 2392);
    memcpy(out + 156, serial, strlen(serial));
    memcpy(out + 176, name, strlen(name));
    memcpy(out + 196, version, strlen(version));
    if (revision >= 3) memcpy(out + 328, product, strlen(product));
    return seal(out, SBF_RECEIVER_SETUP, revision, length);
}
#endif
