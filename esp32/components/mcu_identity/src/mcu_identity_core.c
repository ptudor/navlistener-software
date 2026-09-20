// mcu_identity_core — see include/mcu_identity_core.h. Mirrors go/internal/commissioning.

#include "mcu_identity_core.h"

#include <stdio.h>
#include <string.h>

static const char statement_domain[] = "MFG-COMMISSION-v1";
static const char proof_domain[] = "NAVL-MCU-PROOF-v1";

static bool blank(const uint8_t *b, size_t n)
{
    bool zero = true, ones = true;
    for (size_t i = 0; i < n; i++) { zero = zero && b[i] == 0; ones = ones && b[i] == 0xff; }
    return zero || ones;
}
static bool all_zero(const uint8_t *b, size_t n)
{
    uint8_t any = 0;
    for (size_t i = 0; i < n; i++) any |= b[i];
    return any == 0;
}

const char *nvf_commission_validate(const nvf_commission_statement_t *s)
{
    if (!s) return "statement is missing";
    if (s->profile != NVF_PROFILE_TRUSTED && s->profile != NVF_PROFILE_OPEN && s->profile != NVF_PROFILE_TEST)
        return "profile is unknown";
    if (s->mcu_family != NVF_MCU_ESP32S3) return "microcontroller family is unknown";
    if (s->mcu_key_alg != NVF_MCU_KEY_NONE && s->mcu_key_alg != NVF_MCU_KEY_RSA3072_PSS)
        return "microcontroller key algorithm is unknown";
    if (s->product == 0) return "product is required";
    if (s->security & ~(unsigned)NVF_SEC_TRUSTED) return "security bits include reserved bits";
    if (s->identity_flags & ~(unsigned)NVF_IDENTITY_KNOWN) return "identity flags include reserved bits";
    bool rtc_present = (s->identity_flags & NVF_IDENTITY_RTC_PRESENT) != 0;
    bool rtc_bound = (s->identity_flags & NVF_IDENTITY_RTC_EUI_BOUND) != 0;
    if (rtc_bound && !rtc_present) return "RTC EUI-64 binding requires an RTC declaration";
    if (!rtc_present) {
        if (s->rtc_model_id != NVF_RTC_NONE) return "RTC model must be zero when no RTC is declared";
    } else if (s->rtc_model_id != NVF_RTC_MCP79412) return "RTC model is unknown";
    if (rtc_bound) {
        if (blank(s->rtc_eui64, sizeof s->rtc_eui64)) return "RTC EUI-64 is blank or erased";
    } else if (!all_zero(s->rtc_eui64, sizeof s->rtc_eui64))
        return "RTC EUI-64 must be zero when it is not bound";
    if (s->generation == 0) return "generation starts at 1";
    if (s->commissioned_at == 0) return "commissioning time is required";
    if (blank(s->atecc_serial, sizeof s->atecc_serial)) return "ATECC serial is blank or erased";
    if (blank(s->board_eui64, sizeof s->board_eui64)) return "board EUI-64 is blank or erased";
    if (blank(s->mcu_mac, sizeof s->mcu_mac)) return "microcontroller MAC is blank or erased";
    bool has_key = s->mcu_key_alg != NVF_MCU_KEY_NONE;
    if (has_key == all_zero(s->mcu_key_sha256, 32))
        return "key digest must be present exactly when a key algorithm is named";
    if (!has_key && (s->security & NVF_SEC_MCU_KEY_PROTECTED))
        return "security bits claim a protected key the statement does not name";
    if (((s->security & NVF_SEC_SECURE_BOOT) != 0) == all_zero(s->secure_boot_keys, 32))
        return "Secure Boot key digest must be present exactly when Secure Boot is enabled";
    if (s->profile == NVF_PROFILE_TRUSTED && (!has_key || s->security != NVF_SEC_TRUSTED))
        return "trusted profile requires a microcontroller key and every security bit";
    if (s->profile == NVF_PROFILE_OPEN && (has_key || s->security != 0))
        return "open profile describes a board that is never locked";
    if (s->profile != NVF_PROFILE_TEST && all_zero(s->attestation, 32))
        return "a production board is attested before it is commissioned";
    return NULL;
}

