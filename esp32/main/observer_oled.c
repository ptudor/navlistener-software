#include "observer_oled.h"
#include <stdio.h>
#include <string.h>
#include <time.h>
#include "cJSON.h"
#include "esp_app_desc.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "board.h"
#include "hardware_manifest.h"
#include "observer_time.h"
#include "oled_gnss.h"
#include "oled_service.h"
#include "pusher.h"
#include "receiver.h"
#include "spool.h"

static const char *TAG = "observer_oled";
static bool started;
static bool (*reset_is_armed)(void);
// What the application told the display, copied in and out whole under the lock: the setters
// run on the main task while the display task draws.
static struct {
    bool provisioned, wifi;
    char setup_network[OLED_TEXT], setup_reason[2 * OLED_TEXT], collector[64], station[64];
} told;
static portMUX_TYPE told_lock = portMUX_INITIALIZER_UNLOCKED;

// An interface's IPv4 address, when it is up and has one.
static bool interface_address(const char *key, uint8_t out[4])
{
    esp_netif_t *netif = esp_netif_get_handle_from_ifkey(key);
    esp_netif_ip_info_t ip = {0};
    if (!netif || !esp_netif_is_netif_up(netif) || esp_netif_get_ip_info(netif, &ip) != ESP_OK || !ip.ip.addr) return false;
    memcpy(out, &ip.ip.addr, 4);
    return true;
}

// The display task's model for one frame.
static void fill(oled_model_t *m)
{
    const int64_t now = esp_timer_get_time() / 1000;
    // UTC as the time service has it: the receiver's qualified time or the RTC's while either
    // is fresh, and named as such; otherwise the system clock, when it is plausible, with the
    // origin it was last set from. A clock that only ran on through a restart has none.
    static const char *const sources[] = {[OBS_TIME_RTC] = "RTC", [OBS_TIME_GNSS] = "GNSS", [OBS_TIME_NETWORK] = "NET",
                                          [OBS_TIME_FACTORY] = "SET"};
    const observer_time_state_t time = observer_time_status();
    observer_time_source_t source = OBS_TIME_UNKNOWN;
    int64_t utc_ns = 0;
    struct timespec wall;
    if (observer_time_fresh(&time.independent, now)) {
        utc_ns = observer_time_project(&time.independent, now);
        source = time.independent.source;
    } else if (clock_gettime(CLOCK_REALTIME, &wall) == 0) {
        utc_ns = (int64_t)wall.tv_sec * 1000000000LL + wall.tv_nsec;
        source = time.system.source;
    }
    if (utc_ns >= 1704067200LL * 1000000000LL && utc_ns < 4102444800LL * 1000000000LL) {
        m->time_valid = true;
        m->utc = utc_ns / 1000000000LL;
        m->utc_millis = (uint16_t)(utc_ns / 1000000LL % 1000);
        if (source < sizeof sources / sizeof sources[0] && sources[source])
            snprintf(m->time_source, sizeof m->time_source, "%s", sources[source]);
    }
    m->uptime_s = (uint32_t)(now / 1000);

    const observer_board_t *board = observer_board_current();
    observer_board_view_t view;
    observer_board_view(&view);
    uint32_t serial;
    if (board) snprintf(m->board, sizeof m->board, "%s", board->name);
    if (observer_board_serial(&serial)) snprintf(m->serial, sizeof m->serial, "%08lx", (unsigned long)serial);
    snprintf(m->firmware, sizeof m->firmware, "%s", esp_app_get_description()->version);
    if (view.receiver) snprintf(m->receiver, sizeof m->receiver, "%s", view.receiver);
    bool low_gain;
    m->receiver_lna = !receiver_lna_setting(&low_gain) ? OLED_LNA_NONE : low_gain ? OLED_LNA_LOW : OLED_LNA_NORMAL;
    m->boot = view.boot;
    m->heap_free = (uint32_t)heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    m->brightness = (uint8_t)(view.brightness > 100 ? 100 : view.brightness);
    m->reset_armed = reset_is_armed && reset_is_armed();
    m->receiver_attention = view.receiver_attention;
    m->reception_alarm = view.reception_alarm;

    // Static: the status is over 2 KB, and this task is its only user here.
    static gnss_status_t status;
    receiver_status(&status);
    oled_gnss_from_status(&m->gnss, &status, now);

    oled_network_t *n = &m->network;
    taskENTER_CRITICAL(&told_lock);
    n->provisioned = told.provisioned;
    n->wifi = told.wifi;
    memcpy(n->setup_network, told.setup_network, sizeof n->setup_network);
    memcpy(n->setup_reason, told.setup_reason, sizeof n->setup_reason);
    memcpy(n->collector, told.collector, sizeof n->collector);
    memcpy(n->station, told.station, sizeof n->station);
    taskEXIT_CRITICAL(&told_lock);
    n->ethernet = observer_board_wired_uplink();
    n->ethernet_up = n->ethernet && interface_address("ETH_DEF", n->ethernet_ip);
    n->wifi_up = n->wifi && interface_address("WIFI_STA_DEF", n->wifi_ip);
    if (n->wifi_up) {
        wifi_ap_record_t ap;
        if (esp_wifi_sta_get_ap_info(&ap) == ESP_OK) {
            n->wifi_rssi_valid = true;
            n->wifi_rssi = ap.rssi;
        }
    }
    n->collector_up = pusher_connected();
    n->tunnel = pusher_via_tunnel();
    size_t queued = 0;
    spool_stats(NULL, &n->dropped, &queued);
    n->spool = (uint32_t)queued;

    oled_sensors_t *s = &m->sensors;
    s->temperature_valid = view.temperature_valid;
    s->humidity_valid = view.humidity_valid;
    s->pressure_valid = view.pressure_valid;
    s->centi_c = view.centi_c;
    s->centi_rh = view.centi_rh;
    s->pressure_pa = view.pressure_pa;
    for (unsigned i = 0; i < OLED_RAILS; i++) {
        if (!view.rail[i].name) continue;
        snprintf(s->rail[i].name, sizeof s->rail[i].name, "%s", view.rail[i].name);
        s->rail[i].valid = view.rail[i].valid;
        s->rail[i].bus_mv = view.rail[i].bus_mv;
        s->rail[i].ma = view.rail[i].ma;
    }
    s->rail_alert = view.rail_alert;
    s->rtc_temperature_valid = view.rtc_temperature_valid;
    s->rtc_centi_c = view.rtc_centi_c;
    s->pulse_known = view.pulse_known;
    s->pulse = view.pulse;
}

