// navfeeder-esp — app entry point.
//
// Ties the pipeline together: WiFi STA -> UART producer (u-blox bytes) -> ubx framer ->
// GNF1 record -> spool -> TLS pusher -> the navlistener collector. All decode + orbit math is
// central in the collector (docs/DESIGN.md §1); this box just frames and forwards. A UI task
// mirrors state to the LCD dashboard + the WS2812 status LED.
//
// Config is NVS-first, with Kconfig values only as a development fallback. A board without a
// provisioned WiFi SSID or collector host raises the password-protected SoftAP portal and shows
// its one-time credentials on the display (or the physically trusted serial console fallback).

#include <stdatomic.h>
#include <stdio.h>
#include <string.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "driver/gpio.h"
#include "driver/uart.h"
#include "esp_log.h"
#include "esp_random.h"   // esp_fill_random (regression fix session identity)
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
#include "config_recovery.h"
#include "hardware_manifest.h"

static const char *TAG = "navfeeder";

// Receiver wiring (Waveshare ESP32-C6-LCD-1.47 -> u-blox on UART1).
// regression fix (hardware caveat — ACCEPTED, design constraint): GPIO9 is a C6 boot
// STRAPPING pin — GPIO9=0 at chip reset selects the ROM serial-download boot. UART idle is
// high (safe when quiet), but at 460800 with a continuous SFRBX stream the line is low a
// large fraction of the time, so a power-on/brownout/external reset landing mid-byte can
// latch the chip into the ROM downloader (a field hang the watchdog can't recover — recovery
// needs a manual reset, which can re-strap while the receiver keeps talking). This cannot be
// fixed in firmware: the pin is sampled by ROM before any of our code runs.
//
// Current C6 boards use this wiring and require stable power. A lower UART
// line rate can reduce line occupancy where the frame budget permits.
// DIS_DOWNLOAD_MODE is not burned: development boards retain serial recovery.
// The custom ESP32-S3 board uses a non-strapping receiver RX pin.
#define RX_UART     UART_NUM_1
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
#define RX_PIN_RX   5
#define RX_PIN_TX   4
#else
#define RX_PIN_RX   9
#define RX_PIN_TX   10
#endif
#define RX_BUF_SIZE 4096

static atomic_bool s_wifi_up;
static atomic_bool s_config_reset_armed;
static char s_collector[80];      // "host:port" once provisioned, else empty
static netcfg_t g_cfg;            // live config (NVS over Kconfig defaults)
static ubx_parser_t s_parser;     // static: its buffers are too large for a task stack

#ifndef CONFIG_NVF_CONFIG_RESET_GPIO
// Older generated sdkconfig files predate the option. Fail closed rather than treating an
// undefined preprocessor symbol as numeric zero and unexpectedly claiming GPIO0.
#define CONFIG_NVF_CONFIG_RESET_GPIO -1
#endif

#ifndef CONFIG_NVF_MANIFEST_FACTORY_INIT
// Kconfig does not emit a C macro for a disabled bool. Keep the call site a
// literal false in ordinary builds instead of requiring scattered #ifdefs.
#define CONFIG_NVF_MANIFEST_FACTORY_INIT 0
#endif

#if CONFIG_NVF_CONFIG_RESET_GPIO >= 0
#define CONFIG_RESET_POLL_MS 20u

// config_reset_task recognizes the destructive gesture independently of Wi-Fi/link state,
// so a complete-but-wrong configuration remains recoverable. GPIO0 is sampled only after
// ROM has already latched the S3 boot straps; holding it across reset still enters the ROM
// downloader and never reaches this task, which is the intended emergency fallback.
static void config_reset_task(void *arg)
{
    (void)arg;
    const gpio_num_t pin = (gpio_num_t)CONFIG_NVF_CONFIG_RESET_GPIO;
    const gpio_config_t button = {
        .pin_bit_mask = 1ULL << pin,
        .mode = GPIO_MODE_INPUT,
        .pull_up_en = GPIO_PULLUP_DISABLE,   // the schematic supplies a 10 kΩ pull-up
        .pull_down_en = GPIO_PULLDOWN_DISABLE,
        .intr_type = GPIO_INTR_DISABLE,
    };
    esp_err_t err = gpio_config(&button);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "configuration-reset GPIO %d init failed: %s", (int)pin,
                 esp_err_to_name(err));
        vTaskDelete(NULL);
        return;
    }

    config_recovery_t gesture;
    config_recovery_init(&gesture);
    for (;;) {
        bool pressed = gpio_get_level(pin) == 0;
        config_recovery_event_t event =
            config_recovery_update(&gesture, pressed, CONFIG_RESET_POLL_MS);
        if (event == CONFIG_RECOVERY_EVENT_ARMED) {
            atomic_store_explicit(&s_config_reset_armed, true, memory_order_relaxed);
            ESP_LOGW(TAG, "configuration reset armed; release BOOT to erase settings");
        } else if (event == CONFIG_RECOVERY_EVENT_CONFIRMED) {
            ESP_LOGW(TAG, "physical configuration reset confirmed; erasing navfeeder settings");
            err = netcfg_reset_provisioning();
            if (err != ESP_OK) {
                atomic_store_explicit(&s_config_reset_armed, false, memory_order_relaxed);
                ESP_LOGE(TAG, "configuration reset failed: %s", esp_err_to_name(err));
            } else {
                // Give the display/RMT transfer and the final log line time to complete. The
                // reset marker suppresses compiled defaults, so the next boot raises SoftAP.
                vTaskDelay(pdMS_TO_TICKS(250));
                esp_restart();
            }
        }
        vTaskDelay(pdMS_TO_TICKS(CONFIG_RESET_POLL_MS));
    }
}

