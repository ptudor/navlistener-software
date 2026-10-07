#include "io_expander_service.h"
#include <inttypes.h>
#include <stdio.h>
#include <string.h>
#include "driver/gpio.h"
#include "esp_attr.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

static const char *TAG = "io_expander";
#define EXPANDER_I2C_HZ    100000 // as the bus's other devices; the readback does not speed it up
#define I2C_TIMEOUT_MS     100
#define INT_LOW_RETRY_MS   1000   // INT held low raises no further edge: service it again this often
#define MISMATCH_LOG_MS    60000
#define INT_LOW_LOG_MS     60000

static i2c_master_dev_handle_t device;
static SemaphoreHandle_t lock; // guards everything below and serializes the expander's transfers
static TaskHandle_t task;
static int int_pin = -1;
static bool handler, suspended, unconfigured_logged;
static io_expander_t state;
// Readback outcomes for the task to log, so the panel refresh task never waits on the console.
typedef struct {
    bool pending, error;
    io_chain_result_t result;
    uint32_t frame, samples, diff;
} readback_log_t;
static readback_log_t readback_log;
static uint32_t readback_errors;
static uint16_t rail_alert;
static int64_t rail_alert_ms;

static int64_t now_ms(void) { return esp_timer_get_time() / 1000; }

static bool reg_read(void *ctx, uint8_t reg, uint8_t *value)
{
    (void)ctx;
    return i2c_master_transmit_receive(device, &reg, 1, value, 1, I2C_TIMEOUT_MS) == ESP_OK;
}
static bool reg_write(void *ctx, uint8_t reg, uint8_t value)
{
    (void)ctx;
    const uint8_t data[2] = {reg, value};
    return i2c_master_transmit(device, data, sizeof data, I2C_TIMEOUT_MS) == ESP_OK;
}
static bool int_low(void *ctx)
{
    (void)ctx;
    return gpio_get_level(int_pin) == 0;
}
static const io_expander_io_t io = {.read = reg_read, .write = reg_write, .int_low = int_low};

static void IRAM_ATTR int_isr(void *arg)
{
    (void)arg;
    BaseType_t woken = pdFALSE;
    if (task) vTaskNotifyGiveFromISR(task, &woken);
    portYIELD_FROM_ISR(woken);
}

// With the lock held: the programme, then the falling-edge handler (installed once, enabled
// again after a suspension). GPIO3 is the JTAG_SEL strap and is only ever read.
static bool configure(void)
{
    if (!io_expander_configure(&state, &io, now_ms())) {
        if (!unconfigured_logged)
            ESP_LOGW(TAG, "MCP23008 at 0x%02x: register programme not confirmed; retrying at each board pass",
                     MCP23008_ADDRESS);
        unconfigured_logged = true;
        return false;
    }
    esp_err_t err = ESP_OK;
    if (!handler) {
        err = gpio_install_isr_service(0);
        if (err == ESP_ERR_INVALID_STATE) err = ESP_OK; // another driver installed it
        if (err == ESP_OK) err = gpio_set_intr_type(int_pin, GPIO_INTR_NEGEDGE);
        if (err == ESP_OK) err = gpio_isr_handler_add(int_pin, int_isr, NULL); // and enables it
        handler = err == ESP_OK;
    } else {
        err = gpio_intr_enable(int_pin);
    }
    if (err != ESP_OK) {
        state.configured = false;
        if (!unconfigured_logged)
            ESP_LOGE(TAG, "GPIO%d interrupt unavailable (%s); the expander is not used until it installs", int_pin,
                     esp_err_to_name(err));
        unconfigured_logged = true;
        return false;
    }
    unconfigured_logged = false;
    ESP_LOGI(TAG, "MCP23008 at 0x%02x configured: GPIO=0x%02x, interrupt-on-change on GP1-GP5 to GPIO%d",
             MCP23008_ADDRESS, state.image, int_pin);
    // A change after the programme's GPIO read and before the handler was enabled holds INT
    // low with no edge to come.
    if (int_low(NULL)) xTaskNotifyGive(task);
    return true;
}

static const char *chain_name(uint8_t length) { return length == 24 ? "24" : length == 16 ? "16" : "unknown"; }

