#include "hardware_manifest.h"

#include <string.h>

#include "esp_check.h"
#include "esp_log.h"
#include "nvs.h"

#define OBSERVER_I2C_SDA_GPIO 6
#define OBSERVER_I2C_SCL_GPIO 7

#define MANIFEST_NVS_NAMESPACE "hwmanifest"
#define MANIFEST_NVS_EUI_KEY "eui64"

static const char *TAG = "hw_manifest";
static i2c_master_bus_handle_t s_i2c_bus;

static bool eui_is_factory_value(const uint8_t eui[EEPROM_UNIQUE_ID_SIZE])
{
    bool all_zero = true;
    bool all_ff = true;
    for (size_t i = 0; i < EEPROM_UNIQUE_ID_SIZE; i++) {
        all_zero = all_zero && eui[i] == 0;
        all_ff = all_ff && eui[i] == 0xff;
    }
    return !all_zero && !all_ff;
}

static void log_eui(const char *prefix, const uint8_t eui[EEPROM_UNIQUE_ID_SIZE])
{
    ESP_LOGI(TAG, "%s %02X%02X%02X%02X%02X%02X%02X%02X", prefix,
             eui[0], eui[1], eui[2], eui[3], eui[4], eui[5], eui[6], eui[7]);
}

static esp_err_t known_eui_load(bool *present,
                                uint8_t eui[EEPROM_UNIQUE_ID_SIZE])
{
    nvs_handle_t nvs;
    esp_err_t err = nvs_open(MANIFEST_NVS_NAMESPACE, NVS_READONLY, &nvs);
    if (err == ESP_ERR_NVS_NOT_FOUND) {
        *present = false;
        return ESP_OK;
    }
    ESP_RETURN_ON_ERROR(err, TAG, "open identity NVS");

    size_t len = EEPROM_UNIQUE_ID_SIZE;
    err = nvs_get_blob(nvs, MANIFEST_NVS_EUI_KEY, eui, &len);
    nvs_close(nvs);
    if (err == ESP_ERR_NVS_NOT_FOUND) {
        *present = false;
        return ESP_OK;
    }
    ESP_RETURN_ON_ERROR(err, TAG, "read known manifest EUI-64");
    if (len != EEPROM_UNIQUE_ID_SIZE) {
        ESP_LOGE(TAG, "known manifest EUI-64 has invalid length %u", (unsigned)len);
        return ESP_ERR_INVALID_SIZE;
    }
    *present = true;
    return ESP_OK;
}

static esp_err_t known_eui_store(const uint8_t eui[EEPROM_UNIQUE_ID_SIZE])
{
    nvs_handle_t nvs;
    ESP_RETURN_ON_ERROR(nvs_open(MANIFEST_NVS_NAMESPACE, NVS_READWRITE, &nvs),
                        TAG, "open identity NVS for write");
    esp_err_t err = nvs_set_blob(nvs, MANIFEST_NVS_EUI_KEY, eui,
                                 EEPROM_UNIQUE_ID_SIZE);
    if (err == ESP_OK)
        err = nvs_commit(nvs);
    nvs_close(nvs);
    return err;
}

static void make_gnss_color_neo_defaults(eeprom_capabilities_t *caps)
{
    memset(caps, 0, sizeof(*caps));
    caps->magic = CAP_MAGIC_PREFERRED;
    caps->project_id = PROJECT_GNSS;
    caps->pcb_id = GNSS_PCB_MAIN;
    caps->revision = 1; // first-spin board, revision A
    caps->i2c_address = OBSERVER_MANIFEST_I2C_ADDRESS;

    // Keep this list as the manufacturing truth for the assembled first-spin
    // board. GPIO-valued descriptors identify the two same-part LED drivers
    // by their separate output-enable pins and the two switchable LDOs by EN.
    eeprom_ic_descriptor_t components[] = {
        IC_EEPROM_SELF_24AA025E64(OBSERVER_MANIFEST_I2C_ADDRESS),
        IC_INSTALLED(CAT_MCU, MCU_ESP32_S3),
        IC_INSTALLED(CAT_GPS, GPS_NEO_M9N),
        IC_I2C(CAT_RTC, RTC_MCP79412, 0x6f),
        IC_I2C(CAT_CRYPTO, CRYPTO_ATECC608C, 0x60),
        IC_I2C(CAT_TEMP, TEMP_MCP9808, 0x18),
        IC_I2C(CAT_PRESSURE, PRESSURE_BMP388, 0x76),
        IC_I2C(CAT_SENSOR, SENSOR_HDC2080, 0x40),
        IC_GPIO(CAT_POWER, POWER_ADM7150, 38),
        IC_GPIO(CAT_POWER, POWER_RT9193, 21),
        IC_GPIO(CAT_LED, LED_TLC5916, 47),
        IC_GPIO(CAT_LED, LED_TLC5916, 48),
        IC_INSTALLED(CAT_BATTERY, BATTERY_CR123A),
        IC_INSTALLED(CAT_CONNECTOR, CONNECTOR_USB_OTG),
        IC_INSTALLED(CAT_CONNECTOR, CONNECTOR_QWIIC),
        IC_GPIO(CAT_BUTTON, BUTTON_BOOT, 0),
    };

    _Static_assert(sizeof(components) / sizeof(components[0]) <= CAP_MAX_COMPONENTS,
                   "compiled manifest exceeds EEPROM component capacity");
    caps->component_count = sizeof(components) / sizeof(components[0]);
    memcpy(caps->components, components, sizeof(components));
}

