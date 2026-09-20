// mcu_identity — see include/mcu_identity.h and esp32/docs/COMMISSIONING.md.
//
// Order of operations when a key is made, and why:
//   1. generate the RSA key and the HMAC key in internal RAM, from a seeded DRBG;
//   2. encrypt the private parameters for the Digital Signature peripheral;
//   3. STORE the ciphertext and the public key, marked staged;
//   4. burn the HMAC key into a free eFuse block (read- and write-protected);
//   5. sign through the peripheral, verify under the public key, mark ready;
//   6. only then seal the eFuse read-protection field, so no later block can be protected.
// A reset before 4 leaves ciphertext nobody can use and no eFuse changed: start() discards it
// and the bench retries. A reset after 4 leaves a burned key whose ciphertext is already
// stored: start() runs step 5, and `commission seal` finishes step 6. A burned key never
// exists without its ciphertext. A block that fails 4's protection check or 5 is recorded as
// a fault and the field stays unsealed, so another free block can still be tried. The §4
// security bit for the key is reported only once all of 4, 5 and 6 hold.

#include "mcu_identity.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "sdkconfig.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "bootloader_random.h"
#include "esp_ds.h"
#include "esp_efuse.h"
#include "esp_efuse_table.h"
#include "esp_flash_encrypt.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_random.h"
#include "esp_secure_boot.h"
#include "esp_wifi.h"
#include "mbedtls/ctr_drbg.h"
#include "mbedtls/entropy.h"
#include "mbedtls/pk.h"
#include "mbedtls/platform_util.h"
#include "mbedtls/rsa.h"
#include "mbedtls/sha256.h"
#include "nvs.h"
#include "nvs_flash.h"
#include "journal.h"

#ifndef CONFIG_NVF_UPDATE_TEST_KEYS
#define CONFIG_NVF_UPDATE_TEST_KEYS 0
#endif
#ifndef CONFIG_NVF_UPDATE_PROFILE_OPEN
#define CONFIG_NVF_UPDATE_PROFILE_OPEN 0
#endif
#ifndef CONFIG_NVF_MCU_KEY_UNLOCKED_TEST
#define CONFIG_NVF_MCU_KEY_UNLOCKED_TEST 0
#endif
#ifndef CONFIG_NVS_ENCRYPTION
#define CONFIG_NVS_ENCRYPTION 0
#endif

static const char *TAG = "mcu_identity";
static const char PARTITION[] = "update_meta", NAMESPACE[] = "hwtrust";
static const uint8_t CONTEXT_MAGIC[4] = { 'N', 'D', 'S', 1 };
#define CONTEXT_SIZE (sizeof CONTEXT_MAGIC + sizeof(esp_ds_data_t))
_Static_assert(CONTEXT_SIZE <= NVF_MCU_DS_CONTEXT_MAX, "exported Digital Signature context exceeds its bound");
#define DS_PURPOSE ESP_EFUSE_KEY_PURPOSE_HMAC_DOWN_DIGITAL_SIGNATURE
enum { STORED_NONE = 0, STORED_STAGED = 1, STORED_READY = 2, STORED_FAULT = 3 };

static SemaphoreHandle_t lock;
static nvs_handle_t storage;
static bool storage_ready;
static nvf_mcu_key_state_t key_state = NVF_MCU_KEY_ABSENT;
static int key_block = -1;
static esp_ds_data_t *ds_data;              // internal RAM; NULL without a usable context
static uint8_t spki[NVF_MCU_KEY_DER_MAX];
static size_t spki_len;
static uint8_t record[NVF_COMMISSION_RECORD_SIZE];
static bool record_present;
static uint8_t faulted;                     // key blocks recorded as burned and unusable
static char verdict_trust[16], verdict_error[24];
static uint32_t journaled_verdict = UINT32_MAX;

static bool sha256(const void *data, size_t size, uint8_t out[32]) { return mbedtls_sha256(data, size, out, 0) == 0; }

// --- eFuse state -------------------------------------------------------------------------

static bool block_protected(esp_efuse_block_t block)
{
    return esp_efuse_get_key_purpose(block) == DS_PURPOSE && esp_efuse_get_key_dis_read(block) &&
           esp_efuse_get_key_dis_write(block) && esp_efuse_get_keypurpose_dis_write(block);
}

static bool sealed(void) { return esp_efuse_read_field_bit(ESP_EFUSE_WR_DIS_RD_DIS); }

uint16_t nvf_mcu_identity_security(void)
{
    nvf_chip_security_t chip = {
        .secure_boot = esp_secure_boot_enabled(),
        .flash_enc_release = esp_get_flash_encryption_mode() == ESP_FLASH_ENC_MODE_RELEASE,
        .jtag_disabled = esp_efuse_read_field_bit(ESP_EFUSE_DIS_PAD_JTAG) && esp_efuse_read_field_bit(ESP_EFUSE_DIS_USB_JTAG),
        .secure_download = esp_efuse_read_field_bit(ESP_EFUSE_ENABLE_SECURITY_DOWNLOAD) || esp_efuse_read_field_bit(ESP_EFUSE_DIS_DOWNLOAD_MODE),
        .rd_dis_sealed = sealed(),
    };
    if (lock) {
        xSemaphoreTake(lock, portMAX_DELAY);
        chip.key_ready = key_state == NVF_MCU_KEY_READY;
        chip.key_block_protected = key_block >= 0 && block_protected(EFUSE_BLK_KEY0 + key_block);
        xSemaphoreGive(lock);
    }
    return nvf_commission_security_bits(&chip);
}

