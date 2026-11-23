// UART discovery is independent of EEPROM presence and network provisioning.
#include "receiver.h"
#include "ubx_probe.h"
#include "sbf.h"
#include "sbf_control.h"
#include "sbf_status.h"
#include "gnf1.h"
#include <string.h>
#include "sdkconfig.h"
#include "driver/gpio.h"
#include "driver/uart.h"
#include "esp_heap_caps.h"
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
// The receiver the manifest lists, as MON-VER names it; only that model is configured.
static const char *module;
// A Septentrio receiver (the P3's mosaic-G5) is spoken to in SBF on its own task; its bytes
// never reach the UBX parser, where a chance sync and checksum match inside another protocol's
// stream would become a false record.
static bool sbf_mode;
static receiver_sbf_pins_t sbf_pins = {.rts = -1, .cts = -1, .reset = -1};
static atomic_uint s_ticks;
static atomic_bool snapshot_requested;
static atomic_bool rf_requested;
void receiver_request_snapshot(void) {atomic_store(&snapshot_requested,true);}
bool receiver_request_rf_survey(void)
{
    if (!module || strcmp(module, "ZED-X20P")) return false;
    atomic_store(&rf_requested, true);
    return true;
}
static bool version_seen, expected_model, unique_id_logged;
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
        expected_model = module && ubx_version_is_module(body, len, module);
        version_seen = true;
        if (module) ESP_LOGI(TAG, "MON-VER: software=%s hardware=%s; %s=%s", sw, hw, module,
                             expected_model ? "yes" : "unconfirmed");
        else ESP_LOGI(TAG, "MON-VER: software=%s hardware=%s; no receiver listed, none configured", sw, hw);
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
    } else if (cls == 0x27 && id == 0x03 && !unique_id_logged) {
        char uid[13];
        if (ubx_unique_id(body, len, uid)) {
            unique_id_logged = true;
            ESP_LOGI(TAG, "receiver unique chip ID %s (UBX-SEC-UNIQID)", uid);
        }
    }
}
static void poll_version(void)
{
    uint8_t pkt[8];
    size_t n = ubx_poll_version(pkt);
    uart_write_bytes(RX_UART, pkt, n);
}
static void poll_unique_id(void)
{
    uint8_t pkt[8];
    size_t n = ubx_poll_unique_id(pkt);
    uart_write_bytes(RX_UART, pkt, n);
}
static void poll_message(uint8_t cls, uint8_t id)
{
    uint8_t pkt[8] = {0xb5, 0x62, cls, id, 0, 0, 0, 0};
    for (size_t i = 2; i < 6; i++) { pkt[6] += pkt[i]; pkt[7] += pkt[6]; }
    uart_write_bytes(RX_UART, pkt, sizeof pkt);
}

