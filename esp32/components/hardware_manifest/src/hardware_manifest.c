#include "hardware_manifest.h"

#include <string.h>

#include "esp_check.h"
#include "esp_log.h"
#include "nvs.h"
#include "sdkconfig.h"

#define OBSERVER_I2C_SDA_GPIO 6
#define OBSERVER_I2C_SCL_GPIO 7

#define MANIFEST_NVS_NAMESPACE "hwmanifest"
#define MANIFEST_NVS_UID_KEY "eeprom_uid"

static const char *TAG = "hw_manifest";
static i2c_master_bus_handle_t s_i2c_bus;
static bool s_uid_known;
static uint8_t s_uid[NVF_BOARD_UID_SIZE];

static hardware_manifest_i2c_t probe_result(esp_err_t err)
{
    return err == ESP_OK ? HARDWARE_MANIFEST_I2C_OK :
           err == ESP_ERR_NOT_FOUND ? HARDWARE_MANIFEST_I2C_NOT_FOUND : HARDWARE_MANIFEST_I2C_FAULT;
}

static int identity_read(void *ctx, uint8_t address, uint16_t reg, uint8_t address_bytes, uint8_t *out, size_t n)
{
    (void)ctx;
    hardware_manifest_i2c_t probe = probe_result(i2c_master_probe(s_i2c_bus, address, 100));
    hardware_manifest_i2c_t transfer = HARDWARE_MANIFEST_I2C_FAULT, reprobe = HARDWARE_MANIFEST_I2C_FAULT;
    if (probe == HARDWARE_MANIFEST_I2C_OK) {
        i2c_master_dev_handle_t dev;
        i2c_device_config_t cfg = {.dev_addr_length=I2C_ADDR_BIT_LEN_7,
            .device_address=address, .scl_speed_hz=100000};
        if (i2c_master_bus_add_device(s_i2c_bus, &cfg, &dev) != ESP_OK) return NVF_ID_IO_ERROR;
        uint8_t pointer[2] = {(uint8_t)(reg >> 8), (uint8_t)reg};
        esp_err_t err = i2c_master_transmit_receive(dev, pointer + (address_bytes == 1), address_bytes, out, n, 100);
        if (i2c_master_bus_rm_device(dev) != ESP_OK) return NVF_ID_IO_ERROR;
        // ESP-IDF 5.5 returns ESP_ERR_INVALID_STATE for a NACK and a bus timeout alike;
        // a second probe tells them apart (hardware_manifest_classify_read).
        transfer = err == ESP_OK ? HARDWARE_MANIFEST_I2C_OK :
                   err == ESP_ERR_INVALID_STATE ? HARDWARE_MANIFEST_I2C_TRANSFER_FAILED : HARDWARE_MANIFEST_I2C_FAULT;
        if (transfer == HARDWARE_MANIFEST_I2C_TRANSFER_FAILED)
            reprobe = probe_result(i2c_master_probe(s_i2c_bus, address, 100));
    }
    switch (hardware_manifest_classify_read(probe, transfer, reprobe)) {
    case HARDWARE_MANIFEST_READ_OK: return NVF_ID_READ_OK;
    case HARDWARE_MANIFEST_READ_ABSENT: return NVF_ID_ABSENT;
    case HARDWARE_MANIFEST_READ_REFUSED: return NVF_ID_NACK;
    case HARDWARE_MANIFEST_READ_IO_ERROR: break;
    }
    return NVF_ID_IO_ERROR;
}

esp_err_t hardware_manifest_read_identity(nvf_board_identity_t *out)
{
    if (!out || !s_i2c_bus) return ESP_ERR_INVALID_STATE;
    const char *error = nvf_board_discover(identity_read, NULL, s_uid_known ? s_uid : NULL, 0, out);
    if (error) { memset(out, 0, sizeof(*out)); ESP_LOGE(TAG, "%s", error); return ESP_ERR_INVALID_RESPONSE; }
    return ESP_OK;
}

