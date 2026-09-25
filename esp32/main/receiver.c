// UART discovery is independent of EEPROM presence and network provisioning.
#include "receiver.h"
#include "ubx_probe.h"
#include <string.h>
#include "sdkconfig.h"
#include "driver/uart.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "esp_task_wdt.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#ifndef CONFIG_NVF_RX_AUTOPROBE
#define CONFIG_NVF_RX_AUTOPROBE 0
#endif
#ifndef CONFIG_NVF_RX_CONFIGURE
#define CONFIG_NVF_RX_CONFIGURE 0
#endif
// The receiver each board carries, as MON-VER names it. Only that model is configured.
#if CONFIG_NVF_BOARD_GNSS_COLOR_ZED_X20
#define RX_MODULE "ZED-X20P"
#elif CONFIG_NVF_BOARD_GNSS_COLOR_MAX
#define RX_MODULE "MAX-M10S"
#else
#define RX_MODULE "NEO-M9N"
#endif
#define RX_UART UART_NUM_1
#if CONFIG_NVF_BOARD_GNSS_COLOR
#define RX_PIN_RX 5
#define RX_PIN_TX 4
#else
// C6 GPIO9 is a strapping pin; preserve the documented bench wiring.
#define RX_PIN_RX 9
#define RX_PIN_TX 10
#endif
static const char *TAG = "receiver";
static atomic_uint s_ticks;
static bool version_seen, expected_model;
static int cfg_ack;
static gnss_status_t health = {.supported = 0x6f}, snapshot = {.supported = 0x6f};
static portMUX_TYPE health_lock = portMUX_INITIALIZER_UNLOCKED;