// Same private body that the collector decodes. A customer can capture a survey over
// USB even before enrollment. Static storage keeps the receiver task's stack bounded.
static void log_rf_detail(const uint8_t *body, size_t len)
{
    static const char digits[] = "0123456789abcdef";
    static char hex[2 * GNF1_MAX_RAW + 1];
    for (size_t i = 0; i < len; i++) {
        hex[2*i] = digits[body[i] >> 4];
        hex[2*i+1] = digits[body[i] & 15];
    }
    hex[2*len] = 0;
    ESP_LOGI(TAG, "RF evidence=%s", hex);
    if (body[1] == RF_SENSITIVITY)
        ESP_LOGI(TAG, "RF jamming detector high sensitivity=%s (RAM)", body[2] ? "on" : "off");
    if (body[1] == 0x10) {
        static const char *const changes[] = {"started", "stopped", "triggered", "timed out"};
        ESP_LOGI(TAG, "SEC-SIGLOG: %u recorded events", body[3]);
        for (size_t off = 10; off < len; off += 8) {
            uint8_t detection = body[off+4], event = body[off+5];
            const char *change = event < sizeof changes / sizeof *changes ? changes[event] : "unknown";
            ESP_LOGI(TAG, "SEC-SIGLOG: %s %s age_s=%lu detection=%u event=%u",
                     detection == 4 ? "jamming" : "security indication", change,
                     (unsigned long)rf_le32(body + off), detection, event);
        }
    }
    if (body[1] == 0x31)
        ESP_LOGI(TAG, "MON-SPAN: path=%u center_Hz=%lu span_Hz=%lu resolution_Hz=%lu PGA_dB=%u",
                 body[2], (unsigned long)rf_le32(body + 268), (unsigned long)rf_le32(body + 260),
                 (unsigned long)rf_le32(body + 264), body[272]);
    if (body[1] == 2 || body[1] == 3) {
        static char centers[512];
        size_t used = 0;
        centers[0] = 0;
        for (size_t off = 4; off < len && used < sizeof centers; off += 4) {
            uint32_t word = rf_le32(body + off);
            int n = snprintf(centers + used, sizeof centers - used, " %lu=%s",
                             (unsigned long)(word & RF_CENTER_KHZ), word & RF_JAMMED ? "flagged" : "clear");
            if (n < 0) break;
            used += (size_t)n;
        }
        ESP_LOGI(TAG, "SEC-SIG: detector=%s state=%u assessed=%u centers (kHz):%s",
                 body[2] & 1 ? "on" : "off", (body[2] >> 1) & 3, body[3], centers);
    }
}
static const char *antenna_status(uint8_t v)
{
    static const char *const names[] = {"INIT", "DONTKNOW", "OK", "SHORT", "OPEN"};
    return v < sizeof names / sizeof names[0] ? names[v] : "invalid";
}
static const char *antenna_power(uint8_t v)
{
    static const char *const names[] = {"OFF", "ON", "DONTKNOW"};
    return v < sizeof names / sizeof names[0] ? names[v] : "invalid";
}
// Each RF block's antenna supervisor once it changes, SHORT and OPEN as warnings; ant_status
// + 1 is kept so that 0 means not yet reported.
static void log_antenna_changes(const gnss_status_t *s)
{
    static uint8_t reported[GNSS_RF_BLOCKS_MAX];
    for (unsigned b = 0; b < s->rf_blocks; b++) {
        const gnss_rf_block_t *k = &s->rf_block[b];
        if (reported[b] == k->ant_status + 1) continue;
        const char *was = reported[b] ? antenna_status(reported[b] - 1) : "not reported";
        if (k->ant_status == 3 || k->ant_status == 4)
            ESP_LOGW(TAG, "RF block %u antenna %s, power %s (was %s)", k->id, antenna_status(k->ant_status),
                     antenna_power(k->ant_power), was);
        else
            ESP_LOGI(TAG, "RF block %u antenna %s, power %s (was %s)", k->id, antenna_status(k->ant_status),
                     antenna_power(k->ant_power), was);
        reported[b] = k->ant_status + 1;
    }
}
// One line for the jamming state and where it came from, then MON-RF's blocks: the antenna
// supervisor, the AGC as a share of maximum gain, the noise level and the CW suppression.
static void log_rf(const gnss_status_t *s)
{
    static const char *const jamming[] = {"unknown", "OK", "warning", "critical"};
    static char line[512];
    int lead = snprintf(line, sizeof line, "jamming=%s (%s); ", s->rf_valid ? jamming[s->jam & 3] : "unread",
                        s->sec_sig ? "SEC-SIG" : "MON-RF");
    size_t used = lead > 0 ? (size_t)lead : 0;
    for (unsigned b = 0; b < s->rf_blocks && used < sizeof line; b++) {
        const gnss_rf_block_t *k = &s->rf_block[b];
        int n = snprintf(line + used, sizeof line - used, "%sblock %u antenna=%s power=%s AGC=%u%% noise=%u CW=%u",
                         b ? "; " : "", (unsigned)k->id, antenna_status(k->ant_status), antenna_power(k->ant_power),
                         (unsigned)((k->agc * 100u + 4095u) / 8191u), k->noise_per_ms, k->cw_suppression);
        if (n < 0) break;
        used += (size_t)n;
    }
    if (!s->rf_blocks && used < sizeof line) snprintf(line + used, sizeof line - used, "no blocks");
    ESP_LOGI(TAG, "RF: %s", line);
}
// Code-locked signals per system and kind, "GPS L1C/A=8 L2C=6 L5=5 (5 unhealthy), GAL E1=6",
// then the bands they cover.
static void log_signals(const gnss_status_t *s)
{
    static const uint8_t order[] = {0, 1, 2, 3, 5, 6, 7};
    static const char *const systems[8] = {"GPS", "SBAS", "GAL", "BDS", "", "QZSS", "GLO", "NavIC"};
    static char line[512];
    size_t used = 0;
    unsigned bands = 0;
    for (unsigned o = 0; o < sizeof order; o++) {
        bool first = true;
        for (unsigned k = 0; k < GNSS_SIGNAL_KINDS; k++) {
            if (gnss_signal_kinds[k].gnss != order[o] || !s->signal_locked[k]) continue;
            bands |= 1u << gnss_signal_kinds[k].band;
            if (used >= sizeof line) continue;
            int n = first ? snprintf(line + used, sizeof line - used, "%s%s %s=%u", used ? ", " : "", systems[order[o]],
                                     gnss_signal_kinds[k].name, s->signal_locked[k])
                          : snprintf(line + used, sizeof line - used, " %s=%u", gnss_signal_kinds[k].name,
                                     s->signal_locked[k]);
            if (n > 0) used += (size_t)n;
            if (s->signal_unhealthy[k] && used < sizeof line) {
                n = snprintf(line + used, sizeof line - used, " (%u unhealthy)", s->signal_unhealthy[k]);
                if (n > 0) used += (size_t)n;
            }
            first = false;
        }
    }
    if (s->signal_other && used < sizeof line) {
        int n = snprintf(line + used, sizeof line - used, "%sother=%u", used ? ", " : "", s->signal_other);
        if (n > 0) used += (size_t)n;
    }
    char band_list[32] = "";
    for (unsigned b = 0; b < GNSS_BANDS; b++)
        if (bands & 1u << b) {
            if (*band_list) strlcat(band_list, " ", sizeof band_list);
            strlcat(band_list, gnss_band_names[b], sizeof band_list);
        }
    ESP_LOGI(TAG, "signals: %s; bands %s", used ? line : "none code-locked", bands ? band_list : "none");
}
static const char *key_name(uint32_t key)
{
    const char *name = ubx_key_name(key);
    return name ? name : "unnamed key";
}
static void set_ram(uint32_t key, uint32_t value, unsigned width)
{
    uint8_t pkt[20];
    size_t n = ubx_set_ram(pkt, key, value, width);
    cfg_ack = 0;
    uart_write_bytes(RX_UART, pkt, n);
    uart_wait_tx_done(RX_UART, pdMS_TO_TICKS(200));
}
static void apply_setting(const ubx_setting_t *setting)
{
    uint8_t pkt[UBX_SETTING_MAX];
    size_t n = ubx_encode_setting(pkt, setting);
    cfg_ack = 0;
    uart_write_bytes(RX_UART, pkt, n);
    uart_wait_tx_done(RX_UART, pdMS_TO_TICKS(200));
}
// "CFG-MSGOUT-UBX_NAV_PVT_UART1 (0x20910007)" for one key; "NAME=0+NAME=1 (5 keys)" for more.
static const char *setting_name(const ubx_setting_t *setting)
{
    static char name[256];
    if (setting->count == 1) {
        snprintf(name, sizeof name, "%s (0x%08lx)", key_name(setting->item[0].key),
                 (unsigned long)setting->item[0].key);
        return name;
    }
    size_t used = 0;
    for (unsigned i = 0; i < setting->count && used < sizeof name; i++) {
        int n = snprintf(name + used, sizeof name - used, "%s%s=%u", i ? "+" : "", key_name(setting->item[i].key),
                         setting->item[i].value);
        if (n > 0) used += (size_t)n;
    }
    if (used < sizeof name) snprintf(name + used, sizeof name - used, " (%u keys)", setting->count);
    return name;
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
    // One setting per request: a receiver rejecting SFRBX must still get telemetry. A setting
    // is one key, or a combination the receiver accepts only whole (ubx_probe.h).
    const ubx_setting_t *settings;
    const size_t key_count = ubx_settings(module, &settings);
    unsigned setting = 0, tries = 0;
    bool configured = false, baud_attempted = false, config_started = false;
    int switched_from = 0; // the rate the receiver was found at, when it was moved to CONFIG_NVF_RX_BAUD
    unsigned acknowledged = 0;
    int64_t cfg_deadline = 0, rf_seen_ms = 0;
    int64_t last_survey = -60000;
    uint32_t survey_event = 0;
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
        bool requested = atomic_exchange(&snapshot_requested,false);
        if(requested) {
            const uint8_t polls[][2]={{1,0x35},{1,0x43},{0x0a,0x38},{1,3},{1,7}};
            for(unsigned i=0;i<sizeof polls/sizeof polls[0];i++) poll_message(polls[i][0], polls[i][1]);
        }
        if (locked && expected_model && !strcmp(module, "ZED-X20P") && health.sec_sig) {
            bool manual = atomic_exchange(&rf_requested, false);
            if (manual || requested || now - last_survey >= 60000 ||
                (health.event_count != survey_event && now - last_survey >= 5000)) {
                // Pull a spectrum in clear conditions too, to give a later alert a comparison.
                if (manual || requested) {
                    parser->security_cache.length = 0;
                    poll_message(0x27, 0x09);
                }
                poll_message(0x27, 0x10);
                poll_message(0x0a, 0x31);
                uint8_t poll[16];
                size_t n = ubx_get_ram(poll, RF_JAM_SENSITIVITY_KEY);
                uart_write_bytes(RX_UART, poll, n);
                last_survey = now;
                survey_event = health.event_count;
            }
        }
        uint32_t valid = atomic_load_explicit(&parser->frames_valid, memory_order_relaxed);
        if (valid != prev_valid || nmea_count != prev_nmea) {
            if (!locked) ESP_LOGI(TAG, "valid receiver traffic at %d baud", baud);
            locked = true;
            last_valid = now;
        }
        prev_valid = valid; prev_nmea = nmea_count;
        if (CONFIG_NVF_RX_AUTOPROBE && locked && now - last_valid > 15000) {
            locked = version_seen = expected_model = false;
            configured = baud_attempted = config_started = false; setting = tries = 0; switched_from = 0;
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
                set_ram(UBX_KEY_UART1_BAUDRATE, CONFIG_NVF_RX_BAUD, 4);
                vTaskDelay(pdMS_TO_TICKS(100));
                switched_from = baud;
                baud = CONFIG_NVF_RX_BAUD;
                uart_set_baudrate(RX_UART, baud);
                uart_flush_input(RX_UART);
                parser->state = 0; nmea = (nmea_probe_t){0};
                version_seen = expected_model = false;
                last_valid = now;
                poll_version(); next_probe = now + 3000;
            } else if (setting < key_count) {
                // MON-VER has confirmed the model at this rate, so the link runs at it.
                if (!config_started) {
                    config_started = true; acknowledged = 0;
                    if (!unique_id_logged) poll_unique_id();
                    if (switched_from)
                        ESP_LOGI(TAG, "receiver answering at %d baud, set in its RAM from %d (%s)", baud, switched_from,
                                 key_name(UBX_KEY_UART1_BAUDRATE));
                    else
                        ESP_LOGI(TAG, "receiver answering at %d baud, the configured rate", baud);
                }
                if (tries && cfg_ack != 0) {
                    ESP_LOGI(TAG, "RAM key %s: %s", setting_name(&settings[setting]),
                             cfg_ack > 0 ? "ACK" : "NAK (unsupported)");
                    if (cfg_ack > 0) acknowledged++;
                    // A CFG-SIGNAL change resets the GNSS subsystem: 500 ms after its ACK.
                    if (cfg_ack > 0 && ubx_setting_resets_gnss(&settings[setting]) && cfg_deadline < now + 500)
                        cfg_deadline = now + 500;
                    setting++; tries = 0; cfg_ack = 0;
                } else if (now >= cfg_deadline) {
                    if (tries == 3) {
                        ESP_LOGW(TAG, "RAM key %s: no ACK; output unconfirmed", setting_name(&settings[setting]));
                        if (ubx_setting_resets_gnss(&settings[setting])) cfg_deadline = now + 500; // it may have applied
                        setting++; tries = 0;
                    } else {
                        apply_setting(&settings[setting]);
                        tries++; cfg_deadline = now + 1000;
                    }
                }
            } else if (now >= cfg_deadline) { // after the last key's settling time, too
                configured = true;
                ESP_LOGI(TAG, "receiver RAM configuration done: %u of %u settings acknowledged; the SFRBX counter confirms raw output",
                         acknowledged, (unsigned)key_count);
                if (!unique_id_logged) poll_unique_id(); // once more, after the configuration traffic
            }
        }
        if (health.rf_blocks_valid && health.rf_blocks_ms != rf_seen_ms) {
            rf_seen_ms = health.rf_blocks_ms;
            log_antenna_changes(&health);
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
            if (health.rf_blocks_valid && now >= health.rf_blocks_ms && now - health.rf_blocks_ms <= 15000) log_rf(&health);
            if (health.sec_sig && health.jam == 2 && now - health.rf_ms <= 15000 &&
                health.fix_ms > 0 && now - health.fix_ms <= 15000) {
                if (gnss_status_position_fresh(&health, now))
                    ESP_LOGI(TAG, "RF fix: valid=1 type=%u used=%u hAcc=%lu mm pDOP=%.2f",
                             health.pvt_utc[16], health.pvt_utc[19], (unsigned long)health.h_acc_mm, health.p_dop / 100.0);
                else
                    ESP_LOGI(TAG, "RF fix: valid=0 type=%u used=%u", health.pvt_utc[16], health.pvt_utc[19]);
            }
            if (health.signals_valid && now >= health.signals_ms && now - health.signals_ms <= 15000) log_signals(&health);
            next_log = now + 10000;
        }
    }
}
void receiver_set_module(const char *name) { module = name; }
void receiver_set_sbf(const receiver_sbf_pins_t *pins) { sbf_mode = true; sbf_pins = *pins; }
bool receiver_sbf(void) { return sbf_mode; }

