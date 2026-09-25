// core_test — host tests for the portable commissioning core.
//
// The fixtures in common/fixtures/commissioning-*.hex are written by the Go reference
// implementation (go/internal/commissioning, TestFixtures). Matching them byte for byte is
// what lets the collector verify what this firmware sends: the statement layout, the signed
// digest, the session proof digest and the EVIDENCE payload. The proof fixture is a real
// RSASSA-PSS signature, so the PSS parameters are also checked against it: the reference
// verifier must accept Go's encoding before it is allowed to judge this encoder.
//
//   make -C esp32/components/mcu_identity/test

#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "mcu_identity_core.h"
#include "reference.h"

static size_t fixture(const char *name, uint8_t *out, size_t cap)
{
    char path[160];
    snprintf(path, sizeof path, "../../../../common/fixtures/commissioning-%s-v1.hex", name);
    FILE *f = fopen(path, "r");
    if (!f) { fprintf(stderr, "missing fixture %s\n", path); exit(1); }
    size_t n = 0;
    unsigned byte;
    while (fscanf(f, "%2x", &byte) == 1) { assert(n < cap); out[n++] = (uint8_t)byte; }
    fclose(f);
    return n;
}

static uint8_t statement[NVF_COMMISSION_STATEMENT_SIZE], record[NVF_COMMISSION_RECORD_SIZE];
static uint8_t digest[32], exported[32], proof_digest[32], key[NVF_MCU_KEY_DER_MAX], proof[NVF_MCU_PROOF_MAX];
static uint8_t evidence[NVF_EVIDENCE_MAX];
static size_t key_len, proof_len, evidence_len;

static void test_sha256_reference(void)
{
    static const uint8_t abc[32] = { 0xba,0x78,0x16,0xbf,0x8f,0x01,0xcf,0xea,0x41,0x41,0x40,0xde,0x5d,0xae,0x22,0x23,
                                     0xb0,0x03,0x61,0xa3,0x96,0x17,0x7a,0x9c,0xb4,0x10,0xff,0x61,0xf2,0x00,0x15,0xad };
    uint8_t out[32], long_input[200];
    assert(ref_sha256("abc", 3, out) && !memcmp(out, abc, 32));
    // Cross a block boundary and the 55/56-byte padding edge without a second known answer:
    // hashing must at least be length-sensitive and deterministic.
    memset(long_input, 'a', sizeof long_input);
    uint8_t a[32], b[32];
    ref_sha256(long_input, 55, a); ref_sha256(long_input, 56, b); assert(memcmp(a, b, 32));
    ref_sha256(long_input, 56, a); assert(!memcmp(a, b, 32));
}

static void test_statement_round_trip(void)
{
    nvf_commission_statement_t s;
    assert(nvf_commission_parse(statement, sizeof statement, &s));
    assert(s.profile == NVF_PROFILE_TRUSTED && s.mcu_family == NVF_MCU_ESP32S3 && s.mcu_key_alg == NVF_MCU_KEY_RSA3072_PSS);
    assert(s.product == NVF_COMMISSION_PRODUCT_OBSERVER && statement[4] == 0 && statement[5] == 1);
    assert(s.board_rev == 0x0102 && s.security == NVF_SEC_TRUSTED &&
           s.identity_flags == (NVF_IDENTITY_RTC_PRESENT | NVF_IDENTITY_RTC_EUI_RECORDED) &&
           s.rtc_model_id == NVF_RTC_MCP79412 && s.generation == 1 && s.commissioned_at == 1789646400u);
    static const uint8_t rtc[8] = { 0x00, 0x04, 0xa3, 0x12, 0x34, 0x56, 0x78, 0x90 };
    assert(!memcmp(s.rtc_eui64, rtc, 8) && !memcmp(statement + 70, rtc, 8));
    uint8_t again[NVF_COMMISSION_STATEMENT_SIZE];
    assert(nvf_commission_encode(&s, again) && !memcmp(again, statement, sizeof statement));
    char observer[NVF_BOARD_OBSERVER_SIZE];
    nvf_commission_observer_id(s.board_uid, observer);
    assert(!strcmp(observer, "board-0003-00112233445566778899aabbccddeeff"));

    uint8_t got[32];
    assert(nvf_commission_digest(statement, ref_sha256, got) && !memcmp(got, digest, 32));
    // The key digest in the statement is the digest of the key the evidence carries.
    assert(ref_sha256(key, key_len, got) && !memcmp(got, s.mcu_key_sha256, 32));
}