static void put_be(uint8_t *b, uint64_t v, size_t n) { while (n--) { b[n] = (uint8_t)v; v >>= 8; } }
static uint64_t get_be(const uint8_t *b, size_t n) { uint64_t v = 0; while (n--) v = v << 8 | *b++; return v; }

bool nvf_commission_encode(const nvf_commission_statement_t *s, uint8_t out[NVF_COMMISSION_STATEMENT_SIZE])
{
    if (!out || nvf_commission_validate(s)) return false;
    out[0] = NVF_COMMISSION_VERSION; out[1] = s->profile; out[2] = s->mcu_family; out[3] = s->mcu_key_alg;
    put_be(out + 4, s->product, 2); put_be(out + 6, s->board_rev, 2); put_be(out + 8, s->security, 2);
    put_be(out + 10, s->identity_flags, 2); put_be(out + 12, s->rtc_model_id, 2);
    put_be(out + 14, s->generation, 4); put_be(out + 18, s->commissioned_at, 8);
    memcpy(out + 26, s->board_eui64, 8); memcpy(out + 34, s->atecc_serial, 9);
    memcpy(out + 43, s->rtc_eui64, 8); memcpy(out + 51, s->mcu_mac, 6);
    memcpy(out + 57, s->mcu_key_sha256, 32); memcpy(out + 89, s->secure_boot_keys, 32);
    memcpy(out + 121, s->attestation, 32);
    return true;
}

bool nvf_commission_parse(const uint8_t *in, size_t len, nvf_commission_statement_t *out)
{
    if (!in || !out || len != NVF_COMMISSION_STATEMENT_SIZE || in[0] != NVF_COMMISSION_VERSION) return false;
    nvf_commission_statement_t s = { .profile = in[1], .mcu_family = in[2], .mcu_key_alg = in[3],
        .product = (uint16_t)get_be(in + 4, 2), .board_rev = (uint16_t)get_be(in + 6, 2),
        .security = (uint16_t)get_be(in + 8, 2), .identity_flags = (uint16_t)get_be(in + 10, 2),
        .rtc_model_id = (uint16_t)get_be(in + 12, 2), .generation = (uint32_t)get_be(in + 14, 4),
        .commissioned_at = get_be(in + 18, 8) };
    memcpy(s.board_eui64, in + 26, 8); memcpy(s.atecc_serial, in + 34, 9);
    memcpy(s.rtc_eui64, in + 43, 8); memcpy(s.mcu_mac, in + 51, 6);
    memcpy(s.mcu_key_sha256, in + 57, 32); memcpy(s.secure_boot_keys, in + 89, 32);
    memcpy(s.attestation, in + 121, 32);
    if (nvf_commission_validate(&s)) return false;
    *out = s;
    return true;
}

bool nvf_commission_digest(const uint8_t statement[NVF_COMMISSION_STATEMENT_SIZE], nvf_sha256_fn sha, uint8_t out[32])
{
    if (!statement || !sha || !out) return false;
    uint8_t body[sizeof statement_domain - 1 + NVF_COMMISSION_STATEMENT_SIZE];
    memcpy(body, statement_domain, sizeof statement_domain - 1);
    memcpy(body + sizeof statement_domain - 1, statement, NVF_COMMISSION_STATEMENT_SIZE);
    return sha(body, sizeof body, out);
}

bool nvf_commission_record_parse(const uint8_t *record, size_t len, nvf_commission_statement_t *out)
{
    return record && len == NVF_COMMISSION_RECORD_SIZE &&
           nvf_commission_parse(record, NVF_COMMISSION_STATEMENT_SIZE, out);
}

bool nvf_commission_fingerprint(const uint8_t record[NVF_COMMISSION_RECORD_SIZE], nvf_sha256_fn sha, uint8_t out[32])
{
    return record && sha && out && sha(record, NVF_COMMISSION_RECORD_SIZE, out);
}

