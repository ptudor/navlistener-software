#include "update_runtime.h"
#include "board.h"
#include "sdkconfig.h"
#include "board_reservations.h"
#if CONFIG_NVF_BOARD_GNSS_COLOR
#include <string.h>
#include <stdio.h>
#include <stdatomic.h>
#include <math.h>
#include "esp_app_desc.h"
#include "esp_heap_caps.h"
#include "observer_report.h"
#include "gnf1.h"
#include "spool.h"
#include "driver/gpio.h"
#include "driver/ledc.h"
#include "panel_control.h"
#include "panel_settings.h"
#include "driver/i2c_master.h"
#include "esp_log.h"
#include "esp_rom_sys.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "hardware_manifest.h"
#include "hardware_checks.h"
#include "mcu_identity_core.h"
#include "receiver.h"
#include "observer_rtc.h"
#include "rtc_policy.h"
#include "environment.h"
#include "pusher.h"
#include "nvs.h"
#include "journal.h"
#include "pulse_timing.h"
#if CONFIG_NVF_BOARD_GNSS_COLOR_MAX
#include "motion.h"
#include "sensor_settings.h"
#include "thermocouple.h"
#endif
#if NVF_PIN_BRIGHT_ADC >= 0
#include "esp_adc/adc_cali.h"
#include "esp_adc/adc_cali_scheme.h"
#include "esp_adc/adc_oneshot.h"
#endif

static const char *TAG = "observer_board";
static observer_report_t report;
static report_policy_t report_policy;
static uint64_t (*report_now_ns)(void);
static int64_t next_environment, last_environment = -5000;
static rtc_candidate_t utc_candidate; // trusted UTC for the humidity heater's run interval
static atomic_uint brightness = PANEL_DEFAULT_BRIGHTNESS;
// The brightness trimmer's position when the level was last chosen, saved with it; 0 on
// boards without a trimmer and whenever the position is unknown (panel_brightness_t).
static atomic_uint trimmer_reference;
static uint64_t next_timing;
static unsigned applied_brightness = PANEL_DEFAULT_BRIGHTNESS, saved_reference;
static bool pwm_ready;
// One wake/command/sleep session with the ATECC at a time: the boot diagnostic and the
// bench identity read must not interleave their commands.
static SemaphoreHandle_t crypto_lock;
void observer_board_set_brightness(unsigned percent)
{ atomic_store(&brightness, percent > 100 ? 100 : percent); }
void observer_board_cycle_brightness(void)
{ observer_board_set_brightness(panel_next_brightness(atomic_load(&brightness))); }
void observer_board_manifest(const hardware_manifest_result_t *manifest, uint64_t (*now_ns)(void))
{
    report_now_ns = now_ns;
    report.manifest.action = manifest->action;
    report.manifest.uid_valid = manifest->identity.eeprom_valid;
    if (manifest->identity.eeprom_valid)
        memcpy(report.manifest.board_uid, manifest->identity.eeprom_uid, NVF_BOARD_UID_SIZE);
    report.manifest.capabilities_valid = manifest->capabilities_valid;
    if (manifest->capabilities_valid) {
        report.manifest.revision = manifest->capabilities.revision;
        report.manifest.component_count = manifest->capabilities.component_count;
    }
    snprintf(report.firmware, sizeof report.firmware, "%s", esp_app_get_description()->version);
    // The manifest is the only authority for the HDC variant: both parts return the same
    // ID registers. A board without a usable manifest has not been configured, and one
    // that lists neither part (or both) gets no humidity measurement.
    bool usable = manifest->action == HARDWARE_MANIFEST_ACTION_USE && manifest->capabilities_valid;
    bool hdc2080 = usable && eeprom_has_ic(&manifest->capabilities, CAT_SENSOR, SENSOR_HDC2080);
    bool hdc2022 = usable && eeprom_has_ic(&manifest->capabilities, CAT_SENSOR, SENSOR_HDC2022);
    report.humidity = (report_humidity_t){.present = true,
        .part = hdc2080 && hdc2022 ? HUMIDITY_CONFLICT : hdc2080 ? HUMIDITY_HDC2080 :
                hdc2022 ? HUMIDITY_HDC2022 : HUMIDITY_NOT_LISTED};
    environment_set_hdc(report.humidity.part == HUMIDITY_HDC2080 ? ENV_HDC2080 :
                        report.humidity.part == HUMIDITY_HDC2022 ? ENV_HDC2022 : ENV_HDC_NONE);
    if (!usable)
        ESP_LOGW(TAG, "manifest %s: the humidity sensor is not probed; has this board been configured?",
                 hardware_manifest_action_name(manifest->action));
    else if (report.humidity.part == HUMIDITY_NOT_LISTED)
        ESP_LOGW(TAG, "the manifest lists no HDC2080 or HDC2022; humidity is not measured");
    else if (report.humidity.part == HUMIDITY_CONFLICT)
        ESP_LOGE(TAG, "the manifest lists both HDC2080 and HDC2022; humidity is not measured until it is corrected");
}
enum { LED_DATA = 14, LED_CLOCK = 11, LED_LATCH = 12, LED_GREEN_OE = 47, LED_YELLOW_OE = 48 };
// The panel pins are part of the allocation record, not a private choice here.
_Static_assert((NVF_PIN(LED_DATA) | NVF_PIN(LED_CLOCK) | NVF_PIN(LED_LATCH) |
                NVF_PIN(LED_GREEN_OE) | NVF_PIN(LED_YELLOW_OE)) ==
               ((NVF_PIN(LED_DATA) | NVF_PIN(LED_CLOCK) | NVF_PIN(LED_LATCH) |
                 NVF_PIN(LED_GREEN_OE) | NVF_PIN(LED_YELLOW_OE)) & NVF_PINS_BOARD),
    "a panel pin is missing from the board_reservations.h allocation record");
