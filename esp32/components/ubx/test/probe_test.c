#include "ubx_probe.h"
#include "ubx.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>
static unsigned observed;
static void observe(uint8_t cls, uint8_t id, const uint8_t *body, size_t len, void *ctx)
{ (void)body; (void)ctx; assert(cls == 0x0a && id == 4 && len == 0); observed++; }
int main(void)
{
    uint8_t pkt[20]; ubx_parser_t parser;
    const uint8_t poll[] = {0xb5,0x62,0x0a,4,0,0,0x0e,0x34};
    assert(ubx_poll_version(pkt) == sizeof poll && !memcmp(pkt, poll, sizeof poll));
    ubx_parser_init(&parser, NULL, NULL, NULL); parser.observe = observe;
    for (size_t i = 0; i < sizeof poll; i++) ubx_parser_feed(&parser, pkt+i, 1);
    assert(observed == 1 && parser.frames_valid == 1 && parser.bytes == 8);
    pkt[7] ^= 1; ubx_parser_feed(&parser, pkt, 8); assert(observed == 1 && parser.bad_checksum == 1);
    assert(ubx_set_ram(pkt, 0x20910232, 1, 1) == 17);
    assert(pkt[6] == 0 && pkt[7] == 1 && pkt[8] == 0 && pkt[9] == 0);
    assert(pkt[10] == 0x32 && pkt[11] == 2 && pkt[12] == 0x91 && pkt[13] == 0x20 && pkt[14] == 1);
    assert(ubx_set_ram(pkt, 0x40520001, 460800, 4) == 20);
    assert(pkt[14] == 0 && pkt[15] == 8 && pkt[16] == 7 && pkt[17] == 0);
    // The ZED-X20P's list is the shared one with QZSS and NavIC enabled straight after UBX
    // output, before any status message, and short detection and the SEC-SIG rate last.
    const uint32_t *keys, *x20_keys;
    size_t count = ubx_ram_keys("NEO-M9N", &keys), x20_count = ubx_ram_keys("ZED-X20P", &x20_keys);
    assert(count == 10 && keys[0] == 0x10740001 && keys[1] == 0x20910232 && keys[7] == 0x20910346);
    assert(keys[8] == 0x20910066 && keys[9] == 0x20910160); // NAV-CLOCK, NAV-EOE on UART1
    assert(x20_count == count + 4 && x20_keys[0] == keys[0] && x20_keys[1] == 0x10310024 && x20_keys[2] == 0x10310026);
    assert(!memcmp(keys + 1, x20_keys + 3, (count - 1) * sizeof *keys));
    assert(x20_keys[count + 2] == 0x10a3002f && x20_keys[count + 3] == 0x20910635);
    // Only the CFG-SIGNAL keys reset the GNSS subsystem, and they precede every message rate.
    for (size_t i = 0; i < x20_count; i++) assert(ubx_key_resets_gnss(x20_keys[i]) == (i == 1 || i == 2));
    for (size_t i = 0; i < count; i++) assert(!ubx_key_resets_gnss(keys[i]));
    assert(!ubx_key_resets_gnss(UBX_KEY_UART1_BAUDRATE) && ubx_key_resets_gnss(0x1031001f));
    assert(ubx_ram_keys("MAX-M10S", &keys) == count && ubx_ram_keys(NULL, &keys) == count);
    assert(ubx_ram_keys("ZED-X20", &keys) == count && ubx_ram_keys("ZED-X20P ", &keys) == count);
    for (size_t i = 0; i < count; i++) assert(keys[i] != 0x10a3002f && keys[i] != 0x10310026);
    // Every key the firmware sets has its interface-description name, each a different one.
    for (size_t i = 0; i < x20_count; i++) {
        assert(ubx_key_name(x20_keys[i]) && !strncmp(ubx_key_name(x20_keys[i]), "CFG-", 4));
        for (size_t j = 0; j < i; j++) assert(strcmp(ubx_key_name(x20_keys[i]), ubx_key_name(x20_keys[j])));
    }
    assert(!strcmp(ubx_key_name(0x20910007), "CFG-MSGOUT-UBX_NAV_PVT_UART1"));
    assert(!strcmp(ubx_key_name(0x10a3002f), "CFG-HW-ANT_CFG_SHORTDET"));
    assert(!strcmp(ubx_key_name(UBX_KEY_UART1_BAUDRATE), "CFG-UART1-BAUDRATE"));
    assert(!ubx_key_name(0x40520002) && !ubx_key_name(0));
    assert(!strcmp(ubx_key_name(0x10310026), "CFG-SIGNAL-NAVIC_ENA"));
    // SEC-UNIQID: an empty poll, and 5 bytes in version 1 or 6 in version 2, as hex.
    assert(ubx_poll_unique_id(pkt) == 8 && pkt[2] == 0x27 && pkt[3] == 0x03 && pkt[4] == 0 && pkt[5] == 0);
    assert(pkt[6] == 0x2a && pkt[7] == 0xa5);
    char uid[13];
    const uint8_t uid1[9] = {1, 0, 0, 0, 0x01, 0x23, 0x45, 0x67, 0x89}, uid2[10] = {2, 0, 0, 0, 0xde, 0xad, 0xbe, 0xef, 0x00, 0x2a};
    assert(ubx_unique_id(uid1, sizeof uid1, uid) && !strcmp(uid, "0123456789"));
    assert(ubx_unique_id(uid2, sizeof uid2, uid) && !strcmp(uid, "deadbeef002a"));
    assert(!ubx_unique_id(uid2, sizeof uid1, uid) && !ubx_unique_id(uid1, sizeof uid2, uid) && !ubx_unique_id(NULL, 9, uid));
    assert(ubx_set_ram(pkt, 0x10a3002f, 1, 1) == 17);
    assert(pkt[10] == 0x2f && pkt[11] == 0 && pkt[12] == 0xa3 && pkt[13] == 0x10 && pkt[14] == 1);
    uint8_t version[220] = {0}; strcpy((char *)version+40, "MOD=NEO-M9N");
    assert(ubx_version_is_module(version, 70, "NEO-M9N"));
    assert(!ubx_version_is_module(version, 70, "NEO-M9") && !ubx_version_is_module(version, 70, "ZED-X20P"));
    version[51] = 'X'; assert(!ubx_version_is_module(version, 70, "NEO-M9N"));
    version[51] = 0; assert(!ubx_version_is_module(version, 69, "NEO-M9N"));
    assert(!ubx_version_is_module(version, 70, "") && !ubx_version_is_module(version, 70, NULL));
    assert(!ubx_version_is_module(NULL, 70, "NEO-M9N"));
    // A name too long for one extension can never match.
    assert(!ubx_version_is_module(version, 70, "ABCDEFGHIJKLMNOPQRSTUVWXYZ"));
    // Observed SPG 4.04 response: model appears after three other extensions.
    memset(version, 0, sizeof version);
    const char *extensions[] = {"ROM BASE 0x118B2060", "FWVER=SPG 4.04", "PROTVER=32.01",
                                "MOD=NEO-M9N", "GPS;GLO;GAL;BDS", "SBAS;QZSS"};
    for (size_t i = 0; i < sizeof extensions / sizeof extensions[0]; i++)
        strcpy((char *)version + 40 + i * 30, extensions[i]);
    assert(ubx_version_is_module(version, sizeof version, "NEO-M9N"));
    // The ZED/X20 and MAX receivers report the same way; each board matches only its own.
    strcpy((char *)version + 40 + 3 * 30, "MOD=ZED-X20P");
    assert(ubx_version_is_module(version, sizeof version, "ZED-X20P"));
    assert(!ubx_version_is_module(version, sizeof version, "NEO-M9N"));
    memset(version + 40 + 3 * 30, 0, 30); strcpy((char *)version + 40 + 3 * 30, "MOD=MAX-M10S");
    assert(ubx_version_is_module(version, sizeof version, "MAX-M10S"));
    assert(!ubx_version_is_module(version, sizeof version, "ZED-X20P"));
    nmea_probe_t nmea = {0}; unsigned valid = 0;
    const char *sentence = "$GPGLL,4916.45,N,12311.12,W,225444,A,*1D\r\n";
    for (size_t i = 0; sentence[i]; i++) valid += nmea_probe_feed(&nmea, sentence[i]);
    assert(valid == 1);
    const char *corrupt = "$GPGLL,4916.45,N,12311.12,W,225444,A,*00\r\n";
    for (size_t i = 0; corrupt[i]; i++) valid += nmea_probe_feed(&nmea, corrupt[i]);
    assert(valid == 1);
    puts("UBX probe: fragmented checksums, RAM-only commands, per-model keys, model identity and NMEA detection passed");
}