bool nvf_mcu_proof_digest(const uint8_t exported[NVF_MCU_EXPORTED_SIZE], const uint8_t record[NVF_COMMISSION_RECORD_SIZE],
                          nvf_sha256_fn sha, uint8_t out[32])
{
    if (!exported || !record || !sha || !out) return false;
    uint8_t body[sizeof proof_domain - 1 + NVF_MCU_EXPORTED_SIZE + 32];
    size_t at = sizeof proof_domain - 1;
    memcpy(body, proof_domain, at);
    memcpy(body + at, exported, NVF_MCU_EXPORTED_SIZE);
    return nvf_commission_fingerprint(record, sha, body + at + NVF_MCU_EXPORTED_SIZE) && sha(body, sizeof body, out);
}

size_t nvf_evidence_encode(uint8_t *out, size_t cap, const uint8_t record[NVF_COMMISSION_RECORD_SIZE],
                           const uint8_t *key, size_t key_len, const uint8_t *proof, size_t proof_len)
{
    size_t need = 1 + 2 + NVF_COMMISSION_RECORD_SIZE + 2 + key_len + 2 + proof_len;
    if (!out || !record || key_len > NVF_MCU_KEY_DER_MAX || proof_len > NVF_MCU_PROOF_MAX ||
        (key_len == 0) != (proof_len == 0) || (key_len && (!key || !proof)) ||
        need > cap || need > NVF_EVIDENCE_MAX)
        return 0;
    const uint8_t *fields[] = { record, key, proof };
    const size_t lengths[] = { NVF_COMMISSION_RECORD_SIZE, key_len, proof_len };
    uint8_t *p = out;
    *p++ = NVF_EVIDENCE_VERSION;
    for (unsigned i = 0; i < 3; i++) {
        put_be(p, lengths[i], 2); p += 2;
        if (lengths[i]) memcpy(p, fields[i], lengths[i]);
        p += lengths[i];
    }
    return (size_t)(p - out);
}

bool nvf_pss_encode(const uint8_t digest[32], const uint8_t salt[NVF_MCU_PSS_SALT], nvf_sha256_fn sha, uint8_t *em, size_t em_len)
{
    enum { HLEN = 32 };
    if (!digest || !salt || !sha || !em || em_len < HLEN + NVF_MCU_PSS_SALT + 2) return false;
    uint8_t m[8 + HLEN + NVF_MCU_PSS_SALT] = {0}, *h = em + em_len - HLEN - 1;
    size_t db_len = em_len - HLEN - 1;
    memcpy(m + 8, digest, HLEN); memcpy(m + 8 + HLEN, salt, NVF_MCU_PSS_SALT);
    if (!sha(m, sizeof m, h)) return false;
    // DB = PS || 0x01 || salt, masked in place with MGF1(H).
    memset(em, 0, db_len - NVF_MCU_PSS_SALT - 1);
    em[db_len - NVF_MCU_PSS_SALT - 1] = 0x01;
    memcpy(em + db_len - NVF_MCU_PSS_SALT, salt, NVF_MCU_PSS_SALT);
    uint8_t seed[HLEN + 4], mask[HLEN];
    memcpy(seed, h, HLEN);
    for (size_t at = 0, counter = 0; at < db_len; at += HLEN, counter++) {
        put_be(seed + HLEN, counter, 4);
        if (!sha(seed, sizeof seed, mask)) return false;
        for (size_t i = 0; i < HLEN && at + i < db_len; i++) em[at + i] ^= mask[i];
    }
    em[0] &= 0x7f; // emBits = 8*em_len - 1: the leftmost bit is always clear
    em[em_len - 1] = 0xbc;
    return true;
}

uint16_t nvf_commission_security_bits(const nvf_chip_security_t *c)
{
    uint16_t bits = 0;
    if (c->secure_boot) bits |= NVF_SEC_SECURE_BOOT;
    if (c->flash_enc_release) bits |= NVF_SEC_FLASH_ENC_RELEASE;
    if (c->jtag_disabled) bits |= NVF_SEC_JTAG_DISABLED;
    if (c->secure_download) bits |= NVF_SEC_SECURE_DOWNLOAD;
    if (c->key_ready && c->key_block_protected && c->rd_dis_sealed) bits |= NVF_SEC_MCU_KEY_PROTECTED;
    return bits;
}