typedef struct {
    uint32_t magic;
    int32_t latitude, longitude;
    uint8_t seen, reserved[3];
} reception_history_t;
static reception_history_t history;
static bool history_dirty;
static int64_t next_history_write;
static void history_load(void)
{
    nvs_handle_t nvs;
    if (nvs_open("nvf_panel", NVS_READONLY, &nvs) != ESP_OK) return;
    size_t length = sizeof history;
    esp_err_t err = nvs_get_blob(nvs, "reception", &history, &length);
    nvs_close(nvs);
    if (err != ESP_OK || length != sizeof history || history.magic != 0x4e565001 ||
        history.latitude < -900000000 || history.latitude > 900000000 ||
        history.longitude < -1800000000 || history.longitude > 1800000000 ||
        history.seen & ~GNSS_REGIONAL_MASK || history.reserved[0] || history.reserved[1] || history.reserved[2])
        memset(&history, 0, sizeof history);
}
static uint8_t reception_history(const gnss_status_t *status, int64_t now)
{
    if (!gnss_status_position_fresh(status, now)) return 0;
    bool same = history.magic && gnss_status_same_place(history.latitude, history.longitude,
                                                       status->latitude, status->longitude);
    uint8_t previous = same ? history.seen : 0, seen = previous;
    if (status->satellites_valid && now >= status->satellites_ms && now - status->satellites_ms <= 15000)
        for (unsigned g = 0; g < 8; g++)
            if ((GNSS_REGIONAL_MASK & status->supported & (1u << g)) && status->tracked[g]) seen |= 1u << g;
    if (seen != previous) {
        history = (reception_history_t){.magic = 0x4e565001,
            .latitude = same ? history.latitude : status->latitude,
            .longitude = same ? history.longitude : status->longitude, .seen = seen};
        history_dirty = true;
    }
    if (history_dirty && now >= next_history_write) {
        nvs_handle_t nvs;
        esp_err_t err = nvs_open("nvf_panel", NVS_READWRITE, &nvs);
        if (err == ESP_OK) {
            err = nvs_set_blob(nvs, "reception", &history, sizeof history);
            if (err == ESP_OK) err = nvs_commit(nvs);
            nvs_close(nvs);
        }
        if (err == ESP_OK) { history_dirty = false; ESP_LOGI(TAG, "regional reception history saved, mask=0x%02x", history.seen); }
        else ESP_LOGW(TAG, "could not save reception history: %s", esp_err_to_name(err));
        next_history_write = now + 60000;
    }
    return seen;
}