static esp_err_t board_uid_load(void)
{
    nvs_handle_t nvs;
    esp_err_t err = nvs_open(MANIFEST_NVS_NAMESPACE, NVS_READONLY, &nvs);
    s_uid_known = false;
    if (err == ESP_ERR_NVS_NOT_FOUND) return ESP_OK;
    if (err != ESP_OK) return err;
    size_t n = sizeof s_uid;
    err = nvs_get_blob(nvs, "board_uid", s_uid, &n);
    nvs_close(nvs);
    if (err == ESP_ERR_NVS_NOT_FOUND) return ESP_OK;
    if (err != ESP_OK) return err;
    if (n != sizeof s_uid || !nvf_uid_valid(s_uid)) return ESP_ERR_INVALID_RESPONSE;
    s_uid_known = true;
    return ESP_OK;
}

static esp_err_t board_uid_store(const uint8_t *uid)
{
    nvs_handle_t nvs;
    ESP_RETURN_ON_ERROR(nvs_open(MANIFEST_NVS_NAMESPACE, NVS_READWRITE, &nvs), TAG, "open UID NVS");
    esp_err_t err = nvs_set_blob(nvs, "board_uid", uid, NVF_BOARD_UID_SIZE);
    if (err == ESP_OK) err = nvs_commit(nvs);
    nvs_close(nvs);
    if (err == ESP_OK) { memcpy(s_uid, uid, sizeof s_uid); s_uid_known = true; }
    return err;
}


static esp_err_t known_eeprom_uid_load(bool *present,
                                uint8_t eui[NVF_BOARD_UID_SIZE])
{
    nvs_handle_t nvs;
    esp_err_t err = nvs_open(MANIFEST_NVS_NAMESPACE, NVS_READONLY, &nvs);
    if (err == ESP_ERR_NVS_NOT_FOUND) {
        *present = false;
        return ESP_OK;
    }
    ESP_RETURN_ON_ERROR(err, TAG, "open identity NVS");

    size_t len = NVF_BOARD_UID_SIZE;
    err = nvs_get_blob(nvs, MANIFEST_NVS_UID_KEY, eui, &len);
    nvs_close(nvs);
    if (err == ESP_ERR_NVS_NOT_FOUND) {
        *present = false;
        return ESP_OK;
    }
    ESP_RETURN_ON_ERROR(err, TAG, "read known manifest UID");
    if (len != NVF_BOARD_UID_SIZE) {
        ESP_LOGE(TAG, "known manifest UID has invalid length %u", (unsigned)len);
        return ESP_ERR_INVALID_SIZE;
    }
    *present = true;
    return ESP_OK;
}

static esp_err_t known_eeprom_uid_store(const uint8_t eui[NVF_BOARD_UID_SIZE])
{
    nvs_handle_t nvs;
    ESP_RETURN_ON_ERROR(nvs_open(MANIFEST_NVS_NAMESPACE, NVS_READWRITE, &nvs),
                        TAG, "open identity NVS for write");
    esp_err_t err = nvs_set_blob(nvs, MANIFEST_NVS_UID_KEY, eui,
                                 NVF_BOARD_UID_SIZE);
    if (err == ESP_OK)
        err = nvs_commit(nvs);
    nvs_close(nvs);
    return err;
}

// The board and batch that the hacker-friendly factory-init build writes onto a blank,
// never-seen EEPROM. The list itself is the library's Intsat template, the same one the
// factory CA writes, so both produce identical bytes.
#if CONFIG_NVF_MANIFEST_BOARD_ZED_X20_A
#define MANIFEST_BOARD_ID INTSAT_X20
#define MANIFEST_BOARD_NAME "ZED-X20P square revision A"
#elif CONFIG_NVF_MANIFEST_BOARD_MAX_A
#define MANIFEST_BOARD_ID INTSAT_MAX
#define MANIFEST_BOARD_NAME "MAX-M10S mobile revision A"
#else
#define MANIFEST_BOARD_ID INTSAT_NEO
#define MANIFEST_BOARD_NAME "NEO first-spin revision A"
#endif
// The part at 0x40 on the batch being initialized. Field firmware reads it back from the
// manifest to choose the temperature formula; the two parts' ID registers are identical.
#if CONFIG_NVF_MANIFEST_HDC2022
#define MANIFEST_HDC SENSOR_HDC2022
#else
#define MANIFEST_HDC SENSOR_HDC2080
#endif

static bool capabilities_match_board(const eeprom_capabilities_t *caps)
{
    return caps->is_valid &&
           caps->project_id == PROJECT_GNSS &&
           caps->pcb_id == GNSS_PCB_MAIN &&
           eeprom_validate_self_reference(caps);
}