static void test_record_and_proof_digest(void)
{
    nvf_commission_statement_t s;
    assert(nvf_commission_record_parse(record, sizeof record, &s));
    assert(!memcmp(record, statement, sizeof statement));
    assert(!nvf_commission_record_parse(record, sizeof record - 1, &s));
    uint8_t got[32];
    assert(nvf_mcu_proof_digest(exported, record, ref_sha256, got) && !memcmp(got, proof_digest, 32));
    // A different session or a different record must change what is signed.
    uint8_t other[32]; memcpy(other, exported, 32); other[0] ^= 1;
    assert(nvf_mcu_proof_digest(other, record, ref_sha256, got) && memcmp(got, proof_digest, 32));
    uint8_t changed[NVF_COMMISSION_RECORD_SIZE]; memcpy(changed, record, sizeof changed); changed[sizeof changed - 1] ^= 1;
    assert(nvf_mcu_proof_digest(exported, changed, ref_sha256, got) && memcmp(got, proof_digest, 32));
}

static void test_evidence_encoding(void)
{
    uint8_t out[NVF_EVIDENCE_MAX];
    size_t n = nvf_evidence_encode(out, sizeof out, record, key, key_len, proof, proof_len);
    assert(n == evidence_len && !memcmp(out, evidence, n));
    // A record that names no key travels alone: version, record, two empty fields.
    n = nvf_evidence_encode(out, sizeof out, record, NULL, 0, NULL, 0);
    assert(n == 1 + 2 + NVF_COMMISSION_RECORD_SIZE + 2 + 2 && out[0] == NVF_EVIDENCE_VERSION);
    assert(out[1] == 0 && out[2] == NVF_COMMISSION_RECORD_SIZE && !memcmp(out + n - 4, "\0\0\0\0", 4));
    assert(!nvf_evidence_encode(out, sizeof out, record, key, key_len, NULL, 0));   // key without proof
    assert(!nvf_evidence_encode(out, sizeof out, record, NULL, 0, proof, proof_len)); // proof without key
    assert(!nvf_evidence_encode(out, evidence_len - 1, record, key, key_len, proof, proof_len));
    assert(!nvf_evidence_encode(out, sizeof out, record, key, NVF_MCU_KEY_DER_MAX + 1, proof, proof_len));
    assert(!nvf_evidence_encode(out, sizeof out, record, key, key_len, proof, NVF_MCU_PROOF_MAX + 1));
}

