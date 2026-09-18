// commission — bench commissioning commands on the USB console (esp32/docs/COMMISSIONING.md).
//
// The commands are read from the USB Serial/JTAG console and nowhere else: no HTTP, BLE or
// GNF1 path reaches them. A locked unit cannot be reflashed over USB, so the release firmware
// carries them itself; what each one can do to a unit in the field is bounded in the doc.
//
//   commission report                       print NVF-COMMISSION-REPORT {json}
//   commission keygen                       IRREVERSIBLE: make the microcontroller key, then seal
//   commission install <base64 record>      store the manufacturer-signed record
//   commission restore <base64 ds context> <base64 public key>
//   commission seal                         IRREVERSIBLE: the seal alone, if keygen could not finish it
//
// Every command answers with exactly one NVF-COMMISSION-... line, written with printf so it
// is independent of the log level and easy to pick out of a captured console log.

#include "commission.h"
#include "sdkconfig.h"

#ifndef CONFIG_NVF_COMMISSION_CONSOLE
#define CONFIG_NVF_COMMISSION_CONSOLE 0
#endif

#if CONFIG_NVF_COMMISSION_CONSOLE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "cJSON.h"
#include "driver/usb_serial_jtag.h"
#include "driver/usb_serial_jtag_vfs.h"
#include "esp_app_desc.h"
#include "esp_log.h"
#include "esp_mac.h"
#include "mbedtls/base64.h"
#include "mbedtls/sha256.h"

#include "board.h"
#include "gnf1.h"
#include "mcu_identity.h"
#include "update_runtime.h"
#include "update_tuf.h"

_Static_assert(GNF1_EVIDENCE_EXPORTED_SIZE == NVF_MCU_EXPORTED_SIZE && GNF1_EVIDENCE_MAX == NVF_EVIDENCE_MAX,
               "gnf1 and mcu_identity disagree about the evidence exchange");

static const char *TAG = "commission";
// The longest line is `restore`: a 1608-byte context and a 422-byte key in base64.
#define COMMISSION_LINE_MAX 4096

static void hex(const uint8_t *in, size_t n, char *out)
{
    for (size_t i = 0; i < n; i++) snprintf(out + 2 * i, 3, "%02x", in[i]);
    out[2 * n] = 0;
}
static void add_hex(cJSON *json, const char *name, const uint8_t *in, size_t n, bool valid)
{
    char text[2 * 72 + 1] = "";
    if (valid && n <= 72) hex(in, n, text);
    cJSON_AddStringToObject(json, name, text);
}
static void add_base64(cJSON *json, const char *name, const uint8_t *in, size_t n)
{
    size_t need = 0;
    char *text = NULL;
    if (n) (void)mbedtls_base64_encode(NULL, 0, &need, in, n);
    if (need && (text = malloc(need)) && mbedtls_base64_encode((unsigned char *)text, need, &need, in, n) == 0)
        cJSON_AddStringToObject(json, name, text);
    else
        cJSON_AddStringToObject(json, name, "");
    free(text);
}
// decode returns a heap buffer holding the base64 text's bytes, or NULL.
static uint8_t *decode(const char *text, size_t *len)
{
    size_t n = strlen(text), need = 0;
    if (mbedtls_base64_decode(NULL, 0, &need, (const unsigned char *)text, n) != MBEDTLS_ERR_BASE64_BUFFER_TOO_SMALL || !need) return NULL;
    uint8_t *out = malloc(need);
    if (out && mbedtls_base64_decode(out, need, len, (const unsigned char *)text, n) != 0) { free(out); out = NULL; }
    return out;
}

static void live_identity(const observer_board_identity_t *board, nvf_live_identity_t *live)
{
    *live = (nvf_live_identity_t){ .atecc_valid = board->atecc_valid, .rtc_valid = board->rtc_valid, .board_valid = board->board_valid };
    memcpy(live->atecc_serial, board->atecc_serial, 9); memcpy(live->rtc_eui64, board->rtc_eui64, 8);
    memcpy(live->board_eui64, board->board_eui64, 8);
    live->mac_valid = esp_efuse_mac_get_default(live->mcu_mac) == ESP_OK;
}