static void observe(uint8_t cls, uint8_t id, const uint8_t *body, size_t len, void *ctx)
{
    (void)ctx;
    gnss_status_feed(&health, cls, id, body, len, esp_timer_get_time() / 1000);
    taskENTER_CRITICAL(&health_lock);
    snapshot = health;
    taskEXIT_CRITICAL(&health_lock);
    if (cls == 0x0a && id == 0x04 && len >= 40) {
        char sw[31], hw[11];
        for (size_t i = 0; i < 30; i++) sw[i] = body[i] >= 32 && body[i] <= 126 ? body[i] : ' ';
        for (size_t i = 0; i < 10; i++) hw[i] = body[30+i] >= 32 && body[30+i] <= 126 ? body[30+i] : ' ';
        sw[30] = hw[10] = 0;
        expected_model = ubx_version_is_module(body, len, RX_MODULE);
        version_seen = true;
        ESP_LOGI(TAG, "MON-VER: software=%s hardware=%s; " RX_MODULE "=%s", sw, hw,
                 expected_model ? "yes" : "unconfirmed");
        for (size_t offset = 40; offset + 30 <= len; offset += 30) {
            char extension[31];
            size_t i = 0;
            for (; i < 30 && body[offset + i]; i++)
                extension[i] = body[offset + i] >= 32 && body[offset + i] <= 126 ? body[offset + i] : '?';
            extension[i] = 0;
            if (i) ESP_LOGI(TAG, "MON-VER extension: %s", extension);
        }
    } else if (cls == 0x05 && (id == 0 || id == 1) && len == 2 && body[0] == 0x06 && body[1] == 0x8a) {
        cfg_ack = id == 1 ? 1 : -1;
    }
}
static void poll_version(void)
{
    uint8_t pkt[8];
    size_t n = ubx_poll_version(pkt);
    uart_write_bytes(RX_UART, pkt, n);
}
static void set_ram(uint32_t key, uint32_t value, unsigned width)
{
    uint8_t pkt[20];
    size_t n = ubx_set_ram(pkt, key, value, width);
    cfg_ack = 0;
    uart_write_bytes(RX_UART, pkt, n);
    uart_wait_tx_done(RX_UART, pdMS_TO_TICKS(200));
}
static void rx_task(void *arg)
{
    ubx_parser_t *parser = arg;
    uint8_t buf[512];
    nmea_probe_t nmea = {0};
    uint32_t nmea_count = 0, prev_valid = 0, prev_nmea = 0;
    int baud = CONFIG_NVF_RX_BAUD;
    bool locked = !CONFIG_NVF_RX_AUTOPROBE;
    int64_t last_valid = 0, next_probe = 0, next_log = 0;
    const int rates[] = {CONFIG_NVF_RX_BAUD, 38400, 115200, 9600, 230400, 460800};
    size_t rate_index = 0;
    // One key per request: a receiver rejecting SFRBX must still get telemetry. The IDs are
    // the same on all three receivers (ubx_probe.h).
    const uint32_t keys[] = {0x10740001, 0x20910232, 0x2091035a, 0x20910016, 0x20910007, 0x2091001b,
        0x2091017e}; // TIM-TP UART1: lock/reference metadata, RAM only
    unsigned setting = 0, tries = 0;
    bool configured = false, baud_attempted = false;
    int64_t cfg_deadline = 0;
    esp_task_wdt_add(NULL);
    for (;;) {
        esp_task_wdt_reset();
        atomic_fetch_add_explicit(&s_ticks, 1, memory_order_relaxed);
        int n = uart_read_bytes(RX_UART, buf, sizeof buf, pdMS_TO_TICKS(100));
        if (n > 0) {
            ubx_parser_feed(parser, buf, (size_t)n);
            for (int i = 0; i < n; i++) if (nmea_probe_feed(&nmea, buf[i])) nmea_count++;
        }
        int64_t now = esp_timer_get_time() / 1000;
        uint32_t valid = atomic_load_explicit(&parser->frames_valid, memory_order_relaxed);
        if (valid != prev_valid || nmea_count != prev_nmea) {
            if (!locked) ESP_LOGI(TAG, "valid receiver traffic at %d baud", baud);
            locked = true;
            last_valid = now;
        }
        prev_valid = valid; prev_nmea = nmea_count;
        if (CONFIG_NVF_RX_AUTOPROBE && locked && now - last_valid > 15000) {
            locked = version_seen = expected_model = false;
            configured = baud_attempted = false; setting = tries = 0;
            ESP_LOGW(TAG, "no valid receiver traffic for 15 s; probing again");
        }
        if (CONFIG_NVF_RX_AUTOPROBE && !locked && now >= next_probe) {
            baud = rates[rate_index++ % (sizeof(rates) / sizeof(rates[0]))];
            uart_set_baudrate(RX_UART, baud);
            uart_flush_input(RX_UART);
            parser->state = 0; nmea = (nmea_probe_t){0};
            poll_version();
            next_probe = now + 2500;
        } else if (CONFIG_NVF_RX_AUTOPROBE && locked && !version_seen && now >= next_probe) {
            poll_version(); next_probe = now + 3000;
        }
        if (CONFIG_NVF_RX_CONFIGURE && locked && expected_model && !configured) {
            if (baud != CONFIG_NVF_RX_BAUD && !baud_attempted) {
                // Change only the receiver's RAM setting. Re-probe if the new
                // baud fails; an ACK may be emitted at either side of the change.
                baud_attempted = true;
                set_ram(0x40520001, CONFIG_NVF_RX_BAUD, 4); // CFG-UART1-BAUDRATE
                vTaskDelay(pdMS_TO_TICKS(100));
                baud = CONFIG_NVF_RX_BAUD;
                uart_set_baudrate(RX_UART, baud);
                uart_flush_input(RX_UART);
                parser->state = 0; nmea = (nmea_probe_t){0};
                version_seen = expected_model = false;
                last_valid = now;
                poll_version(); next_probe = now + 3000;
            } else if (setting < sizeof(keys) / sizeof(keys[0])) {
                if (tries && cfg_ack != 0) {
                    ESP_LOGI(TAG, "RAM key 0x%08lx: %s", (unsigned long)keys[setting], cfg_ack > 0 ? "ACK" : "NAK (unsupported)");
                    setting++; tries = 0; cfg_ack = 0;
                } else if (now >= cfg_deadline) {
                    if (tries == 3) {
                        ESP_LOGW(TAG, "RAM key 0x%08lx: no ACK; output unconfirmed", (unsigned long)keys[setting]);
                        setting++; tries = 0;
                    } else {
                        set_ram(keys[setting], 1, 1);
                        tries++; cfg_deadline = now + 1000;
                    }
                }
            } else {
                configured = true;
                ESP_LOGI(TAG, "receiver RAM configuration attempted; SFRBX counter confirms raw output");
            }
        }
        if (now >= next_log) {
            uint32_t bytes = atomic_load_explicit(&parser->bytes, memory_order_relaxed);
            uint32_t nav = atomic_load_explicit(&parser->frames_nav, memory_order_relaxed);
            ESP_LOGI(TAG, "UART1 TX=GPIO%d RX=GPIO%d baud=%d bytes=%lu UBX=%lu NMEA=%lu SFRBX=%lu %s",
                     RX_PIN_TX, RX_PIN_RX, baud, (unsigned long)bytes, (unsigned long)valid,
                     (unsigned long)nmea_count, (unsigned long)nav, locked ? "receiving" : "probing");
            if (!bytes) ESP_LOGW(TAG, "no UART bytes observed; check GNSS power and UART path");
            else if (!valid && nmea_count) ESP_LOGW(TAG, "NMEA present; raw UBX output not yet confirmed");
            if (health.satellites_valid) ESP_LOGI(TAG, "tracked: GPS=%u SBAS=%u GAL=%u BDS=%u QZSS=%u GLO=%u NavIC=%u fix=%s",
                health.tracked[0], health.tracked[1], health.tracked[2], health.tracked[3],
                health.tracked[5], health.tracked[6], health.tracked[7],
                gnss_status_position_fresh(&health, now) ? "valid" : "unavailable");
            next_log = now + 10000;
        }
    }
}
esp_err_t receiver_start(ubx_parser_t *parser)
{
    uart_config_t cfg = {.baud_rate = CONFIG_NVF_RX_BAUD, .data_bits = UART_DATA_8_BITS,
        .parity = UART_PARITY_DISABLE, .stop_bits = UART_STOP_BITS_1,
        .flow_ctrl = UART_HW_FLOWCTRL_DISABLE, .source_clk = UART_SCLK_DEFAULT};
    esp_err_t err = uart_driver_install(RX_UART, 4096, 0, 0, NULL, 0);
    if (err != ESP_OK) return err;
    if ((err = uart_param_config(RX_UART, &cfg)) != ESP_OK) return err;
    if ((err = uart_set_pin(RX_UART, RX_PIN_TX, RX_PIN_RX, UART_PIN_NO_CHANGE, UART_PIN_NO_CHANGE)) != ESP_OK) return err;
    parser->observe = observe;
    return xTaskCreate(rx_task, "rx", 4096, parser, 7, NULL) == pdPASS ? ESP_OK : ESP_ERR_NO_MEM;
}
bool receiver_alive(void)
{
    return atomic_load_explicit(&s_ticks, memory_order_relaxed) >= 10;
}
void receiver_status(gnss_status_t *out)
{
    taskENTER_CRITICAL(&health_lock);
    *out = snapshot;
    taskEXIT_CRITICAL(&health_lock);
}
