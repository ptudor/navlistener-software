#include "environment.h"
#include "sdkconfig.h"
#include <string.h>
#if CONFIG_NVF_BOARD_GNSS_COLOR
#include <string.h>
#include <stdio.h>
#include "env_sensors.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "nvs.h"
static const char *TAG = "environment";
// The parts the manifest lists, set by the board before the first sample; nothing else
// is probed. HDC2080 and HDC2022 share their ID registers, so only the manifest can
// choose the temperature formula.
static env_parts_t parts = {.hdc = ENV_HDC_NONE};
static env_hdc_variant_t hdc_variant = ENV_HDC_NONE;
static const char *hdc_name = "HDC";
static const char *hdc_conversion = "";
// The part in the sample's barometer slot, for the logs.
static const char *barometer_name = "BMP388/BMP384";
void environment_configure(const env_parts_t *listed)
{
    parts = *listed;
    barometer_name = parts.bmp5 == ENV_BMP581 ? "BMP581" : parts.bmp5 == ENV_BMP580 ? "BMP580" : "BMP388/BMP384";
    hdc_variant = parts.hdc;
    hdc_name = hdc_variant == ENV_HDC2022 ? "HDC2022" : hdc_variant == ENV_HDC2080 ? "HDC2080" : "HDC";
    hdc_conversion = hdc_variant == ENV_HDC2022 ? "Rev A conversion" :
                     hdc_variant == ENV_HDC2080 ? "Rev C conversion, nominal 3.3 V correction" : "";
}
// 0x40 is attached even when no HDC is listed, to confirm an unlisted one's heater is off.
static const uint8_t addresses[] = {0x18, 0x40, 0x76, MS5607_ADDRESS, MMC34160_ADDRESS, BMP5_ADDRESS, INA3221_ADDRESS};
enum { TEMPERATURE_DEVICE, HUMIDITY_DEVICE, PRESSURE_DEVICE, BAROMETER_DEVICE, MAGNETOMETER_DEVICE, BMP5_DEVICE,
       RAIL_DEVICE };
#define DEVICE_COUNT (sizeof addresses / sizeof addresses[0])
static i2c_master_dev_handle_t devices[DEVICE_COUNT];
static i2c_master_bus_handle_t sensor_bus;
static env_sensors_t sensors;
static bool initialized;
// Condensation recovery parameters (Kconfig.projbuild); docs/HARDWARE-OBSERVER.md section 6.4.
static const env_heater_config_t heater_config = {
    .trigger_rh = CONFIG_NVF_ENV_HEATER_TRIGGER_RH_TENTHS / 10.0,
    .trigger_ms = CONFIG_NVF_ENV_HEATER_TRIGGER_MINUTES * 60000LL,
    .gap_ms = CONFIG_NVF_ENV_HEATER_GAP_MINUTES * 60000LL,
    .min_c = CONFIG_NVF_ENV_HEATER_MIN_TENTHS_C / 10.0, .max_c = CONFIG_NVF_ENV_HEATER_MAX_TENTHS_C / 10.0,
    .interval_s = CONFIG_NVF_ENV_HEATER_INTERVAL_DAYS * 86400LL,
    .step_ms = CONFIG_NVF_ENV_HEATER_STEP_SECONDS * 1000LL,
    .dry_rh = CONFIG_NVF_ENV_HEATER_DRY_RH_TENTHS / 10.0,
    .max_on_ms = CONFIG_NVF_ENV_HEATER_MAX_ON_SECONDS * 1000LL,
    .overtemp_c = CONFIG_NVF_ENV_HEATER_OVERTEMP_TENTHS_C / 10.0,
    .settle_ms = CONFIG_NVF_ENV_HEATER_SETTLE_MINUTES * 60000LL,
    .stable_ms = CONFIG_NVF_ENV_HEATER_STABLE_MINUTES * 60000LL,
    .stable_c = CONFIG_NVF_ENV_HEATER_STABLE_TENTHS_C / 10.0,
    .recovery_max_ms = CONFIG_NVF_ENV_HEATER_RECOVERY_MAX_MINUTES * 60000LL,
};
_Static_assert(CONFIG_NVF_ENV_HEATER_MIN_TENTHS_C < CONFIG_NVF_ENV_HEATER_MAX_TENTHS_C,
    "humidity heater start temperature window is empty");
