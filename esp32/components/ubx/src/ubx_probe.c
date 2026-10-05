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
size_t ubx_set_ram(uint8_t out[20], uint32_t key, uint32_t value, unsigned width)
{
    if (width != 1 && width != 4) return 0;
    out[2] = 0x06; out[3] = 0x8a; // CFG-VALSET, version 0, RAM layer only
    out[6] = 0; out[7] = 1; out[8] = out[9] = 0;
    for (unsigned i = 0; i < 4; i++) out[10 + i] = key >> (8 * i);
    for (unsigned i = 0; i < width; i++) out[14 + i] = value >> (8 * i);
    return finish(out, 8 + width);
}
// UBX output on UART1, then the SFRBX, MON-RF, NAV-SAT, NAV-PVT, NAV-STATUS, TIM-TP and
// NAV-SIG UART1 rates. CFG-HW-ANT_CFG_SHORTDET comes last and only the ZED-X20P gets it:
// the ZED/X20 board drives its pin 6 ANT_SHORT_N (default active low, internal pull-up)
// from the antenna switch's fault output, so MON-RF reports SHORT. The NEO and MAX boards
// wire no short signal to their receivers, which keep detection off.
static const uint32_t ram_keys[] = {0x10740001, 0x20910232, 0x2091035a, 0x20910016, 0x20910007,
    0x2091001b, 0x2091017e, 0x20910346, 0x10a3002f};
size_t ubx_ram_keys(const char *module, const uint32_t **keys)
{
    size_t n = sizeof ram_keys / sizeof ram_keys[0];
    *keys = ram_keys;
    return module && !strcmp(module, "ZED-X20P") ? n : n - 1;
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