static void config_reset_start(void)
{
    // 4 KiB, not 3: since regression fix netcfg_reset_provisioning() encodes and
    // CRCs the 348-byte record on the stack (replace + read_record frames
    // ~1.4 KiB measured with -fstack-usage) on top of the ESP-IDF NVS write
    // path — and this is the recovery gesture an operator reaches for when
    // the unit is already misbehaving, so it must not be the task that
    // overflows.
    if (xTaskCreate(config_reset_task, "cfg_reset", 4096, NULL, 5, NULL) != pdPASS)
        ESP_LOGE(TAG, "failed to create configuration-reset task");
}
#else
static void config_reset_start(void)
{
    ESP_LOGI(TAG, "runtime configuration reset disabled for this board target");
}
#endif

// s_session is the GNF1 boot/session identity sent in every HELLO : 32 hex
// characters, the same shape navfeeder.c mints. Written once by session_init() during
// single-threaded startup, read-only afterwards (the pusher takes its own copy).
//
// WHY a NEW value every boot, unconditionally: the collector's replay-dedup key is
// (observer, session, seq), and it must change exactly when the feeder's DATA sequence space
// restarts from zero. The C feeder can *re-use* a session because it persists one in its disk
// spool header and resumes the sequence with it; navfeeder-esp cannot and must not — its
// spool is a RAM-only ring (regression fix / regression fix, spool.h), so every reboot both empties the
// ring and restarts seq at 0. Minting fresh here is precisely what makes that acceptable:
// post-reboot frames land in a brand-new sequence space instead of colliding with the
// previous boot's durable ledger rows and being silently discarded as replays. It is also
// mandatory on the wire — since the 2026-07-31 GNF1 contract revision the collector rejects a
// HELLO without a valid session (`{"ok":false,"error":"missing or invalid session"}`).
static char s_session[33];        // 32 hex chars + NUL (wire.SessionMaxLen is 64)

static void session_init(void)
{
    // Called AFTER wifi_start(): esp_random()/esp_fill_random only guarantee true random
    // numbers while the RF subsystem is enabled (ESP-IDF "Random Number Generation"), and the
    // one property this value must have is that it differs from the previous boot's — a
    // deterministic pre-RF seed would silently reinstate the exact collision regression fix fixes.
    uint8_t b[16];
    esp_fill_random(b, sizeof b);
    for (size_t i = 0; i < sizeof b; i++)
        snprintf(s_session + 2 * i, 3, "%02x", b[i]);
}

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
        if (atomic_load_explicit(&s_config_reset_armed, memory_order_relaxed)) {
            // The erase happens only on release. Until then this persistent screen/color is
            // the operator's chance to cancel by power-cycling without releasing into the app.
            display_show_config_reset_armed();
            status_led_set_rgb(255, 0, 255); // magenta: deliberately unlike every run state
            vTaskDelay(pdMS_TO_TICKS(200));
            continue;
        }

        uint64_t dropped = 0;
        size_t depth = 0;
        spool_stats(NULL, &dropped, &depth);
        // relaxed atomic loads — rx_task increments these concurrently.
        uint32_t nav = atomic_load_explicit(&p->frames_nav, memory_order_relaxed);
        uint32_t telem = atomic_load_explicit(&p->frames_telem, memory_order_relaxed);
        uint32_t bad_ck = atomic_load_explicit(&p->bad_checksum, memory_order_relaxed);
        bool link = pusher_connected();
        bool wifi = atomic_load_explicit(&s_wifi_up, memory_order_relaxed);

        nvf_status_t st = {
            .station = g_cfg.station,
            .collector = s_collector[0] ? s_collector : NULL,
            .wifi_up = wifi,
            .link_up = link,
            .nav = nav,
            .nav_rate = nav - last_nav,
            .telem = telem,
            .bad_ck = bad_ck,
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
                 (unsigned)nav, (unsigned)(nav - last_nav), (unsigned)telem,
                 (unsigned)bad_ck, (unsigned)depth, (unsigned long long)dropped,
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

// --- WiFi STA (NVS-first config; Kconfig values are the development fallback) ------------

static void wifi_event_handler(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    (void)arg;
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        atomic_store_explicit(&s_wifi_up, false, memory_order_relaxed);
        ESP_LOGW(TAG, "wifi disconnected; reconnecting");
        esp_wifi_connect();
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *ev = data;
        atomic_store_explicit(&s_wifi_up, true, memory_order_relaxed);
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
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
        // The default NVS partition also holds the last adopted manifest
        // EEPROM EUI-64. Erasing it automatically would turn a previously
        // known blank/replaced EEPROM into an apparent first boot. Preserve
        // that safety boundary and require an explicit service operation.
        ESP_LOGE(TAG, "NVS requires whole-partition erase; refusing automatic "
                      "erase because it contains hardware identity history");
        ESP_ERROR_CHECK(err);
#else
        ESP_ERROR_CHECK(nvs_flash_erase());
        err = nvs_flash_init();
#endif
    }
    ESP_ERROR_CHECK(err);

    ESP_LOGI(TAG, "navfeeder-esp starting (GNF1 edge feeder for navlistener)");

#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
    hardware_manifest_result_t manifest;
    err = hardware_manifest_boot(CONFIG_NVF_MANIFEST_FACTORY_INIT, &manifest);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "hardware manifest inspection failed: %s", esp_err_to_name(err));
    } else if (manifest.action == HARDWARE_MANIFEST_ACTION_USE) {
        const eeprom_ic_descriptor_t *gps =
            eeprom_find_category(&manifest.capabilities, CAT_GPS);
        ESP_LOGI(TAG, "discovered GNSS main board revision %u with %u components%s%s",
                 manifest.capabilities.revision,
                 manifest.capabilities.component_count,
                 gps ? "; receiver " : "",
                 gps ? eeprom_ic_name(gps) : "");
    } else {
        ESP_LOGW(TAG, "hardware manifest unavailable: %s",
                 hardware_manifest_action_name(manifest.action));
    }
