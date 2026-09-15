// navfeeder-esp — app entry point.
//
// Ties the pipeline together: WiFi STA -> UART producer (u-blox bytes) -> ubx framer ->
// GNF1 record -> spool -> TLS pusher -> the navlistener collector. All decode + orbit math is
// central in the collector (docs/DESIGN.md §1); this box just frames and forwards. A UI task
// mirrors state to the LCD dashboard + the WS2812 status LED.
//
// Config is NVS-first, with Kconfig values only as a development fallback. A board without a
// provisioned WiFi SSID or collector host starts encrypted BLE provisioning and a protected
// SoftAP browser fallback. Their persistent setup credential comes from the device label.

#include <stdatomic.h>
#include <stdio.h>
#include <string.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "driver/gpio.h"
#include "driver/uart.h"
#include "esp_log.h"
#include "esp_random.h" // esp_fill_random (regression fix session identity)
#include "esp_system.h" // esp_restart
#include "esp_task_wdt.h" // task watchdog subscription
#include "esp_event.h"
#include "esp_netif.h"
#include "esp_wifi.h"
#include "nvs_flash.h"

#include "ubx.h"
#include "receiver.h"
#include "ota.h"
#include "update_runtime.h"
#include "spool.h"
#include "pusher.h"
#include "tunnel.h"
#include "display_st7789.h"
#include "status_led.h"
#include "netcfg.h"
#include "config_recovery.h"
#include "hardware_manifest.h"
#include "panel_control.h"
#include "board.h"
#include "journal.h"

static const char *TAG = "navfeeder";

static atomic_bool s_wifi_up;
static bool update_online(void) { return atomic_load(&s_wifi_up); }
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
    panel_button_t brightness_button = {0};
    ESP_LOGI(TAG, "BOOT controls ready on GPIO%d", (int)pin);
    for (;;) {
        bool pressed = gpio_get_level(pin) == 0;
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
        if (panel_button_short_press(&brightness_button, pressed, CONFIG_RESET_POLL_MS))
            observer_board_cycle_brightness();
#else
        (void)brightness_button;
#endif
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
                // reset marker suppresses compiled defaults, so the next boot enters setup.
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

// Use a plausible wall clock for reception timestamps. S3 station-mode OTA
// initializes SNTP; hardware RTC/PPS time validation remains P-hw work. Before
// time sync, return 0 so the collector uses its own reception time. SNTP alone
// is not authenticated GNSS time.
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
        size_t used = 0, capacity = 0;
        bool psram = false;
        spool_memory_stats(&used, &capacity, &psram);
        // relaxed atomic loads — rx_task increments these concurrently.
        uint32_t nav = atomic_load_explicit(&p->frames_nav, memory_order_relaxed);
        uint32_t telem = atomic_load_explicit(&p->frames_telem, memory_order_relaxed);
        uint32_t bad_ck = atomic_load_explicit(&p->bad_checksum, memory_order_relaxed);
        bool link = pusher_connected();
        bool via_tunnel = pusher_via_tunnel();
        bool tunnel = tunnel_up();
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

        ESP_LOGI(TAG, "nav=%u (+%u) telem=%u bad_ck=%u spool=%u (%u/%u bytes %s) drop=%llu link=%s tunnel=%s",
                 (unsigned)nav, (unsigned)(nav - last_nav), (unsigned)telem,
                 (unsigned)bad_ck, (unsigned)depth, (unsigned)used, (unsigned)capacity,
                 psram ? "PSRAM" : "internal", (unsigned long long)dropped,
                 link ? (via_tunnel ? "up/tunnel" : "up/direct") : "down",
                 tunnel ? "up" : "down");
        last_nav = nav;
        vTaskDelay(pdMS_TO_TICKS(2000));
    }
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

static void confirm_startup(void)
{
    // Confirm local operation even before provisioning and without a GPS fix,
    // EEPROM or reachable collector. A wedged UART reader is a firmware fault.
    for (int i = 0; i < 30 && !receiver_alive(); i++) vTaskDelay(pdMS_TO_TICKS(100));
    if (!receiver_alive()) {
        ESP_LOGE(TAG, "receiver task did not become ready; rebooting");
        esp_restart();
    }
    ESP_ERROR_CHECK(nvf_ota_confirm_boot());
}

void app_main(void)
{
    journal_start();
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
    hardware_manifest_result_t manifest={0};
    err = hardware_manifest_boot(CONFIG_NVF_MANIFEST_FACTORY_INIT, &manifest);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "hardware manifest inspection failed: %s", esp_err_to_name(err));
        manifest.action = HARDWARE_MANIFEST_ACTION_IO_ERROR;
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
    observer_board_manifest(&manifest, app_now_ns);
