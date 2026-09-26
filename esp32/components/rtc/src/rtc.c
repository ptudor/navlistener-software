#include <string.h>
#include "observer_rtc.h"
#include "rtc_io.h"
#include "rtc_max31328.h"
#include "esp_log.h"
#include "nvs.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
static const char *TAG = "rtc";
static report_rtc_t telemetry;
static uint8_t square_state, square_control, square_trim;
void observer_rtc_square_wave_status(uint8_t *state, uint8_t *control, uint8_t *trim)
{ *state=square_state; *control=square_control; *trim=square_trim; }
report_rtc_t observer_rtc_status(void) { return telemetry; }
static observer_rtc_part_t part;
static i2c_master_dev_handle_t dev;
static rtc_candidate_t candidate;
static int64_t next_poll;
static enum { UNREPORTED, WAITING, RUNNING, UNUSABLE } report_state;
static bool retry_initialization;
void observer_rtc_select(observer_rtc_part_t selected) { part = selected; }
static bool read_registers(void *ctx, uint8_t reg, uint8_t *out, size_t length)
{ return i2c_master_transmit_receive(ctx, &reg, 1, out, length, 100) == ESP_OK; }
static void delay_ms(void *ctx, unsigned ms) { (void)ctx; vTaskDelay(pdMS_TO_TICKS(ms)); }
static bool attach(i2c_master_bus_handle_t bus, uint8_t address, int64_t now)
{
    if (dev) return true;
    if (i2c_master_probe(bus, address, 100) != ESP_OK) { next_poll = now + 60000; return false; }
    i2c_device_config_t config = {.dev_addr_length = I2C_ADDR_BIT_LEN_7,
        .device_address = address, .scl_speed_hz = 100000};
    return i2c_master_bus_add_device(bus, &config, &dev) == ESP_OK;
}

// MCP79412 (NEO and MAX) at 0x6f.
static bool mcp79412_write_registers(void *ctx, uint8_t reg, const uint8_t *data, size_t length)
{
    if (reg > 7 || length > (size_t)(8 - reg)) return false;
    uint8_t buffer[9]; buffer[0] = reg; memcpy(buffer+1, data, length);
    return i2c_master_transmit(ctx, buffer, length + 1, 100) == ESP_OK;
}
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
static void mcp79412_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now)
{
    next_poll = now + 1000;
    if (!attach(bus, 0x6f, now)) return;
    rtc_io_t io = {.ctx = dev, .read = read_registers, .write = mcp79412_write_registers,
        .delay = delay_ms, .save_power_failure = save_power_failure};
    unsigned previous_square=square_state;
    square_state=rtc_square_wave(&io,&square_control,&square_trim);
    if (square_state != previous_square)
        ESP_LOGI(TAG,"MFP GPIO15 square wave state=%u control=0x%02x trim=0x%02x (1=1Hz,2=stopped,3=conflict,4=I/O)",
            square_state,square_control,square_trim);
    uint8_t regs[7]; int64_t epoch;
    telemetry = (report_rtc_t){.sampled_ms = now};
    if (!read_registers(dev, 0, regs, sizeof regs)) {
        ESP_LOGW(TAG, "timekeeping read failed; clock left unchanged");
        candidate = (rtc_candidate_t){0}; next_poll = now + 30000; return;
    }
    telemetry.flags = 1 | ((regs[3] & 0x20) ? 2 : 0) | ((regs[3] & 8) ? 4 : 0) | ((regs[3] & 0x10) ? 8 : 0);
    if (!retry_initialization && rtc_running(regs, &epoch)) {
        telemetry.flags |= 16; telemetry.epoch = epoch;
        if (!rtc_enable_backup(&io)) { ESP_LOGW(TAG, "battery backup enable failed"); return; }
        if (report_state != RUNNING) ESP_LOGI(TAG, "running calendar retained (UTC epoch=%lld); backup enabled, battery presence unverified",
                               (long long)epoch);
        // Re-read after the backup write; the write can clear PWRFAIL.
        telemetry = (report_rtc_t){.sampled_ms = now};
        if (read_registers(dev, 0, regs, sizeof regs)) {
            telemetry.flags = 1 | ((regs[3] & 0x20) ? 2 : 0) | ((regs[3] & 8) ? 4 : 0) | ((regs[3] & 0x10) ? 8 : 0);
            if (rtc_running(regs, &epoch)) { telemetry.flags |= 16; telemetry.epoch = epoch; }
        }
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
        // The verified write advanced through a second boundary; obtain the actual calendar next poll.
        telemetry = (report_rtc_t){.sampled_ms = now};
        ESP_LOGI(TAG, "initialized from GNSS UTC (epoch=%lld); oscillator advancing, backup enabled; battery presence unverified",
                 (long long)epoch);
    } else {
        retry_initialization = true;
        ESP_LOGE(TAG, "RTC initialization/readback failed; time unconfirmed");
        next_poll = now + 30000;
    }
    candidate = (rtc_candidate_t){0};
}

