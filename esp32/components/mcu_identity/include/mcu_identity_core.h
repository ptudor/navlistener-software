// mcu_identity_core — the portable half of the commissioning evidence.
//
// docs/COMMISSIONING.md is normative for every layout here: the 153-byte statement, the
// 225-byte record, the session proof digest and the GNF1 EVIDENCE payload. The Go
// reference is go/internal/commissioning; both are pinned to common/fixtures/commissioning-*.
//
// Pure buffer code: no ESP-IDF, no allocation, no I/O. SHA-256 is injected so the same source
// runs against mbedTLS on the device and a reference implementation in the host tests.

#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define NVF_COMMISSION_VERSION        0x01
#define NVF_COMMISSION_STATEMENT_SIZE 153
#define NVF_COMMISSION_KEY_ID_SIZE    8
#define NVF_COMMISSION_SIGNATURE_SIZE 64
#define NVF_COMMISSION_RECORD_SIZE    (NVF_COMMISSION_STATEMENT_SIZE + NVF_COMMISSION_KEY_ID_SIZE + NVF_COMMISSION_SIGNATURE_SIZE)

#define NVF_EVIDENCE_VERSION 0x01
#define NVF_EVIDENCE_MAX     2048 // the collector reads no more than this
#define NVF_MCU_KEY_DER_MAX  1024
#define NVF_MCU_PROOF_MAX    512

// Length of the TLS keying material a session proof signs. The exporter label lives with
// the wire contract: GNF1_EVIDENCE_EXPORTER_LABEL in gnf1.h.
#define NVF_MCU_EXPORTED_SIZE 32

// The one microcontroller key algorithm: RSA-3072, e = 65537, RSASSA-PSS with SHA-256,
// MGF1-SHA-256 and a 32-byte salt.
#define NVF_MCU_KEY_BITS  3072
#define NVF_MCU_KEY_BYTES (NVF_MCU_KEY_BITS / 8)
#define NVF_MCU_PSS_SALT  32

enum { NVF_PROFILE_TRUSTED = 1, NVF_PROFILE_OPEN = 2, NVF_PROFILE_TEST = 3 };
enum { NVF_MCU_ESP32S3 = 1 };
// One manufacturer key signs for every product line; this firmware is the GNSS observer's.
enum { NVF_COMMISSION_PRODUCT_OBSERVER = 1 };
enum { NVF_MCU_KEY_NONE = 0, NVF_MCU_KEY_RSA3072_PSS = 1 };
enum {
    NVF_IDENTITY_RTC_EUI_BOUND = 1u << 0,
    NVF_IDENTITY_RTC_PRESENT   = 1u << 1,
    NVF_IDENTITY_KNOWN         = 0x0003,
};
enum { NVF_RTC_NONE = 0, NVF_RTC_MCP79412 = 1 };
enum {
    NVF_SEC_SECURE_BOOT       = 1u << 0, // Secure Boot v2 enabled
    NVF_SEC_FLASH_ENC_RELEASE = 1u << 1, // flash encryption in release mode
    NVF_SEC_JTAG_DISABLED     = 1u << 2, // pad JTAG and USB JTAG permanently disabled
    NVF_SEC_SECURE_DOWNLOAD   = 1u << 3, // ROM download mode secure, or disabled
    NVF_SEC_MCU_KEY_PROTECTED = 1u << 4, // key block burned and protected, AND read protection sealed
    NVF_SEC_TRUSTED           = 0x001f,  // a trusted statement carries exactly these five
};

typedef struct {
    uint8_t profile, mcu_family, mcu_key_alg;
    uint16_t product, board_rev, security; // product is never zero
    uint16_t identity_flags, rtc_model_id;
    uint32_t generation;      // 1 at first commissioning; +1 each time the board is commissioned again
    uint64_t commissioned_at; // Unix seconds, UTC
    uint8_t board_eui64[8], atecc_serial[9], rtc_eui64[8];
    uint8_t mcu_mac[6];       // factory base MAC: a label, never a proof
    uint8_t mcu_key_sha256[32], secure_boot_keys[32], attestation[32];
} nvf_commission_statement_t;

// One-shot SHA-256. Returns false only when the platform digest fails.
typedef bool (*nvf_sha256_fn)(const void *data, size_t size, uint8_t out[32]);

// nvf_commission_validate applies the consistency rules every signer and verifier shares
// (§4). Returns NULL for a valid statement, otherwise a short reason for the log.
const char *nvf_commission_validate(const nvf_commission_statement_t *s);

// Encode refuses an invalid statement; parse refuses a wrong length, a wrong version and an
// invalid statement. Both therefore round-trip exactly the statements a verifier accepts.
bool nvf_commission_encode(const nvf_commission_statement_t *s, uint8_t out[NVF_COMMISSION_STATEMENT_SIZE]);
bool nvf_commission_parse(const uint8_t *in, size_t len, nvf_commission_statement_t *out);