static void expander_task(void *arg)
{
    (void)arg;
    TickType_t wait = portMAX_DELAY;
    bool stack_logged = false;
    // Never logged: far enough back that the first is due at any uptime.
    int64_t mismatch_logged_ms = INT64_MIN / 2, int_low_logged_ms = INT64_MIN / 2;
    for (;;) {
        ulTaskNotifyTake(pdTRUE, wait);
        const int64_t now = now_ms();
        bool serviced = false, failed = false, low = false;
        xSemaphoreTake(lock, portMAX_DELAY);
        if (state.configured && !suspended) {
            serviced = true;
            failed = !io_expander_service(&state, &io, now, &low);
        }
        const uint8_t due = io_expander_take_log(&state, now);
        io_expander_line_t lines[IO_SOURCE_COUNT];
        memcpy(lines, state.lines, sizeof lines);
        const int64_t log_wait = io_expander_log_wait_ms(&state, now);
        const io_expander_chain_t chain = state.chain;
        const uint32_t stuck = state.stuck;
        const readback_log_t result = readback_log;
        readback_log.pending = readback_log.error = false;
        xSemaphoreGive(lock);

        for (unsigned s = 0; s < IO_SOURCE_COUNT; s++)
            if (due & IO_SOURCE_PIN(s))
                ESP_LOGI(TAG, "%s %s (%" PRIu32 " changes since boot)", io_source_name(s),
                         lines[s].level ? "high, released" : "low, asserted", lines[s].count);
        if (failed) ESP_LOGW(TAG, "MCP23008 transfer failed; configuring it again at the next board pass");
        if (low && now - int_low_logged_ms >= INT_LOW_LOG_MS) {
            int_low_logged_ms = now;
            ESP_LOGW(TAG, "INT still low after %u GPIO reads (%" PRIu32 " times since boot); serviced every %d ms "
                     "while it stays low", IO_EXPANDER_REREADS, stuck, INT_LOW_RETRY_MS);
        }
        if (result.error) ESP_LOGW(TAG, "panel chain readback: GPIO read failed; frame written unverified");
        if (result.pending) switch (result.result) {
        case IO_CHAIN_DETECTED:
            ESP_LOGI(TAG, "panel chain: %s bits", chain_name(chain.length));
            break;
        case IO_CHAIN_FAULT:
            // Logged until the third, which is reported once at ERROR; later ones are counted.
            if (chain.faults < 3)
                ESP_LOGW(TAG, "panel chain readback matched neither 24 nor 16 bits (read 0x%06" PRIx32
                         " for frame 0x%06" PRIx32 "); retrying at the next run", result.samples, result.frame);
            break;
        case IO_CHAIN_UNDETECTED:
            ESP_LOGE(TAG, "panel chain not detected or wrong length: 3 readbacks matched neither 24 nor 16 bits "
                     "(last read 0x%06" PRIx32 " for frame 0x%06" PRIx32 ")", result.samples, result.frame);
            break;
        case IO_CHAIN_MISMATCH:
            if (now - mismatch_logged_ms >= MISMATCH_LOG_MS) {
                mismatch_logged_ms = now;
                ESP_LOGW(TAG, "panel chain readback mismatch at clocks 0x%06" PRIx32 " (bit k = before clock k): "
                         "read 0x%06" PRIx32 ", expected 0x%06" PRIx32 "; %" PRIu32 " of %" PRIu32 " runs",
                         result.diff, result.samples, result.samples ^ result.diff, chain.mismatches, chain.runs);
            }
            break;
        case IO_CHAIN_MATCH:
        case IO_CHAIN_INCONCLUSIVE:
            break;
        }
        if (serviced && !stack_logged) {
            // The interrupt service and its logging are this task's deepest path.
            stack_logged = true;
            ESP_LOGI(TAG, "stack minimum free=%u bytes after the first service",
                     (unsigned)uxTaskGetStackHighWaterMark(NULL));
        }
        // INT held low raises no edge, so the task services it again after a while; a change
        // waiting for its log line wakes the task when that line may be logged.
        int64_t wait_ms = low ? INT_LOW_RETRY_MS : -1;
        if (log_wait >= 0 && (wait_ms < 0 || log_wait < wait_ms)) wait_ms = log_wait;
        wait = wait_ms < 0 ? portMAX_DELAY : pdMS_TO_TICKS(wait_ms) + 1;
    }
}

esp_err_t io_expander_start(i2c_master_bus_handle_t bus, int int_gpio)
{
    if (!bus || int_gpio < 0) return ESP_ERR_INVALID_ARG;
    if (lock) return ESP_ERR_INVALID_STATE;
    if (!(lock = xSemaphoreCreateMutex())) return ESP_ERR_NO_MEM;
    int_pin = int_gpio;
    const i2c_device_config_t config = {.dev_addr_length = I2C_ADDR_BIT_LEN_7,
        .device_address = MCP23008_ADDRESS, .scl_speed_hz = EXPANDER_I2C_HZ};
    esp_err_t err = i2c_master_bus_add_device(bus, &config, &device);
    // 3 KiB: single-register transfers and integer-format logging, no floats. The task logs
    // its minimum free stack after its first service to confirm the margin.
    if (err == ESP_OK && xTaskCreate(expander_task, "io_expander", 3072, NULL, 4, &task) != pdPASS)
        err = ESP_ERR_NO_MEM;
    if (err != ESP_OK) {
        // Nothing is left half-started: every other call stays a no-op.
        if (device) (void)i2c_master_bus_rm_device(device);
        device = NULL;
        vSemaphoreDelete(lock);
        lock = NULL;
        return err;
    }
    xSemaphoreTake(lock, portMAX_DELAY);
    (void)configure();
    xSemaphoreGive(lock);
    return ESP_OK;
}

