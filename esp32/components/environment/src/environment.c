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
#if CONFIG_NVF_ENV_HDC2022
static const env_hdc_variant_t hdc_variant = ENV_HDC2022;
static const char *hdc_name = "HDC2022";
static const char *hdc_conversion = "Rev A conversion";
#else
static const env_hdc_variant_t hdc_variant = ENV_HDC2080;
static const char *hdc_name = "HDC2080";
static const char *hdc_conversion = "Rev C conversion, nominal 3.3 V correction";
#endif
static const uint8_t addresses[] = {0x18, 0x40, 0x76};
static i2c_master_dev_handle_t devices[3];
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
    for (unsigned i = 0; i < 3; i++) if (addresses[i] == address) return devices[i];
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
        for (unsigned i = 0; i < 3; i++) attach(bus, i);
        env_io_t io = {.read = read_register, .write = write_register, .delay_ms = delay_ms};
        env_sensors_init(&sensors, &io, hdc_variant); initialized = true;
        ESP_LOGI(TAG, "configured humidity sensor: %s (%s)", hdc_name, hdc_conversion);
        ESP_LOGI(TAG, "measurement setup: MCP9808=%s %s=%s BMP388/BMP384=%s",
            sensors.mcp_ready ? "ready" : "unavailable", hdc_name, sensors.hdc_ready ? "ready" : "unavailable",
            sensors.bmp_ready ? "ready" : "unavailable");
        if (sensors.hdc_ready) heater_load();
    } else if (!sensors.hdc_ready) {
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
    if (sensors.hdc_ready) {
        bool recovering = heater.state == ENV_HEATER_RECOVERING;
        if (env_heater_sample(&heater, now, utc_valid, utc, &sample) == ENV_HEATER_START) heater_start(now);
        if (recovering && heater.state == ENV_HEATER_NORMAL) heater_log(&heater.run);
        env_heater_status(&heater, now, status);
    }
    *ready = (sensors.mcp_ready ? 1 : 0) | (sensors.hdc_ready ? 2 : 0) | (sensors.bmp_ready ? 4 : 0);
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
    else if (!heating) ESP_LOGW(TAG, "%s measurement unavailable", hdc_name);
    if (sample.bmp_valid) ESP_LOGI(TAG, "BMP388/BMP384 temperature=%.2f C pressure=%.2f hPa (local absolute)%s", sample.bmp_c, sample.pressure_pa / 100.0, mark);
    else ESP_LOGW(TAG, "BMP388/BMP384 measurement unavailable");
    if (heater.state != ENV_HEATER_NORMAL) { sample.hdc_valid = false; sample.hdc_c = sample.rh_percent = 0; }
    *out = sample;
}
#else
void environment_sample(i2c_master_bus_handle_t bus, int64_t now_ms, bool utc_valid, int64_t utc,
                        env_sample_t *out, uint8_t *ready, env_heater_status_t *heater)
{
    (void)bus; (void)now_ms; (void)utc_valid; (void)utc;
    memset(out, 0, sizeof *out); *ready = 0; *heater = (env_heater_status_t){0};
}
void environment_poll(int64_t now_ms) { (void)now_ms; }
#endif