bool nvf_mcu_identity_secure_boot_keys(uint8_t out[32])
{
    memset(out, 0, 32);
    if (!esp_secure_boot_enabled()) return false;
    uint8_t digests[3][32] = {{0}};
    for (unsigned i = 0; i < 3; i++) {
        esp_efuse_block_t block;
        if (esp_efuse_get_digest_revoke(i) ||
            !esp_efuse_find_purpose(ESP_EFUSE_KEY_PURPOSE_SECURE_BOOT_DIGEST0 + i, &block)) continue;
        if (esp_efuse_read_block(block, digests[i], 0, 256) != ESP_OK) return false;
    }
    return sha256(digests, sizeof digests, out);
}

// --- signing through the peripheral ------------------------------------------------------

// The peripheral takes and returns the operand as little-endian words; PKCS#1 encodings are
// big-endian byte strings. Reversing the bytes converts one to the other on this CPU.
static void reverse(const uint8_t *in, uint8_t *out, size_t n) { for (size_t i = 0; i < n; i++) out[i] = in[n - 1 - i]; }

static esp_err_t ds_sign(const esp_ds_data_t *data, int block, const uint8_t digest[32], uint8_t signature[NVF_MCU_KEY_BYTES])
{
    uint8_t salt[NVF_MCU_PSS_SALT], em[NVF_MCU_KEY_BYTES];
    esp_fill_random(salt, sizeof salt);
    if (!nvf_pss_encode(digest, salt, sha256, em, sizeof em)) return ESP_FAIL;
    uint32_t *words = heap_caps_malloc(2 * NVF_MCU_KEY_BYTES, MALLOC_CAP_INTERNAL | MALLOC_CAP_32BIT);
    if (!words) return ESP_ERR_NO_MEM;
    uint8_t *message = (uint8_t *)words, *result = message + NVF_MCU_KEY_BYTES;
    reverse(em, message, NVF_MCU_KEY_BYTES);
    esp_err_t err = esp_ds_sign(message, data, (hmac_key_id_t)block, result);
    if (err == ESP_OK) reverse(result, signature, NVF_MCU_KEY_BYTES);
    heap_caps_free(words);
    return err;
}

static bool public_key_ok(mbedtls_pk_context *pk, const uint8_t *der, size_t len)
{
    if (mbedtls_pk_parse_public_key(pk, der, len) != 0 || mbedtls_pk_get_type(pk) != MBEDTLS_PK_RSA) return false;
    mbedtls_rsa_context *rsa = mbedtls_pk_rsa(*pk);
    mbedtls_mpi e;
    mbedtls_mpi_init(&e);
    bool ok = mbedtls_rsa_get_len(rsa) == NVF_MCU_KEY_BYTES && mbedtls_pk_get_bitlen(pk) == NVF_MCU_KEY_BITS &&
              mbedtls_rsa_export(rsa, NULL, NULL, NULL, NULL, &e) == 0 && mbedtls_mpi_cmp_int(&e, 65537) == 0;
    mbedtls_mpi_free(&e);
    return ok;
}

// self_test proves the stored ciphertext, the burned eFuse key and the public key belong
// together: a signature made by the peripheral must verify under the public key, with an
// independent verifier (mbedTLS) and the exact parameters the collector uses.
static esp_err_t self_test(const esp_ds_data_t *data, int block, const uint8_t *der, size_t der_len)
{
    static const char message[] = "navlistener microcontroller key self-test";
    uint8_t digest[32], *signature = malloc(NVF_MCU_KEY_BYTES);
    if (!signature) return ESP_ERR_NO_MEM;
    mbedtls_pk_context pk;
    mbedtls_pk_init(&pk);
    esp_err_t err = ESP_FAIL;
    if (sha256(message, sizeof message - 1, digest) && public_key_ok(&pk, der, der_len) &&
        (err = ds_sign(data, block, digest, signature)) == ESP_OK) {
        mbedtls_rsa_context *rsa = mbedtls_pk_rsa(pk);
        mbedtls_rsa_set_padding(rsa, MBEDTLS_RSA_PKCS_V21, MBEDTLS_MD_SHA256);
        err = mbedtls_rsa_rsassa_pss_verify_ext(rsa, MBEDTLS_MD_SHA256, sizeof digest, digest,
                                                MBEDTLS_MD_SHA256, NVF_MCU_PSS_SALT, signature) == 0 ? ESP_OK : ESP_ERR_INVALID_CRC;
    }
    mbedtls_pk_free(&pk);
    free(signature);
    return err;
}

// --- storage -----------------------------------------------------------------------------