static void report(void)
{
    observer_board_identity_t board;
    nvf_live_identity_t live;
    nvf_mcu_identity_status_t status;
    observer_board_identity(&board);
    live_identity(&board, &live);
    nvf_mcu_identity_status(&status);
    uint8_t *scratch = malloc(NVF_MCU_DS_CONTEXT_MAX), digest[32], record[NVF_COMMISSION_RECORD_SIZE];
    cJSON *json = cJSON_CreateObject();
    if (!scratch || !json) { free(scratch); cJSON_Delete(json); puts("NVF-COMMISSION-ERROR report: out of memory"); return; }
    cJSON_AddNumberToObject(json, "v", 1);
    cJSON_AddNumberToObject(json, "product", NVF_COMMISSION_PRODUCT_OBSERVER);
    add_hex(json, "atecc_serial", board.atecc_serial, 9, board.atecc_valid);
    add_hex(json, "rtc_eui64", board.rtc_eui64, 8, board.rtc_valid);
    add_hex(json, "board_eui64", board.board_eui64, 8, board.board_valid);
    if (board.revision_valid) cJSON_AddNumberToObject(json, "board_rev", board.revision);
    else cJSON_AddNullToObject(json, "board_rev");
    cJSON_AddNumberToObject(json, "mcu_family", NVF_MCU_ESP32S3);
    add_hex(json, "mcu_mac", live.mcu_mac, 6, live.mac_valid);
    cJSON_AddNumberToObject(json, "security", status.security);
    add_hex(json, "secure_boot_keys_sha256", digest, 32, nvf_mcu_identity_secure_boot_keys(digest));
    add_hex(json, "attestation_record", board.attestation, sizeof board.attestation, board.attestation_valid);
    size_t n = nvf_mcu_identity_public_key(scratch, NVF_MCU_DS_CONTEXT_MAX);
    bool ready = status.key == NVF_MCU_KEY_READY && n;
    cJSON_AddNumberToObject(json, "mcu_key_alg", ready ? NVF_MCU_KEY_RSA3072_PSS : NVF_MCU_KEY_NONE);
    add_base64(json, "mcu_public_key_der", scratch, ready ? n : 0);
    add_hex(json, "mcu_key_sha256", digest, 32, ready && mbedtls_sha256(scratch, n, digest, 0) == 0);
    n = nvf_mcu_identity_ds_context(scratch, NVF_MCU_DS_CONTEXT_MAX);
    add_base64(json, "ds_context", scratch, ready ? n : 0);
    static const char *const key_states[] = { "absent", "orphaned", "ready", "fault" };
    cJSON_AddStringToObject(json, "key_state", key_states[status.key]);
    cJSON_AddNumberToObject(json, "key_block", status.key_block);
    cJSON_AddBoolToObject(json, "rd_dis_sealed", status.rd_dis_sealed);
    n = nvf_mcu_identity_record(record);
    char *record_hex = calloc(1, 2 * sizeof record + 1);
    if (record_hex && n) hex(record, n, record_hex);
    cJSON_AddStringToObject(json, "record", record_hex ? record_hex : "");
    free(record_hex);
    cJSON_AddStringToObject(json, "firmware", esp_app_get_description()->version);
    cJSON_AddStringToObject(json, "trust_profile", nvf_update_profile_name(nvf_update_profile()));
    char *body = cJSON_PrintUnformatted(json);
    if (body) printf("NVF-COMMISSION-REPORT %s\n", body);
    else puts("NVF-COMMISSION-ERROR report: out of memory");
    cJSON_free(body); cJSON_Delete(json); free(scratch);
}

static void answer(const char *operation, esp_err_t err, const char *reason)
{
    if (err == ESP_OK) printf("NVF-COMMISSION-OK %s\n", operation);
    else printf("NVF-COMMISSION-ERROR %s: %s (%s)\n", operation, reason ? reason : "failed", esp_err_to_name(err));
}