static bool capabilities_match_board(const eeprom_capabilities_t *caps)
{
    return caps->is_valid &&
           caps->project_id == PROJECT_GNSS &&
           caps->pcb_id == GNSS_PCB_MAIN &&
           eeprom_validate_self_reference(caps);
}

static esp_err_t inspect(hardware_manifest_result_t *result,
                         bool known_present,
                         const uint8_t known_eui[EEPROM_UNIQUE_ID_SIZE])
{
    if (!eeprom_read_unique_id(OBSERVER_MANIFEST_I2C_ADDRESS, result->eui64) ||
        !eui_is_factory_value(result->eui64)) {
        ESP_LOGE(TAG, "manifest EEPROM EUI-64 is unreadable or invalid");
        result->action = HARDWARE_MANIFEST_ACTION_IO_ERROR;
        return ESP_ERR_INVALID_RESPONSE;
    }

    hardware_manifest_known_eui_t known = HARDWARE_MANIFEST_KNOWN_NONE;
    if (known_present) {
        known = memcmp(known_eui, result->eui64, EEPROM_UNIQUE_ID_SIZE) == 0
                    ? HARDWARE_MANIFEST_KNOWN_SAME
                    : HARDWARE_MANIFEST_KNOWN_DIFFERENT;
    }

    hardware_manifest_observation_t observation;
    eeprom_program_state_t state =
        eeprom_get_program_state(OBSERVER_MANIFEST_I2C_ADDRESS);
    if (state == EEPROM_PROGRAM_STATE_BUS_ERROR) {
        observation = HARDWARE_MANIFEST_OBS_IO_ERROR;
    } else if (state == EEPROM_PROGRAM_STATE_BLANK) {
        observation = HARDWARE_MANIFEST_OBS_BLANK;
    } else if (state == EEPROM_PROGRAM_STATE_INVALID) {
        observation = HARDWARE_MANIFEST_OBS_PROGRAMMED_INVALID;
    } else if (eeprom_read_capabilities(OBSERVER_MANIFEST_I2C_ADDRESS,
                                        &result->capabilities) &&
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
    uint8_t known_eui[EEPROM_UNIQUE_ID_SIZE] = {0};
    ESP_RETURN_ON_ERROR(known_eui_load(&known_present, known_eui), TAG,
                        "load known manifest identity");
    ESP_RETURN_ON_ERROR(inspect(result, known_present, known_eui), TAG,
                        "inspect manifest EEPROM");
    log_eui("manifest EUI-64", result->eui64);

    if (result->action == HARDWARE_MANIFEST_ACTION_INITIALIZE &&
        allow_factory_init) {
        eeprom_capabilities_t defaults;
        make_gnss_color_neo_defaults(&defaults);
        ESP_LOGW(TAG, "factory-init enabled: programming blank GNSS main-board manifest");
        if (!eeprom_write_capabilities(OBSERVER_MANIFEST_I2C_ADDRESS, &defaults,
                                       false)) {
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
        ESP_RETURN_ON_ERROR(known_eui_store(result->eui64), TAG,
                            "remember manifest EUI-64");
        ESP_LOGI(TAG, "remembered manifest EUI-64 in independent identity NVS");
    }

    ESP_LOGI(TAG, "manifest boot action: %s",
             hardware_manifest_action_name(result->action));
    return ESP_OK;
}

i2c_master_bus_handle_t hardware_manifest_i2c_bus(void)
{
    return s_i2c_bus;
}

const char *hardware_manifest_action_name(hardware_manifest_action_t action)
{
    switch (action) {
    case HARDWARE_MANIFEST_ACTION_IO_ERROR: return "I/O error";
    case HARDWARE_MANIFEST_ACTION_INITIALIZE: return "first-boot initialization required";
    case HARDWARE_MANIFEST_ACTION_RECOVER: return "known EEPROM is blank; recovery required";
    case HARDWARE_MANIFEST_ACTION_CONFIRM_REPLACEMENT: return "EEPROM replacement requires confirmation";
    case HARDWARE_MANIFEST_ACTION_REJECT_INVALID: return "programmed manifest is invalid";
    case HARDWARE_MANIFEST_ACTION_USE: return "use manifest";
    }
    return "unknown";
}