bool io_expander_poll(void)
{
    if (!lock) return false;
    xSemaphoreTake(lock, portMAX_DELAY);
    if (!state.configured && !suspended) (void)configure();
    const bool fell = io_expander_take_fell(&state) & IO_SOURCE_PIN(IO_SOURCE_PWR);
    xSemaphoreGive(lock);
    return fell;
}

bool io_expander_ready(void)
{
    if (!lock) return false;
    xSemaphoreTake(lock, portMAX_DELAY);
    const bool ready = state.configured && !suspended;
    xSemaphoreGive(lock);
    return ready;
}

void io_expander_suspend(void)
{
    if (!lock) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    suspended = true;
    if (handler) (void)gpio_intr_disable(int_pin);
    const bool written = io_expander_disable(&state, &io);
    xSemaphoreGive(lock);
    ESP_LOGI(TAG, "suspended for a 3V3_SENS change%s", written ? "" : "; GPINTEN = 0 not confirmed");
}

bool io_expander_resume(void)
{
    if (!lock) return false;
    xSemaphoreTake(lock, portMAX_DELAY);
    suspended = false;
    const bool ready = configure();
    xSemaphoreGive(lock);
    return ready;
}

bool io_expander_readback(uint32_t frame, void (*shift)(int bit, void *ctx), void *ctx)
{
    bool sampling = false, failed = false;
    uint32_t samples = 0;
    if (lock) {
        xSemaphoreTake(lock, portMAX_DELAY);
        sampling = state.configured && !suspended;
    }
    const int64_t now = now_ms();
    for (unsigned k = 0; k < IO_EXPANDER_FRAME_BITS; k++) {
        uint8_t gpio;
        if (sampling && !failed) {
            if (reg_read(NULL, MCP23008_GPIO, &gpio)) {
                samples |= (uint32_t)(gpio & IO_EXPANDER_PANEL_SDO) << k;
                io_expander_gpio(&state, gpio, now); // the read cleared any pending interrupt
            } else {
                failed = true;
            }
        }
        shift((int)((frame >> (IO_EXPANDER_FRAME_BITS - 1 - k)) & 1), ctx);
    }
    bool compared = false, notify = false;
    if (sampling) {
        if (failed) {
            state.configured = false;
            readback_errors++;
            readback_log.error = true;
        } else {
            readback_log.result = io_expander_chain_check(&state.chain, frame, samples, &readback_log.diff);
            readback_log.frame = frame;
            readback_log.samples = samples;
            readback_log.pending = compared = true;
        }
        notify = true;
    }
    if (lock) xSemaphoreGive(lock);
    if (notify) xTaskNotifyGive(task);
    return compared;
}

void io_expander_rail_alert(uint16_t flags, int64_t now)
{
    if (!lock) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    rail_alert = flags;
    rail_alert_ms = now;
    xSemaphoreGive(lock);
}

void io_expander_status(io_expander_status_t *out)
{
    *out = (io_expander_status_t){0};
    if (!lock) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    *out = (io_expander_status_t){.configured = state.configured, .suspended = suspended,
        .chain_length = state.chain.length, .gpio = state.image, .stuck = state.stuck,
        .readback_runs = state.chain.runs, .readback_mismatches = state.chain.mismatches,
        .readback_faults = state.chain.faults, .readback_errors = readback_errors,
        .rail_alert = rail_alert, .rail_alert_ms = rail_alert_ms};
    memcpy(out->lines, state.lines, sizeof out->lines);
    xSemaphoreGive(lock);
}

void io_expander_log_status(const char *when)
{
    if (!lock) return;
    io_expander_status_t s;
    io_expander_status(&s);
    // Each line's changes, with the uptime and level of the latest: "PWR=2@81234ms:high".
    char lines[IO_SOURCE_COUNT * 48] = ""; // " TEMP=4294967295@<19 digits>ms:high" fits in 48
    size_t used = 0;
    for (unsigned i = 0; i < IO_SOURCE_COUNT && used < sizeof lines; i++) {
        const io_expander_line_t *l = &s.lines[i];
        int n = l->count ? snprintf(lines + used, sizeof lines - used, "%s%s=%" PRIu32 "@%" PRId64 "ms:%s",
                                    i ? " " : "", io_source_name(i), l->count, l->last_ms, l->level ? "high" : "low")
                         : snprintf(lines + used, sizeof lines - used, "%s%s=0", i ? " " : "", io_source_name(i));
        if (n < 0) break;
        used += (size_t)n;
    }
    ESP_LOGI(TAG, "status %s: %s, GPIO=0x%02x, panel chain %s bits (readback runs=%" PRIu32 " mismatches=%" PRIu32
             " faults=%" PRIu32 " errors=%" PRIu32 "), changes %s, INT held low %" PRIu32 " times, "
             "INA3221 alert flags 0x%04x at %" PRId64 " ms", when,
             s.suspended ? "suspended" : s.configured ? "configured" : "NOT configured", s.gpio,
             chain_name(s.chain_length), s.readback_runs, s.readback_mismatches, s.readback_faults, s.readback_errors,
             lines, s.stuck, s.rail_alert, s.rail_alert_ms);
}
