#include "board.h"
#include "sdkconfig.h"
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
#include <string.h>
#include "driver/gpio.h"
#include "driver/i2c_master.h"
#include "esp_log.h"
#include "esp_rom_sys.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "hardware_manifest.h"
#include "hardware_checks.h"
#include "receiver.h"
#include "observer_rtc.h"
#include "pusher.h"
#include "nvs.h"

static const char *TAG = "observer_board";
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
            ESP_LOGW(TAG, "ATECC RNG unavailable; output is not trusted"); return;
        }
        if (!hardware_rng_sample_ok(response + 1, i ? previous : NULL)) {
            ESP_LOGE(TAG, "ATECC RNG FAIL: fixed/repeating output; output is not trusted"); return;
        }
        memcpy(previous, response + 1, sizeof previous);
    }
    memset(response, 0, sizeof response); memset(previous, 0, sizeof previous);
    ESP_LOGI(TAG, "ATECC RNG: 3 samples passed repetition screening; diagnostic only, not an entropy certification");
}
static void identify_crypto(void)
{
    // A sleeping ATECC does not ACK an ordinary scan. A 100 kHz zero-address
    // wake token holds SDA low long enough; NACK on this token is expected.
    // Info, lock-byte Read, Random(no seed update), Sleep; no key/config writes or locks.
    i2c_master_dev_handle_t wake, dev;
    if (device_add(0, &wake) != ESP_OK) return;
    uint8_t zero = 0;
    (void)i2c_master_transmit(wake, &zero, 1, 100);
    i2c_master_bus_rm_device(wake);
    vTaskDelay(pdMS_TO_TICKS(10));
    if (device_add(0x60, &dev) != ESP_OK) return;
    uint8_t response[7] = {0};
    esp_err_t err = i2c_master_receive(dev, response, 4, 100);
    if (err == ESP_OK && hardware_crypto_response(response, 4) && response[1] == 0x11) {
        if (crypto_command(dev, 0x30, 0, 0, response, sizeof response)) {
            ESP_LOGI(TAG, "ATECC Info revision=%02x%02x%02x%02x%s (CRC verified)",
                response[1], response[2], response[3], response[4],
                !memcmp(response + 1, "\x00\x00\x60\x05", 4) ? "; ATECC608C" : "");
            if (response[1] == 0 && response[2] == 0 && response[3] == 0x60) {
                if (crypto_command(dev, 2, 0, 21, response, sizeof response))
                    ESP_LOGI(TAG, "ATECC config=%s data=%s (lock bytes %02x/%02x); provisioning not certified",
                        hardware_crypto_lock_state(response[4]), hardware_crypto_lock_state(response[3]), response[4], response[3]);
                else ESP_LOGW(TAG, "ATECC lock state unknown");
                crypto_rng_check(dev);
            }
        } else ESP_LOGW(TAG, "ATECC woke, but Info revision was not verified (%s)", esp_err_to_name(err));
    } else ESP_LOGW(TAG, "ATECC at 0x60: wake response unconfirmed (%s)", esp_err_to_name(err));
    uint8_t sleep = 1;
    (void)i2c_master_transmit(dev, &sleep, 1, 100);
    i2c_master_bus_rm_device(dev);
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
    // U12 (green) is nearest SDI; U13 (yellow) receives the first byte.
    uint16_t bits = ((uint16_t)yellow << 8) | green;
    for (int i = 15; i >= 0; i--) clock_bit((bits >> i) & 1);
    gpio_set_level(LED_LATCH, 1); esp_rom_delay_us(2); gpio_set_level(LED_LATCH, 0);
}
static void board_task(void *arg)
{
    (void)arg;
    identify_peripherals();
    history_load();
    uint8_t previous_green = 255, previous_yellow = 255;
    for (;;) {
        gnss_status_t status;
        receiver_status(&status);
        uint8_t green, yellow;
        int64_t now = esp_timer_get_time() / 1000;
        gnss_status_leds(&status, now, reception_history(&status, now), pusher_connected(), &green, &yellow);
        if (green != previous_green || yellow != previous_yellow) {
            panel_write(green, yellow);
            ESP_LOGI(TAG, "panel green=0x%02x yellow=0x%02x (GPS SBAS GAL BDS QZSS GLO NavIC uplink)", green, yellow);
            previous_green = green; previous_yellow = yellow;
        }
        observer_rtc_poll(hardware_manifest_i2c_bus(), &status, now);
        vTaskDelay(pdMS_TO_TICKS(500));
    }
}
esp_err_t observer_board_start(void)
{
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
    gpio_set_level(LED_GREEN_OE, 0); gpio_set_level(LED_YELLOW_OE, 0);
    return xTaskCreate(board_task, "board", 4096, NULL, 3, NULL) == pdPASS ? ESP_OK : ESP_ERR_NO_MEM;
}
#else
esp_err_t observer_board_start(void) { return ESP_OK; }
#endif
