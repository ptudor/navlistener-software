// navfeeder-esp — app entry point.
//
// Ties the pipeline together: WiFi STA -> UART producer (u-blox bytes) -> ubx framer ->
// GNF1 record -> spool -> TLS pusher -> the navlistener collector. All decode + orbit math is
// central in the collector (docs/DESIGN.md §1); this box just frames and forwards. A UI task
// mirrors state to the LCD dashboard + the WS2812 status LED.
//
// Config today comes from Kconfig (idf.py menuconfig -> "navfeeder-esp"); NVS provisioning is
// P5. With no WiFi SSID or collector host set, the firmware still boots, brings up the display,
// runs the receiver producer, and shows "NO WIFI / unprovisioned" — it notices the problem.

#include <string.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "driver/uart.h"
#include "esp_log.h"
#include "esp_system.h"   // esp_restart 
#include "esp_task_wdt.h" // task watchdog subscription 
#include "esp_event.h"
#include "esp_netif.h"
#include "esp_wifi.h"
#include "nvs_flash.h"

#include "ubx.h"
#include "spool.h"
#include "pusher.h"
#include "display_st7789.h"
#include "status_led.h"
#include "netcfg.h"

static const char *TAG = "navfeeder";

// Receiver wiring (Waveshare ESP32-C6-LCD-1.47 -> u-blox on UART1).
// regression fix (hardware caveat): GPIO9 is a C6 boot STRAPPING pin — GPIO9=0 at chip reset selects
// the ROM serial-download boot. UART idle is high (safe when quiet), but at 460800 with a
// continuous SFRBX stream the line is low a large fraction of the time, so a power-on/
// brownout/external reset landing mid-byte can latch the chip into the ROM downloader (a
// field hang the watchdog can't recover — recovery needs a manual reset, which can re-strap
// while the receiver keeps talking). This can't be fixed in firmware alone: on a board re-spin
// move the receiver RX to a non-strapping GPIO; on production units of THIS board, burn the
// DIS_DOWNLOAD_MODE eFuse (`espefuse.py burn_efuse DIS_DOWNLOAD_MODE`, also aligned with the
// regression fix secure-provisioning direction) so the strap combination becomes harmless.
#define RX_UART     UART_NUM_1
#define RX_PIN_RX   9
#define RX_PIN_TX   10
#define RX_BUF_SIZE 4096

static volatile bool s_wifi_up;
static char s_collector[80];      // "host:port" once provisioned, else empty
static netcfg_t g_cfg;            // live config (NVS over Kconfig defaults)
static ubx_parser_t s_parser;     // static: its buffers are too large for a task stack

// app_now_ns returns the reception timestamp stamped into each GNF1 record. Until SNTP/RTC
// lands (P-hw), there is no trustworthy wall clock, so we return 0 and the collector stamps
// its own receive time (push.go recordToFrame) — the safe default.
static uint64_t app_now_ns(void)
{
    struct timespec ts;
    if (clock_gettime(CLOCK_REALTIME, &ts) == 0 && ts.tv_sec > 1600000000) // ~2020-09
        return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
    return 0;
}

// on_record hands each framed GNF1 record to the spool, which the pusher drains.
static void on_record(const uint8_t *record, size_t record_len, void *ctx)
{
    (void)ctx;
    spool_append(record, (uint32_t)record_len);
}

static void rx_task(void *arg)
{
    ubx_parser_t *parser = arg;
    uint8_t buf[512];
    // subscribe the receiver reader to the task WDT (CONFIG_ESP_TASK_WDT_INIT) and
    // reset it each ≤200 ms loop — so a wedged rx_task actually reboots the unit, which the
    // sdkconfig comment promises but nothing implemented (only the idle tasks were watched).
    // pusher_task is deliberately NOT subscribed: its inter-connect backoff vTaskDelay can
    // exceed the 10 s WDT and would false-trigger; its wedges are bounded by regression fix timeouts.
    esp_task_wdt_add(NULL);
    for (;;) {
        esp_task_wdt_reset();
        int n = uart_read_bytes(RX_UART, buf, sizeof buf, pdMS_TO_TICKS(200));
        if (n > 0) ubx_parser_feed(parser, buf, (size_t)n);
    }
}

