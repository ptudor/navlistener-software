// navfeeder-esp — app entry point.
//
// Ties the pipeline together: WiFi STA -> UART producer (u-blox bytes) -> ubx framer ->
// GNF1 record -> spool -> TLS pusher -> the navlistener collector. All decode + orbit math is
// central in the collector (docs/DESIGN.md §1); this box just frames and forwards.
//
// Config today comes from Kconfig (idf.py menuconfig -> "navfeeder-esp"); NVS provisioning is
// P5. With no WiFi SSID or collector host set, the firmware still runs the receiver producer
// and logs throughput, so a wired u-blox module can be bench-tested before any network.

#include <inttypes.h>
#include <string.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/event_groups.h"
#include "driver/uart.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "esp_event.h"
#include "esp_netif.h"
#include "esp_wifi.h"
#include "nvs_flash.h"

#include "ubx.h"
#include "spool.h"
#include "pusher.h"

static const char *TAG = "navfeeder";

// A Kconfig bool left at 'n' emits no #define, so give it a concrete 0 for the struct init.
#ifndef CONFIG_NVF_INSECURE
#define CONFIG_NVF_INSECURE 0
#endif

// Receiver wiring (Waveshare ESP32-C6-LCD-1.47 -> u-blox on UART1).
#define RX_UART     UART_NUM_1
#define RX_PIN_RX   9
#define RX_PIN_TX   10
#define RX_BUF_SIZE 4096

// app_now_ns returns the reception timestamp stamped into each GNF1 record. Until SNTP/RTC
// lands (P-hw), there is no trustworthy wall clock, so we return 0 and the collector stamps
// its own receive time (push.go recordToFrame) — the safe default. Once a real clock is synced
// this returns CLOCK_REALTIME in nanoseconds, matching navfeeder.c.
static uint64_t app_now_ns(void)
{
    struct timespec ts;
    if (clock_gettime(CLOCK_REALTIME, &ts) == 0 && ts.tv_sec > 1600000000) // ~2020-09
        return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
    return 0;
}

// on_record is the UBX parser's emit sink: hand each GNF1 record to the spool, which the
// pusher drains to the collector.
static void on_record(const uint8_t *record, size_t record_len, void *ctx)
{
    (void)ctx;
    spool_append(record, (uint32_t)record_len);
}

// A single parser instance lives for the life of the app (its scratch + payload buffers are
// too large for a task stack, so it is static).
static ubx_parser_t s_parser;

static void rx_task(void *arg)
{
    ubx_parser_t *parser = arg;
    uint8_t buf[512];
    int64_t last_log_us = esp_timer_get_time();
    uint32_t last_nav = 0;

    for (;;) {
        int n = uart_read_bytes(RX_UART, buf, sizeof buf, pdMS_TO_TICKS(200));
        if (n > 0) ubx_parser_feed(parser, buf, (size_t)n);

        int64_t now_us = esp_timer_get_time();
        if (now_us - last_log_us >= 5000000) { // every 5 s
            uint64_t dropped = 0;
            size_t depth = 0;
            spool_stats(NULL, &dropped, &depth);
            ESP_LOGI(TAG, "nav=%" PRIu32 " (+%" PRIu32 "/5s) telem=%" PRIu32
                          " bad_ck=%" PRIu32 " spool=%u drop=%llu link=%s",
                     parser->frames_nav, parser->frames_nav - last_nav, parser->frames_telem,
                     parser->bad_checksum, (unsigned)depth, (unsigned long long)dropped,
                     pusher_connected() ? "up" : "down");
            last_nav = parser->frames_nav;
            last_log_us = now_us;
        }
    }
}

static void rx_uart_init(void)
{
    const uart_config_t cfg = {
        .baud_rate = CONFIG_NVF_RX_BAUD,
        .data_bits = UART_DATA_8_BITS,
        .parity = UART_PARITY_DISABLE,
        .stop_bits = UART_STOP_BITS_1,
        .flow_ctrl = UART_HW_FLOWCTRL_DISABLE,
        .source_clk = UART_SCLK_DEFAULT,
    };
    ESP_ERROR_CHECK(uart_driver_install(RX_UART, RX_BUF_SIZE, 0, 0, NULL, 0));
    ESP_ERROR_CHECK(uart_param_config(RX_UART, &cfg));
    ESP_ERROR_CHECK(uart_set_pin(RX_UART, RX_PIN_TX, RX_PIN_RX,
                                 UART_PIN_NO_CHANGE, UART_PIN_NO_CHANGE));
}

// --- WiFi STA (Kconfig creds; NVS provisioning is P5) -----------------------------------

static void wifi_event_handler(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    (void)arg;
    (void)data;
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        ESP_LOGW(TAG, "wifi disconnected; reconnecting");
        esp_wifi_connect();
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *ev = data;
        ESP_LOGI(TAG, "wifi up: " IPSTR, IP2STR(&ev->ip_info.ip));
    }
}

static void wifi_start(void)
{
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_sta();
    wifi_init_config_t ic = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&ic));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(WIFI_EVENT, ESP_EVENT_ANY_ID,
                                                        wifi_event_handler, NULL, NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(IP_EVENT, IP_EVENT_STA_GOT_IP,
                                                        wifi_event_handler, NULL, NULL));
    wifi_config_t wc = {0};
    strlcpy((char *)wc.sta.ssid, CONFIG_NVF_WIFI_SSID, sizeof wc.sta.ssid);
    strlcpy((char *)wc.sta.password, CONFIG_NVF_WIFI_PASS, sizeof wc.sta.password);
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &wc));
    ESP_ERROR_CHECK(esp_wifi_start());
}

void app_main(void)
{
    esp_err_t err = nvs_flash_init();
    if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        ESP_ERROR_CHECK(nvs_flash_init());
    }

    ESP_LOGI(TAG, "navfeeder-esp starting (GNF1 edge feeder for navlistener)");

    if (!spool_init(CONFIG_NVF_SPOOL_FRAMES)) {
        ESP_LOGE(TAG, "spool init failed (cap=%d) — out of memory", CONFIG_NVF_SPOOL_FRAMES);
        return;
    }
    rx_uart_init();
    ubx_parser_init(&s_parser, on_record, app_now_ns, NULL);
    xTaskCreate(rx_task, "rx", 4096, &s_parser, 5, NULL);

    const bool have_net = strlen(CONFIG_NVF_WIFI_SSID) > 0 && strlen(CONFIG_NVF_COLLECTOR_HOST) > 0;
    if (!have_net) {
        ESP_LOGW(TAG, "unprovisioned: set WiFi SSID + collector host via 'idf.py menuconfig' "
                      "(navfeeder-esp), or provision NVS (P5). Running receiver-only.");
        return;
    }

    wifi_start();

    pusher_cfg_t pc = {
        .host = CONFIG_NVF_COLLECTOR_HOST,
        .port = CONFIG_NVF_COLLECTOR_PORT,
        .token = CONFIG_NVF_TOKEN,
        .station = CONFIG_NVF_STATION,
        .feed = "ubx",
        .ca_pem = NULL, // P5: pin the collector CA; today rely on the Mozilla bundle or --insecure
        .insecure = CONFIG_NVF_INSECURE,
    };
    if (!pusher_start(&pc)) {
        ESP_LOGE(TAG, "failed to start pusher task");
    }
}