esp_err_t observer_oled_start(bool (*reset_armed)(void))
{
    const observer_board_t *board = observer_board_current();
    if (!board || !board->display) return ESP_ERR_NOT_SUPPORTED;
    reset_is_armed = reset_armed;
    const oled_service_config_t config = {.bus = hardware_manifest_i2c_bus(), .address = board->display, .model = fill};
    const esp_err_t err = oled_service_start(&config);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "status display service unavailable: %s; the board runs without it", esp_err_to_name(err));
        return err;
    }
    started = true;
    ESP_LOGI(TAG, "status display: an SSD1306 module on the %s's display header is looked for at 0x%02x", board->name,
             board->display);
    return ESP_OK;
}

void observer_oled_setup(const char *network, const char *reason)
{
    char setup_network[sizeof told.setup_network], setup_reason[sizeof told.setup_reason];
    snprintf(setup_network, sizeof setup_network, "%s", network ? network : "");
    snprintf(setup_reason, sizeof setup_reason, "%s", reason ? reason : "");
    taskENTER_CRITICAL(&told_lock);
    told.provisioned = false;
    memcpy(told.setup_network, setup_network, sizeof told.setup_network);
    memcpy(told.setup_reason, setup_reason, sizeof told.setup_reason);
    taskEXIT_CRITICAL(&told_lock);
}

void observer_oled_provisioned(const char *collector, const char *station, bool wifi)
{
    char collector_text[sizeof told.collector], station_text[sizeof told.station];
    snprintf(collector_text, sizeof collector_text, "%s", collector ? collector : "");
    snprintf(station_text, sizeof station_text, "%s", station ? station : "");
    taskENTER_CRITICAL(&told_lock);
    told.provisioned = true;
    told.wifi = wifi;
    memcpy(told.collector, collector_text, sizeof told.collector);
    memcpy(told.station, station_text, sizeof told.station);
    taskEXIT_CRITICAL(&told_lock);
}

void observer_oled_next_page(void)
{
    if (started) oled_service_next_page();
}

