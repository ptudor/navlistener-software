/* Read-only discovery for approved 24CS128 / 24AA025E64 assemblies.
 * A bus error is never evidence of absence. No write-based device tests. */
#ifndef NVF_BOARD_IDENTITY_H
#define NVF_BOARD_IDENTITY_H
#include "board_uid.h"
enum { NVF_ID_READ_OK = 0, NVF_ID_ABSENT = 1, NVF_ID_NACK = 2, NVF_ID_IO_ERROR = 3 };
typedef int (*nvf_identity_read_fn)(void *, uint8_t address, uint16_t reg, uint8_t address_bytes, uint8_t *, size_t);
typedef struct {
    bool board_valid, eeprom_valid;
    uint8_t board_uid[NVF_BOARD_UID_SIZE], board_address;
    uint8_t eeprom_eui64[8], eeprom_address, eeprom_uid[NVF_BOARD_UID_SIZE];
    bool eeprom_cs128;
} nvf_board_identity_t;

static inline bool nvf_identity_present(const uint8_t *p, size_t n) {
    bool zero = true, erased = true;
    for (size_t i = 0; i < n; i++) { zero &= p[i] == 0; erased &= p[i] == 0xff; }
    return !zero && !erased;
}

/* known is the adopted/signed typed identity, or NULL only before adoption.
 * required_kind=0 permits discovery; an assembly policy can require one kind.
 * Missing or changed adopted identities fail, including a valid alternate source. */
static inline const char *nvf_board_discover(nvf_identity_read_fn read, void *ctx,
        const uint8_t *known, uint16_t required_kind, nvf_board_identity_t *out) {
    if (!read || !out) return "identity reader is missing";
    memset(out, 0, sizeof(*out));
    uint8_t cs[2][16] = {{0}}, manufacturer[3], repeated[16];
    bool found_cs[2] = {false, false};
    for (unsigned i = 0; i < 2; i++) {
        int status = read(ctx, 0x7c, (uint16_t)((0x50+i)<<1), 1, manufacturer, 3);
        if (status == NVF_ID_ABSENT || status == NVF_ID_NACK) continue;
        if (status != NVF_ID_READ_OK) return "Manufacturer ID bus failure";
        if (memcmp(manufacturer, "\x00\xd0\xb8", 3)) return "unqualified EEPROM Manufacturer ID";
        if (read(ctx, 0x58+i, 0x0800, 2, cs[i], 16) != NVF_ID_READ_OK ||
            read(ctx, 0x58+i, 0x0800, 2, repeated, 16) != NVF_ID_READ_OK ||
            memcmp(cs[i], repeated, 16) || !nvf_identity_present(cs[i], 16)) return "invalid or unstable 24CS128 serial";
        found_cs[i] = true;
    }
    if (found_cs[0] && found_cs[1]) return "multiple EEPROM candidates; assembly selection required";
    uint8_t e50[8] = {0}, e51[8] = {0}, again[8];
    bool ep50 = false, ep51 = false;
    int r50 = found_cs[0] ? NVF_ID_ABSENT : read(ctx, 0x50, 0xf8, 1, e50, 8);
    if (r50 == NVF_ID_READ_OK) {
        if (!nvf_identity_present(e50, 8)) return "EEPROM identity at 0x50 is blank or erased";
        if (read(ctx, 0x50, 0xf8, 1, again, 8) != NVF_ID_READ_OK || memcmp(e50, again, 8)) return "unstable EEPROM identity";
        ep50 = true;
    } else if (r50 != NVF_ID_ABSENT) return "EEPROM probe at 0x50 failed";
    int r51 = found_cs[1] ? NVF_ID_ABSENT : read(ctx, 0x51, 0xf8, 1, e51, 8);
    if (r51 == NVF_ID_READ_OK) {
        if (!nvf_identity_present(e51, 8)) return "EEPROM identity at 0x51 is blank or erased";
        if (read(ctx, 0x51, 0xf8, 1, again, 8) != NVF_ID_READ_OK || memcmp(e51, again, 8)) return "unstable EEPROM identity";
        ep51 = true;
    } else if (r51 != NVF_ID_ABSENT) return "EEPROM probe at 0x51 failed";
    if (ep50 && ep51) return "multiple manifest EEPROM candidates; assembly selection required";
    if (ep50 || ep51) {
        out->eeprom_valid = true; out->eeprom_address = ep51 ? 0x51 : 0x50;
        memcpy(out->eeprom_eui64, ep51 ? e51 : e50, 8);
    }
    if (out->eeprom_valid) nvf_uid_pack(NVF_UID_MICROCHIP_EUI64, out->eeprom_eui64, 8, out->eeprom_uid);
    if (found_cs[0] || found_cs[1]) {
        if (out->eeprom_valid) return "multiple EEPROM models require assembly selection";
        unsigned i = found_cs[1] ? 1 : 0;
        out->eeprom_valid = true; out->eeprom_cs128 = true; out->eeprom_address = 0x50+i;
        nvf_uid_pack(NVF_UID_MICROCHIP_CS128, cs[i], 16, out->eeprom_uid);
    }
    uint16_t selected = known ? nvf_uid_kind(known) : required_kind;
    if (!selected) selected = out->eeprom_cs128 ? NVF_UID_MICROCHIP_CS128 : NVF_UID_MICROCHIP_EUI64;
    if (known && (!nvf_uid_valid(known) || (required_kind && required_kind != selected))) return "invalid adopted identity or assembly policy mismatch";
    if (selected == nvf_uid_kind(out->eeprom_uid) && out->eeprom_valid) {
        memcpy(out->board_uid, out->eeprom_uid, NVF_BOARD_UID_SIZE); out->board_address = out->eeprom_address;
    } else if (known || required_kind || out->eeprom_valid) return "selected identity source is missing or unsupported by this reader";
    else return NULL; /* generic board without supported identity hardware */
    if (known && memcmp(known, out->board_uid, NVF_BOARD_UID_SIZE)) return "adopted board identity changed";
    out->board_valid = true;
    return NULL;
}
#endif