static esp_err_t inspect(hardware_manifest_result_t *result,
                         bool known_present,
                         const uint8_t known_eui[NVF_BOARD_UID_SIZE])
{
    uint8_t address = result->identity.eeprom_address;
    eeprom_factory_id_t factory_id;
    uint8_t uid[NVF_BOARD_UID_SIZE];
    // The library's own read of the factory serial must agree with discovery's.
    if (!eeprom_read_factory_id(address, &factory_id)) return ESP_ERR_INVALID_RESPONSE;
    uint16_t kind = factory_id.kind == EEPROM_FACTORY_ID_SERIAL128 ? NVF_UID_SERIAL128 :
                    factory_id.kind == EEPROM_FACTORY_ID_ST_UID128 ? NVF_UID_ST_UID128 : 0;
    if (!nvf_uid_pack(kind, factory_id.bytes, factory_id.length, uid) ||
        memcmp(uid, result->identity.eeprom_uid, sizeof uid)) return ESP_ERR_INVALID_RESPONSE;

    hardware_manifest_known_eui_t known = HARDWARE_MANIFEST_KNOWN_NONE;
    if (known_present) {
        known = memcmp(known_eui, result->identity.eeprom_uid, NVF_BOARD_UID_SIZE) == 0
                    ? HARDWARE_MANIFEST_KNOWN_SAME
                    : HARDWARE_MANIFEST_KNOWN_DIFFERENT;
    }

    hardware_manifest_observation_t observation;
    eeprom_program_state_t state =
        eeprom_get_program_state(address);
    if (state == EEPROM_PROGRAM_STATE_BUS_ERROR) {
        observation = HARDWARE_MANIFEST_OBS_IO_ERROR;
    } else if (state == EEPROM_PROGRAM_STATE_BLANK) {
        observation = HARDWARE_MANIFEST_OBS_BLANK;
    } else if (state == EEPROM_PROGRAM_STATE_INVALID) {
        observation = HARDWARE_MANIFEST_OBS_PROGRAMMED_INVALID;
    } else if (eeprom_read_capabilities(address, &result->capabilities) &&
               capabilities_match_board(&result->capabilities)) {
        result->capabilities_valid = true;
        observation = HARDWARE_MANIFEST_OBS_PROGRAMMED_VALID;
    } else {
        observation = HARDWARE_MANIFEST_OBS_PROGRAMMED_INVALID;
    }

    result->action = hardware_manifest_decide(observation, known);
    return ESP_OK;
}

