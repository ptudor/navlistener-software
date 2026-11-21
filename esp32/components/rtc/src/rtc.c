#include <string.h>
#include <stdio.h>
#include "observer_rtc.h"
#include "rtc_io.h"
#include "rtc_discipline.h"
#include "rtc_max31328.h"
#include "esp_log.h"
#include "esp_timer.h"
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
static rtc_candidate_t service_candidate;
static portMUX_TYPE service_lock=portMUX_INITIALIZER_UNLOCKED;
static rtc_service_request_t service_request;
static rtc_service_record_t service_record;
static enum { SERVICE_IDLE, SERVICE_QUEUED, SERVICE_ACTIVE, SERVICE_DONE } service_state;
bool observer_rtc_service_request(const rtc_service_request_t *r)
{
    portENTER_CRITICAL(&service_lock);
    bool ok=service_state==SERVICE_IDLE || service_state==SERVICE_DONE;
    if(ok){service_request=*r;service_state=SERVICE_QUEUED;}
    portEXIT_CRITICAL(&service_lock);return ok;
}
bool observer_rtc_service_result(rtc_service_record_t *r)
{portENTER_CRITICAL(&service_lock);bool ok=service_state==SERVICE_DONE;if(ok)*r=service_record;portEXIT_CRITICAL(&service_lock);return ok;}
static bool service_latest(nvs_handle_t nvs,uint32_t *latest)
{
    *latest=0;
    for(unsigned i=0;i<32;i++) {
        char key[16];snprintf(key,sizeof key,"service%02u",i);
        rtc_service_record_t r;size_t size=sizeof r;
        esp_err_t err=nvs_get_blob(nvs,key,&r,&size);
        if(err==ESP_ERR_NVS_NOT_FOUND)continue;
        if(err!=ESP_OK || size!=sizeof r || r.version!=1 || !r.sequence || r.sequence%32!=i)return false;
        if(r.sequence>*latest)*latest=r.sequence;
    }
    return true;
}
bool observer_rtc_service_read(uint32_t sequence,rtc_service_record_t *r)
{
    nvs_handle_t nvs;if(nvs_open_from_partition("journal","rtc_service",NVS_READONLY,&nvs)!=ESP_OK)return false;
    if(!sequence && !service_latest(nvs,&sequence)){nvs_close(nvs);return false;}
    char key[16];snprintf(key,sizeof key,"service%02lu",(unsigned long)(sequence%32));size_t size=sizeof *r;
    bool ok=sequence && nvs_get_blob(nvs,key,r,&size)==ESP_OK && size==sizeof *r && r->version==1 && r->sequence==sequence && memchr(r->request.reason,0,sizeof r->request.reason) && memchr(r->request.reference_id,0,sizeof r->request.reference_id);
    nvs_close(nvs);return ok;
}
static bool service_save(void *ctx,rtc_service_record_t *r)
{
    r->rtc_part=(uint8_t)part;
    (void)ctx;nvs_handle_t nvs;if(nvs_open_from_partition("journal","rtc_service",NVS_READWRITE,&nvs)!=ESP_OK)return false;
    esp_err_t err=ESP_OK;
    if(!r->sequence){uint32_t seq=0;if(!service_latest(nvs,&seq) || seq==UINT32_MAX)err=ESP_FAIL;else r->sequence=seq+1;}
    char key[16];snprintf(key,sizeof key,"service%02lu",(unsigned long)(r->sequence%32));
    if(err==ESP_OK)err=nvs_set_blob(nvs,key,r,sizeof *r);
    if(err==ESP_OK)err=nvs_commit(nvs);
    nvs_close(nvs);return err==ESP_OK;
}
void observer_rtc_select(observer_rtc_part_t selected) { part = selected; }
// What the MCP79412's power-fail records have shown of its backup supply (rtc_policy.h).
// The proof is kept as the raw record that showed it, in "nvf_rtc"/"proof", in the layout of
// "last_pfail": version 1, RTCSEC..RTCYEAR as found, PWRDNMIN..PWRUPMTH. The board task
// owns it; another task takes a copy.
#define PFAIL_RECORD 16
static portMUX_TYPE backup_lock = portMUX_INITIALIZER_UNLOCKED;
static observer_rtc_backup_t backup;
static bool backup_loaded;
// Since this start: PWRFAIL has been read set, and the clock read stopped while it was.
static bool power_failed, stopped_on_backup;
static unsigned lost_polls;
observer_rtc_backup_t observer_rtc_backup(void)
{
    portENTER_CRITICAL(&backup_lock);
    const observer_rtc_backup_t copy = backup;
    portEXIT_CRITICAL(&backup_lock);
    return copy;
}
static void backup_set(bool proven, const rtc_power_loss_t *loss)
{
    portENTER_CRITICAL(&backup_lock);
    backup = (observer_rtc_backup_t){.tracked = true, .proven = proven};
    if (proven) backup.loss = *loss;
    portEXIT_CRITICAL(&backup_lock);
}
static void backup_load(void)
{
    if (backup_loaded) return;
    backup_loaded = true;
    uint8_t record[PFAIL_RECORD]; size_t size = sizeof record;
    rtc_power_loss_t loss = {0}; nvs_handle_t nvs;
    esp_err_t err = nvs_open("nvf_rtc", NVS_READONLY, &nvs);
    if (err == ESP_OK) { err = nvs_get_blob(nvs, "proof", record, &size); nvs_close(nvs); }
    const bool proven = err == ESP_OK && size == sizeof record && record[0] == 1 &&
                        rtc_power_loss(record + 1, record + 8, &loss) && rtc_backup_proof(record + 1, &loss);
    if (!proven && err != ESP_ERR_NVS_NOT_FOUND)
        ESP_LOGW(TAG, "saved backup proof not usable (%s); the backup counts as not proven",
                 err == ESP_OK ? "the record does not decode" : esp_err_to_name(err));
    backup_set(proven, &loss);
}
// A proof that no longer holds is erased before the clock is set or armed again, so that a
// failed erase cannot leave it standing beside a clock that has since lost its time.
static bool backup_withdraw(const char *why)
{
    nvs_handle_t nvs;
    esp_err_t err = nvs_open("nvf_rtc", NVS_READWRITE, &nvs);
    if (err == ESP_OK) {
        err = nvs_erase_key(nvs, "proof");
        if (err == ESP_OK) err = nvs_commit(nvs);
        nvs_close(nvs);
    }
    if (err != ESP_OK && err != ESP_ERR_NVS_NOT_FOUND) {
        ESP_LOGE(TAG, "backup proof could not be withdrawn; RTC left as it is: %s", esp_err_to_name(err));
        return false;
    }
    char when[96];
    rtc_power_loss_text(&backup.loss, when, sizeof when);
    ESP_LOGW(TAG, "backup no longer proven: %s; the proof of %s is withdrawn", why, when);
    backup_set(false, NULL);
    return true;
}
static void backup_text(char *out, size_t size)
{
    char when[96];
    if (backup.proven && rtc_power_loss_text(&backup.loss, when, sizeof when))
        snprintf(out, size, "backup proven by the power loss of %s", when);
    else snprintf(out, size, "backup enabled, not yet proven by a power loss");
}
static bool read_registers(void *ctx, uint8_t reg, uint8_t *out, size_t length)
{ return i2c_master_transmit_receive(ctx, &reg, 1, out, length, 100) == ESP_OK; }
static void delay_ms(void *ctx, unsigned ms) { (void)ctx; vTaskDelay(pdMS_TO_TICKS(ms)); }
// The clock the board task samples the receiver on (receiver.c), so a candidate's
// reference projects onto it.
static int64_t now_ms(void *ctx) { (void)ctx; return esp_timer_get_time() / 1000; }
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
// OSCTRIM alone: the register write above stops at CONTROL on purpose.
static bool mcp79412_write_trim(void *ctx, uint8_t osctrim)
{
    const uint8_t buffer[2] = {8, osctrim};
    return i2c_master_transmit(ctx, buffer, sizeof buffer, 100) == ESP_OK;
}
static bool save_power_failure(void *ctx, const uint8_t calendar[7], const uint8_t stamps[8])
{
    (void)ctx;
    uint8_t record[PFAIL_RECORD] = {1};
    memcpy(record + 1, calendar, 7); memcpy(record + 8, stamps, 8);
    rtc_power_loss_t loss = {0};
    const bool decoded = rtc_power_loss(calendar, stamps, &loss);
    // A clock read stopped while this record was on the part did not count through the loss,
    // whatever its stamps say of the time it did count.
    const bool proof = decoded && !stopped_on_backup && rtc_backup_proof(calendar, &loss);
    nvs_handle_t nvs;
    esp_err_t err = nvs_open("nvf_rtc", NVS_READWRITE, &nvs);
    if (err == ESP_OK) {
        err = nvs_set_blob(nvs, "last_pfail", record, sizeof record);
        if (err == ESP_OK && proof) err = nvs_set_blob(nvs, "proof", record, sizeof record);
        if (err == ESP_OK) err = nvs_commit(nvs);
        nvs_close(nvs);
    }
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "power-fail record could not be saved; RTC write deferred: %s", esp_err_to_name(err));
        return false;
    }
    char when[96];
    if (!decoded) {
        ESP_LOGW(TAG, "power loss recorded, but its stamps do not decode (%02x %02x %02x %02x, %02x %02x %02x %02x); saved as read",
                 stamps[0], stamps[1], stamps[2], stamps[3], stamps[4], stamps[5], stamps[6], stamps[7]);
        return true;
    }
    rtc_power_loss_text(&loss, when, sizeof when);
    if (proof) {
        backup_set(true, &loss);
        ESP_LOGI(TAG, "power loss recorded, %s: the clock counted through it, which proves the backup", when);
    } else if (loss.up - loss.down < RTC_BACKUP_PROOF_S)
        ESP_LOGI(TAG, "power loss recorded, %s: too short to prove the backup, which takes more than a minute", when);
    else
        ESP_LOGW(TAG, "power loss recorded, %s: the clock did not come through it running on a plausible date, so it proves nothing", when);
    return true;
}
// Power-fail stays reported for the rest of this start. The part's own flag is cleared as
// soon as its record is saved, so that the next loss is stamped.
static uint8_t mcp79412_flags(const uint8_t regs[7])
{ return 1 | ((regs[3] & 0x20) ? 2 : 0) | ((regs[3] & 8) ? 4 : 0) | (power_failed ? 8 : 0); }
static void mcp79412_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now)
{
    next_poll = now + 1000;
    if (!attach(bus, 0x6f, now)) return;
    rtc_io_t io = {.ctx = dev, .read = read_registers, .write = mcp79412_write_registers,
        .delay = delay_ms, .now_ms = now_ms, .save_power_failure = save_power_failure,
        .write_trim = mcp79412_write_trim};
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
    const bool running = rtc_running(regs, &epoch);
    if (!(regs[3] & 0x10)) stopped_on_backup = false;
    else { power_failed = true; if (!running) stopped_on_backup = true; }
    telemetry.flags = mcp79412_flags(regs);
    // A proof stands only while the part keeps what earned it. VBATEN reads clear only after
    // a power-on reset, when main power and the backup were lost together; a clock stopped
    // with a power failure on record went over to its backup and did not keep counting.
    // Either must be read twice running, with a weekday the part can hold: a bus held low
    // reads as zeros, and one bad read is not grounds for erasing anything.
    if (!backup.proven || !(regs[3] & 7) || ((regs[3] & 8) && !stopped_on_backup)) lost_polls = 0;
    else if (++lost_polls < 2 ||
             !backup_withdraw(regs[3] & 8 ? "the clock was found stopped with a power failure on record"
                                          : "the RTC was found reset, as after losing main power and its backup together")) {
        candidate = (rtc_candidate_t){0};
        if (lost_polls >= 2) next_poll = now + 30000;
        return;
    }
    if (!retry_initialization && running) {
        telemetry.flags |= 16; telemetry.epoch = epoch;
        if (!rtc_enable_backup(&io)) { ESP_LOGW(TAG, "battery backup enable failed"); return; }
        const bool taken = !(regs[3] & 0x10) || rtc_take_power_failure(&io);
        if (!taken) ESP_LOGW(TAG, "power-fail record not taken; it stays on the RTC and is tried again");
        if (report_state != RUNNING) {
            char proof[160];
            backup_text(proof, sizeof proof);
            ESP_LOGI(TAG, "running calendar retained (UTC epoch=%lld); %s", (long long)epoch, proof);
        }
        // Re-read after the writes above, either of which clears PWRFAIL.
        telemetry = (report_rtc_t){.sampled_ms = now};
        if (read_registers(dev, 0, regs, sizeof regs)) {
            telemetry.flags = mcp79412_flags(regs);
            if (rtc_running(regs, &epoch)) { telemetry.flags |= 16; telemetry.epoch = epoch; }
        }
        report_state = RUNNING; candidate = (rtc_candidate_t){0}; next_poll = now + (taken ? 10000 : 30000);
        return;
    }
    if (report_state != WAITING) ESP_LOGW(TAG, "RTC stopped or calendar unconfirmed; waiting for valid GNSS lock and UTC");
    report_state = WAITING;
    // A subsequent oscillator failure is also recoverable, but never from a
    // guessed build date or an unqualified receiver calendar.
    if (!rtc_gnss_candidate(&candidate, gnss, now, &epoch)) return;
    const rtc_time_ref_t reference = rtc_candidate_ref(&candidate);
    int64_t written = 0;
    if (rtc_set_verified(&io, &reference, &written)) {
        retry_initialization = false; report_state = RUNNING;
        // The verified write advanced through a second boundary; obtain the actual calendar next poll.
        telemetry = (report_rtc_t){.sampled_ms = now};
        char proof[160];
        backup_text(proof, sizeof proof);
        ESP_LOGI(TAG, "initialized from GNSS UTC (epoch=%lld); oscillator advancing; %s", (long long)written, proof);
    } else {
        retry_initialization = true;
        ESP_LOGE(TAG, "RTC initialization/readback failed; time unconfirmed");
        next_poll = now + 30000;
    }
    candidate = (rtc_candidate_t){0};
}

