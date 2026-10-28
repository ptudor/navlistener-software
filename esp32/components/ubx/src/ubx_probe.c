#include "ubx_probe.h"
#include <string.h>
static size_t finish(uint8_t *out, size_t payload_len)
{
    out[0] = 0xb5; out[1] = 0x62;
    out[4] = payload_len; out[5] = payload_len >> 8;
    uint8_t a = 0, b = 0;
    for (size_t i = 2; i < payload_len + 6; i++) { a += out[i]; b += a; }
    out[payload_len + 6] = a; out[payload_len + 7] = b;
    return payload_len + 8;
}
size_t ubx_poll_version(uint8_t out[8])
{
    out[2] = 0x0a; out[3] = 0x04;
    return finish(out, 0);
}
size_t ubx_poll_unique_id(uint8_t out[8])
{
    out[2] = 0x27; out[3] = 0x03;
    return finish(out, 0);
}
bool ubx_unique_id(const uint8_t *body, size_t len, char out[13])
{
    // Version 1 carries 5 bytes (M9, F9), version 2 carries 6 (M10, X20).
    if (!body || !((len == 9 && body[0] == 1) || (len == 10 && body[0] == 2))) return false;
    static const char digits[] = "0123456789abcdef";
    size_t n = 0;
    for (size_t i = 4; i < len; i++) { out[n++] = digits[body[i] >> 4]; out[n++] = digits[body[i] & 15]; }
    out[n] = 0;
    return true;
}
size_t ubx_set_ram(uint8_t out[20], uint32_t key, uint32_t value, unsigned width)
{
    if (width != 1 && width != 4) return 0;
    out[2] = 0x06; out[3] = 0x8a; // CFG-VALSET, version 0, RAM layer only
    out[6] = 0; out[7] = 1; out[8] = out[9] = 0;
    for (unsigned i = 0; i < 4; i++) out[10 + i] = key >> (8 * i);
    for (unsigned i = 0; i < width; i++) out[14 + i] = value >> (8 * i);
    return finish(out, 8 + width);
}
// UBX output on UART1, then the SFRBX, MON-RF, NAV-SAT, NAV-PVT, NAV-STATUS, TIM-TP,
// NAV-SIG, NAV-CLOCK and NAV-EOE UART1 rates. NAV-PVT, NAV-CLOCK and NAV-STATUS form the
// receiver-solution record; NAV-EOE completes each epoch promptly. The ZED-X20P adds four:
// - CFG-SIGNAL-QZSS_ENA and CFG-SIGNAL-NAVIC_ENA, right after UBX output and before any
//   status message is on: both are off by default in every X20 signal plan (integration
//   manual UBXDOC-963802114-12901, 2.2.4.1), with all their bands on beneath them, and each
//   resets the GNSS subsystem, which a running NAV-STATUS or SEC-SIG would report as a
//   spoofing or jamming transition. The M9 and M10 enable QZSS by default and have no NavIC.
// - CFG-HW-ANT_CFG_SHORTDET: the ZED/X20 board drives its pin 6 ANT_SHORT_N (default active
//   low, internal pull-up) from the antenna switch's fault output, so MON-RF reports SHORT.
//   The NEO and MAX boards wire no short signal to their receivers, which keep detection off.
// - The SEC-SIG rate: from HPG 2.00 on, MON-RF's jamming flag reads 0 and SEC-SIG carries it.
static const uint32_t ram_keys[] = {0x10740001, 0x20910232, 0x2091035a, 0x20910016, 0x20910007,
    0x2091001b, 0x2091017e, 0x20910346, 0x20910066, 0x20910160};
static const uint32_t x20_keys[] = {0x10740001, 0x10310024, 0x10310026, 0x20910232, 0x2091035a,
    0x20910016, 0x20910007, 0x2091001b, 0x2091017e, 0x20910346, 0x20910066, 0x20910160, 0x10a3002f,
    0x20910635};
size_t ubx_ram_keys(const char *module, const uint32_t **keys)
{
    if (module && !strcmp(module, "ZED-X20P")) { *keys = x20_keys; return sizeof x20_keys / sizeof x20_keys[0]; }
    *keys = ram_keys;
    return sizeof ram_keys / sizeof ram_keys[0];
}
// The CFG-SIGNAL group, 0x31: "some changes to items within this group trigger a reset to the
// GNSS subsystem" (X20 HPG 2.11 interface description, 6.9.22).
bool ubx_key_resets_gnss(uint32_t key) { return (key >> 16 & 0xff) == 0x31; }
const char *ubx_key_name(uint32_t key)
{
    switch (key) {
    case 0x10740001: return "CFG-UART1OUTPROT-UBX";
    case 0x20910232: return "CFG-MSGOUT-UBX_RXM_SFRBX_UART1";
    case 0x2091035a: return "CFG-MSGOUT-UBX_MON_RF_UART1";
    case 0x20910016: return "CFG-MSGOUT-UBX_NAV_SAT_UART1";
    case 0x20910007: return "CFG-MSGOUT-UBX_NAV_PVT_UART1";
    case 0x2091001b: return "CFG-MSGOUT-UBX_NAV_STATUS_UART1";
    case 0x2091017e: return "CFG-MSGOUT-UBX_TIM_TP_UART1";
    case 0x20910346: return "CFG-MSGOUT-UBX_NAV_SIG_UART1";
    case 0x20910066: return "CFG-MSGOUT-UBX_NAV_CLOCK_UART1";
    case 0x20910160: return "CFG-MSGOUT-UBX_NAV_EOE_UART1";
    case 0x10a3002f: return "CFG-HW-ANT_CFG_SHORTDET";
    case 0x20910635: return "CFG-MSGOUT-UBX_SEC_SIG_UART1";
    case 0x10310024: return "CFG-SIGNAL-QZSS_ENA";
    case 0x10310026: return "CFG-SIGNAL-NAVIC_ENA";
    case UBX_KEY_UART1_BAUDRATE: return "CFG-UART1-BAUDRATE";
    }
    return NULL;
}
bool ubx_version_is_module(const uint8_t *body, size_t len, const char *module)
{
    if (!body || !module || len < 40 || (len - 40) % 30) return false;
    // "MOD=" plus the name and its terminator must fit one 30-byte extension.
    size_t name = strlen(module);
    if (!name || name > 25) return false;
    for (size_t i = 40; i + 30 <= len; i += 30)
        if (memcmp(body + i, "MOD=", 4) == 0 && memcmp(body + i + 4, module, name + 1) == 0) return true;
    return false;
}
static int hex(uint8_t c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    return -1;
}
bool nmea_probe_feed(nmea_probe_t *p, uint8_t b)
{
    if (b == '$') { *p = (nmea_probe_t){.state = 1}; return false; }
    switch (p->state) {
    case 1:
        if (b == '*' && p->length >= 5) p->state = 2;
        else if (b < 32 || b > 126 || ++p->length > 120) p->state = 0;
        else p->checksum ^= b;
        break;
    case 2:
        if (hex(b) < 0) p->state = 0;
        else { p->expected = hex(b) << 4; p->state = 3; }
        break;
    case 3:
        if (hex(b) < 0) p->state = 0;
        else { p->expected |= hex(b); p->state = 4; }
        break;
    case 4:
        if (b == '\r') p->state = 5;
        else { p->state = 0; return b == '\n' && p->checksum == p->expected; }
        break;
    case 5:
        p->state = 0; return b == '\n' && p->checksum == p->expected;
    }
    return false;
}