const char *nvf_mcu_keygen_refusal(const nvf_mcu_keygen_state_t *s)
{
    if (!s->locked && !s->unlocked_allowed)
        return "Secure Boot and release-mode flash encryption must be active before the key is made";
    if (s->key_ready) return "this chip already has a microcontroller key";
    if (s->key_orphaned) return "a key block is burned but its ciphertext is not stored; restore it instead";
    if (s->fault_unresolved) return "a burned key block has not completed its self-test; restart and check the report first";
    if (s->rd_dis_sealed) return "eFuse read protection is already sealed on this chip, so a key block can no longer be protected";
    if (!s->free_block) return "no free eFuse key block";
    return NULL;
}

void nvf_commission_observer_id(const uint8_t eui[8], char out[24])
{
    snprintf(out, 24, "%02x-%02x-%02x-%02x-%02x-%02x-%02x-%02x",
             eui[0], eui[1], eui[2], eui[3], eui[4], eui[5], eui[6], eui[7]);
}

const char *nvf_commission_match(const nvf_commission_statement_t *s, const nvf_live_identity_t *live,
                                 nvf_sha256_fn sha)
{
    if (!s || !live || !sha) return "identity verifier is missing";
    if (s->product != NVF_COMMISSION_PRODUCT_OBSERVER) return "record is for another product, not an observer";
    if (!live->atecc_valid) return "this board's ATECC serial could not be read";
    if (!live->board_valid) return "this board's EEPROM EUI-64 could not be read";
    if (!live->revision_valid) return "this board's revision could not be read";
    if (!live->mac_valid) return "this microcontroller's factory MAC could not be read";
    if (memcmp(s->atecc_serial, live->atecc_serial, 9)) return "record names a different ATECC serial";
    if (memcmp(s->board_eui64, live->board_eui64, 8)) return "record names a different board EUI-64";
    if (s->board_rev != live->board_rev) return "record names a different board revision";
    if (memcmp(s->mcu_mac, live->mcu_mac, 6)) return "record names a different microcontroller MAC";
    bool rtc_present = (s->identity_flags & NVF_IDENTITY_RTC_PRESENT) != 0;
    bool rtc_bound = (s->identity_flags & NVF_IDENTITY_RTC_EUI_BOUND) != 0;
    if (rtc_present) {
        if (!live->rtc_expected) return "record names an RTC that this product configuration does not support";
        if (s->rtc_model_id != live->rtc_model_id) return "record names a different RTC model";
        if (!live->rtc_present) return "this board's expected RTC could not be verified";
        if (rtc_bound) {
            if (!live->rtc_valid) return "this board's RTC EUI-64 could not be read";
            if (memcmp(s->rtc_eui64, live->rtc_eui64, 8)) return "record names a different RTC EUI-64";
        }
    } else if (live->rtc_expected) return "record omits the RTC required by this product configuration";
    if (!live->security_valid) return "this microcontroller's security state could not be read";
    if (s->security != live->security) return "record names a different microcontroller security state";
    if (s->security & NVF_SEC_SECURE_BOOT) {
        if (!live->secure_boot_keys_valid) return "this microcontroller's Secure Boot keys could not be read";
        if (memcmp(s->secure_boot_keys, live->secure_boot_keys, 32)) return "record names different Secure Boot keys";
    }
    if (!all_zero(s->attestation, 32)) {
        uint8_t digest[32];
        if (!live->attestation_valid) return "this board's core attestation record could not be read";
        if (!sha(live->attestation_record, sizeof live->attestation_record, digest))
            return "this board's core attestation record could not be hashed";
        if (memcmp(s->attestation, digest, 32)) return "record names a different core attestation record";
    }
    if (s->mcu_key_alg != NVF_MCU_KEY_NONE) {
        if (!live->key_valid) return "record names a microcontroller key and this chip holds none";
        if (memcmp(s->mcu_key_sha256, live->mcu_key_sha256, 32)) return "record names a different microcontroller key";
    }
    return NULL;
}