static void test_validation(void)
{
    nvf_commission_statement_t good, s;
    assert(nvf_commission_parse(statement, sizeof statement, &good) && !nvf_commission_validate(&good));
#define REJECT(change) do { s = good; change; assert(nvf_commission_validate(&s)); \
        uint8_t b[NVF_COMMISSION_STATEMENT_SIZE]; assert(!nvf_commission_encode(&s, b)); } while (0)
    REJECT(s.profile = 9);
    REJECT(s.mcu_family = 2);
    REJECT(s.mcu_key_alg = 7);
    REJECT(s.product = 0);
    REJECT(s.security |= 1u << 9);
    REJECT(s.identity_flags |= 1u << 9);
    REJECT(s.identity_flags = NVF_IDENTITY_RTC_EUI_RECORDED);
    REJECT(s.rtc_model_id = 99);
    REJECT(s.generation = 0);
    REJECT(s.commissioned_at = 0);
    REJECT(memset(s.atecc_serial, 0xff, 9));
    REJECT(memset(s.rtc_eui64, 0, 8));
    REJECT(memset(s.board_uid, 0, 8));
    REJECT(s.board_uid[1] = 1); // the retired 64-bit kind is never a board identity
    REJECT(memset(s.mcu_mac, 0, 6));
    REJECT(s.mcu_key_alg = NVF_MCU_KEY_NONE; memset(s.mcu_key_sha256, 0, 32)); // trusted without a key
    REJECT(s.security &= ~(unsigned)NVF_SEC_FLASH_ENC_RELEASE);                  // trusted but not locked
    REJECT(memset(s.mcu_key_sha256, 0, 32));                                    // key named, no digest
    REJECT(memset(s.secure_boot_keys, 0, 32));                                  // Secure Boot, no keys
    REJECT(memset(s.attestation, 0, 32));                                       // production unattested
    // An open board is never locked and holds no key.
    nvf_commission_statement_t open = good;
    open.profile = NVF_PROFILE_OPEN; open.mcu_key_alg = NVF_MCU_KEY_NONE; open.security = 0;
    memset(open.mcu_key_sha256, 0, 32); memset(open.secure_boot_keys, 0, 32);
    assert(!nvf_commission_validate(&open));
    s = open; s.security = NVF_SEC_SECURE_BOOT; memset(s.secure_boot_keys, 1, 32); assert(nvf_commission_validate(&s));
    s = open; s.mcu_key_alg = NVF_MCU_KEY_RSA3072_PSS; memset(s.mcu_key_sha256, 1, 32); assert(nvf_commission_validate(&s));
    s = open; s.security = NVF_SEC_MCU_KEY_PROTECTED; assert(nvf_commission_validate(&s));
    // Only a test board may be unattested and keyless.
    s = open; s.profile = NVF_PROFILE_TEST; memset(s.attestation, 0, 32); assert(!nvf_commission_validate(&s));
    // The recorded RTC is absent, a model alone, or an MCP79412 with its factory EUI-64.
    s = good; s.identity_flags = 0; s.rtc_model_id = NVF_RTC_NONE; memset(s.rtc_eui64, 0, 8);
    assert(!nvf_commission_validate(&s));
    s.identity_flags = NVF_IDENTITY_RTC_PRESENT; s.rtc_model_id = NVF_RTC_MCP79412;
    assert(!nvf_commission_validate(&s));
    s.rtc_model_id = NVF_RTC_DS3231;
    assert(!nvf_commission_validate(&s));
    s.rtc_model_id = NVF_RTC_MAX31328;
    assert(!nvf_commission_validate(&s));
    s.identity_flags |= NVF_IDENTITY_RTC_EUI_RECORDED;
    memcpy(s.rtc_eui64, good.rtc_eui64, 8);
    assert(nvf_commission_validate(&s)); /* MAX31328 has no factory EUI. */
    s.rtc_model_id = NVF_RTC_DS3231;
    assert(nvf_commission_validate(&s)); /* nor does DS3231. */
    s.rtc_model_id = NVF_RTC_MAX31328 + 1; s.identity_flags = NVF_IDENTITY_RTC_PRESENT; memset(s.rtc_eui64, 0, 8);
    assert(nvf_commission_validate(&s));
    s.rtc_model_id = NVF_RTC_DS3231;
    s.identity_flags = NVF_IDENTITY_RTC_PRESENT;
    memset(s.rtc_eui64, 0, 8);
    assert(!nvf_commission_validate(&s));
    s.rtc_eui64[7] = 1; assert(nvf_commission_validate(&s));
#undef REJECT

    for (size_t n = 0; n < sizeof statement; n++) assert(!nvf_commission_parse(statement, n, &s));
    uint8_t longer[NVF_COMMISSION_STATEMENT_SIZE + 1] = {0};
    memcpy(longer, statement, sizeof statement);
    assert(!nvf_commission_parse(longer, sizeof longer, &s));
    uint8_t version[NVF_COMMISSION_STATEMENT_SIZE];
    memcpy(version, statement, sizeof version); version[0] = 2;
    assert(!nvf_commission_parse(version, sizeof version, &s));
    memcpy(version, statement, sizeof version); version[9] |= 0x20; // reserved security bit on the wire
    assert(!nvf_commission_parse(version, sizeof version, &s));
    memcpy(version, statement, sizeof version); version[11] |= 0x20; // reserved identity flag on the wire
    assert(!nvf_commission_parse(version, sizeof version, &s));
}

