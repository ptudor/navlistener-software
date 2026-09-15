#include "observer_rtc.h"
#include "rtc_io.h"
#include "sdkconfig.h"
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
#include <string.h>
#include "esp_log.h"
#include "nvs.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
static const char *TAG = "rtc";
static i2c_master_dev_handle_t dev;
static rtc_candidate_t candidate;
static int64_t next_poll;
static enum { UNREPORTED, WAITING, RUNNING } report_state;
static bool retry_initialization;
static bool read_registers(void *ctx, uint8_t reg, uint8_t *out, size_t length)
{ return i2c_master_transmit_receive(ctx, &reg, 1, out, length, 100) == ESP_OK; }
static bool write_registers(void *ctx, uint8_t reg, const uint8_t *data, size_t length)
{
    if (reg > 7 || length > (size_t)(8 - reg)) return false;
    uint8_t buffer[9]; buffer[0] = reg; memcpy(buffer+1, data, length);
    return i2c_master_transmit(ctx, buffer, length + 1, 100) == ESP_OK;
}
static void delay_ms(void *ctx, unsigned ms) { (void)ctx; vTaskDelay(pdMS_TO_TICKS(ms)); }
static bool save_power_failure(void *ctx, const uint8_t calendar[7], const uint8_t stamps[8])
{
    (void)ctx;
    // Version, raw calendar at discovery, then PWRDNMIN..PWRUPMTH. The
    // hardware timestamps have no year; keep the calendar as context only.
    uint8_t record[16] = {1};
    memcpy(record + 1, calendar, 7); memcpy(record + 8, stamps, 8);
    nvs_handle_t nvs;
    esp_err_t err = nvs_open("nvf_rtc", NVS_READWRITE, &nvs);
    if (err == ESP_OK) {
        err = nvs_set_blob(nvs, "last_pfail", record, sizeof record);
        if (err == ESP_OK) err = nvs_commit(nvs);
        nvs_close(nvs);
    }
    if (err != ESP_OK) ESP_LOGE(TAG, "power-fail evidence could not be saved; RTC write deferred: %s", esp_err_to_name(err));
    else ESP_LOGI(TAG, "power-fail calendar/timestamps saved in NVS before RTCWKDAY clears them");
    return err == ESP_OK;
}
void observer_rtc_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now)
{
    if (!bus || now < next_poll) return;
    next_poll = now + 1000;
    if (!dev) {
        if (i2c_master_probe(bus, 0x6f, 100) != ESP_OK) { next_poll = now + 60000; return; }
        i2c_device_config_t config = {.dev_addr_length = I2C_ADDR_BIT_LEN_7,
            .device_address = 0x6f, .scl_speed_hz = 100000};
        if (i2c_master_bus_add_device(bus, &config, &dev) != ESP_OK) return;
    }
    rtc_io_t io = {.ctx = dev, .read = read_registers, .write = write_registers,
        .delay = delay_ms, .save_power_failure = save_power_failure};
    uint8_t regs[7]; int64_t epoch;
    if (!read_registers(dev, 0, regs, sizeof regs)) {
        ESP_LOGW(TAG, "timekeeping read failed; clock left unchanged");
        candidate = (rtc_candidate_t){0}; next_poll = now + 30000; return;
    }
    if (!retry_initialization && rtc_running(regs, &epoch)) {
        if (!rtc_enable_backup(&io)) { ESP_LOGW(TAG, "battery backup enable failed"); return; }
        if (report_state != RUNNING) ESP_LOGI(TAG, "running calendar retained (UTC epoch=%lld); backup enabled, battery presence unverified",
                               (long long)epoch);
        report_state = RUNNING; candidate = (rtc_candidate_t){0}; next_poll = now + 10000;
        return;
    }
    if (report_state != WAITING) ESP_LOGW(TAG, "RTC stopped or calendar unconfirmed; waiting for valid GNSS lock and UTC");
    report_state = WAITING;
    // A subsequent oscillator failure is also recoverable, but never from a
    // guessed build date or an unqualified receiver calendar.
    if (!rtc_gnss_candidate(&candidate, gnss, now, &epoch)) return;
    if (rtc_set_verified(&io, epoch)) {
        retry_initialization = false; report_state = RUNNING;
        ESP_LOGI(TAG, "initialized from GNSS UTC (epoch=%lld); oscillator advancing, backup enabled; battery presence unverified",
                 (long long)epoch);
    } else {
        retry_initialization = true;
        ESP_LOGE(TAG, "RTC initialization/readback failed; time unconfirmed");
        next_poll = now + 30000;
    }
    candidate = (rtc_candidate_t){0};
}
#else
void observer_rtc_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now)
{ (void)bus; (void)gnss; (void)now; }
#endif