// ui_task refreshes the LCD dashboard + the status LED and logs a heartbeat every 2 s.
static void ui_task(void *arg)
{
    ubx_parser_t *p = arg;
    uint32_t last_nav = 0;
    led_state_t last_led = LED_BOOT;
    for (;;) {
        uint64_t dropped = 0;
        size_t depth = 0;
        spool_stats(NULL, &dropped, &depth);
        uint32_t nav = p->frames_nav;
        bool link = pusher_connected();
        bool wifi = s_wifi_up;

        nvf_status_t st = {
            .station = g_cfg.station,
            .collector = s_collector[0] ? s_collector : NULL,
            .wifi_up = wifi,
            .link_up = link,
            .nav = nav,
            .nav_rate = nav - last_nav,
            .telem = p->frames_telem,
            .bad_ck = p->bad_checksum,
            .spool_depth = (unsigned)depth,
            .dropped = dropped,
        };
        display_render_status(&st);

        led_state_t ls = !wifi ? LED_NO_WIFI
                       : !link ? LED_NO_LINK
                       : depth > 0 ? LED_SPOOL_FILLING
                       : LED_STREAMING;
        if (ls != last_led) { status_led_state(ls); last_led = ls; }

        ESP_LOGI(TAG, "nav=%u (+%u) telem=%u bad_ck=%u spool=%u drop=%llu link=%s",
                 (unsigned)nav, (unsigned)(nav - last_nav), (unsigned)p->frames_telem,
                 (unsigned)p->bad_checksum, (unsigned)depth, (unsigned long long)dropped,
                 link ? "up" : "down");
        last_nav = nav;
        vTaskDelay(pdMS_TO_TICKS(2000));
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
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        s_wifi_up = false;
        ESP_LOGW(TAG, "wifi disconnected; reconnecting");
        esp_wifi_connect();
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *ev = data;
        s_wifi_up = true;
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
    // wifi_sta_config_t's ssid[32]/password[64] accept a full-length, non-NUL-
    // terminated value (a 32-byte SSID, a 64-hex-char raw PSK) -- strlcpy always reserves a
    // byte for its own trailing NUL, silently dropping the last byte of exactly such a value
    // and making that network permanently unjoinable. memcpy a strnlen-bounded length
    // instead; wc is zero-initialized above, so a shorter value is still correctly
    // zero-padded (equivalent to NUL-terminated) past its own length.
    size_t ssid_len = strnlen(g_cfg.wifi_ssid, sizeof wc.sta.ssid);
    memcpy(wc.sta.ssid, g_cfg.wifi_ssid, ssid_len);
    size_t pass_len = strnlen(g_cfg.wifi_pass, sizeof wc.sta.password);
    memcpy(wc.sta.password, g_cfg.wifi_pass, pass_len);
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

    // Display + LED first, so the board shows life (and any problem) even if unprovisioned.
    if (display_init() != ESP_OK) ESP_LOGW(TAG, "display init failed — continuing headless");
    if (status_led_init() != ESP_OK) ESP_LOGW(TAG, "status LED init failed");

    if (!spool_init(CONFIG_NVF_SPOOL_FRAMES)) {
        ESP_LOGE(TAG, "spool init failed (cap=%d) — out of memory", CONFIG_NVF_SPOOL_FRAMES);
        return;
    }
    rx_uart_init();
    ubx_parser_init(&s_parser, on_record, app_now_ns, NULL);
    // rx_task is the receiver reader — if it can't start, the unit reads no bytes and
    // runs silently half-dead. Boot OOM is typically transient, so reboot to retry rather
    // than continue degraded ("the receiver must never go down").
    // rx_task (the UART producer) runs at priority 7 — ABOVE pusher_task (6) — so on
    // this single-core C6 a TLS-handshake CPU burst in the pusher can never starve the reader
    // and overflow the 4 KB UART ring (~89 ms of headroom at 460800), silently dropping nav
    // frames exactly at reconnect. This restores the design rule "the consumer never blocks
    // the producer". rx blocks on uart_read_bytes, so the high priority only preempts to drain
    // the FIFO, then yields. (A bench measurement of the worst-case handshake holdoff is still
    // worth running to confirm no residual risk.)
    if (xTaskCreate(rx_task, "rx", 4096, &s_parser, 7, NULL) != pdPASS) {
        ESP_LOGE(TAG, "failed to create rx task (OOM); rebooting");
        esp_restart();
    }

    // Config precedence: NVS (field-provisioned) over Kconfig defaults.
    bool provisioned = netcfg_load(&g_cfg);
    if (!provisioned) {
        // First boot / factory reset: raise the SoftAP provisioning portal and show its
        // credentials on the LCD, so the board is configured from a phone (no serial console).
        char ap_ssid[33] = {0}, ap_pass[16] = {0};
        if (netcfg_start_portal(ap_ssid, ap_pass) == ESP_OK) {
            status_led_state(LED_BOOT);
            display_show_portal(ap_ssid, ap_pass);
            ESP_LOGW(TAG, "unprovisioned: join AP '%s' and open http://192.168.4.1/ to configure",
                     ap_ssid);
            // the AP password normally appears ONLY on the LCD (never logged).
            // could ever join the AP. Physical serial-console access is equivalent trust to
            // reading the panel, so when the display is not ready, print the one-time password
            // to the serial console as the sole field-recovery path.
            if (!display_is_ready())
                ESP_LOGW(TAG, "display unavailable — AP password (serial console only): %s", ap_pass);
        } else {
            ESP_LOGE(TAG, "provisioning portal failed to start");
        }
        return; // rx_task keeps counting frames; reboots into station mode after provisioning
    }

    // fill s_collector BEFORE ui_task starts reading it — otherwise the write races
    // the reader (formally UB; in practice a partial/empty collector string on one frame).
    snprintf(s_collector, sizeof s_collector, "%s:%d", g_cfg.host, g_cfg.port);
    // the UI is non-essential — log a create failure but keep forwarding.
    if (xTaskCreate(ui_task, "ui", 4096, &s_parser, 4, NULL) != pdPASS)
        ESP_LOGW(TAG, "failed to create ui task; continuing without the dashboard");
    wifi_start();

    pusher_cfg_t pc = {
        .host = g_cfg.host,
        .port = g_cfg.port,
        .token = g_cfg.token,
        .station = g_cfg.station,
        .feed = "ubx",
        .ca_pem = NULL, // P-hw: pin the collector CA; today rely on the Mozilla bundle or insecure
        .insecure = g_cfg.insecure,
    };
    // retry pusher_start with backoff rather than spooling-until-overflow-and-never-
    // pushing on a transient boot OOM. pusher_cfg_free (fa67e92) leaves s_cfg zeroed, so
    // retry is safe. Bounded so a persistent failure eventually reboots to a clean slate.
    int delay_ms = 500;
    for (int attempt = 0; !pusher_start(&pc); attempt++) {
        ESP_LOGE(TAG, "failed to start pusher task (attempt %d)", attempt + 1);
        if (attempt >= 5) {
            ESP_LOGE(TAG, "pusher task will not start; rebooting");
            esp_restart();
        }
        vTaskDelay(pdMS_TO_TICKS(delay_ms));
        if ((delay_ms *= 2) > 8000) delay_ms = 8000;
    }
}