static void test_match(void)
{
    nvf_commission_statement_t s;
    assert(nvf_commission_parse(statement, sizeof statement, &s));
    nvf_live_identity_t live = {
        .atecc_valid = true, .board_valid = true, .revision_valid = true, .mac_valid = true,
        .attestation_valid = true,
        .key_valid = true, .security_valid = true, .secure_boot_keys_valid = true,
        .board_rev = s.board_rev, .security = s.security,
    };
    memcpy(live.atecc_serial, s.atecc_serial, 9);
    memcpy(live.board_uid, s.board_uid, NVF_BOARD_UID_SIZE); memcpy(live.mcu_mac, s.mcu_mac, 6);
    memcpy(live.mcu_key_sha256, s.mcu_key_sha256, 32);
    memcpy(live.secure_boot_keys, s.secure_boot_keys, 32);
    memset(live.attestation_record, 0x5a, sizeof live.attestation_record);
    assert(ref_sha256(live.attestation_record, sizeof live.attestation_record, s.attestation));
    assert(!nvf_commission_match(&s, &live, ref_sha256));
    nvf_live_identity_t x;
#define MISMATCH(change) do { x = live; change; assert(nvf_commission_match(&s, &x, ref_sha256)); } while (0)
    MISMATCH(x.atecc_serial[0] ^= 1); MISMATCH(x.board_uid[3] ^= 1);
    MISMATCH(x.mcu_mac[5] ^= 1);      MISMATCH(x.mcu_key_sha256[31] ^= 1);
    MISMATCH(x.atecc_valid = false);
    MISMATCH(x.board_valid = false);  MISMATCH(x.revision_valid = false); MISMATCH(x.board_rev++);
    MISMATCH(x.mac_valid = false);    MISMATCH(x.key_valid = false);
    MISMATCH(x.security ^= NVF_SEC_JTAG_DISABLED); MISMATCH(x.secure_boot_keys[0] ^= 1);
    MISMATCH(x.attestation_record[0] ^= 1); MISMATCH(x.attestation_valid = false);
#undef MISMATCH
    // The recorded RTC describes the part fitted at commissioning. A replaced, removed or
    // different RTC leaves the board's identity, and so the record, unchanged.
    nvf_commission_statement_t described = s;
    described.identity_flags = NVF_IDENTITY_RTC_PRESENT; described.rtc_model_id = NVF_RTC_MAX31328;
    memset(described.rtc_eui64, 0, 8);
    assert(!nvf_commission_validate(&described) && !nvf_commission_match(&described, &live, ref_sha256));
    described.identity_flags = 0; described.rtc_model_id = NVF_RTC_NONE;
    assert(!nvf_commission_validate(&described) && !nvf_commission_match(&described, &live, ref_sha256));
    // The manufacturer key signs for other product lines; their records are not this board's.
    s.product = 2; assert(!nvf_commission_validate(&s) && nvf_commission_match(&s, &live, ref_sha256)); s.product = NVF_COMMISSION_PRODUCT_OBSERVER;
    // A record that names no key does not care whether the chip holds one.
    s.profile = NVF_PROFILE_TEST; s.mcu_key_alg = NVF_MCU_KEY_NONE; s.security = 0;
    memset(s.mcu_key_sha256, 0, 32); memset(s.secure_boot_keys, 0, 32);
    x = live; x.key_valid = false; x.security = 0; x.secure_boot_keys_valid = false;
    assert(!nvf_commission_validate(&s) && !nvf_commission_match(&s, &x, ref_sha256));
}

static void test_security_bits_and_keygen_rules(void)
{
    // Bit 4 is the claim a trusted statement rests on, and it needs all three inputs. A
    // keyed chip whose read-protection field is still writable must not report it.
    nvf_chip_security_t chip = { .secure_boot = true, .flash_enc_release = true, .jtag_disabled = true,
        .secure_download = true, .key_ready = true, .key_block_protected = true, .rd_dis_sealed = true };
    assert(nvf_commission_security_bits(&chip) == NVF_SEC_TRUSTED);
    nvf_chip_security_t x;
    x = chip; x.rd_dis_sealed = false; assert(nvf_commission_security_bits(&x) == 0x000f);
    x = chip; x.key_ready = false; assert(nvf_commission_security_bits(&x) == 0x000f);
    x = chip; x.key_block_protected = false; assert(nvf_commission_security_bits(&x) == 0x000f);
    x = chip; x.flash_enc_release = false; assert(nvf_commission_security_bits(&x) == (NVF_SEC_TRUSTED & ~NVF_SEC_FLASH_ENC_RELEASE));
    x = (nvf_chip_security_t){ .rd_dis_sealed = true }; assert(nvf_commission_security_bits(&x) == 0); // sealed alone is no key
    // ...and so a trusted statement cannot be prepared for the unsealed chip.
    nvf_commission_statement_t s;
    assert(nvf_commission_parse(statement, sizeof statement, &s));
    x = chip; x.rd_dis_sealed = false; s.security = nvf_commission_security_bits(&x);
    assert(nvf_commission_validate(&s));

    // Key generation: every refusal precedes any burn; a confirmed fault leaves a retry open.
    nvf_mcu_keygen_state_t fresh = { .locked = true, .free_block = true }, k;
    assert(!nvf_mcu_keygen_refusal(&fresh));
    k = fresh; k.locked = false; assert(nvf_mcu_keygen_refusal(&k));
    k.unlocked_allowed = true; assert(!nvf_mcu_keygen_refusal(&k));
    k = fresh; k.key_ready = true; assert(nvf_mcu_keygen_refusal(&k));
    k = fresh; k.key_orphaned = true; assert(nvf_mcu_keygen_refusal(&k));
    k = fresh; k.fault_unresolved = true; assert(nvf_mcu_keygen_refusal(&k));
    k = fresh; k.rd_dis_sealed = true; assert(nvf_mcu_keygen_refusal(&k));   // sealed with no key: nothing can be protected
    k = fresh; k.free_block = false; assert(nvf_mcu_keygen_refusal(&k));
    k = fresh; k.key_ready = true; k.rd_dis_sealed = true; assert(nvf_mcu_keygen_refusal(&k));
}