// The Septentrio path: COM2 at the receiver's default 115200 baud, RTS throughout and CTS once
// the receiver confirms RTS|CTS (sbf_control.h), the reset line held low but for a recovery
// pulse. The reader, whose buffer holds the largest block, and the record each forwarded block
// is framed into are allocated only on this path. Every block the configuration requests goes
// to the collector whole, through the same emit callback and spool as u-blox records.
#define SBF_BAUD 115200
_Static_assert(SBF_MAX_BLOCK <= GNF1_SBF_RAW_MAX, "every block the reader returns fits an SBF record");
static sbf_reader_t *sbf_reader;
static uint8_t *sbf_record;
static ubx_emit_fn sbf_emit;
static ubx_now_fn sbf_now;
static void *sbf_emit_ctx;
static uint32_t sbf_forwarded;
static sbf_status_t sbf_adapter;
static sbf_control_t sbf_ctl;
static sbf_setup_t sbf_identity;
static bool sbf_status_known;
static sbf_rx_status_t sbf_last_status;
static int64_t sbf_status_ms;
static void sbf_publish(void)
{
    taskENTER_CRITICAL(&health_lock);
    snapshot = health;
    taskEXIT_CRITICAL(&health_lock);
}
static void sbf_on_block(const uint8_t *block, size_t length, void *ctx)
{
    (void)ctx;
    const int64_t now = esp_timer_get_time() / 1000;
    sbf_control_block(&sbf_ctl, block, length, now);
    if (sbf_status_feed(&sbf_adapter, &health, block, length, now)) sbf_publish();
    if (sbf_control_forwarded(sbf_number(block))) {
        const size_t n = gnf1_encode_sbf(sbf_record, sbf_now(), block, length);
        if (n) {
            sbf_emit(sbf_record, n, sbf_emit_ctx);
            sbf_forwarded++;
        }
    }
    sbf_setup_t setup;
    static sbf_rx_status_t status; // static: over a hundred bytes, and this task's stack is 4 KiB
    if (sbf_number(block) == SBF_RECEIVER_SETUP && sbf_receiver_setup(block, length, &setup) &&
        memcmp(&setup, &sbf_identity, sizeof setup)) {
        sbf_identity = setup;
        ESP_LOGI(TAG, "receiver: %s (engine %s), firmware %s, serial %s%s", setup.product[0] ? setup.product : "unnamed",
                 setup.name, setup.version, setup.serial,
                 strncmp(setup.product, "mosaic-G5", 9) && strncmp(setup.name, "mosaic-G5", 9) ?
                 "; not a mosaic-G5, the receiver the manifest lists" : "");
    } else if (sbf_number(block) == SBF_RECEIVER_STATUS && sbf_receiver_status(block, length, &status)) {
        const uint32_t watched = SBF_RXERROR_ANTENNA | SBF_RXERROR_CONGESTION;
        if (!sbf_status_known || (status.rx_state ^ sbf_last_status.rx_state) & SBF_RXSTATE_ACTIVE_ANTENNA ||
            (status.rx_error ^ sbf_last_status.rx_error) & watched)
            ESP_LOGI(TAG, "receiver status: active antenna %s, antenna overcurrent %s, output congestion %s, CPU %u%%, up %lu s",
                     status.rx_state & SBF_RXSTATE_ACTIVE_ANTENNA ? "sensed" : "not sensed",
                     status.rx_error & SBF_RXERROR_ANTENNA ? "yes" : "no",
                     status.rx_error & SBF_RXERROR_CONGESTION ? "yes" : "no", status.cpu_load,
                     (unsigned long)status.uptime_s);
        sbf_status_known = true;
        sbf_last_status = status;
        sbf_status_ms = now;
    }
}
// The receiver's own account of its RF input, from the last ReceiverStatus: each front end's
// AGC gain, sample variance and blanking, then the receiver's temperature and CPU load. A
// lower gain is more power at the input, so this line shows an antenna arriving or leaving
// and a noise floor rising. False while no status block is recent enough to speak for now.
static bool sbf_log_front_ends(int64_t now)
{
    static char text[SBF_FRONT_ENDS_TEXT];
    if (!sbf_status_known || now - sbf_status_ms > 5000) return false;
    const sbf_rx_status_t *s = &sbf_last_status;
    char temperature[16] = "unknown", cpu[16] = "unknown";
    if (s->temperature_valid) snprintf(temperature, sizeof temperature, "%d C", (int)s->temperature_c);
    if (s->cpu_load != 255) snprintf(cpu, sizeof cpu, "%u%%", (unsigned)s->cpu_load);
    if (!sbf_front_ends_text(s, text, sizeof text)) snprintf(text, sizeof text, "none reported");
    ESP_LOGI(TAG, "front ends (gain dB/variance/blanked %%): %s; receiver temperature %s, CPU %s", text, temperature, cpu);
    return true;
}
// After a configuration: the signals the receiver tracks, when its list has changed; those the
// configuration added, each time it added any, since that restarted the tracking loops; and
// those the receiver does not offer, when that set has changed.
static void sbf_log_signals(void)
{
    static bool logged;
    static uint64_t tracked, refused;
    static char names[320];
    const sbf_control_t *c = &sbf_ctl;
    if (!c->signals_known) return;
    if (!logged || tracked != c->signals) {
        sbf_signal_names(c->signals, names, sizeof names);
        ESP_LOGI(TAG, "receiver signal tracking: %d signals: %s", __builtin_popcountll(c->signals), names);
    }
    if (c->signals_added) {
        sbf_signal_names(c->signals_added, names, sizeof names);
        ESP_LOGI(TAG, "receiver signal tracking: enabled by the observer: %s", names);
    }
    if (c->signals_refused != refused) {
        sbf_signal_names(c->signals_refused, names, sizeof names);
        ESP_LOGI(TAG, "receiver signal tracking: not offered by this receiver: %s", names);
    }
    logged = true;
    tracked = c->signals; refused = c->signals_refused;
}
// After a configuration: the receiver's elevation masks, when either has changed; what the
// configuration set, each time it set one; and a mask the receiver would not change.
static void sbf_log_masks(void)
{
    static const char *const names[SBF_MASKS] = {[SBF_MASK_TRACKING] = "tracking", [SBF_MASK_PVT] = "PVT"};
    static bool logged;
    static int8_t last[SBF_MASKS];
    static uint8_t known, refused;
    const sbf_control_t *c = &sbf_ctl;
    char now[64] = "", set[96] = "", kept[32] = "";
    bool changed = !logged || known != c->masks_known;
    for (unsigned i = 0; i < SBF_MASKS; i++) {
        const unsigned bit = 1u << i;
        if (!(c->masks_known & bit)) continue;
        if (last[i] != c->mask[i]) changed = true;
        snprintf(now + strlen(now), sizeof now - strlen(now), "%s%s %d deg", now[0] ? ", " : "", names[i], (int)c->mask[i]);
        if (c->masks_set & bit)
            snprintf(set + strlen(set), sizeof set - strlen(set), "%s%s from %d to %d deg", set[0] ? ", " : "", names[i],
                     (int)c->mask_before[i], (int)c->mask[i]);
        if (c->masks_refused & bit)
            snprintf(kept + strlen(kept), sizeof kept - strlen(kept), "%s%s", kept[0] ? " and " : "", names[i]);
    }
    if (!c->masks_known) return;
    if (changed) ESP_LOGI(TAG, "receiver elevation masks: %s", now);
    if (set[0]) ESP_LOGI(TAG, "receiver elevation masks: set by the observer: %s", set);
    if (c->masks_refused != refused && kept[0])
        ESP_LOGW(TAG, "receiver elevation masks: the receiver would not change its %s mask", kept);
    logged = true;
    known = c->masks_known; refused = c->masks_refused;
    memcpy(last, c->mask, sizeof last);
}
// The satellites behind the counts, by name, and how many of them each signal is tracked on:
// which satellite a count gained or lost, and whether a signal that was switched on is heard.
// False while no measurement epoch is recent enough to speak for now.
static bool sbf_log_satellites(int64_t now)
{
    static char text[1280];
    if (!health.satellites_valid || now - health.satellites_ms > 5000) return false;
    if (!gnss_status_satellites_text(&health, text, sizeof text)) snprintf(text, sizeof text, "none");
    ESP_LOGI(TAG, "satellites (elevation deg/C/N0 dB-Hz): %s", text);
    if (sbf_adapter.signals_valid && now - sbf_adapter.signals_ms <= 5000) {
        if (!sbf_signal_counts(sbf_adapter.signal_satellites, text, sizeof text)) snprintf(text, sizeof text, "none");
        ESP_LOGI(TAG, "satellites per signal: %s", text);
    }
    return true;
}
static void sbf_on_line(const char *line, void *ctx)
{
    (void)ctx;
    // A signal this receiver does not offer is refused by design (sbf_control.h), and reported
    // once with the signal list; any other refusal is a fault in the configuration.
    if (!strncmp(line, "$R?", 3) && !sbf_ctl.adding) ESP_LOGW(TAG, "receiver refused a command: %s", line);
    sbf_control_line(&sbf_ctl, line);
}
static void sbf_on_prompt(const char *prompt, void *ctx) { (void)ctx; (void)prompt; sbf_control_prompt(&sbf_ctl); }
static int sbf_rts_threshold(void) { return UART_HW_FIFO_LEN(RX_UART) - 8; }
// Carries out the control's actions until it has none for now.
static void sbf_act(int64_t now)
{
    for (unsigned i = 0; i < 8; i++) {
        const sbf_action_t a = sbf_control_poll(&sbf_ctl, now);
        esp_err_t err = ESP_OK;
        switch (a.kind) {
        case SBF_ACTION_NONE:
            return;
        case SBF_ACTION_SEND:
            // The TX ring takes the line whole, so a write never waits on the receiver.
            if (uart_write_bytes(RX_UART, a.text, strlen(a.text)) < 0) err = ESP_FAIL;
            break;
        case SBF_ACTION_CTS_ON:
            err = uart_set_hw_flow_ctrl(RX_UART, UART_HW_FLOWCTRL_CTS_RTS, sbf_rts_threshold());
            ESP_LOGI(TAG, "COM2 RTS/CTS confirmed by the receiver; honoring CTS on GPIO%d", sbf_pins.cts);
            break;
        case SBF_ACTION_CTS_OFF:
            err = uart_set_hw_flow_ctrl(RX_UART, UART_HW_FLOWCTRL_RTS, sbf_rts_threshold());
            break;
        case SBF_ACTION_RESET_ON:
            ESP_LOGW(TAG, "receiver not answering commands; reset pulse %u of this boot on GPIO%d", sbf_ctl.resets,
                     sbf_pins.reset);
            err = gpio_set_level(sbf_pins.reset, 1);
            break;
        case SBF_ACTION_RESET_OFF:
            err = gpio_set_level(sbf_pins.reset, 0);
            break;
        }
        if (err != ESP_OK) ESP_LOGW(TAG, "receiver action %d failed: %s", a.kind, esp_err_to_name(err));
    }
}
static void sbf_task(void *arg)
{
    (void)arg;
    uint8_t buf[512];
    int64_t next_log = 0, next_front_ends = 0, next_satellites = 0;
    unsigned configurations = 0;
    sbf_reader_init(sbf_reader, sbf_on_block, sbf_on_line, sbf_on_prompt, NULL);
    sbf_status_init(&sbf_adapter);
    sbf_control_init(&sbf_ctl, sbf_pins.reset >= 0, esp_timer_get_time() / 1000);
    esp_task_wdt_add(NULL);
    for (;;) {
        esp_task_wdt_reset();
        atomic_fetch_add_explicit(&s_ticks, 1, memory_order_relaxed);
        int n = uart_read_bytes(RX_UART, buf, sizeof buf, pdMS_TO_TICKS(50));
        if (n > 0) sbf_reader_feed(sbf_reader, buf, (size_t)n);
        // Each epoch is fresh within a second, so a snapshot request needs no poll.
        atomic_store(&snapshot_requested, false);
        const int64_t now = esp_timer_get_time() / 1000;
        sbf_act(now);
        if (sbf_ctl.configurations != configurations) {
            configurations = sbf_ctl.configurations;
            ESP_LOGI(TAG, "receiver COM2 configured (%u time%s this boot, %u command error%s): SBF at 1 Hz",
                     configurations, configurations == 1 ? "" : "s", sbf_ctl.command_errors,
                     sbf_ctl.command_errors == 1 ? "" : "s");
            if (sbf_ctl.supported_known && health.supported != sbf_ctl.supported) {
                health.supported = sbf_ctl.supported;
                sbf_publish();
                ESP_LOGI(TAG, "receiver tracks constellations 0x%02x (UBX gnssId bits)", health.supported);
            }
            sbf_log_signals();
            sbf_log_masks();
        }
        if (now >= next_log) {
            static const char *const states[] = {"starting", "configuring", "running", "waiting to retry", "resetting"};
            ESP_LOGI(TAG, "UART1 TX=GPIO%d RX=GPIO%d RTS=GPIO%d CTS=GPIO%d baud=%d bytes=%lu SBF=%lu forwarded=%lu "
                     "CRC errors=%lu scrambled epochs=%lu resets=%u %s%s",
                     RX_PIN_TX, RX_PIN_RX, sbf_pins.rts, sbf_pins.cts, SBF_BAUD, (unsigned long)sbf_reader->bytes,
                     (unsigned long)sbf_reader->blocks, (unsigned long)sbf_forwarded, (unsigned long)sbf_reader->crc_errors,
                     (unsigned long)sbf_adapter.scrambled_epochs, sbf_ctl.resets, states[sbf_ctl.state],
                     sbf_ctl.cts ? ", CTS honored" : "");
            if (!sbf_reader->bytes) ESP_LOGW(TAG, "no UART bytes observed; check GNSS power and the COM2 path");
            if (sbf_adapter.scrambled_epochs)
                ESP_LOGW(TAG, "MeasEpoch scrambled: the receiver lacks the Measurement Availability permission");
            if (health.satellites_valid)
                ESP_LOGI(TAG, "tracked: GPS=%u SBAS=%u GAL=%u BDS=%u QZSS=%u GLO=%u NavIC=%u fix=%s",
                         health.tracked[0], health.tracked[1], health.tracked[2], health.tracked[3], health.tracked[5],
                         health.tracked[6], health.tracked[7], gnss_status_position_fresh(&health, now) ? "valid" : "unavailable");
            if (now >= next_front_ends && sbf_log_front_ends(now)) next_front_ends = now + 30000;
            if (now >= next_satellites && sbf_log_satellites(now)) next_satellites = now + 60000;
            next_log = now + 10000;
        }
    }
}
static esp_err_t sbf_start(const ubx_parser_t *parser)
{
    if (sbf_pins.rts < 0 || sbf_pins.cts < 0) return ESP_ERR_INVALID_ARG;
    sbf_emit = parser->emit;
    sbf_now = parser->now;
    sbf_emit_ctx = parser->ctx;
    sbf_reader = heap_caps_malloc_prefer(sizeof *sbf_reader, 2, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT,
                                         MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    sbf_record = heap_caps_malloc_prefer(GNF1_SBF_RECORD_MAX, 2, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT,
                                         MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    if (!sbf_reader || !sbf_record) return ESP_ERR_NO_MEM;
    // Q1 off before anything else: the receiver runs unless a recovery pulse holds it.
    if (sbf_pins.reset >= 0) {
        gpio_set_level(sbf_pins.reset, 0);
        const gpio_config_t reset = {.pin_bit_mask = 1ULL << sbf_pins.reset, .mode = GPIO_MODE_OUTPUT};
        esp_err_t err = gpio_config(&reset);
        if (err != ESP_OK) return err;
    }
    uart_config_t cfg = {.baud_rate = SBF_BAUD, .data_bits = UART_DATA_8_BITS, .parity = UART_PARITY_DISABLE,
        .stop_bits = UART_STOP_BITS_1, .flow_ctrl = UART_HW_FLOWCTRL_RTS, .rx_flow_ctrl_thresh = (uint8_t)sbf_rts_threshold(),
        .source_clk = UART_SCLK_DEFAULT};
    // An 8 KiB RX ring holds about two seconds of the 1 Hz output; the TX ring keeps writes from blocking.
    esp_err_t err = uart_driver_install(RX_UART, 8192, 1024, 0, NULL, 0);
    if (err == ESP_OK) err = uart_param_config(RX_UART, &cfg);
    if (err == ESP_OK) err = uart_set_pin(RX_UART, RX_PIN_TX, RX_PIN_RX, sbf_pins.rts, sbf_pins.cts);
    if (err != ESP_OK) return err;
    return xTaskCreate(sbf_task, "rx", 4096, NULL, 7, NULL) == pdPASS ? ESP_OK : ESP_ERR_NO_MEM;
}

esp_err_t receiver_start(ubx_parser_t *parser)
{
    if (sbf_mode) return sbf_start(parser);
    uart_config_t cfg = {.baud_rate = CONFIG_NVF_RX_BAUD, .data_bits = UART_DATA_8_BITS,
        .parity = UART_PARITY_DISABLE, .stop_bits = UART_STOP_BITS_1,
        .flow_ctrl = UART_HW_FLOWCTRL_DISABLE, .source_clk = UART_SCLK_DEFAULT};
    esp_err_t err = uart_driver_install(RX_UART, 4096, 0, 0, NULL, 0);
    if (err != ESP_OK) return err;
    if ((err = uart_param_config(RX_UART, &cfg)) != ESP_OK) return err;
    if ((err = uart_set_pin(RX_UART, RX_PIN_TX, RX_PIN_RX, UART_PIN_NO_CHANGE, UART_PIN_NO_CHANGE)) != ESP_OK) return err;
    parser->observe = observe;
    parser->rf_detail = log_rf_detail;
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
