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
    static const ubx_setting_t empty_setting = {0}, oversized_setting = {UBX_SETTING_ITEMS + 1, {{0}}};
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
    // Settings: the shared list (NEO-M9N, and any other listed model), the ZED-X20P's with
    // its LNA modes, QZSS and NavIC straight after UBX output and short detection and SEC-SIG
    // last, and the MAX-M10S's with its four GNSS as one message in the same place.
    const ubx_setting_t *shared, *x20, *max;
    size_t count = ubx_settings("NEO-M9N", true, &shared), x20_count = ubx_settings("ZED-X20P", true, &x20),
           max_count = ubx_settings("MAX-M10S", true, &max);
    assert(count == 10 && x20_count == count + 5 && max_count == count + 1);
    const uint32_t rates[] = {0x20910232, 0x2091035a, 0x20910016, 0x20910007, 0x2091001b, 0x2091017e,
                              0x20910346, 0x20910066, 0x20910160};
    for (size_t i = 0; i < count; i++) {
        const uint32_t want = i ? rates[i - 1] : 0x10740001;
        assert(shared[i].count == 1 && shared[i].item[0].key == want && shared[i].item[0].value == 1);
        assert(x20[i ? i + 3 : 0].count == 1 && x20[i ? i + 3 : 0].item[0].key == want);
        assert(max[i ? i + 1 : 0].count == 1 && max[i ? i + 1 : 0].item[0].key == want);
    }
    const ubx_setting_t *lna = &x20[1];
    assert(lna->count == 3 && lna->item[0].key == 0x10a3006a && lna->item[1].key == 0x10a3006b);
    assert(lna->item[2].key == 0x10a3006c);
    for (unsigned i = 0; i < 3; i++) assert(lna->item[i].value == 1); // low gain on every RF block
    // A unit set to normal gain gets the same list with the receiver's default written out,
    // since a receiver that kept its RAM may hold low gain; no other list depends on the mode.
    const ubx_setting_t *x20_normal, *same;
    assert(ubx_settings("ZED-X20P", false, &x20_normal) == x20_count && x20_normal != x20);
    for (size_t i = 0; i < x20_count; i++) {
        assert(x20_normal[i].count == x20[i].count);
        for (unsigned k = 0; k < x20[i].count; k++) {
            assert(x20_normal[i].item[k].key == x20[i].item[k].key);
            assert(x20_normal[i].item[k].value == (i == 1 ? 0 : x20[i].item[k].value));
        }
    }
    assert(ubx_settings("NEO-M9N", false, &same) == count && same == shared);
    assert(ubx_settings("MAX-M10S", false, &same) == max_count && same == max);
    assert(x20[2].item[0].key == 0x10310024 && x20[3].item[0].key == 0x10310026);
    assert(x20[count + 3].item[0].key == 0x10a3002f && x20[count + 4].item[0].key == 0x20910635);
    const ubx_setting_t *four = &max[1];
    assert(four->count == 5 && four->item[0].key == 0x1031000d && four->item[0].value == 0); // B1I off
    assert(four->item[1].key == 0x1031000f && four->item[2].key == 0x10310018 && four->item[3].key == 0x10310025);
    assert(four->item[4].key == 0x10310022);
    for (unsigned i = 1; i < 5; i++) assert(four->item[i].value == 1);
    // Only the signal settings reset the GNSS subsystem, and each precedes every message rate.
    for (size_t i = 0; i < x20_count; i++) assert(ubx_setting_resets_gnss(&x20[i]) == (i == 2 || i == 3));
    for (size_t i = 0; i < max_count; i++) assert(ubx_setting_resets_gnss(&max[i]) == (i == 1));
    for (size_t i = 0; i < count; i++) assert(!ubx_setting_resets_gnss(&shared[i]));
    // Only the LNA modes wait for a restart of the GNSS subsystem, and they too precede the rates.
    for (size_t i = 0; i < x20_count; i++) assert(ubx_setting_needs_restart(&x20[i]) == (i == 1));
    for (size_t i = 0; i < x20_count; i++) assert(ubx_setting_needs_restart(&x20_normal[i]) == (i == 1));
    for (size_t i = 0; i < max_count; i++) assert(!ubx_setting_needs_restart(&max[i]));
    for (size_t i = 0; i < count; i++) assert(!ubx_setting_needs_restart(&shared[i]));
    // The poll that reads them: CFG-VALGET version 0, the RAM layer, from the first result.
    uint8_t ask[UBX_SETTING_POLL_MAX];
    const uint8_t asked[] = {0xb5, 0x62, 0x06, 0x8b, 16, 0, 0, 0, 0, 0,
                             0x6a, 0, 0xa3, 0x10, 0x6b, 0, 0xa3, 0x10, 0x6c, 0, 0xa3, 0x10};
    assert(ubx_encode_setting_poll(ask, lna) == sizeof asked + 2 && !memcmp(ask, asked, sizeof asked));
    uint8_t sum_a = 0, sum_b = 0;
    for (size_t i = 2; i < sizeof asked; i++) { sum_a += ask[i]; sum_b += sum_a; }
    assert(ask[sizeof asked] == sum_a && ask[sizeof asked + 1] == sum_b);
    assert(ubx_encode_setting_poll(ask, &x20[2]) == 16 && ubx_get_ram(pkt, 0x10310024) == 16 && !memcmp(ask, pkt, 16));
    // Its answer is version 1: held when every key is at its value, in any order and among
    // other keys of any size; differing when one is not; and no answer to this poll when a
    // key is missing, the layer is not RAM, the version is the poll's own or the body is cut.
    uint8_t held[] = {1, 0, 0, 0, 0x6a, 0, 0xa3, 0x10, 1, 0x6b, 0, 0xa3, 0x10, 1, 0x6c, 0, 0xa3, 0x10, 1};
    assert(ubx_setting_read(lna, held, sizeof held) == UBX_SETTING_HELD);
    const uint8_t mixed[] = {1, 0, 0, 0, 0x6c, 0, 0xa3, 0x10, 1, 0x01, 0, 0x52, 0x40, 0, 8, 7, 0,
                             0x6a, 0, 0xa3, 0x10, 1, 0x34, 0, 0x21, 0x30, 0xe8, 3, 0x6b, 0, 0xa3, 0x10, 1};
    assert(ubx_setting_read(lna, mixed, sizeof mixed) == UBX_SETTING_HELD);
    for (unsigned i = 0; i < 3; i++) {
        held[8 + 5 * i] = 0;
        assert(ubx_setting_read(lna, held, sizeof held) == UBX_SETTING_DIFFERS);
        held[8 + 5 * i] = 1;
    }
    assert(ubx_setting_read(lna, held, sizeof held - 5) == UBX_SETTING_OTHER); // RF3 not answered
    assert(ubx_setting_read(lna, held, sizeof held - 1) == UBX_SETTING_OTHER); // cut inside a value
    assert(ubx_setting_read(lna, held, 4) == UBX_SETTING_OTHER && ubx_setting_read(lna, held, 3) == UBX_SETTING_OTHER);
    assert(ubx_setting_read(lna, NULL, sizeof held) == UBX_SETTING_OTHER);
    held[1] = 1; assert(ubx_setting_read(lna, held, sizeof held) == UBX_SETTING_OTHER); // the BBR layer
    held[1] = 0; held[0] = 0; assert(ubx_setting_read(lna, held, sizeof held) == UBX_SETTING_OTHER);
    held[0] = 1; assert(ubx_setting_read(lna, held, sizeof held) == UBX_SETTING_HELD);
    const uint8_t sensitivity[] = {1, 0, 0, 0, 0x51, 0, 0xf6, 0x10, 1}; // another poll's answer
    assert(ubx_setting_read(lna, sensitivity, sizeof sensitivity) == UBX_SETTING_OTHER);
    const uint8_t sizeless[] = {1, 0, 0, 0, 0x6a, 0, 0xa3, 0x00, 1, 0x6b, 0, 0xa3, 0x10, 1, 0x6c, 0, 0xa3, 0x10, 1};
    assert(ubx_setting_read(lna, sizeless, sizeof sizeless) == UBX_SETTING_OTHER); // a key of no known size
    const uint8_t twice[] = {1, 0, 0, 0, 0x6a, 0, 0xa3, 0x10, 0, 0x6a, 0, 0xa3, 0x10, 1,
                             0x6b, 0, 0xa3, 0x10, 1, 0x6c, 0, 0xa3, 0x10, 1};
    assert(ubx_setting_read(lna, twice, sizeof twice) == UBX_SETTING_DIFFERS); // the first answer for a key counts
    // The same answers read the other way for a unit set to normal gain.
    assert(ubx_setting_read(&x20_normal[1], held, sizeof held) == UBX_SETTING_DIFFERS);
    for (unsigned i = 0; i < 3; i++) held[8 + 5 * i] = 0;
    assert(ubx_setting_read(&x20_normal[1], held, sizeof held) == UBX_SETTING_HELD);
    assert(ubx_setting_read(lna, held, sizeof held) == UBX_SETTING_DIFFERS);
    for (unsigned i = 0; i < 3; i++) held[8 + 5 * i] = 1;
    uint8_t normal[UBX_SETTING_MAX];
    assert(ubx_encode_setting(normal, &x20_normal[1]) == 12 + 15 && normal[14] == 0 && normal[19] == 0 && normal[24] == 0);
    assert(ubx_setting_read(&x20[2], held, sizeof held) == UBX_SETTING_OTHER);
    assert(ubx_setting_read(&empty_setting, held, sizeof held) == UBX_SETTING_OTHER);
    assert(!ubx_encode_setting_poll(ask, &empty_setting) && !ubx_encode_setting_poll(ask, &oversized_setting));
    // The restart that applies them: UBX-CFG-RST, nothing cleared, the GNSS subsystem alone.
    const uint8_t restart[] = {0xb5, 0x62, 0x06, 0x04, 4, 0, 0, 0, 0x02, 0, 0x10, 0x68};
    assert(ubx_restart_gnss(pkt) == sizeof restart && !memcmp(pkt, restart, sizeof restart));
    assert(!ubx_key_resets_gnss(UBX_KEY_UART1_BAUDRATE) && ubx_key_resets_gnss(0x1031001f));
    const ubx_setting_t *other;
    assert(ubx_settings(NULL, true, &other) == count && other == shared);
    assert(ubx_settings("ZED-X20", true, &other) == count && ubx_settings("MAX-M10S ", true, &other) == count);
    // Every key the firmware sets has its interface-description name, each a different one.
    const char *names[64]; size_t named = 0;
    const ubx_setting_t *lists[] = {x20, max};
    const size_t counts[] = {x20_count, max_count};
    for (unsigned l = 0; l < 2; l++)
        for (size_t i = 0; i < counts[l]; i++)
            for (unsigned k = 0; k < lists[l][i].count; k++) {
                const char *name = ubx_key_name(lists[l][i].item[k].key);
                assert(name && !strncmp(name, "CFG-", 4));
                bool seen = false;
                for (size_t j = 0; j < named; j++) seen |= !strcmp(names[j], name);
                if (!seen) names[named++] = name;
            }
    assert(named == x20_count + 2 + 5); // the X20's 17, three of them in one setting, and the MAX's five signal keys
    // A one-item setting encodes as ubx_set_ram does; five items in one message.
    uint8_t one[UBX_SETTING_MAX], single[20];
    assert(ubx_encode_setting(one, &x20[2]) == 17 && ubx_set_ram(single, 0x10310024, 1, 1) == 17);
    assert(!memcmp(one, single, 17));
    uint8_t five[UBX_SETTING_MAX];
    assert(ubx_encode_setting(five, four) == 12 + 25 && five[4] == 29 && five[5] == 0);
    assert(five[10] == 0x0d && five[11] == 0 && five[12] == 0x31 && five[13] == 0x10 && five[14] == 0);
    assert(five[15] == 0x0f && five[19] == 1 && five[30] == 0x22 && five[34] == 1);
    uint8_t a = 0, b = 0;
    for (size_t i = 2; i < 6 + 29; i++) { a += five[i]; b += a; }
    assert(five[35] == a && five[36] == b);
    assert(!ubx_encode_setting(five, &empty_setting) && !ubx_encode_setting(five, &oversized_setting));
    uint8_t three[UBX_SETTING_MAX];
    assert(ubx_encode_setting(three, lna) == 12 + 15 && three[4] == 19 && three[7] == 1); // the RAM layer alone
    assert(three[10] == 0x6a && three[13] == 0x10 && three[14] == 1 && three[15] == 0x6b && three[19] == 1);
    assert(three[20] == 0x6c && three[24] == 1);
    assert(!strcmp(ubx_key_name(0x20910007), "CFG-MSGOUT-UBX_NAV_PVT_UART1"));
    assert(!strcmp(ubx_key_name(0x10a3002f), "CFG-HW-ANT_CFG_SHORTDET"));
    assert(!strcmp(ubx_key_name(UBX_KEY_UART1_BAUDRATE), "CFG-UART1-BAUDRATE"));
    assert(!ubx_key_name(0x40520002) && !ubx_key_name(0));
    assert(!strcmp(ubx_key_name(0x10310026), "CFG-SIGNAL-NAVIC_ENA"));
    assert(!strcmp(ubx_key_name(0x10a3006a), "CFG-HW-RF1_LNA_MODE_LOWGAIN"));
    assert(!strcmp(ubx_key_name(0x10a3006c), "CFG-HW-RF3_LNA_MODE_LOWGAIN"));
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
    puts("UBX probe: fragmented checksums, RAM-only commands, per-model keys, settings read back and applied by a GNSS restart, model identity and NMEA detection passed");
}
