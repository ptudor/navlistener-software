/* Read-only discovery for approved 24CS128/24CS256/24CS512 and M24128-U
 * assemblies, whose 128-bit factory serial is the board identity. A bus error
 * is never evidence of absence, and an EEPROM without a supported serial is an
 * error, not a board without identity hardware. No write-based device tests.
 * eeprom_kbit is the array density (128, 256 or 512) so a caller can select the
 * matching EEPROM profile. */
#ifndef NVF_BOARD_IDENTITY_H
#define NVF_BOARD_IDENTITY_H
#include "board_uid.h"
enum { NVF_ID_READ_OK = 0, NVF_ID_ABSENT = 1, NVF_ID_NACK = 2, NVF_ID_IO_ERROR = 3 };
typedef int (*nvf_identity_read_fn)(void *, uint8_t address, uint16_t reg, uint8_t address_bytes, uint8_t *, size_t);
typedef struct {
    bool board_valid, eeprom_valid;
    uint8_t board_uid[NVF_BOARD_UID_SIZE], board_address;
    uint8_t eeprom_address, eeprom_uid[NVF_BOARD_UID_SIZE];
    uint16_t eeprom_kind, eeprom_kbit;
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
    /* Microchip 24CS Manufacturer ID (reserved host code F8h), revision 1 parts:
     * DS20006913B 24CS128, DS20005998D 24CS256, DS20005769H 24CS512. */
    static const struct { uint8_t id[3]; uint16_t kbit; } models[] = {
        {{0x00, 0xd0, 0xb8}, 128}, {{0x00, 0xd0, 0xc0}, 256}, {{0x00, 0xd0, 0xc8}, 512},
    };
    if (!read || !out) return "identity reader is missing";
    memset(out, 0, sizeof(*out));
    for (unsigned i = 0; i < 2; i++) {
        uint8_t value[NVF_BOARD_UID_BYTES] = {0}, again[NVF_BOARD_UID_BYTES], manufacturer[3];
        uint8_t address = 0x50 + i;
        uint16_t kind = 0, reg = 0, kbit = 0;
        const size_t length = NVF_BOARD_UID_BYTES;
        int status = read(ctx, 0x7c, (uint16_t)(address << 1), 1, manufacturer, 3);
        if (status == NVF_ID_READ_OK) {
            for (size_t m = 0; m < sizeof models / sizeof models[0]; m++)
                if (!memcmp(manufacturer, models[m].id, 3)) kbit = models[m].kbit;
            if (!kbit) return "unqualified EEPROM Manufacturer ID";
            kind = NVF_UID_SERIAL128;
            reg = 0x0800;
            status = read(ctx, address + 8, reg, 2, value, length);
            if (status != NVF_ID_READ_OK) return "24CS serial read failed";
        } else {
            if (status != NVF_ID_ABSENT && status != NVF_ID_NACK) return "Manufacturer ID bus failure";
            // ST ignores upper identification-page address bits. A read ACK at
            // Microchip's offset would not identify it; require the ST header.
            status = read(ctx, address + 8, 0, 2, value, length);
            if (status == NVF_ID_READ_OK) {
                if (memcmp(value, "\x20\xe0\x0e\xff", 4)) return "unqualified M24128-U identification page";
                kind = NVF_UID_ST_UID128;
                kbit = 128;
            } else if (status != NVF_ID_ABSENT) return "identification page bus failure";
        }
        if (!kind) {
            // Neither serial interface answered. A device at the main array address is
            // an EEPROM without a supported 128-bit factory serial, never an absence.
            status = read(ctx, address, 0, 1, value, 1);
            if (status == NVF_ID_ABSENT) continue;
            if (status != NVF_ID_READ_OK) return "EEPROM probe failed";
            return "EEPROM has no supported 128-bit factory serial";
        }
        if (read(ctx, address + 8, reg, 2, again, length) != NVF_ID_READ_OK ||
            memcmp(value, again, length) || !nvf_identity_present(value, length))
            return "invalid or unstable EEPROM identity";
        if (out->eeprom_valid) return "multiple EEPROM candidates; assembly selection required";
        out->eeprom_valid = true;
        out->eeprom_address = address;
        out->eeprom_kind = kind;
        out->eeprom_kbit = kbit;
        if (!nvf_uid_pack(kind, value, length, out->eeprom_uid)) return "invalid EEPROM identity";
    }
    uint16_t selected = known ? nvf_uid_kind(known) : required_kind;
    if (!selected) selected = out->eeprom_kind;
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