static void keygen(void)
{
    const char *reason = NULL;
    // Prime generation keeps a core busy for minutes. At the idle priority it shares time
    // slices with the idle task, which keeps the task watchdog fed.
    UBaseType_t priority = uxTaskPriorityGet(NULL);
    vTaskPrioritySet(NULL, tskIDLE_PRIORITY);
    esp_err_t err = nvf_mcu_identity_provision(&reason);
    vTaskPrioritySet(NULL, priority);
    answer("keygen", err, reason);
    report(); // what the chip holds now, whichever way it went
}

static void install(char *argument)
{
    const char *reason = "the record is not valid base64";
    size_t len = 0;
    uint8_t *record = decode(argument, &len);
    esp_err_t err = ESP_ERR_INVALID_ARG;
    if (record) {
        observer_board_identity_t board;
        nvf_live_identity_t live;
        observer_board_identity(&board);
        live_identity(&board, &live);
        err = nvf_mcu_identity_install(record, len, &live, &reason);
    }
    free(record);
    answer("install", err, reason);
}

static void restore(char *arguments)
{
    const char *reason = "expected <base64 ds context> <base64 public key>";
    esp_err_t err = ESP_ERR_INVALID_ARG;
    char *key_text = strchr(arguments, ' ');
    if (key_text) {
        *key_text++ = 0;
        size_t context_len = 0, key_len = 0;
        uint8_t *context = decode(arguments, &context_len), *key = decode(key_text, &key_len);
        if (context && key) err = nvf_mcu_identity_restore(context, context_len, key, key_len, &reason);
        else reason = "an argument is not valid base64";
        free(context); free(key);
    }
    answer("restore", err, reason);
}

static void run(char *line)
{
    static const char prefix[] = "commission ";
    if (strncmp(line, prefix, sizeof prefix - 1)) return; // not ours: the console sees stray keys
    char *command = line + sizeof prefix - 1, *argument = strchr(command, ' ');
    if (argument) *argument++ = 0;
    const char *reason = NULL;
    if (!strcmp(command, "report") && !argument) report();
    else if (!strcmp(command, "keygen") && !argument) keygen();
    else if (!strcmp(command, "seal") && !argument) { esp_err_t err = nvf_mcu_identity_seal(&reason); answer("seal", err, reason); }
    else if (!strcmp(command, "install") && argument) install(argument);
    else if (!strcmp(command, "restore") && argument) restore(argument);
    else puts("NVF-COMMISSION-ERROR usage: commission report | keygen | install <record> | restore <context> <key> | seal");
}

static void console_task(void *arg)
{
    (void)arg;
    char *line = malloc(COMMISSION_LINE_MAX);
    if (!line) { ESP_LOGE(TAG, "no memory for the commissioning console"); vTaskDelete(NULL); return; }
    size_t used = 0;
    bool overflow = false;
    for (;;) {
        char c;
        if (usb_serial_jtag_read_bytes(&c, 1, portMAX_DELAY) != 1) continue;
        if (c != '\n' && c != '\r') {
            if (used + 1 < COMMISSION_LINE_MAX) line[used++] = c;
            else overflow = true;
            continue;
        }
        line[used] = 0;
        if (overflow) puts("NVF-COMMISSION-ERROR line too long");
        else if (used) run(line);
        memset(line, 0, used); // a record or a context is not secret, but nothing lingers either
        used = 0; overflow = false;
    }
}

void commission_console_start(void)
{
    if (!usb_serial_jtag_is_driver_installed()) {
        // The driver gives blocking reads. Console output moves onto it as well, which keeps
        // logging non-blocking when no USB host is attached (it drops instead of waiting).
        usb_serial_jtag_driver_config_t config = USB_SERIAL_JTAG_DRIVER_CONFIG_DEFAULT();
        config.rx_buffer_size = COMMISSION_LINE_MAX;
        esp_err_t err = usb_serial_jtag_driver_install(&config);
        if (err != ESP_OK) { ESP_LOGW(TAG, "USB console driver unavailable (%s); no commissioning commands", esp_err_to_name(err)); return; }
        usb_serial_jtag_vfs_use_driver();
    }
    if (xTaskCreate(console_task, "commission", 8192, NULL, 1, NULL) != pdPASS)
        ESP_LOGW(TAG, "could not start the commissioning console");
}
#else
void commission_console_start(void) {}
#endif