static esp_err_t store_key(const esp_ds_data_t *data, const uint8_t *der, size_t der_len, int block, uint8_t state)
{
    uint8_t *context = malloc(CONTEXT_SIZE);
    if (!context) return ESP_ERR_NO_MEM;
    memcpy(context, CONTEXT_MAGIC, sizeof CONTEXT_MAGIC);
    memcpy(context + sizeof CONTEXT_MAGIC, data, sizeof *data);
    esp_err_t err = nvs_set_blob(storage, "ds_ctx", context, CONTEXT_SIZE);
    free(context);
    if (err == ESP_OK) err = nvs_set_blob(storage, "spki", der, der_len);
    if (err == ESP_OK) err = nvs_set_u8(storage, "block", (uint8_t)block);
    if (err == ESP_OK) err = nvs_set_u8(storage, "state", state);
    return err == ESP_OK ? nvs_commit(storage) : err;
}
static esp_err_t store_state(uint8_t state)
{
    esp_err_t err = nvs_set_u8(storage, "state", state);
    return err == ESP_OK ? nvs_commit(storage) : err;
}
static void discard_key(void)
{
    (void)nvs_erase_key(storage, "ds_ctx"); (void)nvs_erase_key(storage, "spki");
    (void)nvs_erase_key(storage, "block"); (void)nvs_erase_key(storage, "state");
    (void)nvs_commit(storage);
}
static void adopt(esp_ds_data_t *data, const uint8_t *der, size_t der_len, int block, nvf_mcu_key_state_t state)
{
    if (ds_data && ds_data != data) heap_caps_free(ds_data);
    ds_data = data; key_block = block; key_state = state;
    if (der != spki) memcpy(spki, der, der_len);
    spki_len = der_len;
}
static esp_ds_data_t *context_alloc(void) { return heap_caps_calloc(1, sizeof(esp_ds_data_t), MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT); }

// A recorded fault outlives discard_key(): it is what tells a later boot that a burned
// Digital Signature block is a known reject and not a key waiting for its ciphertext.
static void record_fault(int block)
{
    faulted |= (uint8_t)(1u << block);
    (void)nvs_set_u8(storage, "faulted", faulted);
    (void)store_state(STORED_FAULT);
}
static bool fault_confirmed(void) { return key_state == NVF_MCU_KEY_FAULT && key_block >= 0 && (faulted >> key_block & 1); }
// The lowest Digital Signature key block that no recorded fault explains, or -1.
static int unexplained_key_block(void)
{
    for (int i = 0; i < 6; i++)
        if (!(faulted >> i & 1) && esp_efuse_get_key_purpose(EFUSE_BLK_KEY0 + i) == DS_PURPOSE) return i;
    return -1;
}
// With nothing usable stored: a block nobody has explained is a key awaiting its ciphertext;
// otherwise recorded faults are all there is; otherwise the chip has no key.
static void resolve_unstored(void)
{
    int orphan = unexplained_key_block();
    key_state = orphan >= 0 ? NVF_MCU_KEY_ORPHANED : faulted ? NVF_MCU_KEY_FAULT : NVF_MCU_KEY_ABSENT;
    key_block = orphan;
    for (int i = 0; orphan < 0 && i < 6; i++) if (faulted >> i & 1) key_block = i;
    if (orphan >= 0) ESP_LOGW(TAG, "eFuse key block %d holds a Digital Signature key but its ciphertext is not stored; restore it from the factory record", orphan);
}

// load_key resolves the stored state against the eFuses. Caller holds the lock.
static void load_key(void)
{
    uint8_t state = STORED_NONE, block = 0;
    if (nvs_get_u8(storage, "faulted", &faulted) != ESP_OK) faulted = 0;
    faulted &= 0x3f;
    size_t context_len = CONTEXT_SIZE, der_len = sizeof spki;
    uint8_t *context = malloc(CONTEXT_SIZE);
    esp_ds_data_t *data = context_alloc();
    bool stored = context && data && nvs_get_u8(storage, "state", &state) == ESP_OK && state != STORED_NONE &&
        nvs_get_u8(storage, "block", &block) == ESP_OK && block < 6 &&
        nvs_get_blob(storage, "ds_ctx", context, &context_len) == ESP_OK && context_len == CONTEXT_SIZE &&
        !memcmp(context, CONTEXT_MAGIC, sizeof CONTEXT_MAGIC) &&
        nvs_get_blob(storage, "spki", spki, &der_len) == ESP_OK;
    if (stored) memcpy(data, context + sizeof CONTEXT_MAGIC, sizeof *data);
    if (!context || !data) ESP_LOGE(TAG, "out of memory while loading the microcontroller key; it is unavailable until the next boot");
    free(context);
    if (!stored) {
        heap_caps_free(data);
        resolve_unstored();
        return;
    }
    if (state == STORED_STAGED && esp_efuse_key_block_unused(EFUSE_BLK_KEY0 + block)) {
        // Reset before the burn: the HMAC key died with RAM, so this ciphertext is unusable.
        ESP_LOGW(TAG, "discarding a key generation interrupted before its eFuse burn; nothing was burned");
        discard_key();
        heap_caps_free(data);
        resolve_unstored();
        return;
    }
    nvf_mcu_key_state_t resolved = NVF_MCU_KEY_FAULT;
    if (state == STORED_FAULT) {
        if (!(faulted >> block & 1)) record_fault(block);
    } else if (!block_protected(EFUSE_BLK_KEY0 + block)) {
        ESP_LOGE(TAG, "eFuse key block %u is not a fully protected Digital Signature key", block);
        record_fault(block);
    } else {
        esp_err_t err = self_test(data, block, spki, der_len);
        if (err == ESP_OK) resolved = NVF_MCU_KEY_READY;
        else ESP_LOGE(TAG, "microcontroller key self-test failed on eFuse key block %u: %s", block, esp_err_to_name(err));
        // Only a completed test changes what is stored. An allocation failure leaves the
        // state as it was: unresolved for this boot, and tested again at the next.
        if (err == ESP_OK && state == STORED_STAGED) (void)store_state(STORED_READY);
        else if (err != ESP_OK && err != ESP_ERR_NO_MEM) record_fault(block);
    }
    adopt(data, spki, der_len, block, resolved);
    if (resolved == NVF_MCU_KEY_READY && !sealed())
        ESP_LOGW(TAG, "the microcontroller key is ready but eFuse read protection is not sealed; run `commission seal`");
}