static void test_pss(void)
{
    // 1. The fixture proof, opened with the fixture public key, is an encoding the reference
    //    verifier accepts for the fixture proof digest — so the verifier agrees with Go.
    const uint8_t *modulus = ref_spki_modulus(key, key_len);
    assert(modulus && proof_len == NVF_MCU_KEY_BYTES);
    uint8_t em[NVF_MCU_KEY_BYTES];
    assert(ref_rsa_public(modulus, proof, em));
    assert(ref_pss_verify(proof_digest, em, sizeof em));
    uint8_t wrong[32]; memcpy(wrong, proof_digest, 32); wrong[5] ^= 1;
    assert(!ref_pss_verify(wrong, em, sizeof em));

    // 2. This encoder produces encodings that same verifier accepts.
    uint8_t salt[NVF_MCU_PSS_SALT];
    for (unsigned i = 0; i < sizeof salt; i++) salt[i] = (uint8_t)(0xa0 + i);
    memset(em, 0xee, sizeof em);
    assert(nvf_pss_encode(proof_digest, salt, ref_sha256, em, sizeof em));
    assert(!(em[0] & 0x80) && em[sizeof em - 1] == 0xbc);
    assert(ref_pss_verify(proof_digest, em, sizeof em) && !ref_pss_verify(wrong, em, sizeof em));
    uint8_t other[NVF_MCU_KEY_BYTES]; salt[0] ^= 1;
    assert(nvf_pss_encode(proof_digest, salt, ref_sha256, other, sizeof other) && memcmp(em, other, sizeof em));
    assert(ref_pss_verify(proof_digest, other, sizeof other));
    em[100] ^= 1; assert(!ref_pss_verify(proof_digest, em, sizeof em));
    assert(!nvf_pss_encode(proof_digest, salt, ref_sha256, em, 32 + NVF_MCU_PSS_SALT + 1)); // no room for the salt
}

int main(void)
{
    const char *variants[] = {"trusted-no-rtc", "open-no-rtc", "test-no-rtc", "mcp79412-model", "ds3231-model",
                              "max31328-model"};
    for (unsigned i = 0; i < sizeof variants / sizeof variants[0]; i++) {
        char name[64];
        uint8_t bytes[NVF_COMMISSION_STATEMENT_SIZE], expected[32], actual[32];
        snprintf(name, sizeof name, "%s-statement", variants[i]);
        assert(fixture(name, bytes, sizeof bytes) == sizeof bytes);
        nvf_commission_statement_t parsed;
        assert(nvf_commission_parse(bytes, sizeof bytes, &parsed));
        assert(parsed.identity_flags == (i < 3 ? 0 : NVF_IDENTITY_RTC_PRESENT));
        assert(parsed.rtc_model_id == (i < 3 ? 0 : i - 2));
        snprintf(name, sizeof name, "%s-digest", variants[i]);
        assert(fixture(name, expected, sizeof expected) == sizeof expected);
        assert(nvf_commission_digest(bytes, ref_sha256, actual));
        assert(!memcmp(expected, actual, 32));
    }
    assert(fixture("statement", statement, sizeof statement) == sizeof statement);
    assert(fixture("record", record, sizeof record) == sizeof record);
    assert(fixture("digest", digest, 32) == 32 && fixture("exported", exported, 32) == 32);
    assert(fixture("proof-digest", proof_digest, 32) == 32);
    key_len = fixture("mcu-key", key, sizeof key);
    proof_len = fixture("proof", proof, sizeof proof);
    evidence_len = fixture("evidence", evidence, sizeof evidence);
    assert(key_len && proof_len && evidence_len);

    test_sha256_reference();
    test_statement_round_trip();
    test_record_and_proof_digest();
    test_evidence_encoding();
    test_validation();
    test_match();
    test_security_bits_and_keygen_rules();
    test_pss();
    puts("Commissioning statement, record, proof digest, evidence and EMSA-PSS match the shared fixtures");
    return 0;
}
