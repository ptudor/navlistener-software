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
    puts("UBX probe: fragmented checksums, RAM-only commands, model identity and NMEA detection passed");
}
