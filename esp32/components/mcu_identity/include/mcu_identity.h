// mcu_identity — the microcontroller's own key, the commissioning record it was issued, and
// the evidence it presents to a collector (docs/COMMISSIONING.md, esp32/docs/COMMISSIONING.md).
//
// The key is a 3072-bit RSA key whose private half exists only as ciphertext for the
// ESP32-S3 Digital Signature peripheral, under an HMAC key in a read-protected eFuse block.
// Generating it burns that block and then seals eFuse read protection: it happens at the
// bench, on a locked chip. Everything here except nvf_mcu_identity_provision() and
// nvf_mcu_identity_seal() is repeatable.

#pragma once
#include "esp_err.h"
#include "mcu_identity_core.h"

#ifdef __cplusplus
extern "C" {
#endif

typedef enum {
    NVF_MCU_KEY_ABSENT,   // no key: nothing stored and no Digital Signature key block burned
    NVF_MCU_KEY_ORPHANED, // a key block is burned but its ciphertext is missing: restore it
    NVF_MCU_KEY_READY,    // burned, protected, and a signature through the peripheral verified
    NVF_MCU_KEY_FAULT,    // burned, but protection is incomplete or the self-test failed: the
                          // block is spent; another free block may be tried while unsealed
} nvf_mcu_key_state_t;

// The exported Digital Signature context: "NDS\1" followed by the peripheral's ciphertext
// structure. It is useless without the eFuse key of the chip that made it, so the factory
// keeps a copy and can return it to a unit whose flash was wiped.
#define NVF_MCU_DS_CONTEXT_MAX 1700

typedef struct {
    nvf_mcu_key_state_t key;
    int key_block;          // eFuse key block 0..5, or -1
    bool rd_dis_sealed;     // no further eFuse block can be read-protected
    uint16_t security;      // §4 security bits now; bit 4 only for a ready, protected AND sealed key
    bool record;            // a commissioning record is installed
    uint8_t record_profile; // NVF_PROFILE_*, 0 without a record
    uint32_t record_generation;
    // The collector's most recent verdict on this boot, both empty before the first WELCOME.
    char hardware_trust[16], evidence_error[24];
} nvf_mcu_identity_status_t;

// Call once, after nvf_update_start() has initialised the update_meta partition. Loads the
// stored state and finishes or discards a key generation that a reset interrupted.
esp_err_t nvf_mcu_identity_start(void);
void nvf_mcu_identity_status(nvf_mcu_identity_status_t *out);

uint16_t nvf_mcu_identity_security(void);
// SHA-256 over the three Secure Boot key digests in slot order. False, and zeros, when
// Secure Boot is off.
bool nvf_mcu_identity_secure_boot_keys(uint8_t out[32]);

// Each returns 0 when there is nothing to copy or the buffer is too small.
size_t nvf_mcu_identity_public_key(uint8_t *out, size_t cap); // DER SubjectPublicKeyInfo
size_t nvf_mcu_identity_ds_context(uint8_t *out, size_t cap);
size_t nvf_mcu_identity_record(uint8_t out[NVF_COMMISSION_RECORD_SIZE]);
bool nvf_mcu_identity_key_sha256(uint8_t out[32]);

// IRREVERSIBLE: generates the key, burns one eFuse key block and, only once that block is
// protected and a signature through the peripheral has verified, write-protects the eFuse
// read-protection field — the state the bootloader would have left had it not been asked to
// keep read protection available for this key. Sealing cannot be skipped: until it holds the
// chip does not report the key's security bit, so no trusted statement can describe it. A
// block that fails is recorded as a fault and nothing is sealed, so another free block can be
// tried. Refusals (nvf_mcu_keygen_refusal) all precede any burn. Not compiled into
// open-profile builds. *reason receives a static explanation on failure.
esp_err_t nvf_mcu_identity_provision(const char **reason);
// IRREVERSIBLE: the seal alone, for a board that lost power between the self-test and the
// seal. Refused unless a committed key exists and passes its self-test now. Not compiled into
// open-profile builds.
esp_err_t nvf_mcu_identity_seal(const char **reason);
// Returns the ciphertext and public key to a chip whose key block is already burned. Nothing
// is stored unless a signature made with the supplied context verifies under the supplied key.
esp_err_t nvf_mcu_identity_restore(const uint8_t *context, size_t context_len,
                                   const uint8_t *spki, size_t spki_len, const char **reason);
// Stores a commissioning record after checking that it describes this board and this chip.
// The manufacturer signature is not checked here: the device holds no trust root for it, and
// the collector verifies it on every session. live->key_* are filled in by this call.
esp_err_t nvf_mcu_identity_install(const uint8_t *record, size_t len, nvf_live_identity_t *live, const char **reason);

// pusher_cfg_t.evidence: builds the EVIDENCE payload for one TLS session. exported is that
// session's keying material, or NULL when it could not be derived. Returns 0 to present
// nothing: no record, or a trusted record whose proof could not be made.
size_t nvf_mcu_identity_evidence(const uint8_t *exported, uint8_t *out, size_t cap,
                                 nvf_live_identity_t *live);
// pusher_cfg_t.hardware_trust: the collector's verdict from WELCOME. Either may be NULL.
void nvf_mcu_identity_verdict(const char *trust, const char *error);

#ifdef __cplusplus
}
#endif