// The MCP79412's crystal, measured against the GNSS pulse and trimmed to it. A window takes
// about ten minutes; each one is logged, and OSCTRIM is written only when the step in force
// has stopped being the right one. A board without a cell loses the register with its power
// and has it back ten minutes into the next fix.
static rtc_discipline_t discipline;
void observer_rtc_discipline(const report_timing_t *t, bool position_valid)
{
    if (part != OBSERVER_RTC_MCP79412 || !dev) return;
    const timing_channel_report_t *g = &t->channel[0], *r = &t->channel[1];
    const uint32_t live = TIMING_FRESH | TIMING_PERIOD_VALID;
    const rtc_discipline_sample_t sample = {
        .valid = position_valid && (t->flags & 1) && (g->flags & live) == live && (r->flags & live) == live &&
                 square_state == 1 && !(square_control & 0x0c),
        .phase_ticks = t->rtc_minus_gnss_ticks, .gnss_period_ticks = g->period_ticks,
        .gnss_captured = g->captured, .rtc_captured = r->captured,
        .gnss_missing = g->missing_estimate, .rtc_missing = r->missing_estimate,
        .gnss_discontinuities = g->discontinuities, .rtc_discontinuities = r->discontinuities,
    };
    rtc_discipline_measure_t measure;
    if (!rtc_discipline_feed(&discipline, &sample, &measure)) return;
    if (!rtc_discipline_usable(&measure)) {
        ESP_LOGW(TAG, "rate not used: crystal %+.3f ppm with a spread of %.3f ppm over %u s; trim left as it is",
                 measure.crystal_ppb / 1000.0, measure.spread_ppb / 1000.0, RTC_DISCIPLINE_SAMPLES);
        return;
    }
    const rtc_io_t io = {.ctx = dev, .read = read_registers, .write_trim = mcp79412_write_trim};
    uint8_t control, osctrim;
    if (!rtc_trim_read(&io, &control, &osctrim)) { ESP_LOGW(TAG, "rate measured but the trim register could not be read"); return; }
    const int current = rtc_trim_steps(osctrim), wanted = rtc_discipline_trim(measure.crystal_ppb, current);
    if (wanted == current) {
        ESP_LOGI(TAG, "rate: crystal %+.3f ppm (spread %.3f); trim %+d steps (0x%02x) leaves %+.3f ppm",
                 measure.crystal_ppb / 1000.0, measure.spread_ppb / 1000.0, current, osctrim,
                 (measure.crystal_ppb + rtc_trim_ppb(current)) / 1000.0);
        return;
    }
    if (!rtc_trim_write(&io, rtc_trim_register(wanted), NULL)) {
        ESP_LOGW(TAG, "rate: crystal %+.3f ppm wants trim %+d steps, but OSCTRIM was not written (control=0x%02x)",
                 measure.crystal_ppb / 1000.0, wanted, control);
        return;
    }
    square_trim = rtc_trim_register(wanted);
    ESP_LOGI(TAG, "rate: crystal %+.3f ppm (spread %.3f); trim changed from %+d to %+d steps (0x%02x to 0x%02x), leaving %+.3f ppm",
             measure.crystal_ppb / 1000.0, measure.spread_ppb / 1000.0, current, wanted, osctrim, rtc_trim_register(wanted),
             (measure.crystal_ppb + rtc_trim_ppb(wanted)) / 1000.0);
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
// The DS3231M and the MAX31328 share this register map and driver; logs name the fitted part.
static const char *tcxo_name(void) { return part == OBSERVER_RTC_DS3231M ? "DS3231M" : "MAX31328"; }
static report_rtc_temperature_t temperature;
report_rtc_temperature_t observer_rtc_temperature(void)
{
    if (part != OBSERVER_RTC_MAX31328 && part != OBSERVER_RTC_DS3231M) return (report_rtc_temperature_t){0};
    const report_rtc_temperature_t previous = temperature;
    temperature = (report_rtc_temperature_t){.present = true};
    uint8_t regs[4];
    if (!dev) {
        ESP_LOGW(TAG, "%s temperature not read: the RTC has not answered", tcxo_name());
        return temperature;
    }
    if (!read_registers(dev, MAX31328_STATUS, regs, sizeof regs)) {
        ESP_LOGW(TAG, "%s temperature read failed", tcxo_name());
        return temperature;
    }
    temperature.state = 1;
    int16_t centi_c;
    switch (max31328_temperature(regs, previous.valid ? &previous.centi_c : NULL, &centi_c)) {
    case MAX31328_TEMPERATURE_OK:
        temperature.valid = 1; temperature.centi_c = centi_c;
        ESP_LOGI(TAG, "%s temperature=%.2f C", tcxo_name(), centi_c / 100.0);
        break;
    case MAX31328_TEMPERATURE_DEFAULT:
        ESP_LOGI(TAG, "%s temperature withheld: 0.00 C with the oscillator-stop flag set may be its power-on default",
                 tcxo_name());
        break;
    default:
        ESP_LOGW(TAG, "%s temperature withheld: registers 0x%02x 0x%02x have unused bits set", tcxo_name(),
                 regs[2], regs[3]);
        break;
    }
    return temperature;
}
static void max31328_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now)
{
    if (now < MAX31328_RECOVERY_MS) return;
    next_poll = now + 1000;
    if (!attach(bus, MAX31328_ADDRESS, now)) return;
    rtc_io_t io = {.ctx = dev, .read = read_registers, .write = max31328_write_registers,
        .delay = delay_ms, .now_ms = now_ms};
    unsigned previous_square = square_state;
    square_state = max31328_square_wave(&io, &square_control, &square_trim);
    if (square_state != previous_square)
        ESP_LOGI(TAG, "INT/SQW GPIO15 square wave state=%u control=0x%02x aging=0x%02x (1=1Hz,2=stopped,3=alarm,4=I/O)",
                 square_state, square_control, square_trim);
    uint8_t regs[7], state[2]; int64_t epoch;
    telemetry = (report_rtc_t){.sampled_ms = now};
    if (!read_registers(dev, 0, regs, sizeof regs) || !read_registers(dev, MAX31328_CONTROL, state, 2)) {
        ESP_LOGW(TAG, "%s read failed; clock left unchanged", tcxo_name());
        candidate = (rtc_candidate_t){0}; next_poll = now + 30000; return;
    }
    // Backup switchover is automatic, so backup is always enabled; a stop, including a
    // failed switchover, shows as OSF and leaves the oscillator-running flag clear.
    bool oscillator = !(state[0] & MAX31328_EOSC) && !(state[1] & MAX31328_OSF);
    telemetry.flags = 1 | (oscillator ? 2 : 0) | 4;
    if (!retry_initialization && max31328_running(regs, state[0], state[1], &epoch)) {
        telemetry.flags |= 16; telemetry.epoch = epoch;
        if (report_state != RUNNING)
            ESP_LOGI(TAG, "%s running calendar retained (UTC epoch=%lld); no oscillator stop recorded",
                     tcxo_name(), (long long)epoch);
        report_state = RUNNING; candidate = (rtc_candidate_t){0}; next_poll = now + 10000;
        return;
    }
    if (report_state == UNUSABLE) { next_poll = now + 600000; return; }
    if (report_state != WAITING)
        ESP_LOGW(TAG, "%s calendar invalid (oscillator-stop flag %s); waiting for valid GNSS lock and UTC",
                 tcxo_name(), state[1] & MAX31328_OSF ? "set: power was lost or the switch to the backup cell failed" : "clear");
    report_state = WAITING;
    if (!rtc_gnss_candidate(&candidate, gnss, now, &epoch)) return;
    const rtc_time_ref_t reference = rtc_candidate_ref(&candidate);
    int64_t written = 0;
    switch (max31328_set_verified(&io, &reference, &written)) {
    case MAX31328_SET_OK:
        retry_initialization=false;
        report_state = RUNNING;
        telemetry = (report_rtc_t){.sampled_ms = now};
        ESP_LOGI(TAG, "%s set from GNSS UTC (epoch=%lld); oscillator-stop flag cleared and advancing",
                 tcxo_name(), (long long)written);
        break;
    case MAX31328_SET_OSF_STUCK:
        // The flag would not clear, so this part can never show valid time. Stop
        // rewriting it; GNSS remains the only trusted time source.
        report_state = UNUSABLE; next_poll = now + 600000;
        ESP_LOGE(TAG, "%s oscillator-stop flag does not clear; RTC time will not be trusted", tcxo_name());
        break;
    default:
        retry_initialization=true;
        ESP_LOGE(TAG, "%s initialization/readback failed; time unconfirmed", tcxo_name());
        next_poll = now + 30000;
        break;
    }
    candidate = (rtc_candidate_t){0};
}