#endif

    // Display + LED first, so the board shows life (and any problem) even if unprovisioned.
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
    // This board has two TLC5916s and no LCD/WS2812. The Waveshare drivers'
    // GPIO6/GPIO7 pins conflict with this board's shared I2C bus.
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
    ubx_parser_init(&s_parser, on_record, app_now_ns, NULL);
    // Priority 7 drains UART ahead of TLS work. Startup failure must not leave
    // a network-connected observer silently collecting no bytes.
    ESP_ERROR_CHECK(receiver_start(&s_parser));
    if (observer_board_start() != ESP_OK)
        ESP_LOGW(TAG, "observer panel/diagnostics task unavailable");

    nvf_update_hooks_t update_hooks={.online=update_online,.durable_link=pusher_durable_connected,
        .pause=spool_pause_producers,.resume=spool_resume_producers};
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
    update_hooks.device.hardware_known=manifest.action==HARDWARE_MANIFEST_ACTION_USE && manifest.capabilities_valid && manifest.eui64_valid;
    update_hooks.device.hardware_revision=manifest.capabilities.revision;
    memcpy(update_hooks.device.eui,manifest.eui64,8);
#endif
    err=nvf_update_start(&update_hooks);
    if(err!=ESP_OK && err!=ESP_ERR_NOT_SUPPORTED)
        ESP_LOGW(TAG,"signed update service unavailable: %s",esp_err_to_name(err));

    // Config precedence: NVS (field-provisioned) over Kconfig defaults.
    // netcfg_load now applies the full station-mode rule (ssid/host/port/station/
    // token), not just "ssid and host are set", and names the field that failed. A unit
    // half-provisioned via Kconfig or external NVS tooling raises the portal instead of
    // looping forever on WiFi/TLS/auth with no way back except a serial cable.
    char cfg_err[NETCFG_ERR_CAP] = {0};
    bool provisioned = netcfg_load(&g_cfg, cfg_err, sizeof cfg_err);
    // The BOOT button also controls panel brightness in setup mode. Start its
    // task before the unprovisioned path returns to the portal.
    config_reset_start();
    if (!provisioned) {
        // The S3 advertises encrypted BLE provisioning while keeping the browser
        // portal available immediately on its AP. The C6 uses the same persistent
        // credential for its browser-only development flow.
        netcfg_provisioning_info_t setup;
        if (netcfg_start_provisioning(&setup) == ESP_OK) {
            status_led_state(LED_BOOT);
            // The reason goes on the panel too: "wifi ssid is empty" (a factory-fresh board)
            // and "bearer token is empty" (a half-provisioned one) are the same screen
            // otherwise, and the second is the one an operator would never guess.
            display_show_portal(setup.name, setup.password, cfg_err);
            ESP_LOGW(TAG, "unprovisioned (%s): %s; browser fallback AP '%s' at http://192.168.4.1/",
                     cfg_err, setup.ble_active ? "scan the device label in the Station app"
                                               : "BLE unavailable",
                     setup.name);
#if !defined(CONFIG_SECURE_BOOT) || !CONFIG_SECURE_BOOT
            if (setup.credential_created) {
                ESP_LOGW(TAG, "NEW SETUP LABEL — print and attach before deployment: %s",
                         setup.qr_payload);
#if CONFIG_NVF_SETUP_CONSOLE_PASSWORD
            } else {
                ESP_LOGW(TAG, "DEVELOPMENT SETUP LABEL — persistent setup password: %s",
                         setup.qr_payload);
#endif
            }
#endif
            if (!display_is_ready()) {
                ESP_LOGW(TAG, "setup password is available from the physical label or paired app");
            }
            confirm_startup();
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
    err = nvf_ota_start();
    if (err != ESP_OK && err != ESP_ERR_NOT_SUPPORTED)
        ESP_LOGW(TAG, "OTA control unavailable (%s); pair through the provisioning AP", esp_err_to_name(err));

    // The optional WireGuard uplink. It waits for Wi-Fi and time sync on its own; if it
    // cannot start (or this build has no tunnel support) the pusher simply keeps using the
    // public collector endpoint, which remains valid for every provisioned unit.
    if (g_cfg.tunnel.enabled) {
        err = tunnel_start(&g_cfg.tunnel);
        if (err != ESP_OK)
            ESP_LOGW(TAG, "WireGuard tunnel not started (%s); using the public collector endpoint",
                     esp_err_to_name(err));
    }

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
        .tunnel_host = tunnel_collector_address(), // NULL without a started tunnel
        .tunnel_up = tunnel_up,
        .update_control = nvf_update_control,
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
    confirm_startup();
}