#endif

    // Display + LED first, so the board shows life (and any problem) even if unprovisioned.
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
    // This board has two TLC5916s and no LCD/WS2812. Their driver will consume
    // the discovered manifest later; keeping the Waveshare drivers dormant is
    // essential because their fixed GPIO6/GPIO7 pins are this board's I2C bus.
    ESP_LOGI(TAG, "custom observer: Waveshare LCD/WS2812 drivers disabled");
#else
    if (display_init() != ESP_OK) ESP_LOGW(TAG, "display init failed — continuing headless");
    if (status_led_init() != ESP_OK) ESP_LOGW(TAG, "status LED init failed");
#endif

    if (!spool_init(CONFIG_NVF_SPOOL_FRAMES)) {
        ESP_LOGE(TAG, "spool init failed (cap=%d) — out of memory; rebooting", CONFIG_NVF_SPOOL_FRAMES);
        vTaskDelay(pdMS_TO_TICKS(250));
        esp_restart();
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
    // netcfg_load now applies the full station-mode rule (ssid/host/port/station/
    // token), not just "ssid and host are set", and names the field that failed. A unit
    // half-provisioned via Kconfig or external NVS tooling raises the portal instead of
    // looping forever on WiFi/TLS/auth with no way back except a serial cable.
    char cfg_err[NETCFG_ERR_CAP] = {0};
    bool provisioned = netcfg_load(&g_cfg, cfg_err, sizeof cfg_err);
    if (!provisioned) {
        // First boot / factory reset / incomplete config: raise the SoftAP provisioning
        // portal and show its credentials on the LCD, so the board is configured from a
        // phone (no serial console).
        char ap_ssid[33] = {0}, ap_pass[16] = {0};
        if (netcfg_start_portal(ap_ssid, ap_pass) == ESP_OK) {
            status_led_state(LED_BOOT);
            // The reason goes on the panel too: "wifi ssid is empty" (a factory-fresh board)
            // and "bearer token is empty" (a half-provisioned one) are the same screen
            // otherwise, and the second is the one an operator would never guess.
            display_show_portal(ap_ssid, ap_pass, cfg_err);
            ESP_LOGW(TAG, "unprovisioned (%s): join AP '%s' and open http://192.168.4.1/ to configure",
                     cfg_err, ap_ssid);
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
    config_reset_start();
    wifi_start();

    // mint the boot session before the pusher can send its first HELLO (see
    // s_session). Logged because it is the field-side handle for "which boot's sequence space
    // is this?" when reading the collector's ingest log — it is an opaque per-boot label, not
    // a credential, so unlike the bearer token it is safe to print.
    session_init();
    ESP_LOGI(TAG, "session %s", s_session);

    pusher_cfg_t pc = {
        .host = g_cfg.host,
        .port = g_cfg.port,
        .token = g_cfg.token,
        .station = g_cfg.station,
        .feed = "ubx",
        .session = s_session,
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