static esp_err_t device_add(uint8_t address, i2c_master_dev_handle_t *dev)
{
    i2c_device_config_t config = {.dev_addr_length = I2C_ADDR_BIT_LEN_7,
        .device_address = address, .scl_speed_hz = 100000};
    return i2c_master_bus_add_device(hardware_manifest_i2c_bus(), &config, dev);
}
static esp_err_t read_reg(uint8_t address, uint8_t reg, uint8_t *out, size_t length)
{
    i2c_master_dev_handle_t dev;
    esp_err_t err = device_add(address, &dev);
    if (err != ESP_OK) return err;
    err = i2c_master_transmit_receive(dev, &reg, 1, out, length, 100);
    i2c_master_bus_rm_device(dev);
    return err;
}
static bool crypto_command(i2c_master_dev_handle_t dev, uint8_t opcode, uint8_t mode,
                           uint16_t address, uint8_t *response, size_t length)
{
    uint8_t command[] = {3, 7, opcode, mode, address, address >> 8, 0, 0};
    uint16_t crc = hardware_crypto_crc(command + 1, 5);
    command[6] = crc; command[7] = crc >> 8;
    esp_err_t err = i2c_master_transmit(dev, command, sizeof command, 100);
    // Allow the slow clock-divider setting as well as the normal Info/Read path.
    vTaskDelay(pdMS_TO_TICKS(opcode == 0x1b ? 150 : 30));
    if (err == ESP_OK) err = i2c_master_receive(dev, response, length, 100);
    if (err != ESP_OK) { ESP_LOGW(TAG, "ATECC opcode 0x%02x transport: %s", opcode, esp_err_to_name(err)); return false; }
    if (response[0] == 4 && hardware_crypto_response(response, 4)) {
        ESP_LOGW(TAG, "ATECC opcode 0x%02x status=0x%02x", opcode, response[1]); return false;
    }
    return hardware_crypto_response(response, length);
}
static void crypto_rng_check(i2c_master_dev_handle_t dev)
{
    uint8_t response[35], previous[32];
    for (int i = 0; i < 3; i++) {
        // Random mode 1 requests no seed update. Discard all diagnostic samples.
        if (!crypto_command(dev, 0x1b, 1, 0, response, sizeof response)) {
            report.crypto.rng = 3;
            ESP_LOGW(TAG, "ATECC RNG unavailable; output is not trusted"); return;
        }
        if (!hardware_rng_sample_ok(response + 1, i ? previous : NULL)) {
            report.crypto.rng = 2;
            ESP_LOGE(TAG, "ATECC RNG FAIL: fixed/repeating output; output is not trusted"); return;
        }
        memcpy(previous, response + 1, sizeof previous);
    }
    memset(response, 0, sizeof response); memset(previous, 0, sizeof previous);
    report.crypto.rng = 1;
    ESP_LOGI(TAG, "ATECC RNG: 3 samples passed repetition screening; diagnostic only, not an entropy certification");
}
// crypto_open wakes the ATECC and opens a device handle. A sleeping ATECC does not ACK an
// ordinary scan. A 100 kHz zero-address wake token holds SDA low long enough; NACK on this
// token is expected. False means no handle could be opened; otherwise *woke says whether the
// wake response was confirmed, *err is that read's result, and the caller ends the session
// with crypto_close().
static bool crypto_open(i2c_master_dev_handle_t *dev, bool *woke, esp_err_t *err)
{
    i2c_master_dev_handle_t wake;
    if (device_add(0, &wake) != ESP_OK) return false;
    uint8_t zero = 0, response[4] = {0};
    (void)i2c_master_transmit(wake, &zero, 1, 100);
    i2c_master_bus_rm_device(wake);
    vTaskDelay(pdMS_TO_TICKS(10));
    if (device_add(0x60, dev) != ESP_OK) return false;
    *err = i2c_master_receive(*dev, response, 4, 100);
    *woke = *err == ESP_OK && hardware_crypto_response(response, 4) && response[1] == 0x11;
    return true;
}
static void crypto_close(i2c_master_dev_handle_t dev)
{
    uint8_t sleep = 1;
    (void)i2c_master_transmit(dev, &sleep, 1, 100);
    i2c_master_bus_rm_device(dev);
}
static void identify_crypto(void)
{
    report.crypto.checked_ms = esp_timer_get_time() / 1000;
    // Info, lock-byte Read, Random(no seed update), Sleep; no key/config writes or locks.
    i2c_master_dev_handle_t dev;
    bool woke;
    esp_err_t err;
    xSemaphoreTake(crypto_lock, portMAX_DELAY);
    if (!crypto_open(&dev, &woke, &err)) { xSemaphoreGive(crypto_lock); return; }
    uint8_t response[7] = {0};
    if (woke) {
        if (crypto_command(dev, 0x30, 0, 0, response, sizeof response)) {
            report.crypto.revision_valid = 1;
            memcpy(report.crypto.revision, response+1, 4);
            ESP_LOGI(TAG, "ATECC Info revision=%02x%02x%02x%02x%s (CRC verified)",
                response[1], response[2], response[3], response[4],
                !memcmp(response + 1, "\x00\x00\x60\x05", 4) ? "; ATECC608C" : "");
            if (response[1] == 0 && response[2] == 0 && response[3] == 0x60) {
                if (crypto_command(dev, 2, 0, 21, response, sizeof response)) {
                    report.crypto.config_lock = response[4] == 0x55 ? 1 : response[4] == 0 ? 2 : 3;
                    report.crypto.data_lock = response[3] == 0x55 ? 1 : response[3] == 0 ? 2 : 3;
                    ESP_LOGI(TAG, "ATECC config=%s data=%s (lock bytes %02x/%02x); provisioning not certified",
                        hardware_crypto_lock_state(response[4]), hardware_crypto_lock_state(response[3]), response[4], response[3]);
                } else ESP_LOGW(TAG, "ATECC lock state unknown");
                crypto_rng_check(dev);
            }
        } else ESP_LOGW(TAG, "ATECC woke, but Info revision was not verified (%s)", esp_err_to_name(err));
    } else ESP_LOGW(TAG, "ATECC at 0x60: wake response unconfirmed (%s)", esp_err_to_name(err));
    crypto_close(dev);
    xSemaphoreGive(crypto_lock);
}
static bool identifier_blank(const uint8_t *b, size_t n)
{
    bool zero = true, ones = true;
    for (size_t i = 0; i < n; i++) { zero = zero && b[i] == 0; ones = ones && b[i] == 0xff; }
    return zero || ones;
}
void observer_board_identity(observer_board_identity_t *out)
{
    *out = (observer_board_identity_t){0};
    nvf_board_identity_t identity;
    if (hardware_manifest_read_identity(&identity) == ESP_OK) {
        out->board_valid = identity.board_valid;
        memcpy(out->board_uid, identity.board_uid, sizeof out->board_uid);
        out->board_uid_address = identity.board_address;
        out->eeprom_valid = identity.eeprom_valid;
        memcpy(out->eeprom_uid, identity.eeprom_uid, sizeof out->eeprom_uid);
        out->eeprom_address = identity.eeprom_address;
    }
    out->revision_valid = report.manifest.capabilities_valid;
    out->revision = report.manifest.revision;
    if (!hardware_manifest_i2c_bus() || !crypto_lock) return;
#if CONFIG_NVF_BOARD_GNSS_COLOR_ZED_X20
    // MAX31328: its timekeeping registers answering is its presence; it has no serial.
    out->rtc_model_id = NVF_RTC_MAX31328;
    uint8_t rtc_registers[7];
    out->rtc_present = read_reg(NVF_I2C_RTC_TCXO, 0, rtc_registers, sizeof rtc_registers) == ESP_OK;
#else
    // MCP79412: verify the clock function at 0x6f as well as the factory EUI-64 in the
    // protected EEPROM block at 0x57. The EEPROM alone is not proof the expected RTC is fitted.
    out->rtc_model_id = NVF_RTC_MCP79412;
    uint8_t rtc_registers[7];
    out->rtc_present = read_reg(0x6f, 0, rtc_registers, sizeof rtc_registers) == ESP_OK;
    out->rtc_valid = out->rtc_present && read_reg(0x57, 0xf0, out->rtc_eui64, 8) == ESP_OK &&
                     !identifier_blank(out->rtc_eui64, 8);
#endif
    i2c_master_dev_handle_t dev;
    bool woke;
    esp_err_t err;
    xSemaphoreTake(crypto_lock, portMAX_DELAY);
    if (crypto_open(&dev, &woke, &err)) {
        uint8_t block[35], word[7];
        // Read is opcode 2; mode bit 7 selects a 32-byte block and bits 0-1 the zone.
        // Configuration block 0 is readable in every lock state: SN[0:3] is bytes 0-3 and
        // SN[4:8] is bytes 8-12.
        if (woke && crypto_command(dev, 2, 0x80, 0, block, sizeof block)) {
            memcpy(out->atecc_serial, block + 1, 4); memcpy(out->atecc_serial + 4, block + 1 + 8, 5);
            out->atecc_valid = !identifier_blank(out->atecc_serial, 9);
        }
        // Slot 14 (72 bytes) is two 32-byte blocks and two 4-byte words of the data zone:
        // address = slot << 3 | word offset, block in bits 8-11. The part refuses this read
        // until its data zone is locked, which leaves the record reported as unreadable.
        bool ok = woke && out->atecc_valid;
        for (unsigned b = 0; ok && b < 2; b++) {
            ok = crypto_command(dev, 2, 0x82, 14 << 3 | b << 8, block, sizeof block);
            if (ok) memcpy(out->attestation + 32 * b, block + 1, 32);
        }
        for (unsigned w = 0; ok && w < 2; w++) {
            ok = crypto_command(dev, 2, 0x02, 14 << 3 | 2 << 8 | w, word, sizeof word);
            if (ok) memcpy(out->attestation + 64 + 4 * w, word + 1, 4);
        }
        out->attestation_valid = ok && !identifier_blank(out->attestation, sizeof out->attestation);
        if (!out->attestation_valid) memset(out->attestation, 0, sizeof out->attestation);
        crypto_close(dev);
    }
    xSemaphoreGive(crypto_lock);
}
static void identify_peripherals(void)
{
    i2c_master_bus_handle_t bus = hardware_manifest_i2c_bus();
    if (!bus) { ESP_LOGW(TAG, "shared I2C bus unavailable"); return; }
    // This board's fitted parts (board_reservations.h); another board's are not probed.
    static const struct { uint8_t address; const char *name; } parts[] = {
        {NVF_I2C_TEMPERATURE, "temperature"}, {NVF_I2C_HUMIDITY, "humidity"},
#if CONFIG_NVF_BOARD_GNSS_COLOR_ZED_X20
        {NVF_I2C_RTC_TCXO, "RTC"}, {NVF_I2C_PRESSURE, "pressure"},
#elif CONFIG_NVF_BOARD_GNSS_COLOR_MAX
        {NVF_I2C_RTC, "RTC"}, {NVF_I2C_PRESSURE_ALT, "pressure"}, {NVF_I2C_IMU, "IMU"},
        {NVF_I2C_MAGNETOMETER, "magnetometer"},
#else
        {NVF_I2C_RTC, "RTC"}, {NVF_I2C_PRESSURE, "pressure"},
#endif
    };
    for (size_t i = 0; i < sizeof parts / sizeof parts[0]; i++) {
        const uint8_t address = parts[i].address;
        esp_err_t err = i2c_master_probe(bus, address, 100);
        ESP_LOGI(TAG, "%s at 0x%02x: %s", parts[i].name, address, esp_err_to_name(err));
        if (err != ESP_OK) continue;
        uint8_t data[7] = {0};
        if (address == 0x18 && read_reg(0x18, 6, data, 2) == ESP_OK &&
            read_reg(0x18, 7, data + 2, 2) == ESP_OK) {
            ESP_LOGI(TAG, "temperature IDs=%02x%02x/%02x%02x%s", data[0], data[1], data[2], data[3],
                data[0] == 0 && data[1] == 0x54 && data[2] == 4 ? "; MCP9808 verified" : "; unexpected identity");
        } else if (address == 0x40 && read_reg(0x40, 0xfc, data, 4) == ESP_OK) {
            ESP_LOGI(TAG, "humidity IDs=%02x%02x/%02x%02x%s", data[1], data[0], data[3], data[2],
                !memcmp(data, "\x49\x54\xd0\x07", 4) ? "; HDC2080/HDC2022 family verified (the manifest names the variant)" : "; unexpected identity");
        } else if (address == 0x76 && read_reg(0x76, 0, data, 1) == ESP_OK) {
            ESP_LOGI(TAG, "pressure chip ID=0x%02x%s", data[0], data[0] == 0x50 ?
                "; BMP388/BMP384 family (ID cannot distinguish them)" : "; unexpected identity");
        } else if (address == 0x6f && read_reg(0x6f, 0, data, 7) == ESP_OK) {
            ESP_LOGI(TAG, "RTC registers readable: oscillator=%s battery-enable=%s power-fail=%s; clock not adopted",
                data[3] & 0x20 ? "running" : "stopped", data[3] & 8 ? "yes" : "no", data[3] & 0x10 ? "yes" : "no");
        } else if (address == NVF_I2C_RTC_TCXO && read_reg(NVF_I2C_RTC_TCXO, 0x0e, data, 2) == ESP_OK) {
            ESP_LOGI(TAG, "MAX31328 control=0x%02x status=0x%02x: oscillator-stop flag %s; clock not adopted",
                data[0], data[1], data[1] & 0x80 ? "set" : "clear");
        }
    }
    identify_crypto();
}
static void clock_bit(int bit)
{
    gpio_set_level(LED_CLOCK, 0);
    gpio_set_level(LED_DATA, bit);
#if NVF_PIN_LED_PANEL_SDI >= 0
    // The optional front panel's own chain shows the same frame and shares clock,
    // latch and output enables, so both SDI lines carry each bit.
    gpio_set_level(NVF_PIN_LED_PANEL_SDI, bit);
#endif
    esp_rom_delay_us(2);
    gpio_set_level(LED_CLOCK, 1);
    esp_rom_delay_us(2);
    gpio_set_level(LED_CLOCK, 0);
}
static void panel_write(uint8_t green, uint8_t yellow)
{
    // OE also participates in TLC5916 mode switching. Hold both OE pins high
    // for the entire serial/latch transaction; PWM runs only while CLK is idle.
    if (pwm_ready) for (unsigned c = 0; c < 2; c++)
        ledc_stop(LEDC_LOW_SPEED_MODE, c, 1);
    // U12 (green) is nearest SDI; U13 (yellow) receives the first byte.
    uint16_t bits = ((uint16_t)yellow << 8) | green;
    for (int i = 15; i >= 0; i--) clock_bit((bits >> i) & 1);
    gpio_set_level(LED_LATCH, 1); esp_rom_delay_us(2); gpio_set_level(LED_LATCH, 0);
    if (pwm_ready && applied_brightness) for (unsigned c = 0; c < 2; c++) {
        ledc_set_duty(LEDC_LOW_SPEED_MODE, c, panel_pwm_off_ticks(applied_brightness));
        ledc_update_duty(LEDC_LOW_SPEED_MODE, c);
    }
}
static void heater_report(const env_heater_status_t *s, report_heater_t *h)
{
    const env_heater_run_t *r = &s->run;
    *h = (report_heater_t){.present = s->active};
    if (!s->active) return;
    h->state = s->state; h->flags = (s->utc_valid ? 1 : 0) | (s->last_known ? 2 : 0);
    h->runs = s->runs > 255 ? 255 : s->runs; h->last_utc = s->last_utc;
    h->rh95_s = s->rh95_ms / 1000; h->rh98_s = s->rh98_ms / 1000; h->streak_s = s->streak_ms / 1000;
    if (!s->runs) return;
    h->start_ms = r->start_ms; h->stop = r->stop; h->valid = r->valid;
    h->on_ms = r->on_ms > UINT32_MAX ? UINT32_MAX : r->on_ms;
    h->recovery_ms = r->recovery_ms > UINT32_MAX ? UINT32_MAX : r->recovery_ms;
    // Invalid measurements stay zero on the wire.
    if (r->valid & ENV_RUN_RH_BEFORE) h->rh_before = lround(r->rh_before * 100);
    if (r->valid & ENV_RUN_RH_STOP) h->rh_stop = lround(r->rh_stop * 100);
    if (r->valid & ENV_RUN_HDC_BEFORE) h->hdc_before = lround(r->hdc_before * 100);
    if (r->valid & ENV_RUN_HDC_PEAK) h->hdc_peak = lround(r->hdc_peak * 100);
    if (r->valid & ENV_RUN_HDC_END) h->hdc_end = lround(r->hdc_end * 100);
    if (r->valid & ENV_RUN_MCP_BEFORE) h->mcp_before = lround(r->mcp_before * 100);
    if (r->valid & ENV_RUN_MCP_PEAK) h->mcp_peak = lround(r->mcp_peak * 100);
    if (r->valid & ENV_RUN_MCP_END) h->mcp_end = lround(r->mcp_end * 100);
}
#if CONFIG_NVF_BOARD_GNSS_COLOR_MAX
// The latest IMU sample is reported only while it is this recent: still, the FIFO is
// drained every 1.28 s on INT1, or every 2.56 s without it.
#define MOTION_FRESH_MS 5000
static sensor_settings_t sensor_settings;
static const icm45686_profile_t *motion_profile;
static void max_sensors_start(void)
{
    esp_err_t err = sensor_settings_load(&sensor_settings);
    if (err != ESP_OK)
        ESP_LOGW(TAG, "sensor settings unreadable (%s); using the defaults", esp_err_to_name(err));
    motion_profile = sensor_settings.motion == SENSOR_MOTION_AERIAL ? &ICM45686_AERIAL : &ICM45686_SURFACE;
    ESP_LOGI(TAG, "sensor settings: %u Hz mains notch, %s motion profile", sensor_settings.mains_hz,
             sensor_motion_name(sensor_settings.motion));
    err = thermocouple_start(NVF_PIN_TC_SCK, NVF_PIN_TC_MOSI, NVF_PIN_TC_MISO, NVF_PIN_TC_CS_N,
                             NVF_PIN_TC_DRDY_N, sensor_settings.mains_hz == 50);
    if (err != ESP_OK) ESP_LOGE(TAG, "thermocouple interface unavailable: %s", esp_err_to_name(err));
    err = motion_start(hardware_manifest_i2c_bus(), NVF_PIN_IMU_INT1, NVF_PIN_IMU_INT2, motion_profile);
    if (err != ESP_OK) ESP_LOGE(TAG, "IMU service unavailable: %s", esp_err_to_name(err));
}
// A window sum over its time, rounded half away from zero.
static int16_t window_mean(int64_t sum, uint32_t span_ms)
{
    return (int16_t)(sum >= 0 ? (sum + span_ms / 2) / span_ms : -((-sum + span_ms / 2) / span_ms));
}
// Tags 11-13: the barometer, the thermocouple, and the IMU and magnetometer summary.
static void max_sensors_report(void)
{
    env_barometer_t barometer;
    env_magnetometer_t magnetometer;
    environment_sample_max(&barometer, &magnetometer);
    int64_t magnetometer_ms = esp_timer_get_time() / 1000;
    report_barometer_t *b = &report.barometer;
    *b = (report_barometer_t){.present = true, .state = barometer.state};
    if (barometer.valid) {
        b->valid = 1; b->centi_c = barometer.centi_c; b->pressure_pa = barometer.pressure_pa;
        b->flags = barometer.pressure_pa < MS5607_FULL_MIN_PA || barometer.pressure_pa > MS5607_FULL_MAX_PA;
    }
    env_thermocouple_t thermocouple;
    thermocouple_sample(&thermocouple);
    report_thermocouple_t *t = &report.thermocouple;
    *t = (report_thermocouple_t){.present = true, .state = thermocouple.configured,
        .config = MAX31856_TYPE_K | (thermocouple.filter_50hz ? 0x10 : 0)};
    if (thermocouple.configured) {
        const max31856_sample_t *s = &thermocouple.sample;
        t->valid = s->valid; t->flags = s->fresh; t->fault = s->fault;
        t->tc_centi_c = s->tc_centi_c; t->cj_centi_c = s->cj_centi_c;
    }
    motion_status_t motion;
    motion_snapshot(&motion);
    int64_t now = esp_timer_get_time() / 1000;
    report_motion_t *m = &report.motion;
    const icm45686_profile_t *p = motion_profile;
    *m = (report_motion_t){.present = true, .imu_state = motion.ready, .mag_state = magnetometer.ready,
        .profile = sensor_settings.motion, .moving = motion.stats.moving,
        .rate_decihz = motion.stats.moving ? p->moving_decihz : ICM45686_STILL_DECIHZ,
        .accel_fs_g = p->accel_fs_g, .gyro_fs_dps = p->gyro_fs_dps, .packets = motion.stats.packets,
        .overflows = motion.stats.overflows, .resyncs = motion.stats.resyncs,
        .rate_changes = motion.stats.rate_changes};
    if (motion.ready && motion.stats.latest_valid && (int64_t)motion.latest_ms + MOTION_FRESH_MS >= now) {
        m->valid |= 1;
        memcpy(m->accel, motion.stats.accel, sizeof m->accel);
        memcpy(m->gyro, motion.stats.gyro, sizeof m->gyro);
        m->imu_centi_c = (int16_t)(motion.stats.temp * 50 + 2500); // FIFO temperature: C = raw / 2 + 25
        m->imu_ms = motion.latest_ms;
    }
    const icm45686_window_t *w = &motion.stats.window[ICM45686_WINDOW_REPORT];
    if (w->valid && w->span_ms) {
        // One count is range/32768 of a g or of a degree per second.
        double g = p->accel_fs_g / 32768.0, dps = p->gyro_fs_dps / 32768.0;
        m->valid |= 2;
        m->window_samples = w->samples; m->window_ms = w->span_ms;
        for (unsigned axis = 0; axis < 3; axis++) {
            m->accel_mean[axis] = window_mean(w->accel_sum[axis], w->span_ms);
            m->gyro_mean[axis] = window_mean(w->gyro_sum[axis], w->span_ms);
        }
        m->accel_min_mg = (uint16_t)lround(sqrt((double)w->accel_min_sq) * g * 1000.0);
        m->accel_max_mg = (uint16_t)lround(sqrt((double)w->accel_max_sq) * g * 1000.0);
        m->gyro_max_decidps = (uint16_t)lround(sqrt((double)w->gyro_max_sq) * dps * 10.0);
    }
    if (magnetometer.valid) {
        m->valid |= 4;
        memcpy(m->mag, magnetometer.sample.field, sizeof m->mag);
        memcpy(m->mag_offset, magnetometer.sample.offset, sizeof m->mag_offset);
        m->mag_ms = (uint64_t)magnetometer_ms;
    }
}
#endif
static void report_poll(const gnss_status_t *status, int64_t now, uint8_t expected, bool utc_valid, int64_t utc)
{
    bool event_pending = status->event_count != report_policy.last.event_count;
    bool force = event_pending && now - last_environment >= 5000;
    if (now < next_environment && !force) return;
    env_sample_t sample;
    env_heater_status_t heater;
    report_environment_t *e = &report.environment;
    *e = (report_environment_t){0};
    environment_sample(hardware_manifest_i2c_bus(), now, utc_valid, utc, &sample, &e->ready, &heater);
#if CONFIG_NVF_BOARD_GNSS_COLOR_MAX
    max_sensors_report();
#endif
    // After every sample, so no component's time is later than the report's.
    last_environment = esp_timer_get_time() / 1000;
    next_environment = last_environment + 30000;
    if (sample.mcp_valid) { e->valid |= 1; e->mcp_centi_c = lround(sample.mcp_c * 100); }
    if (sample.hdc_valid) { e->valid |= 2; e->hdc_centi_c = lround(sample.hdc_c * 100); e->rh_centi_percent = lround(sample.rh_percent * 100); }
    if (sample.bmp_valid) { e->valid |= 4; e->bmp_centi_c = lround(sample.bmp_c * 100); e->pressure_pa = lround(sample.pressure_pa); }
    heater_report(&heater, &report.heater);
    report.uptime_ms = last_environment;
    report.event_count = status->event_count; report.event_ms = status->event_ms;
    report.event_flags = status->event_flags; report.event_states = status->event_states;
    report.rtc = observer_rtc_status();
    report.reason = observer_report_due(&report_policy, &report);
    if (!report.reason) return;
    report_receiver_t *r = &report.receiver;
    *r = (report_receiver_t){.supported=status->supported, .expected=expected,
        .jam=status->jam, .spoof=status->spoof, .rf_ms=status->rf_ms, .status_ms=status->status_ms};
    bool sats_fresh = status->satellites_valid && now >= status->satellites_ms && now - status->satellites_ms <= 15000;
    if (sats_fresh) { r->valid |= 1; memcpy(r->tracked, status->tracked, 8); }
    if (gnss_status_position_fresh(status, now)) r->valid |= 2;
    if (status->rf_valid && now >= status->rf_ms && now - status->rf_ms <= 15000) r->valid |= 4;
    if (status->status_valid && now >= status->status_ms && now - status->status_ms <= 15000) r->valid |= 8;
    size_t used, capacity, count; bool psram;
    spool_memory_stats(&used, &capacity, &psram);
    spool_stats(NULL, &report.resources.dropped, &count);
    report.resources.psram=psram; report.resources.used=used;
    report.resources.capacity=capacity; report.resources.queued=count;
    report.resources.internal_free=heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    report.resources.psram_free=heap_caps_get_free_size(MALLOC_CAP_SPIRAM);
    // Static: the board task is the only caller, and its stack is sized for its work, not these.
    static uint8_t body[OBSERVER_REPORT_MAX], record[OBSERVER_REPORT_MAX + GNF1_RECORD_HDR];
    size_t length = observer_report_encode(body, sizeof body, &report);
    if (!length) return;
    size_t n = gnf1_encode_telem(record, report_now_ns ? report_now_ns() : 0, GNF1_T_OBSERVER, body, length);
    if (n && spool_append(record, n)) {
        observer_report_sent(&report_policy, &report);
#if CONFIG_NVF_BOARD_GNSS_COLOR_MAX
        motion_report_sent();
#endif
        ESP_LOGI(TAG, "ObserverDetails queued: reason=0x%02x environment=0x%02x RTC=0x%02x EEPROM=%u RNG=%u events=%lu bytes=%u",
            report.reason, e->valid, report.rtc.flags, report.manifest.action, report.crypto.rng,
            (unsigned long)report.event_count, (unsigned)n);
    }
}
static size_t append_update(uint8_t *body,size_t length,size_t capacity) {
    if(!length || length+143>capacity)return length;
    if(!nvf_update_wire(body+length+3))return length;
    body[length]=9;body[length+1]=0;body[length+2]=140;return length+143;
}
static void timing_poll(const gnss_status_t *status)
{
    uint64_t now=esp_timer_get_time()/1000;
    pulse_timing_poll(now*1000,&report.timing);
    observer_rtc_square_wave_status(&report.timing.rtc_state,&report.timing.rtc_control,&report.timing.rtc_trim);
    if (status->tp_valid && now >= (uint64_t)status->tp_ms && now-status->tp_ms <= 3000) {
        report.timing.flags |= 2; report.timing.tp_flags=status->tp_flags;
        report.timing.tp_ref=status->tp_ref; report.timing.tp_ms=status->tp_ms;
    }
    if (now < next_timing) return;
    next_timing=now+1000;
    static uint8_t body[OBSERVER_REPORT_MAX], record[OBSERVER_REPORT_MAX+GNF1_RECORD_HDR];
    size_t length=observer_timing_encode(body,sizeof body,&report.timing,esp_timer_get_time()/1000);
    if (!length) return;
    length=append_update(body,length,sizeof body);
    size_t n=gnf1_encode_telem(record,report_now_ns ? report_now_ns() : 0,GNF1_T_OBSERVER,body,length);
    if (n) (void)spool_append(record,n);
    // Same versioned bytes as collector telemetry. A serial capture can produce
    // plots and count audits before networking/enrollment is commissioned.
    static const char digits[]="0123456789abcdef";
    static char hex[2*OBSERVER_REPORT_MAX+1];
    for (size_t i=0;i<length;i++) { hex[2*i]=digits[body[i]>>4]; hex[2*i+1]=digits[body[i]&15]; }
    hex[2*length]=0;
    ESP_LOGI("pulse_timing","sample=%s",hex);
}
static void board_task(void *arg)
{
    (void)arg;
    esp_err_t timing_error=pulse_timing_start();
    if (timing_error != ESP_OK) ESP_LOGW(TAG,"pulse capture unavailable: %s; GNSS continues",esp_err_to_name(timing_error));
    identify_peripherals();
#if CONFIG_NVF_BOARD_GNSS_COLOR_MAX
    max_sensors_start();
#endif
    history_load();
    uint8_t previous_green = 255, previous_yellow = 255;
    bool brightness_dirty = false;
    int64_t next_brightness_write = 0;
    for (;;) {
        gnss_status_t status;
        receiver_status(&status);
        uint8_t green, yellow;
        int64_t now = esp_timer_get_time() / 1000;
        uint8_t learned = reception_history(&status, now);
        gnss_status_leds(&status, now, learned, pusher_connected(), &green, &yellow);
        unsigned requested = atomic_load(&brightness);
        bool brightness_changed = requested != applied_brightness;
        applied_brightness = requested;
        if (green != previous_green || yellow != previous_yellow || brightness_changed) {
            panel_write(green, yellow);
            if (brightness_changed) ESP_LOGI(TAG, "panel brightness=%u%%", applied_brightness);
            ESP_LOGI(TAG, "panel green=0x%02x yellow=0x%02x (GPS SBAS GAL BDS QZSS GLO NavIC uplink)", green, yellow);
            previous_green = green; previous_yellow = yellow;
        }
        unsigned reference = atomic_load(&trimmer_reference);
        if (brightness_changed || reference != saved_reference) brightness_dirty = true;
        if (brightness_dirty && now >= next_brightness_write) {
            esp_err_t err = panel_brightness_save(applied_brightness, reference);
            if (err == ESP_OK) {
                brightness_dirty = false;
                saved_reference = reference;
                ESP_LOGI(TAG, "panel brightness saved=%u%%", applied_brightness);
            } else {
                ESP_LOGW(TAG, "could not save panel brightness: %s; retrying in 60 seconds",
                         esp_err_to_name(err));
            }
            next_brightness_write = err == ESP_OK ? 0 : now + 60000;
        }
        observer_rtc_poll(hardware_manifest_i2c_bus(), &status, now);
        report_rtc_t rtc = observer_rtc_status();
        int64_t utc;
        bool utc_valid = rtc_trusted_utc(&utc_candidate, &status, rtc.flags, (int64_t)rtc.epoch,
                                         (int64_t)rtc.sampled_ms, now, &utc) != RTC_UTC_UNKNOWN;
        report_poll(&status, now, gnss_status_expected(&status, now, learned), utc_valid, utc);
        // Every pass: while the humidity heater is on it converts at its own step interval.
        environment_poll(esp_timer_get_time() / 1000);
        timing_poll(&status);
        observer_report_t journal_report=report;
        journal_report.rtc=observer_rtc_status();
        journal_poll(&status,&journal_report,esp_timer_get_time()/1000);
        vTaskDelay(pdMS_TO_TICKS(500));
    }
}
#if NVF_PIN_BRIGHT_BUTTON >= 0
#define PANEL_INPUT_POLL_MS 20u
#define PANEL_TRIMMER_SAMPLE_MS 200u
#if NVF_PIN_BRIGHT_ADC >= 0
static adc_oneshot_unit_handle_t trimmer_unit;
static adc_cali_handle_t trimmer_cali;
static adc_channel_t trimmer_channel;
static bool trimmer_init(void)
{
    adc_unit_t unit;
    if (adc_oneshot_io_to_channel(NVF_PIN_BRIGHT_ADC, &unit, &trimmer_channel) != ESP_OK) return false;
    adc_oneshot_unit_init_cfg_t unit_config = {.unit_id = unit};
    adc_oneshot_chan_cfg_t channel = {.atten = ADC_ATTEN_DB_12, .bitwidth = ADC_BITWIDTH_DEFAULT};
    adc_cali_curve_fitting_config_t cali = {.unit_id = unit, .chan = trimmer_channel,
        .atten = ADC_ATTEN_DB_12, .bitwidth = ADC_BITWIDTH_DEFAULT};
    if (adc_oneshot_new_unit(&unit_config, &trimmer_unit) != ESP_OK) return false;
    if (adc_oneshot_config_channel(trimmer_unit, trimmer_channel, &channel) != ESP_OK ||
        adc_cali_create_scheme_curve_fitting(&cali, &trimmer_cali) != ESP_OK) {
        adc_oneshot_del_unit(trimmer_unit); trimmer_unit = NULL; return false;
    }
    return true;
}
// The average of eight calibrated conversions as a brightness percent; 0 when the wiper
// is open or a conversion fails.
static unsigned trimmer_read(void)
{
    if (!trimmer_unit) return 0;
    int sum = 0;
    for (unsigned i = 0; i < 8; i++) {
        int mv;
        if (adc_oneshot_get_calibrated_result(trimmer_unit, trimmer_cali, trimmer_channel, &mv) != ESP_OK) return 0;
        sum += mv;
    }
    return panel_trimmer_percent(sum / 8, CONFIG_NVF_PANEL_TRIMMER_OPEN_MV);
}
#else
static bool trimmer_init(void) { return false; }
static unsigned trimmer_read(void) { return 0; }
#endif
// Owns the brightness choice on boards with preset buttons and a trimmer: the control
// that changed last wins (panel_brightness_t), and the board task applies and saves it.
static void panel_input_task(void *arg)
{
    (void)arg;
    const gpio_config_t button = {.pin_bit_mask = NVF_PIN(NVF_PIN_BRIGHT_BUTTON), .mode = GPIO_MODE_INPUT,
        .pull_up_en = GPIO_PULLUP_DISABLE, .pull_down_en = GPIO_PULLDOWN_DISABLE}; // R47 pulls up
    if (gpio_config(&button) != ESP_OK) { ESP_LOGE(TAG, "brightness button GPIO%d unavailable", NVF_PIN_BRIGHT_BUTTON); vTaskDelete(NULL); }
    bool trimmer = trimmer_init();
    if (!trimmer) ESP_LOGW(TAG, "brightness trimmer unavailable; presets only");
    panel_brightness_t lamp = {.percent = atomic_load(&brightness), .reference = atomic_load(&trimmer_reference)};
    panel_button_t press = {0};
    bool settled = false;
    unsigned position = 0, elapsed = 0;
    for (;;) {
        vTaskDelay(pdMS_TO_TICKS(PANEL_INPUT_POLL_MS));
        elapsed += PANEL_INPUT_POLL_MS;
        bool sampled = trimmer && elapsed % PANEL_TRIMMER_SAMPLE_MS == 0;
        if (sampled) position = trimmer_read();
        bool changed = false;
        if (!settled) {
            // Ignore both controls until the wiper filter settles.
            if (elapsed < PANEL_TRIMMER_SETTLE_MS) continue;
            settled = true;
            unsigned saved = lamp.percent;
            if (trimmer) position = trimmer_read();
            panel_brightness_boot(&lamp, lamp.percent, lamp.reference, position);
            if (lamp.percent != saved) ESP_LOGI(TAG, "brightness trimmer moved while off: %u%%", lamp.percent);
            changed = true;
        } else if (sampled) {
            changed = panel_brightness_trimmer(&lamp, position);
        }
        if (panel_button_short_press(&press, gpio_get_level(NVF_PIN_BRIGHT_BUTTON) == 0, PANEL_INPUT_POLL_MS)) {
            panel_brightness_preset(&lamp, position);
            changed = true;
        }
        if (changed) {
            atomic_store(&trimmer_reference, lamp.reference);
            observer_board_set_brightness(lamp.percent);
        }
    }
}
#endif
esp_err_t observer_board_start(void)
{
    // Restore before PWM starts, including in provisioning mode.
    esp_err_t load_err = panel_brightness_load(&applied_brightness, &saved_reference);
    if (load_err != ESP_OK)
        ESP_LOGW(TAG, "could not load panel brightness: %s; using %u%%",
                 esp_err_to_name(load_err), applied_brightness);
    atomic_store(&brightness, applied_brightness);
    atomic_store(&trimmer_reference, saved_reference);
    if (!crypto_lock && !(crypto_lock = xSemaphoreCreateMutex())) return ESP_ERR_NO_MEM;
    uint64_t outputs = (1ULL << LED_DATA) | (1ULL << LED_CLOCK) | (1ULL << LED_LATCH) |
                       (1ULL << LED_GREEN_OE) | (1ULL << LED_YELLOW_OE);
#if NVF_PIN_LED_PANEL_SDI >= 0
    outputs |= NVF_PIN(NVF_PIN_LED_PANEL_SDI);
#endif
    gpio_set_level(LED_GREEN_OE, 1); gpio_set_level(LED_YELLOW_OE, 1);
    gpio_config_t config = {.pin_bit_mask = outputs, .mode = GPIO_MODE_OUTPUT};
    esp_err_t err = gpio_config(&config);
    if (err != ESP_OK) return err;
    gpio_set_level(LED_LATCH, 0);
    // Explicit normal-mode sequence, including after an MCU-only reset.
    const int oe[] = {1, 0, 1, 1, 1};
    for (size_t i = 0; i < sizeof oe / sizeof oe[0]; i++) {
        gpio_set_level(LED_GREEN_OE, oe[i]); gpio_set_level(LED_YELLOW_OE, oe[i]); clock_bit(0);
    }
    panel_write(0, 0xbf); // six constellations and a disconnected uplink until the receiver reports
    ledc_timer_config_t timer = {.speed_mode=LEDC_LOW_SPEED_MODE, .duty_resolution=LEDC_TIMER_10_BIT,
        .timer_num=LEDC_TIMER_0, .freq_hz=4000, .clk_cfg=LEDC_AUTO_CLK};
    err = ledc_timer_config(&timer);
    if (err != ESP_OK) return err;
    const int pins[] = {LED_GREEN_OE, LED_YELLOW_OE};
    for (unsigned c=0; c<2; c++) {
        ledc_channel_config_t channel = {.gpio_num=pins[c], .speed_mode=LEDC_LOW_SPEED_MODE,
            .channel=c, .timer_sel=LEDC_TIMER_0, .duty=panel_pwm_off_ticks(applied_brightness)};
        err = ledc_channel_config(&channel);
        if (err != ESP_OK) return err;
    }
    pwm_ready = true;
    ESP_LOGI(TAG, "panel brightness=%u%% (4 kHz PWM)", applied_brightness);
#if NVF_PIN_BRIGHT_BUTTON >= 0
    if (xTaskCreate(panel_input_task, "panel_input", 3072, NULL, 4, NULL) != pdPASS) return ESP_ERR_NO_MEM;
#endif
    return xTaskCreate(board_task, "board", 6144, NULL, 3, NULL) == pdPASS ? ESP_OK : ESP_ERR_NO_MEM;
}
#else
void observer_board_set_brightness(unsigned percent) { (void)percent; }
void observer_board_cycle_brightness(void) {}
void observer_board_manifest(const hardware_manifest_result_t *manifest, uint64_t (*now_ns)(void))
{ (void)manifest; (void)now_ns; }
esp_err_t observer_board_start(void) { return ESP_OK; }
void observer_board_identity(observer_board_identity_t *out) { *out = (observer_board_identity_t){0}; }
#endif