static void console_status(void)
{
    static const char *const screens[] = {[OLED_SCREEN_SPLASH] = "start", [OLED_SCREEN_RESET_ARMED] = "reset-armed",
                                          [OLED_SCREEN_PAGE] = "page"};
    oled_service_status_t s;
    oled_service_status(&s);
    cJSON *json = cJSON_CreateObject();
    if (!json) { puts("NVF-OLED-ERROR status: out of memory"); return; }
    char hex[8];
    cJSON_AddBoolToObject(json, "present", s.present);
    cJSON_AddBoolToObject(json, "on", s.present && s.powered);
    snprintf(hex, sizeof hex, "0x%02x", s.address);
    cJSON_AddStringToObject(json, "address", hex);
    cJSON_AddNumberToObject(json, "rotation", s.rotated ? 180 : 0);
    snprintf(hex, sizeof hex, "0x%02x", s.status);
    cJSON_AddStringToObject(json, "status_register", hex);
    cJSON_AddBoolToObject(json, "reset_watched", s.status_tracks);
    cJSON_AddNumberToObject(json, "contrast", s.contrast);
    cJSON_AddStringToObject(json, "screen", screens[s.screen]);
    cJSON_AddStringToObject(json, "page", oled_page_name(s.page));
    cJSON_AddNumberToObject(json, "attached", s.attached);
    cJSON_AddNumberToObject(json, "lost", s.lost);
    cJSON_AddNumberToObject(json, "resets", s.resets);
    cJSON_AddNumberToObject(json, "frames", s.frames);
    cJSON_AddNumberToObject(json, "writes", s.writes);
    cJSON_AddNumberToObject(json, "bytes", s.bytes);
    cJSON_AddNumberToObject(json, "failures", s.failures);
    char *body = cJSON_PrintUnformatted(json);
    if (body) printf("NVF-OLED-STATUS %s\n", body);
    else puts("NVF-OLED-ERROR status: out of memory");
    cJSON_free(body);
    cJSON_Delete(json);
}

// The frame as rows of '#' and '.', as it would light the panel: what the display shows now,
// or a named page drawn from the model of the latest frame.
static void console_dump(const char *name)
{
    static oled_canvas_t canvas; // the console task's alone
    oled_page_t page = OLED_PAGE_COUNT;
    if (name && !oled_page_parse(name, &page)) {
        puts("NVF-OLED-ERROR dump: pages are now, gnss, signal, network, sensors, system and attention");
        return;
    }
    if (!oled_service_frame(name ? &page : NULL, &canvas)) {
        puts("NVF-OLED-ERROR dump: no frame has been drawn yet");
        return;
    }
    oled_service_status_t s;
    oled_service_status(&s);
    printf("NVF-OLED-FRAME %s %dx%d\n", name ? name : s.screen == OLED_SCREEN_PAGE ? oled_page_name(s.page) : "screen",
           OLED_WIDTH, OLED_HEIGHT);
    for (int y = 0; y < OLED_HEIGHT; y++) {
        char row[OLED_WIDTH + 1];
        for (int x = 0; x < OLED_WIDTH; x++) row[x] = oled_get(&canvas, x, y) ? '#' : '.';
        row[OLED_WIDTH] = 0;
        puts(row);
    }
    puts("NVF-OLED-END");
}

void observer_oled_console(char *command)
{
    if (!started) {
        puts("NVF-OLED-ERROR this board has no display header, or its display service did not start");
        return;
    }
    if (!strcmp(command, "status")) {
        console_status();
    } else if (!strcmp(command, "rotate 0") || !strcmp(command, "rotate 180")) {
        const bool rotated = command[7] == '1';
        const esp_err_t err = oled_service_set_rotated(rotated);
        if (err == ESP_OK) printf("NVF-OLED-OK rotate %d\n", rotated ? 180 : 0);
        else printf("NVF-OLED-ERROR rotate: the setting was not saved (%s)\n", esp_err_to_name(err));
    } else if (!strcmp(command, "off") || !strcmp(command, "on")) {
        oled_service_set_power(command[1] == 'n');
        printf("NVF-OLED-OK %s\n", command);
    } else if (!strcmp(command, "dump")) {
        console_dump(NULL);
    } else if (!strncmp(command, "dump ", 5)) {
        console_dump(command + 5);
    } else {
        puts("NVF-OLED-ERROR usage: oled status | rotate <0|180> | off | on | dump [now|gnss|signal|network|sensors|system|attention]");
    }
}