esp_err_t hardware_manifest_boot(bool allow_factory_init,
                                 hardware_manifest_result_t *result)
{
    ESP_RETURN_ON_FALSE(result != NULL, ESP_ERR_INVALID_ARG, TAG, "NULL result");
    memset(result, 0, sizeof(*result));
    result->action = HARDWARE_MANIFEST_ACTION_IO_ERROR;

    if (s_i2c_bus == NULL) {
        i2c_master_bus_config_t cfg = {
            .i2c_port = -1,
            .sda_io_num = OBSERVER_I2C_SDA_GPIO,
            .scl_io_num = OBSERVER_I2C_SCL_GPIO,
            .clk_source = I2C_CLK_SRC_DEFAULT,
            .glitch_ignore_cnt = 7,
            .flags.enable_internal_pullup = false,
        };
        ESP_RETURN_ON_ERROR(i2c_new_master_bus(&cfg, &s_i2c_bus), TAG,
                            "create observer I2C bus");
        ESP_RETURN_ON_ERROR(eeprom_discovery_init(s_i2c_bus), TAG,
                            "initialize EEPROM discovery");
    }

    bool known_present = false;
    uint8_t known_eui[NVF_BOARD_UID_SIZE] = {0};
    ESP_RETURN_ON_ERROR(known_eeprom_uid_load(&known_present, known_eui), TAG,
                        "load known manifest identity");
    ESP_RETURN_ON_ERROR(board_uid_load(), TAG, "load adopted board UID");
    ESP_RETURN_ON_ERROR(hardware_manifest_read_identity(&result->identity), TAG, "discover board identity");
    uint8_t address = result->identity.eeprom_address;
    if (!result->identity.eeprom_valid) {
        result->action = hardware_manifest_decide(HARDWARE_MANIFEST_OBS_ABSENT,
            known_present ? HARDWARE_MANIFEST_KNOWN_SAME : HARDWARE_MANIFEST_KNOWN_NONE);
        return known_present ? ESP_ERR_NOT_FOUND : ESP_OK;
    }
    // The library identifies the part from its Manufacturer ID or identification page; the
    // factory-serial comparison in inspect() confirms it is the part discovery found.
    eeprom_profile_t profile;
    ESP_RETURN_ON_ERROR(eeprom_identify(address, &profile), TAG, "identify manifest EEPROM");
    ESP_RETURN_ON_ERROR(eeprom_set_profile(address, profile), TAG, "select verified EEPROM profile");
    ESP_RETURN_ON_ERROR(inspect(result, known_present, known_eui), TAG,
                        "inspect manifest EEPROM");
    char observer[NVF_BOARD_OBSERVER_SIZE];
    if (nvf_uid_observer(result->identity.eeprom_uid, observer))
        ESP_LOGI(TAG, "manifest EEPROM factory serial: %s", observer);

    if (result->action == HARDWARE_MANIFEST_ACTION_INITIALIZE &&
        allow_factory_init) {
        eeprom_capabilities_t defaults;
        const eeprom_intsat_options_t batch = {.humidity_id = MANIFEST_HDC};
        ESP_RETURN_ON_FALSE(eeprom_intsat_template(MANIFEST_BOARD_ID, 1, &batch, profile, &defaults) &&
                            defaults.i2c_address == address, ESP_ERR_NOT_SUPPORTED, TAG,
                            "no " MANIFEST_BOARD_NAME " manifest for this EEPROM");
        ESP_LOGW(TAG, "factory-init enabled: programming blank " MANIFEST_BOARD_NAME " manifest");
        if (!eeprom_write_capabilities(address, &defaults, false)) {
            result->action = HARDWARE_MANIFEST_ACTION_IO_ERROR;
            return ESP_FAIL;
        }

        memset(&result->capabilities, 0, sizeof(result->capabilities));
        result->capabilities_valid = false;
        ESP_RETURN_ON_ERROR(inspect(result, false, known_eui), TAG,
                            "verify programmed manifest");
        if (result->action != HARDWARE_MANIFEST_ACTION_USE) {
            ESP_LOGE(TAG, "programmed manifest did not validate");
            return ESP_ERR_INVALID_RESPONSE;
        }
    }

    if (result->action == HARDWARE_MANIFEST_ACTION_USE && !known_present) {
        ESP_RETURN_ON_ERROR(known_eeprom_uid_store(result->identity.eeprom_uid), TAG,
                            "remember manifest UID");
        ESP_LOGI(TAG, "remembered manifest UID in independent identity NVS");
    }

    if (result->action == HARDWARE_MANIFEST_ACTION_USE && !s_uid_known && result->identity.board_valid)
        ESP_RETURN_ON_ERROR(board_uid_store(result->identity.board_uid), TAG, "remember board UID");

    if (result->action == HARDWARE_MANIFEST_ACTION_USE && result->capabilities_valid) {
        uint8_t id, revision;
        eeprom_board_result_t found = eeprom_find_board(&result->capabilities, CAT_INTSAT, &id, &revision);
        result->board = manifest_board_decide(found, id, revision);
    }
    ESP_LOGI(TAG, "manifest boot action: %s; board: %s",
             hardware_manifest_action_name(result->action), manifest_board_name(result->board));
    return ESP_OK;
}

i2c_master_bus_handle_t hardware_manifest_i2c_bus(void)
{
    return s_i2c_bus;
}

const char *hardware_manifest_action_name(hardware_manifest_action_t action)
{
    switch (action) {
    case HARDWARE_MANIFEST_ACTION_ABSENT: return "EEPROM absent; compiled wiring only";
    case HARDWARE_MANIFEST_ACTION_IO_ERROR: return "I/O error";
    case HARDWARE_MANIFEST_ACTION_INITIALIZE: return "first-boot initialization required";
    case HARDWARE_MANIFEST_ACTION_RECOVER: return "known EEPROM is blank; recovery required";
    case HARDWARE_MANIFEST_ACTION_CONFIRM_REPLACEMENT: return "EEPROM replacement requires confirmation";
    case HARDWARE_MANIFEST_ACTION_REJECT_INVALID: return "programmed manifest is invalid";
    case HARDWARE_MANIFEST_ACTION_USE: return "use manifest";
    }
    return "unknown";
}