esp_err_t nvf_mcu_identity_start(void)
{
    if (!lock && !(lock = xSemaphoreCreateMutex())) return ESP_ERR_NO_MEM;
    esp_err_t err = nvs_open_from_partition(PARTITION, NAMESPACE, NVS_READWRITE, &storage);
#if !CONFIG_NVS_ENCRYPTION
    // Without the updater nothing else initialises this partition. An encrypted partition
    // is only ever opened by the updater's secure initialisation, never as plaintext here.
    if (err == ESP_ERR_NVS_PART_NOT_FOUND && nvs_flash_init_partition(PARTITION) == ESP_OK)
        err = nvs_open_from_partition(PARTITION, NAMESPACE, NVS_READWRITE, &storage);
#endif
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "hardware trust storage unavailable: %s; no evidence will be presented", esp_err_to_name(err));
        return err;
    }
    xSemaphoreTake(lock, portMAX_DELAY);
    storage_ready = true;
    load_key();
    size_t len = sizeof record;
    nvf_commission_statement_t s;
    record_present = nvs_get_blob(storage, "record", record, &len) == ESP_OK && nvf_commission_record_parse(record, len, &s);
    if (record_present)
        ESP_LOGI(TAG, "commissioning record installed: profile=%u generation=%u key=%s",
                 s.profile, (unsigned)s.generation, key_state == NVF_MCU_KEY_READY ? "ready" : "unavailable");
    xSemaphoreGive(lock);
    return ESP_OK;
}

void nvf_mcu_identity_status(nvf_mcu_identity_status_t *out)
{
    *out = (nvf_mcu_identity_status_t){ .key_block = -1 };
    if (!lock) return;
    out->security = nvf_mcu_identity_security();
    out->rd_dis_sealed = sealed();
    xSemaphoreTake(lock, portMAX_DELAY);
    out->key = key_state; out->key_block = key_block; out->record = record_present;
    nvf_commission_statement_t s;
    if (record_present && nvf_commission_record_parse(record, sizeof record, &s)) {
        out->record_profile = s.profile; out->record_generation = s.generation;
    }
    memcpy(out->hardware_trust, verdict_trust, sizeof verdict_trust);
    memcpy(out->evidence_error, verdict_error, sizeof verdict_error);
    xSemaphoreGive(lock);
}

size_t nvf_mcu_identity_public_key(uint8_t *out, size_t cap)
{
    if (!lock) return 0;
    xSemaphoreTake(lock, portMAX_DELAY);
    size_t n = ds_data && spki_len <= cap ? spki_len : 0;
    if (n) memcpy(out, spki, n);
    xSemaphoreGive(lock);
    return n;
}
size_t nvf_mcu_identity_ds_context(uint8_t *out, size_t cap)
{
    if (!lock) return 0;
    xSemaphoreTake(lock, portMAX_DELAY);
    size_t n = ds_data && cap >= CONTEXT_SIZE ? CONTEXT_SIZE : 0;
    if (n) { memcpy(out, CONTEXT_MAGIC, sizeof CONTEXT_MAGIC); memcpy(out + sizeof CONTEXT_MAGIC, ds_data, sizeof *ds_data); }
    xSemaphoreGive(lock);
    return n;
}
size_t nvf_mcu_identity_record(uint8_t out[NVF_COMMISSION_RECORD_SIZE])
{
    if (!lock) return 0;
    xSemaphoreTake(lock, portMAX_DELAY);
    size_t n = record_present ? sizeof record : 0;
    if (n) memcpy(out, record, n);
    xSemaphoreGive(lock);
    return n;
}
bool nvf_mcu_identity_key_sha256(uint8_t out[32])
{
    if (!lock) return false;
    xSemaphoreTake(lock, portMAX_DELAY);
    bool ok = key_state == NVF_MCU_KEY_READY && sha256(spki, spki_len, out);
    xSemaphoreGive(lock);
    return ok;
}

// --- bench operations --------------------------------------------------------------------

#if !CONFIG_NVF_UPDATE_PROFILE_OPEN
// True random numbers need an entropy source. The radio provides one whenever Wi-Fi is up,
// which is the normal state at the bench (station mode or the setup access point). Without
// it the SAR ADC source is enabled for the duration; nothing in this firmware uses the ADC.
static bool entropy_begin(void)
{
    wifi_mode_t mode;
    if (esp_wifi_get_mode(&mode) == ESP_OK && mode != WIFI_MODE_NULL) return false;
    bootloader_random_enable();
    return true;
}
static void entropy_end(bool enabled) { if (enabled) bootloader_random_disable(); }