_Static_assert(CONFIG_NVF_ENV_HEATER_MAX_TENTHS_C < CONFIG_NVF_ENV_HEATER_OVERTEMP_TENTHS_C,
    "humidity heater could start at or above its over-temperature stop");
_Static_assert(CONFIG_NVF_ENV_HEATER_DRY_RH_TENTHS < CONFIG_NVF_ENV_HEATER_TRIGGER_RH_TENTHS,
    "humidity heater dry stop must be below the condensing trigger");
_Static_assert(CONFIG_NVF_ENV_HEATER_STEP_SECONDS < CONFIG_NVF_ENV_HEATER_MAX_ON_SECONDS,
    "humidity heater needs at least one conversion step before its on-time limit");
_Static_assert(CONFIG_NVF_ENV_HEATER_SETTLE_MINUTES <= CONFIG_NVF_ENV_HEATER_RECOVERY_MAX_MINUTES &&
    CONFIG_NVF_ENV_HEATER_STABLE_MINUTES <= CONFIG_NVF_ENV_HEATER_RECOVERY_MAX_MINUTES,
    "humidity heater recovery limit is shorter than its settling requirements");
static env_heater_t heater;

// Adds addresses[i] to the bus once it answers a probe.
static void attach(i2c_master_bus_handle_t bus, unsigned i)
{
    if (devices[i] || i2c_master_probe(bus, addresses[i], 100) != ESP_OK) return;
    i2c_device_config_t config = {.dev_addr_length = I2C_ADDR_BIT_LEN_7,
        .device_address = addresses[i], .scl_speed_hz = 100000};
    if (i2c_master_bus_add_device(bus, &config, &devices[i]) != ESP_OK) devices[i] = NULL;
}
static i2c_master_dev_handle_t device(uint8_t address)
{
    for (unsigned i = 0; i < DEVICE_COUNT; i++) if (addresses[i] == address) return devices[i];
    return NULL;
}
static bool read_register(void *ctx, uint8_t address, uint8_t reg, uint8_t *p, size_t n)
{
    (void)ctx; i2c_master_dev_handle_t dev = device(address);
    return dev && i2c_master_transmit_receive(dev, &reg, 1, p, n, 100) == ESP_OK;
}
static bool write_register(void *ctx, uint8_t address, uint8_t reg, const uint8_t *p, size_t n)
{
    (void)ctx; i2c_master_dev_handle_t dev = device(address);
    uint8_t data[65];
    if (!dev || n > sizeof data - 1) return false;
    data[0] = reg; memcpy(data + 1, p, n);
    return i2c_master_transmit(dev, data, n + 1, 100) == ESP_OK;
}
static void delay_ms(void *ctx, unsigned ms)
{
    (void)ctx;
    vTaskDelay(pdMS_TO_TICKS(ms + portTICK_PERIOD_MS - 1));
}
// Unix seconds of the last automatic heater run in nvf_env/heater_utc; absent means never.
static void heater_load(void)
{
    int64_t utc = 0;
    nvs_handle_t nvs;
    esp_err_t err = nvs_open("nvf_env", NVS_READONLY, &nvs);
    if (err == ESP_OK) { err = nvs_get_i64(nvs, "heater_utc", &utc); nvs_close(nvs); }
    bool known = err == ESP_ERR_NVS_NOT_FOUND || (err == ESP_OK && utc >= 946684800 && utc < 4102444800LL);
    env_heater_init(&heater, &heater_config, known, err == ESP_OK && known ? utc : 0);
    if (!known) ESP_LOGW(TAG, "humidity heater: last run record unreadable (%s); no automatic run this boot",
                         err == ESP_OK ? "out of range" : esp_err_to_name(err));
    else if (heater.last_utc) ESP_LOGI(TAG, "humidity heater: last automatic run UTC=%lld", (long long)utc);
    else ESP_LOGI(TAG, "humidity heater: no automatic run recorded");
}
static bool heater_save(int64_t utc)
{
    nvs_handle_t nvs;
    esp_err_t err = nvs_open("nvf_env", NVS_READWRITE, &nvs);
    if (err == ESP_OK) {
        err = nvs_set_i64(nvs, "heater_utc", utc);
        if (err == ESP_OK) err = nvs_commit(nvs);
        nvs_close(nvs);
    }
    if (err != ESP_OK) ESP_LOGE(TAG, "humidity heater: run time not saved (%s); not starting, condensing count restarts",
                                esp_err_to_name(err));
    return err == ESP_OK;
}
static const char *reading(char buffer[12], bool valid, double value)
{
    if (valid) snprintf(buffer, 12, "%.2f", value); else snprintf(buffer, 12, "n/a");
    return buffer;
}
// The BMP580/BMP581 fills the sample's barometer slot. A part that is absent or rejected, or a
// conversion that fails (as after a 3V3_SENS power cycle, which resets its configuration), is
// identified and configured again at the next sample.
static bmp5_state_t bmp5_state;
static bool bmp5_tried;
static void bmp5_sample(const env_io_t *io, env_sample_t *sample)
{
    if (bmp5_state != BMP5_READY) {
        attach(sensor_bus, BMP5_DEVICE);
        bmp5_state_t before = bmp5_state;
        if ((bmp5_state = bmp5_init(io)) != before || !bmp5_tried)
            ESP_LOGI(TAG, "%s: %s", barometer_name, bmp5_state == BMP5_READY ? "reset and configured" :
                     bmp5_state == BMP5_REJECTED ? "answers, but not as a BMP5 that completed its reset" : "not answering");
        bmp5_tried = true;
    }
    if (bmp5_state != BMP5_READY) return;
    int32_t centi_c, pa;
    sample->bmp_valid = bmp5_read(io, &centi_c, &pa);
    if (sample->bmp_valid) { sample->bmp_c = centi_c / 100.0; sample->pressure_pa = pa; }
    else bmp5_state = BMP5_ABSENT;
}
static void heater_log(const env_heater_run_t *r)
{
    char b[8][12];
    ESP_LOGI(TAG, "humidity heater run: start UTC=%lld uptime=%llds on=%.1fs stop=%s recovery=%llds "
        "RH before/stop=%s/%s %%RH %s before/peak/end=%s/%s/%s C MCP9808 before/peak/end=%s/%s/%s C",
        (long long)r->start_utc, (long long)(r->start_ms / 1000), r->on_ms / 1000.0, env_heater_stop_name(r->stop),
        (long long)(r->recovery_ms / 1000), reading(b[0], r->valid & ENV_RUN_RH_BEFORE, r->rh_before),
        reading(b[1], r->valid & ENV_RUN_RH_STOP, r->rh_stop), hdc_name,
        reading(b[2], r->valid & ENV_RUN_HDC_BEFORE, r->hdc_before), reading(b[3], r->valid & ENV_RUN_HDC_PEAK, r->hdc_peak),
        reading(b[4], r->valid & ENV_RUN_HDC_END, r->hdc_end), reading(b[5], r->valid & ENV_RUN_MCP_BEFORE, r->mcp_before),
        reading(b[6], r->valid & ENV_RUN_MCP_PEAK, r->mcp_peak), reading(b[7], r->valid & ENV_RUN_MCP_END, r->mcp_end));
}
// Retried here and then at every heater step: the heater must never be left on.
static void heater_off(int64_t now)
{
    bool off = false;
    for (unsigned i = 0; i < 3 && !off; i++) {
        if (i) delay_ms(NULL, 10);
        off = env_hdc_heater_set(&sensors, false);
    }
    env_heater_cleared(&heater, now, off);
    if (!off) {
        ESP_LOGE(TAG, "%s heater NOT confirmed off (%s, stop=%s); retrying every %d s", hdc_name,
                 sensors.io_error ? "I2C error" : "read-back mismatch", env_heater_stop_name(heater.run.stop),
                 CONFIG_NVF_ENV_HEATER_STEP_SECONDS);
        return;
    }
    char b[12];
    ESP_LOGI(TAG, "%s heater off after %.1f s (stop=%s, RH %.2f -> %s %%RH); HDC readings withheld while it cools",
             hdc_name, heater.run.on_ms / 1000.0, env_heater_stop_name(heater.run.stop), heater.run.rh_before,
             reading(b, heater.run.valid & ENV_RUN_RH_STOP, heater.run.rh_stop));
}
static void heater_start(int64_t now)
{
    long long minutes = (now - heater.wet_ms) / 60000;
    if (!heater_save(heater.next.start_utc)) { env_heater_declined(&heater); return; }
    bool on = env_hdc_heater_set(&sensors, true);
    env_heater_stop_t failure = on ? ENV_HEATER_RUNNING : sensors.io_error ? ENV_HEATER_BUS_ERROR : ENV_HEATER_SENSOR_LOST;
    if (on) ESP_LOGW(TAG, "condensing for %lld min (%.2f %%RH at %.2f C): %s heater on, UTC=%lld",
                     minutes, heater.next.rh_before, heater.next.hdc_before, hdc_name, (long long)heater.next.start_utc);
    else ESP_LOGE(TAG, "condensing for %lld min: %s heater did not confirm on (%s)", minutes, hdc_name,
                  env_heater_stop_name(failure));
    if (env_heater_started(&heater, now, failure) == ENV_HEATER_STOP) heater_off(now);
}
void environment_poll(int64_t now)
{
    if (!env_heater_step_due(&heater, now)) return;
    if (heater.state == ENV_HEATER_STOPPING) { heater_off(now); return; }
    env_sample_t sample;
    bool on = false;
    env_sensors_read_some(&sensors, &sample, 3);
    if (!env_hdc_heater_get(&sensors, &on)) sample.bus_error = true;
    char b[3][12];
    ESP_LOGI(TAG, "humidity heater step t=%llds: %s %s C %s %%RH, MCP9808 %s C, HEAT_EN=%d%s",
             (long long)((now - heater.run.start_ms) / 1000), hdc_name, reading(b[0], sample.hdc_valid, sample.hdc_c),
             reading(b[1], sample.hdc_valid, sample.rh_percent), reading(b[2], sample.mcp_valid, sample.mcp_c), on,
             sample.bus_error ? ", I2C error" : "");
    if (env_heater_step(&heater, now, &sample, on) == ENV_HEATER_STOP) heater_off(now);
}
void environment_sample(i2c_master_bus_handle_t bus, int64_t now, bool utc_valid, int64_t utc,
                        env_sample_t *out, uint8_t *ready, env_heater_status_t *status)
{
    memset(out, 0, sizeof *out); *ready = 0; *status = (env_heater_status_t){0};
    if (!bus) return;
    if (!initialized) {
        sensor_bus = bus;
        if (parts.mcp9808) attach(bus, TEMPERATURE_DEVICE);
        attach(bus, HUMIDITY_DEVICE);
        if (parts.bmp388) attach(bus, PRESSURE_DEVICE);
        env_io_t io = {.read = read_register, .write = write_register, .delay_ms = delay_ms};
        env_sensors_init(&sensors, &io, hdc_variant,
                         (parts.mcp9808 ? ENV_PART_MCP9808 : 0) | (parts.bmp388 ? ENV_PART_BMP388 : 0));
        initialized = true;
        if (hdc_variant == ENV_HDC_NONE) {
            // Not measured, but a heater left on by an earlier boot is still turned off.
            bool present = false;
            bool safe = env_hdc_heater_off_unlisted(&io, &present);
            if (!present) ESP_LOGI(TAG, "humidity sensor not listed in the manifest and not answering at 0x40");
            else if (safe) ESP_LOGW(TAG, "an HDC answers at 0x40 but the manifest does not list it: heater confirmed "
                                         "off, humidity not measured until the manifest names the part");
            else ESP_LOGE(TAG, "an unlisted HDC at 0x40 did not confirm its heater off");
        } else {
            ESP_LOGI(TAG, "humidity sensor from the manifest: %s (%s)", hdc_name, hdc_conversion);
        }
        ESP_LOGI(TAG, "measurement setup: MCP9808=%s %s=%s %s=%s",
            !parts.mcp9808 ? "not listed" : sensors.mcp_ready ? "ready" : "unavailable", hdc_name,
            hdc_variant == ENV_HDC_NONE ? "not listed" : sensors.hdc_ready ? "ready" : "unavailable", barometer_name,
            parts.bmp5 ? "identified at each sample" : !parts.bmp388 ? "not listed" : sensors.bmp_ready ? "ready" : "unavailable");
        if (sensors.hdc_ready) heater_load();
    } else if (!sensors.hdc_ready && hdc_variant != ENV_HDC_NONE) {
        // A reboot during a heater run leaves HEAT_EN set until the HDC is configured again, so
        // an HDC that was not identified at boot is retried at every sample.
        attach(bus, 1);
        if (devices[1] && env_sensors_retry_hdc(&sensors)) {
            ESP_LOGI(TAG, "%s identified on retry: heater off, measurements enabled", hdc_name);
            heater_load();
        }
    }
    env_sample_t sample;
    // While HEAT_EN may be set, only the heater steps convert the HDC.
    bool heating = heater.state == ENV_HEATER_HEATING || heater.state == ENV_HEATER_STOPPING;
    env_sensors_read_some(&sensors, &sample, heating ? 5 : 7);
    if (parts.bmp5) {
        env_io_t io = {.read = read_register, .write = write_register, .delay_ms = delay_ms};
        bmp5_sample(&io, &sample);
    }
    if (sensors.hdc_ready) {
        bool recovering = heater.state == ENV_HEATER_RECOVERING;
        if (env_heater_sample(&heater, now, utc_valid, utc, &sample) == ENV_HEATER_START) heater_start(now);
        if (recovering && heater.state == ENV_HEATER_NORMAL) heater_log(&heater.run);
        env_heater_status(&heater, now, status);
    }
    bool barometer_ready = parts.bmp5 ? bmp5_state == BMP5_READY : sensors.bmp_ready;
    *ready = (sensors.mcp_ready ? 1 : 0) | (sensors.hdc_ready ? 2 : 0) | (barometer_ready ? 4 : 0);
    // MCP9808 and BMP readings continue, marked as taken while the heater is on or recovering.
    const char *mark = heater.state == ENV_HEATER_NORMAL ? "" :
        heater.state == ENV_HEATER_RECOVERING ? " (humidity heater recovery)" : " (humidity heater on)";
    if (sample.mcp_valid) ESP_LOGI(TAG, "MCP9808 temperature=%.2f C%s", sample.mcp_c, mark);
    else ESP_LOGW(TAG, "MCP9808 measurement unavailable");
    if (sample.hdc_valid && heater.state == ENV_HEATER_NORMAL)
        ESP_LOGI(TAG, "%s temperature=%.2f C humidity=%.2f %%RH (%s); since boot >=95%%RH %.2f h, >=98%%RH %.2f h",
                 hdc_name, sample.hdc_c, sample.rh_percent, hdc_conversion, status->rh95_ms / 3600000.0, status->rh98_ms / 3600000.0);
    else if (sample.hdc_valid)
        ESP_LOGI(TAG, "%s withheld%s: temperature=%.2f C humidity=%.2f %%RH", hdc_name, mark, sample.hdc_c, sample.rh_percent);
    else if (!heating && hdc_variant != ENV_HDC_NONE) ESP_LOGW(TAG, "%s measurement unavailable", hdc_name);
    if (sample.bmp_valid) ESP_LOGI(TAG, "%s temperature=%.2f C pressure=%.2f hPa (local absolute)%s", barometer_name,
                                   sample.bmp_c, sample.pressure_pa / 100.0, mark);
    else if (parts.bmp388 || parts.bmp5) ESP_LOGW(TAG, "%s measurement unavailable", barometer_name);
    if (heater.state != ENV_HEATER_NORMAL) { sample.hdc_valid = false; sample.hdc_c = sample.rh_percent = 0; }
    *out = sample;
}
static ms5607_t barometer;
static bool barometer_tried, magnetometer_ready, magnetometer_tried;
// Absolute pressure: sea-level correction and any derived height are left to consumers.
void environment_sample_max(env_barometer_t *b, env_magnetometer_t *m)
{
    *b = (env_barometer_t){0}; *m = (env_magnetometer_t){0};
    if (!initialized) return;
    env_io_t io = {.read = read_register, .write = write_register, .delay_ms = delay_ms};
    if (parts.ms5607 && barometer.state != MS5607_READY) {
        attach(sensor_bus, BAROMETER_DEVICE);
        ms5607_state_t before = barometer.state;
        if (ms5607_init(&barometer, &io) != before || !barometer_tried)
            ESP_LOGI(TAG, "MS5607: %s", barometer.state == MS5607_READY ? "calibration PROM verified" :
                     barometer.state == MS5607_PROM_REJECTED ? "calibration PROM failed its CRC" : "not answering");
        barometer_tried = true;
    }
    b->state = barometer.state;
    if (barometer.state == MS5607_READY) {
        b->valid = ms5607_read(&barometer, &io, &b->centi_c, &b->pressure_pa);
        if (b->valid) ESP_LOGI(TAG, "MS5607 temperature=%.2f C pressure=%.2f hPa (local absolute%s)",
                               b->centi_c / 100.0, b->pressure_pa / 100.0,
                               b->pressure_pa < MS5607_FULL_MIN_PA || b->pressure_pa > MS5607_FULL_MAX_PA ?
                               ", extended range" : "");
        else ESP_LOGW(TAG, "MS5607 measurement unavailable");
    }
    if (parts.mmc34160 && !magnetometer_ready) {
        attach(sensor_bus, MAGNETOMETER_DEVICE);
        magnetometer_ready = mmc34160_init(&io);
        if (magnetometer_ready) ESP_LOGI(TAG, "MMC34160PJ identified; 16-bit SET/RESET measurements");
        else if (!magnetometer_tried) ESP_LOGW(TAG, "MMC34160PJ not identified; retrying at each sample");
        magnetometer_tried = true;
    }
    m->ready = magnetometer_ready;
    if (magnetometer_ready) {
        m->valid = mmc34160_measure(&io, &m->sample);
        if (m->valid) ESP_LOGI(TAG, "MMC34160PJ field X=%d Y=%d Z=%d counts (2048/G), bridge offset %u/%u/%u",
                               m->sample.field[0], m->sample.field[1], m->sample.field[2],
                               m->sample.offset[0], m->sample.offset[1], m->sample.offset[2]);
        else ESP_LOGW(TAG, "MMC34160PJ measurement unavailable");
    }
}
static bool rails_ready, rails_tried;
void environment_sample_rails(env_rails_t *r)
{
    *r = (env_rails_t){0};
    if (!initialized || !parts.ina3221) return;
    env_io_t io = {.read = read_register, .write = write_register, .delay_ms = delay_ms};
    if (!rails_ready) {
        attach(sensor_bus, RAIL_DEVICE);
        rails_ready = ina3221_init(&io);
        if (rails_ready) ESP_LOGI(TAG, "INA3221 identified; 64-sample averages of 1.1 ms shunt and bus conversions");
        else if (!rails_tried) ESP_LOGW(TAG, "INA3221 not identified; retrying at each sample");
        rails_tried = true;
    }
    r->ready = rails_ready;
    if (!rails_ready) return;
    r->valid = ina3221_read(&io, &r->sample);
    if (!r->valid) {
        rails_ready = false;
        ESP_LOGW(TAG, "INA3221 measurement unavailable; configuring it again at the next sample");
    }
}
#else
void environment_configure(const env_parts_t *listed) { (void)listed; }
void environment_sample_rails(env_rails_t *r) { *r = (env_rails_t){0}; }
void environment_sample_max(env_barometer_t *b, env_magnetometer_t *m)
{
    *b = (env_barometer_t){0}; *m = (env_magnetometer_t){0};
}
void environment_sample(i2c_master_bus_handle_t bus, int64_t now_ms, bool utc_valid, int64_t utc,
                        env_sample_t *out, uint8_t *ready, env_heater_status_t *heater)
{
    (void)bus; (void)now_ms; (void)utc_valid; (void)utc;
    memset(out, 0, sizeof *out); *ready = 0; *heater = (env_heater_status_t){0};
}
void environment_poll(int64_t now_ms) { (void)now_ms; }
#endif