// SHA-256("MFG-COMMISSION-v1" || statement): the value the manufacturer key signs.
bool nvf_commission_digest(const uint8_t statement[NVF_COMMISSION_STATEMENT_SIZE], nvf_sha256_fn sha, uint8_t out[32]);

// A record is statement || signer key id || R || S. Parsing checks the shape and the embedded
// statement only: the device never verifies the manufacturer signature, the collector does.
bool nvf_commission_record_parse(const uint8_t *record, size_t len, nvf_commission_statement_t *out);
bool nvf_commission_fingerprint(const uint8_t record[NVF_COMMISSION_RECORD_SIZE], nvf_sha256_fn sha, uint8_t out[32]);

// SHA-256("NAVL-MCU-PROOF-v1" || exported || record fingerprint): what the microcontroller
// key signs for one TLS session.
bool nvf_mcu_proof_digest(const uint8_t exported[NVF_MCU_EXPORTED_SIZE], const uint8_t record[NVF_COMMISSION_RECORD_SIZE],
                          nvf_sha256_fn sha, uint8_t out[32]);

// nvf_evidence_encode writes [version][2B len][record][2B len][key][2B len][proof]. The key
// and the proof are present together or absent together. Returns the payload length, or 0
// when an argument breaks a limit or the payload does not fit.
size_t nvf_evidence_encode(uint8_t *out, size_t cap, const uint8_t record[NVF_COMMISSION_RECORD_SIZE],
                           const uint8_t *key, size_t key_len, const uint8_t *proof, size_t proof_len);

// nvf_pss_encode is EMSA-PSS-ENCODE (RFC 8017 §9.1.1) for SHA-256/MGF1-SHA-256 with a caller
// supplied 32-byte salt. em_len is the modulus length in bytes; the encoding is over
// 8*em_len-1 bits, which is what a modulus of exactly 8*em_len bits requires.
bool nvf_pss_encode(const uint8_t digest[32], const uint8_t salt[NVF_MCU_PSS_SALT], nvf_sha256_fn sha, uint8_t *em, size_t em_len);

// What the eFuses and the key state say about this chip, and the §4 security bits they
// amount to. Bit 4 needs all three of its inputs: a key that passed its self-test, a key
// block that is read- and write-protected, and a sealed read-protection field. A keyed but
// unsealed chip reports bit 4 clear, so no trusted statement can be prepared for it.
typedef struct {
    bool secure_boot, flash_enc_release, jtag_disabled, secure_download;
    bool key_ready, key_block_protected, rd_dis_sealed;
} nvf_chip_security_t;
uint16_t nvf_commission_security_bits(const nvf_chip_security_t *chip);

// Whether a microcontroller key may be generated now. Every refusal comes before anything
// is burned. A confirmed fault (a key block that was burned but failed its protection check
// or its self-test) does not end the matter: while the read-protection field is unsealed and
// a key block is free, another may be tried. Returns NULL to proceed, else the reason.
typedef struct {
    bool locked;           // Secure Boot enabled and flash encryption in release mode
    bool unlocked_allowed; // a test-profile build that opted out of the lock requirement
    bool key_ready;        // a key already passed its self-test
    bool key_orphaned;     // a key block is burned and its ciphertext is not stored
    bool fault_unresolved; // a burned block whose self-test has not completed this boot
    bool rd_dis_sealed;    // no further key block can be read-protected
    bool free_block;       // an unused eFuse key block exists
} nvf_mcu_keygen_state_t;
const char *nvf_mcu_keygen_refusal(const nvf_mcu_keygen_state_t *state);

// The canonical observer id for a board EUI-64: lowercase hyphen-separated byte pairs.
void nvf_commission_observer_id(const uint8_t eui[8], char out[24]);

// What the firmware read from the board it is running on. An identifier that could not be
// read is marked invalid, never guessed.
typedef struct {
    bool atecc_valid, board_valid, revision_valid, mac_valid, attestation_valid;
    bool rtc_expected, rtc_present, rtc_valid;
    bool key_valid, security_valid, secure_boot_keys_valid;
    uint16_t board_rev, rtc_model_id, security;
    uint8_t board_eui64[8], atecc_serial[9], rtc_eui64[8], mcu_mac[6];
    uint8_t attestation_record[72], mcu_key_sha256[32], secure_boot_keys[32];
} nvf_live_identity_t;

// nvf_commission_match decides whether a record belongs to this board: it must be an
// observer's, every identifier the statement names must have been read and must be equal,
// and a statement that names a microcontroller key must name the key this chip holds.
// Returns NULL on a match.
const char *nvf_commission_match(const nvf_commission_statement_t *s, const nvf_live_identity_t *live,
                                 nvf_sha256_fn sha);

#ifdef __cplusplus
}
#endif