// generate fills the peripheral parameters, the ciphertext and the public key, and burns the
// key block. *burned is the spent block, or -1 when nothing was burned. Every intermediate
// that depends on the private key is erased before it returns.
static const char *generate(esp_ds_data_t *data, uint8_t *der, size_t *der_len, int *burned)
{
    *burned = -1;
    static const unsigned char personalization[] = "navlistener microcontroller key";
    const char *fault = NULL;
    uint8_t hmac_key[32], iv[ESP_DS_IV_LEN], buffer[NVF_MCU_KEY_DER_MAX];
    esp_ds_p_data_t *params = heap_caps_calloc(1, sizeof *params, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    mbedtls_entropy_context entropy; mbedtls_ctr_drbg_context drbg; mbedtls_pk_context pk;
    mbedtls_mpi n, d, r;
    mbedtls_entropy_init(&entropy); mbedtls_ctr_drbg_init(&drbg); mbedtls_pk_init(&pk);
    mbedtls_mpi_init(&n); mbedtls_mpi_init(&d); mbedtls_mpi_init(&r);
    if (!params) { fault = "out of internal memory"; goto done; }
    if (mbedtls_ctr_drbg_seed(&drbg, mbedtls_entropy_func, &entropy, personalization, sizeof personalization - 1) != 0 ||
        mbedtls_pk_setup(&pk, mbedtls_pk_info_from_type(MBEDTLS_PK_RSA)) != 0) { fault = "random generator setup failed"; goto done; }
    ESP_LOGI(TAG, "generating a %d-bit RSA key; this takes from tens of seconds to a few minutes", NVF_MCU_KEY_BITS);
    mbedtls_rsa_context *rsa = mbedtls_pk_rsa(pk);
    if (mbedtls_rsa_gen_key(rsa, mbedtls_ctr_drbg_random, &drbg, NVF_MCU_KEY_BITS, 65537) != 0 ||
        mbedtls_rsa_check_privkey(rsa) != 0 || mbedtls_rsa_get_len(rsa) != NVF_MCU_KEY_BYTES) { fault = "RSA key generation failed"; goto done; }
    int written = mbedtls_pk_write_pubkey_der(&pk, buffer, sizeof buffer);
    if (written <= 0) { fault = "public key encoding failed"; goto done; }
    memcpy(der, buffer + sizeof buffer - written, (size_t)written); *der_len = (size_t)written; // written at the end of the buffer
    // Peripheral operands: Y = d, M = n, Rb = 2^(2*bits) mod n, M' = -n^-1 mod 2^32, all as
    // little-endian words, zero-extended to the peripheral's maximum width.
    if (mbedtls_rsa_export(rsa, &n, NULL, NULL, &d, NULL) != 0 || mbedtls_mpi_lset(&r, 1) != 0 ||
        mbedtls_mpi_shift_l(&r, 2 * NVF_MCU_KEY_BITS) != 0 || mbedtls_mpi_mod_mpi(&r, &r, &n) != 0 ||
        mbedtls_mpi_write_binary_le(&d, (unsigned char *)params->Y, NVF_MCU_KEY_BYTES) != 0 ||
        mbedtls_mpi_write_binary_le(&n, (unsigned char *)params->M, NVF_MCU_KEY_BYTES) != 0 ||
        mbedtls_mpi_write_binary_le(&r, (unsigned char *)params->Rb, NVF_MCU_KEY_BYTES) != 0) { fault = "peripheral operand derivation failed"; goto done; }
    uint32_t inverse = params->M[0]; // Newton's iteration doubles the correct low bits: 3, 6, 12, 24, 48
    for (int i = 0; i < 5; i++) inverse *= 2 - params->M[0] * inverse;
    params->M_prime = 0u - inverse;
    params->length = ESP_DS_RSA_3072;
    if (mbedtls_ctr_drbg_random(&drbg, hmac_key, sizeof hmac_key) != 0 || mbedtls_ctr_drbg_random(&drbg, iv, sizeof iv) != 0) { fault = "random generator failed"; goto done; }
    if (esp_ds_encrypt_params(data, iv, params, hmac_key) != ESP_OK || data->rsa_length != ESP_DS_RSA_3072) { fault = "parameter encryption failed"; goto done; }

    esp_efuse_block_t block = esp_efuse_find_unused_key_block();
    if (block == EFUSE_BLK_KEY_MAX) { fault = "no free eFuse key block"; goto done; }
    int index = (int)(block - EFUSE_BLK_KEY0);
    if (store_key(data, der, *der_len, index, STORED_STAGED) != ESP_OK) { discard_key(); fault = "could not store the ciphertext; nothing was burned"; goto done; }
    esp_err_t err = esp_efuse_write_key(block, DS_PURPOSE, hmac_key, sizeof hmac_key);
    if (err != ESP_OK && esp_efuse_key_block_unused(block)) {
        // Refused while the batch was being prepared: it was cancelled and the block is free.
        ESP_LOGE(TAG, "eFuse key block %d was not burned: %s", index, esp_err_to_name(err));
        discard_key();
        fault = "the eFuse burn was refused; nothing was burned";
        goto done;
    }
    // A failure while the batch was being committed can leave the block partly burned. It
    // is spent either way; the caller's protection check and self-test decide what it is.
    if (err != ESP_OK) ESP_LOGE(TAG, "eFuse key block %d burn reported %s after it began", index, esp_err_to_name(err));
    *burned = index;
done:
    mbedtls_platform_zeroize(hmac_key, sizeof hmac_key); mbedtls_platform_zeroize(iv, sizeof iv);
    mbedtls_platform_zeroize(buffer, sizeof buffer);
    if (params) { mbedtls_platform_zeroize(params, sizeof *params); heap_caps_free(params); }
    mbedtls_mpi_free(&n); mbedtls_mpi_free(&d); mbedtls_mpi_free(&r);
    mbedtls_pk_free(&pk); mbedtls_ctr_drbg_free(&drbg); mbedtls_entropy_free(&entropy);
    return fault;
}

// seal_field write-protects the eFuse read-protection field. Callers have just seen the
// key pass its self-test and its state stored.
static esp_err_t seal_field(void)
{
    if (sealed()) return ESP_OK;
    esp_err_t err = esp_efuse_write_field_bit(ESP_EFUSE_WR_DIS_RD_DIS);
    return err == ESP_OK && !sealed() ? ESP_FAIL : err;
}

esp_err_t nvf_mcu_identity_provision(const char **reason)
{
    *reason = NULL;
    if (!lock || !storage_ready) { *reason = "hardware trust storage is unavailable"; return ESP_ERR_INVALID_STATE; }
    xSemaphoreTake(lock, portMAX_DELAY);
    // Every refusal comes before anything is burned. In particular a key block that could
    // not be read-protected would leave the HMAC key readable by software: the bootloader
    // write-protects the read-protection field when it enables Secure Boot unless it was
    // built with SECURE_BOOT_V2_ALLOW_EFUSE_RD_DIS.
    nvf_mcu_keygen_state_t state = {
        .locked = esp_secure_boot_enabled() && esp_get_flash_encryption_mode() == ESP_FLASH_ENC_MODE_RELEASE,
        .unlocked_allowed = CONFIG_NVF_UPDATE_TEST_KEYS && CONFIG_NVF_MCU_KEY_UNLOCKED_TEST,
        .key_ready = key_state == NVF_MCU_KEY_READY,
        .key_orphaned = key_state == NVF_MCU_KEY_ORPHANED,
        .fault_unresolved = key_state == NVF_MCU_KEY_FAULT && !fault_confirmed(),
        .rd_dis_sealed = sealed(),
        .free_block = esp_efuse_find_unused_key_block() != EFUSE_BLK_KEY_MAX,
    };
    const char *why = nvf_mcu_keygen_refusal(&state);
    if (why) { xSemaphoreGive(lock); *reason = why; return ESP_ERR_NOT_ALLOWED; }

    esp_err_t result = ESP_FAIL;
    int burned = -1;
    bool ready = false;
    esp_ds_data_t *data = context_alloc();
    uint8_t *der = malloc(NVF_MCU_KEY_DER_MAX);
    size_t der_len = 0;
    if (!data || !der) { why = "out of internal memory"; result = ESP_ERR_NO_MEM; heap_caps_free(data); }
    else {
        bool entropy = entropy_begin();
        why = generate(data, der, &der_len, &burned);
        entropy_end(entropy);
        if (why) heap_caps_free(data); // nothing was burned; the earlier state stands
        else if (!block_protected(EFUSE_BLK_KEY0 + burned)) {
            adopt(data, der, der_len, burned, NVF_MCU_KEY_FAULT); record_fault(burned);
            why = "the key block was burned without full read and write protection";
        } else if ((result = self_test(data, burned, der, der_len)) == ESP_ERR_NO_MEM) {
            // Not a verdict on the key: it stays staged and is tested again at the next boot.
            adopt(data, der, der_len, burned, NVF_MCU_KEY_FAULT);
            why = "the key block was burned but its self-test ran out of memory; restart to test it again";
        } else if (result != ESP_OK) {
            adopt(data, der, der_len, burned, NVF_MCU_KEY_FAULT); record_fault(burned);
            why = "the key block was burned but a signature through the peripheral did not verify";
        } else {
            adopt(data, der, der_len, burned, NVF_MCU_KEY_READY);
            ready = true;
            if ((result = store_state(STORED_READY)) != ESP_OK) why = "the key is ready but its state could not be stored; restart, then run `commission seal`";
        }
    }
    free(der);
    if (burned >= 0 && fault_confirmed())
        ESP_LOGE(TAG, "eFuse key block %d is spent and unusable: %s. Read protection stays unsealed, so another free block can be tried once the cause is understood", burned, why);
    xSemaphoreGive(lock);
    journal_event(JOURNAL_COMMISSION, (int32_t)(1 | (ready ? 0 : 1) << 8 | (burned + 1) << 16));
    // Sealing is the last step and only follows a key that is burned, protected, tested and
    // stored. Until it succeeds the chip does not report the key's security bit.
    if (ready && !why) {
        result = seal_field();
        journal_event(JOURNAL_COMMISSION, (int32_t)(4 | (result != ESP_OK) << 8));
        if (result != ESP_OK) why = "the key is ready but eFuse read protection could not be sealed; run `commission seal`";
    }
    *reason = why;
    return why ? (result == ESP_OK ? ESP_FAIL : result) : ESP_OK;
}

// The explicit seal exists for a board that lost power between the self-test and the seal,
// or whose ready state could not be stored. It re-proves the key first: a committed key whose
// signature verifies now, or nothing is sealed.
esp_err_t nvf_mcu_identity_seal(const char **reason)
{
    *reason = NULL;
    if (!lock || !storage_ready) { *reason = "hardware trust storage is unavailable"; return ESP_ERR_INVALID_STATE; }
    xSemaphoreTake(lock, portMAX_DELAY);
    esp_err_t err = ESP_ERR_INVALID_STATE;
    if (key_state != NVF_MCU_KEY_READY || !ds_data || key_block < 0 || !block_protected(EFUSE_BLK_KEY0 + key_block))
        *reason = "seal only after the microcontroller key is ready and its block protected";
    else if ((err = self_test(ds_data, key_block, spki, spki_len)) != ESP_OK)
        *reason = "the microcontroller key did not pass its self-test; nothing was sealed";
    else if ((err = store_state(STORED_READY)) != ESP_OK)
        *reason = "the key's ready state could not be stored; nothing was sealed";
    else if ((err = seal_field()) != ESP_OK)
        *reason = "the eFuse write was refused";
    xSemaphoreGive(lock);
    journal_event(JOURNAL_COMMISSION, (int32_t)(4 | (err != ESP_OK) << 8));
    return err;
}
#else
esp_err_t nvf_mcu_identity_provision(const char **reason) { *reason = "an open build changes no eFuse and makes no key"; return ESP_ERR_NOT_SUPPORTED; }
esp_err_t nvf_mcu_identity_seal(const char **reason) { *reason = "an open build changes no eFuse"; return ESP_ERR_NOT_SUPPORTED; }
#endif

esp_err_t nvf_mcu_identity_restore(const uint8_t *context, size_t context_len, const uint8_t *der, size_t der_len, const char **reason)
{
    *reason = NULL;
    if (!lock || !storage_ready) { *reason = "hardware trust storage is unavailable"; return ESP_ERR_INVALID_STATE; }
    if (!context || context_len != CONTEXT_SIZE || memcmp(context, CONTEXT_MAGIC, sizeof CONTEXT_MAGIC) ||
        !der || !der_len || der_len > NVF_MCU_KEY_DER_MAX) { *reason = "the context or the public key is malformed"; return ESP_ERR_INVALID_ARG; }
    xSemaphoreTake(lock, portMAX_DELAY);
    esp_err_t err = ESP_ERR_INVALID_STATE;
    esp_ds_data_t *data = NULL;
    if (key_state == NVF_MCU_KEY_READY) *reason = "the microcontroller key is already ready; nothing to restore";
    else if (!(data = context_alloc())) { *reason = "out of internal memory"; err = ESP_ERR_NO_MEM; }
    else {
        memcpy(data, context + sizeof CONTEXT_MAGIC, sizeof *data);
        int block = -1;
        *reason = "this chip has no protected Digital Signature key block to restore for";
        if (data->rsa_length != ESP_DS_RSA_3072) { *reason = "the context is not for a 3072-bit key"; err = ESP_ERR_INVALID_ARG; }
        // A chip may hold a recorded fault below its good key: try each protected block.
        else for (int i = 0; i < 6 && block < 0; i++) {
            if (!block_protected(EFUSE_BLK_KEY0 + i)) continue;
            err = self_test(data, i, der, der_len);
            if (err == ESP_OK) block = i;
            else *reason = "a signature made with this context does not verify under this public key on this chip";
        }
        if (block >= 0 && (err = store_key(data, der, der_len, block, STORED_READY)) != ESP_OK) *reason = "could not store the restored key";
        else if (block >= 0) {
            if (faulted >> block & 1) { faulted &= (uint8_t)~(1u << block); (void)nvs_set_u8(storage, "faulted", faulted); (void)nvs_commit(storage); }
            adopt(data, der, der_len, block, NVF_MCU_KEY_READY); data = NULL; *reason = NULL;
        }
        heap_caps_free(data);
    }
    xSemaphoreGive(lock);
    journal_event(JOURNAL_COMMISSION, (int32_t)(3 | (err != ESP_OK) << 8));
    return err;
}

esp_err_t nvf_mcu_identity_install(const uint8_t *candidate, size_t len, nvf_live_identity_t *live, const char **reason)
{
    *reason = NULL;
    nvf_commission_statement_t s;
    if (!lock || !storage_ready) { *reason = "hardware trust storage is unavailable"; return ESP_ERR_INVALID_STATE; }
    if (!nvf_commission_record_parse(candidate, len, &s)) { *reason = "not a valid commissioning record"; return ESP_ERR_INVALID_ARG; }
    if (!live) { *reason = "live hardware identity is unavailable"; return ESP_ERR_INVALID_ARG; }
    live->key_valid = nvf_mcu_identity_key_sha256(live->mcu_key_sha256);
    live->security = nvf_mcu_identity_security(); live->security_valid = true;
    live->secure_boot_keys_valid = nvf_mcu_identity_secure_boot_keys(live->secure_boot_keys);
    if ((*reason = nvf_commission_match(&s, live, sha256))) return ESP_ERR_INVALID_ARG;
    xSemaphoreTake(lock, portMAX_DELAY);
    esp_err_t err = nvs_set_blob(storage, "record", candidate, len);
    if (err == ESP_OK) err = nvs_commit(storage);
    if (err == ESP_OK) { memcpy(record, candidate, sizeof record); record_present = true; journaled_verdict = UINT32_MAX; }
    else *reason = "could not store the record";
    xSemaphoreGive(lock);
    journal_event(JOURNAL_COMMISSION, (int32_t)(2 | (err != ESP_OK) << 8 | (uint32_t)s.profile << 16));
    return err;
}

// --- evidence ----------------------------------------------------------------------------

// Journal codes: low byte the verdict, next byte the reason. The lifecycle lane is a small
// FIFO shared with boot history, so only a change of verdict is written.
static const char *const trusts[] = { "none", "open", "test", "trusted" };
static const char *const reasons[] = { "", "unconfigured", "malformed", "signature", "identity", "unlisted",
                                       "revoked", "superseded", "proof_missing", "proof", "product" };
enum { VERDICT_UNREPORTED = 0x10, VERDICT_WITHHELD = 0x20, REASON_OTHER = 0x0f,
       REASON_NO_EXPORT = 0x20, REASON_NO_KEY = 0x21, REASON_SIGN = 0x22, REASON_LIVE_IDENTITY = 0x23 };

static void note(uint32_t code)
{
    if (code == journaled_verdict) return;
    journaled_verdict = code;
    journal_event(JOURNAL_HW_TRUST, (int32_t)code);
}

size_t nvf_mcu_identity_evidence(const uint8_t *exported, uint8_t *out, size_t cap,
                                 nvf_live_identity_t *live)
{
    if (!lock || !live) return 0;
    live->key_valid = nvf_mcu_identity_key_sha256(live->mcu_key_sha256);
    live->security = nvf_mcu_identity_security(); live->security_valid = true;
    live->secure_boot_keys_valid = nvf_mcu_identity_secure_boot_keys(live->secure_boot_keys);
    size_t n = 0;
    uint32_t withheld = 0;
    const char *mismatch = NULL;
    nvf_commission_statement_t s;
    xSemaphoreTake(lock, portMAX_DELAY);
    if (record_present && nvf_commission_record_parse(record, sizeof record, &s)) {
        uint8_t digest[32], key_digest[32], *signature = NULL;
        if ((mismatch = nvf_commission_match(&s, live, sha256))) withheld = REASON_LIVE_IDENTITY;
        else if (s.mcu_key_alg == NVF_MCU_KEY_NONE) n = nvf_evidence_encode(out, cap, record, NULL, 0, NULL, 0);
        else if (key_state != NVF_MCU_KEY_READY || !sha256(spki, spki_len, key_digest) || memcmp(key_digest, s.mcu_key_sha256, 32)) withheld = REASON_NO_KEY;
        else if (!exported) withheld = REASON_NO_EXPORT;
        else if (!(signature = malloc(NVF_MCU_KEY_BYTES)) || !nvf_mcu_proof_digest(exported, record, sha256, digest) ||
                 ds_sign(ds_data, key_block, digest, signature) != ESP_OK) withheld = REASON_SIGN;
        else n = nvf_evidence_encode(out, cap, record, spki, spki_len, signature, NVF_MCU_KEY_BYTES);
        free(signature);
        // A record that cannot be proved still says what it says for an open or test board.
        // A trusted record without its proof would only be rejected: present nothing instead.
        if (withheld && !mismatch && s.profile != NVF_PROFILE_TRUSTED) n = nvf_evidence_encode(out, cap, record, NULL, 0, NULL, 0);
        if (mismatch) {
            ESP_LOGE(TAG, "commissioning record does not match live hardware: %s; presenting no evidence", mismatch);
            note(VERDICT_WITHHELD | withheld << 8);
        } else if (withheld) {
            ESP_LOGW(TAG, "session proof unavailable (%s); %s", withheld == REASON_NO_EXPORT ? "TLS keying material was not exported" :
                     withheld == REASON_NO_KEY ? "the commissioned key is not ready on this chip" : "the peripheral did not sign",
                     n ? "presenting the record alone" : "presenting no evidence");
            if (!n) note(VERDICT_WITHHELD | withheld << 8);
        }
    }
    xSemaphoreGive(lock);
    return n;
}

void nvf_mcu_identity_verdict(const char *trust, const char *error)
{
    if (!lock) return;
    uint32_t verdict = VERDICT_UNREPORTED, reason = 0;
    for (unsigned i = 0; trust && i < sizeof trusts / sizeof trusts[0]; i++) if (!strcmp(trust, trusts[i])) verdict = i;
    if (error && error[0]) {
        reason = REASON_OTHER;
        for (unsigned i = 1; i < sizeof reasons / sizeof reasons[0]; i++) if (!strcmp(error, reasons[i])) reason = i;
    }
    xSemaphoreTake(lock, portMAX_DELAY);
    bool changed = strncmp(verdict_trust, trust ? trust : "", sizeof verdict_trust - 1) || strncmp(verdict_error, error ? error : "", sizeof verdict_error - 1);
    snprintf(verdict_trust, sizeof verdict_trust, "%s", trust ? trust : "");
    snprintf(verdict_error, sizeof verdict_error, "%s", error ? error : "");
    if (changed) {
        if (!trust) ESP_LOGW(TAG, "collector did not report a hardware trust verdict; it may predate the evidence exchange");
        else if (reason) ESP_LOGW(TAG, "collector hardware trust: %s (evidence rejected: %s)", verdict_trust, verdict_error);
        else ESP_LOGI(TAG, "collector hardware trust: %s", verdict_trust);
    }
    note(verdict | reason << 8);
    xSemaphoreGive(lock);
}