void observer_rtc_poll(i2c_master_bus_handle_t bus, const gnss_status_t *gnss, int64_t now)
{
    if (part == OBSERVER_RTC_MCP79412) backup_load();
    int64_t qualified;
    bool gnss_ready=rtc_gnss_candidate(&service_candidate,gnss,now,&qualified);
    // Repeated snapshots do not add samples, but a fully qualified fresh
    // candidate remains usable for an explicit service action.
    gnss_ready=gnss_ready || (service_candidate.samples>=3 && service_candidate.last_sample_ms-service_candidate.first_sample_ms>=2000 && now>=service_candidate.last_sample_ms && now-service_candidate.last_sample_ms<=2000);
    portENTER_CRITICAL(&service_lock);
    bool requested=service_state==SERVICE_QUEUED;
    rtc_service_request_t req=service_request;
    if(requested)service_state=SERVICE_ACTIVE;
    portEXIT_CRITICAL(&service_lock);
    if(requested) {
        rtc_service_record_t result={.version=1,.request=req,.started_ms=now,.result=RTC_SERVICE_NO_DEVICE};
        bool tcxo=part==OBSERVER_RTC_MAX31328 || part==OBSERVER_RTC_DS3231M;
        if(tcxo && now<MAX31328_RECOVERY_MS) {
            portENTER_CRITICAL(&service_lock);service_state=SERVICE_QUEUED;portEXIT_CRITICAL(&service_lock);return;
        }
        if(bus && part!=OBSERVER_RTC_NONE && attach(bus,tcxo?MAX31328_ADDRESS:0x6f,now)) {
            rtc_io_t io={.ctx=dev,.read=read_registers,.write=tcxo?max31328_write_registers:mcp79412_write_registers,.delay=delay_ms,.now_ms=now_ms,.save_power_failure=save_power_failure};
            if(req.source==OBS_TIME_GNSS) {
                req.reference=gnss_ready?rtc_candidate_ref(&service_candidate):(rtc_time_ref_t){0};
                const uint8_t *p=gnss->pvt_utc+8;
                req.uncertainty_ns=p[0]|((uint32_t)p[1]<<8)|((uint32_t)p[2]<<16)|((uint32_t)p[3]<<24);
            }
            (void)rtc_service_execute(&io,tcxo,&req,service_save,NULL,&result);
            if(result.result==RTC_SERVICE_OK){report_state=RUNNING;retry_initialization=false;}
            else if(result.result==RTC_SERVICE_WRITE_FAILED || result.result==RTC_SERVICE_OSF_STUCK || (result.result==RTC_SERVICE_AUDIT_FAILED && result.written_epoch)) {
                report_state=result.result==RTC_SERVICE_OSF_STUCK?UNUSABLE:WAITING;retry_initialization=true;
            }
            candidate=(rtc_candidate_t){0};next_poll=0;
            telemetry=(report_rtc_t){0}; // never reuse pre-service calendar evidence
        } else { result.completed_ms=now;result.audit_complete=true;if(!service_save(NULL,&result)){result.audit_complete=false;result.result=RTC_SERVICE_AUDIT_FAILED;} }
        portENTER_CRITICAL(&service_lock);service_record=result;service_state=SERVICE_DONE;portEXIT_CRITICAL(&service_lock);
        return;
    }
    if (!bus || now < next_poll) return;
    if (part == OBSERVER_RTC_MCP79412) mcp79412_poll(bus, gnss, now);
    else if (part == OBSERVER_RTC_MAX31328 || part == OBSERVER_RTC_DS3231M) max31328_poll(bus, gnss, now);
}
