// navfeeder-esp — app entry point.
//
// P0/P1 scope: bring up NVS + the receiver UART, run the clean-room UBX framer over the
// incoming byte stream, and log what it frames. The spool (P2), TLS pusher (P3), display
// (P4), and provisioning (P5) land in later phases behind their own components — this file
// grows a spool handoff and a pusher task then. No stubs that pretend to push; today it
// genuinely parses real receiver frames and counts them, which is testable on the bench with
// a u-blox module wired to UART1 (RX=GPIO9, TX=GPIO10) before any network exists.

#include <inttypes.h>
#include <string.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "driver/uart.h"
#include "driver/gpio.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "nvs_flash.h"

#include "ubx.h"
#include "gnf1.h"

static const char *TAG = "navfeeder";

// Receiver wiring (Waveshare ESP32-C6-LCD-1.47 -> u-blox on UART1). Baud becomes an NVS
// setting in P5; the fleet runs 460800 but u-blox USB-CDC ignores the line rate.
#define RX_UART     UART_NUM_1
#define RX_PIN_RX   9
#define RX_PIN_TX   10
#define RX_BAUD     460800
#define RX_BUF_SIZE 4096

// app_now_ns returns the reception timestamp stamped into each GNF1 record. Until SNTP/RTC
// lands (P3/P-hw), we have no trustworthy wall clock, so we return 0 — the collector then
// stamps its own receive time (push.go recordToFrame), which is the safe default. Once a real
// clock is synced this returns CLOCK_REALTIME in nanoseconds, matching navfeeder.c.
static uint64_t app_now_ns(void)
{
    struct timespec ts;
    if (clock_gettime(CLOCK_REALTIME, &ts) == 0 && ts.tv_sec > 1600000000) // ~2020-09
        return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
    return 0; // no trustworthy clock yet -> let the collector stamp reception time
}

// on_record is the UBX parser's emit sink. In P2 this hands each record to the spool
// (spool_append), which the pusher then drains to the collector. Today the parser tallies
// throughput internally (frames_nav/frames_telem), so this sink is the seam and stays empty.
static void on_record(const uint8_t *record, size_t record_len, void *ctx)
{
    (void)record;
    (void)record_len;
    (void)ctx;
}

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
            uint32_t d_nav = parser->frames_nav - last_nav;
            ESP_LOGI(TAG, "nav=%" PRIu32 " (+%" PRIu32 "/5s) telem=%" PRIu32
                          " bad_ck=%" PRIu32 " oversize=%" PRIu32,
                     parser->frames_nav, d_nav, parser->frames_telem,
                     parser->bad_checksum, parser->oversize);
            last_nav = parser->frames_nav;
            last_log_us = now_us;
        }
    }
}

static void rx_uart_init(void)
{
    const uart_config_t cfg = {
        .baud_rate = RX_BAUD,
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

// A single parser instance lives for the life of the app (it owns a scratch + payload buffer;
// too large for a task stack, so it is static).
static ubx_parser_t s_parser;

void app_main(void)
{
    esp_err_t err = nvs_flash_init();
    if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        ESP_ERROR_CHECK(nvs_flash_init());
    }

    ESP_LOGI(TAG, "navfeeder-esp starting (GNF1 edge feeder for navlistener)");
    rx_uart_init();
    ubx_parser_init(&s_parser, on_record, app_now_ns, NULL);

    xTaskCreate(rx_task, "rx", 4096, &s_parser, 5, NULL);
}