// MAX31328 (ZED/X20) at 0x68.
// tREC: after VCC rises the MAX31328 holds RST for up to 300 ms (ADI 19-100978 p4).
#define MAX31328_RECOVERY_MS 300
static bool max31328_write_registers(void *ctx, uint8_t reg, const uint8_t *data, size_t length)
{
    // Calendar, control and status only: alarms and aging stay as they are.
    if (length > 7 || !((reg + length <= 7) || ((reg == MAX31328_CONTROL || reg == MAX31328_STATUS) &&
        reg + length <= MAX31328_STATUS + 1))) return false;
    uint8_t buffer[8]; buffer[0] = reg; memcpy(buffer + 1, data, length);
    return i2c_master_transmit(ctx, buffer, length + 1, 100) == ESP_OK;
}
static void max31328_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now)
{
    if (now < MAX31328_RECOVERY_MS) return;
    next_poll = now + 1000;
    if (!attach(bus, MAX31328_ADDRESS, now)) return;
    rtc_io_t io = {.ctx = dev, .read = read_registers, .write = max31328_write_registers, .delay = delay_ms};
    unsigned previous_square = square_state;
    square_state = max31328_square_wave(&io, &square_control, &square_trim);
    if (square_state != previous_square)
        ESP_LOGI(TAG, "INT/SQW GPIO15 square wave state=%u control=0x%02x aging=0x%02x (1=1Hz,2=stopped,3=alarm,4=I/O)",
                 square_state, square_control, square_trim);
    uint8_t regs[7], state[2]; int64_t epoch;
    telemetry = (report_rtc_t){.sampled_ms = now};
    if (!read_registers(dev, 0, regs, sizeof regs) || !read_registers(dev, MAX31328_CONTROL, state, 2)) {
        ESP_LOGW(TAG, "MAX31328 read failed; clock left unchanged");
        candidate = (rtc_candidate_t){0}; next_poll = now + 30000; return;
    }
    // Backup switchover is automatic, so backup is always enabled; a stop, including a
    // failed switchover, shows as OSF and leaves the oscillator-running flag clear.
    bool oscillator = !(state[0] & MAX31328_EOSC) && !(state[1] & MAX31328_OSF);
    telemetry.flags = 1 | (oscillator ? 2 : 0) | 4;
    if (max31328_running(regs, state[0], state[1], &epoch)) {
        telemetry.flags |= 16; telemetry.epoch = epoch;
        if (report_state != RUNNING)
            ESP_LOGI(TAG, "MAX31328 running calendar retained (UTC epoch=%lld); no oscillator stop recorded",
                     (long long)epoch);
        report_state = RUNNING; candidate = (rtc_candidate_t){0}; next_poll = now + 10000;
        return;
    }
    if (report_state == UNUSABLE) { next_poll = now + 600000; return; }
    if (report_state != WAITING)
        ESP_LOGW(TAG, "MAX31328 calendar invalid (oscillator-stop flag %s); waiting for valid GNSS lock and UTC",
                 state[1] & MAX31328_OSF ? "set: power was lost or the switch to the backup cell failed" : "clear");
    report_state = WAITING;
    if (!rtc_gnss_candidate(&candidate, gnss, now, &epoch)) return;
    switch (max31328_set_verified(&io, epoch)) {
    case MAX31328_SET_OK:
        report_state = RUNNING;
        telemetry = (report_rtc_t){.sampled_ms = now};
        ESP_LOGI(TAG, "MAX31328 set from GNSS UTC (epoch=%lld); oscillator-stop flag cleared and advancing",
                 (long long)epoch);
        break;
    case MAX31328_SET_OSF_STUCK:
        // The flag would not clear, so this part can never show valid time. Stop
        // rewriting it; GNSS remains the only trusted time source.
        report_state = UNUSABLE; next_poll = now + 600000;
        ESP_LOGE(TAG, "MAX31328 oscillator-stop flag does not clear; RTC time will not be trusted");
        break;
    default:
        ESP_LOGE(TAG, "MAX31328 initialization/readback failed; time unconfirmed");
        next_poll = now + 30000;
        break;
    }
    candidate = (rtc_candidate_t){0};
}

void observer_rtc_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now)
{
    if (!bus || now < next_poll) return;
    if (part == OBSERVER_RTC_MCP79412) mcp79412_poll(bus, gnss, now);
    else if (part == OBSERVER_RTC_MAX31328) max31328_poll(bus, gnss, now);
}
