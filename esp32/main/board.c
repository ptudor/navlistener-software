#include "update_runtime.h"
#include "board.h"
#include "sdkconfig.h"
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
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
#include "receiver.h"
#include "observer_rtc.h"
#include "environment.h"
#include "pusher.h"
#include "nvs.h"
#include "journal.h"
#include "pulse_timing.h"

static const char *TAG = "observer_board";
static observer_report_t report;
static report_policy_t report_policy;
static uint64_t (*report_now_ns)(void);
static int64_t next_environment, last_environment = -5000;
static atomic_uint brightness = PANEL_DEFAULT_BRIGHTNESS;
static uint64_t next_timing;
static unsigned applied_brightness = PANEL_DEFAULT_BRIGHTNESS;
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
    report.manifest.eui_valid = manifest->eui64_valid;
    if (manifest->eui64_valid) memcpy(report.manifest.eui, manifest->eui64, 8);
    report.manifest.capabilities_valid = manifest->capabilities_valid;
    if (manifest->capabilities_valid) {
        report.manifest.revision = manifest->capabilities.revision;
        report.manifest.component_count = manifest->capabilities.component_count;
    }
    snprintf(report.firmware, sizeof report.firmware, "%s", esp_app_get_description()->version);
}
enum { LED_DATA = 14, LED_CLOCK = 11, LED_LATCH = 12, LED_GREEN_OE = 47, LED_YELLOW_OE = 48 };
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
    // Written once by observer_board_manifest() before any task runs.
    out->board_valid = report.manifest.eui_valid && !identifier_blank(report.manifest.eui, 8);
    memcpy(out->board_eui64, report.manifest.eui, 8);
    out->revision_valid = report.manifest.capabilities_valid;
    out->revision = report.manifest.revision;
    if (!hardware_manifest_i2c_bus() || !crypto_lock) return;
    // MCP79412: the factory EUI-64 is in the protected EEPROM block, device 0x57, 0xF0..0xF7.
    out->rtc_valid = read_reg(0x57, 0xf0, out->rtc_eui64, 8) == ESP_OK && !identifier_blank(out->rtc_eui64, 8);
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
    const uint8_t addresses[] = {0x18, 0x40, 0x6f, 0x76};
    const char *names[] = {"temperature", "humidity", "RTC", "pressure"};
    for (size_t i = 0; i < sizeof addresses; i++) {
        esp_err_t err = i2c_master_probe(bus, addresses[i], 100);
        ESP_LOGI(TAG, "%s at 0x%02x: %s", names[i], addresses[i], esp_err_to_name(err));
        if (err != ESP_OK) continue;
        uint8_t data[7] = {0};
        if (addresses[i] == 0x18 && read_reg(0x18, 6, data, 2) == ESP_OK &&
            read_reg(0x18, 7, data + 2, 2) == ESP_OK) {
            ESP_LOGI(TAG, "temperature IDs=%02x%02x/%02x%02x%s", data[0], data[1], data[2], data[3],
                data[0] == 0 && data[1] == 0x54 && data[2] == 4 ? "; MCP9808 verified" : "; unexpected identity");
        } else if (addresses[i] == 0x40 && read_reg(0x40, 0xfc, data, 4) == ESP_OK) {
            ESP_LOGI(TAG, "humidity IDs=%02x%02x/%02x%02x%s", data[1], data[0], data[3], data[2],
                !memcmp(data, "\x49\x54\xd0\x07", 4) ? "; HDC2080 verified" : "; unexpected identity");
        } else if (addresses[i] == 0x76 && read_reg(0x76, 0, data, 1) == ESP_OK) {
            ESP_LOGI(TAG, "pressure chip ID=0x%02x%s", data[0], data[0] == 0x50 ?
                "; BMP388/BMP384 family (ID cannot distinguish them)" : "; unexpected identity");
        } else if (addresses[i] == 0x6f && read_reg(0x6f, 0, data, 7) == ESP_OK) {
            ESP_LOGI(TAG, "RTC registers readable: oscillator=%s battery-enable=%s power-fail=%s; clock not adopted",
                data[3] & 0x20 ? "running" : "stopped", data[3] & 8 ? "yes" : "no", data[3] & 0x10 ? "yes" : "no");
        }
    }
    identify_crypto();
}
static void clock_bit(int bit)
{
    gpio_set_level(LED_CLOCK, 0);
    gpio_set_level(LED_DATA, bit);
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
static void report_poll(const gnss_status_t *status, int64_t now, uint8_t expected)
{
    bool event_pending = status->event_count != report_policy.last.event_count;
    bool force = event_pending && now - last_environment >= 5000;
    if (now < next_environment && !force) return;
    env_sample_t sample;
    report_environment_t *e = &report.environment;
    *e = (report_environment_t){0};
    environment_sample(hardware_manifest_i2c_bus(), &sample, &e->ready);
    last_environment = esp_timer_get_time() / 1000;
    next_environment = last_environment + 30000;
    if (sample.mcp_valid) { e->valid |= 1; e->mcp_centi_c = lround(sample.mcp_c * 100); }
    if (sample.hdc_valid) { e->valid |= 2; e->hdc_centi_c = lround(sample.hdc_c * 100); e->rh_centi_percent = lround(sample.rh_percent * 100); }
    if (sample.bmp_valid) { e->valid |= 4; e->bmp_centi_c = lround(sample.bmp_c * 100); e->pressure_pa = lround(sample.pressure_pa); }
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
    uint8_t body[OBSERVER_REPORT_MAX], record[OBSERVER_REPORT_MAX + GNF1_RECORD_HDR];
    size_t length = observer_report_encode(body, sizeof body, &report);
    if (!length) return;
    size_t n = gnf1_encode_telem(record, report_now_ns ? report_now_ns() : 0, GNF1_T_OBSERVER, body, length);
    if (n && spool_append(record, n)) {
        observer_report_sent(&report_policy, &report);
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
    uint8_t body[OBSERVER_REPORT_MAX], record[OBSERVER_REPORT_MAX+GNF1_RECORD_HDR];
    size_t length=observer_timing_encode(body,sizeof body,&report.timing,esp_timer_get_time()/1000);
    if (!length) return;
    length=append_update(body,length,sizeof body);
    size_t n=gnf1_encode_telem(record,report_now_ns ? report_now_ns() : 0,GNF1_T_OBSERVER,body,length);
    if (n) (void)spool_append(record,n);
    // Same versioned bytes as collector telemetry. A serial capture can produce
    // plots and count audits before networking/enrollment is commissioned.
    static const char digits[]="0123456789abcdef";
    char hex[2*OBSERVER_REPORT_MAX+1];
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
        if (brightness_changed) brightness_dirty = true;
        if (brightness_dirty && now >= next_brightness_write) {
            esp_err_t err = panel_brightness_save(applied_brightness);
            if (err == ESP_OK) {
                brightness_dirty = false;
                ESP_LOGI(TAG, "panel brightness saved=%u%%", applied_brightness);
            } else {
                ESP_LOGW(TAG, "could not save panel brightness: %s; retrying in 60 seconds",
                         esp_err_to_name(err));
            }
            next_brightness_write = err == ESP_OK ? 0 : now + 60000;
        }
        observer_rtc_poll(hardware_manifest_i2c_bus(), &status, now);
        report_poll(&status, now, gnss_status_expected(&status, now, learned));
        timing_poll(&status);
        observer_report_t journal_report=report;
        journal_report.rtc=observer_rtc_status();
        journal_poll(&status,&journal_report,esp_timer_get_time()/1000);
        vTaskDelay(pdMS_TO_TICKS(500));
    }
}
esp_err_t observer_board_start(void)
{
    // Restore before PWM starts, including in provisioning mode.
    esp_err_t load_err = panel_brightness_load(&applied_brightness);
    if (load_err != ESP_OK)
        ESP_LOGW(TAG, "could not load panel brightness: %s; using %u%%",
                 esp_err_to_name(load_err), applied_brightness);
    atomic_store(&brightness, applied_brightness);
    if (!crypto_lock && !(crypto_lock = xSemaphoreCreateMutex())) return ESP_ERR_NO_MEM;
    const uint64_t outputs = (1ULL << LED_DATA) | (1ULL << LED_CLOCK) | (1ULL << LED_LATCH) |
                            (1ULL << LED_GREEN_OE) | (1ULL << LED_YELLOW_OE);
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
    panel_write(0, 0xbf); // NEO-M9N's six constellations and disconnected uplink
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
