#include "oled_service.h"
#include <stdatomic.h>
#include <string.h>
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "nvs.h"
#include "ssd1306.h"

static const char *TAG = "oled";
#define OLED_I2C_HZ    100000 // as the bus's other devices
#define I2C_TIMEOUT_MS 100
// How often an absent module is looked for: one address on the bus, answered or not.
#define PROBE_MS       2000
// How often the settings are written again and the whole memory with them, which puts right
// anything a disturbance on the bus changed in a controller that cannot be read back.
#define REFRESH_MS     (5 * 60 * 1000)
#define NVS_NAMESPACE  "nvf_oled"
#define NVS_ROTATED    "rotated"

static oled_service_config_t config;
static i2c_master_dev_handle_t device;
static TaskHandle_t task;
// Held for each of the display's transfers: its turn on the bus.
static SemaphoreHandle_t bus_turn;
// How many callers of oled_service_hold want the display off the bus. It starts no transfer
// while there is one; they do not wait for each other.
static atomic_uint holders;
// Guards the model, the frame, the director and the status, which the console reads.
static SemaphoreHandle_t state_lock;
static oled_model_t model;
static oled_canvas_t frame;
static bool frame_drawn;
static oled_director_t director;
static oled_service_status_t status;
static ssd1306_shadow_t shadow; // the display task's alone
static atomic_bool next_requested, rotation_requested;
static atomic_bool rotation_wanted;
static atomic_bool power_wanted = true;
// When the frame after the one just drawn is due, in uptime milliseconds: just after the
// model's clock turns its next second, so the time on the display turns with it.
static int64_t next_frame_ms;
// Set once the task and everything it uses exist; never cleared. Another task may be asking
// for the bus while oled_service_start is still at work.
static atomic_bool running;

static int64_t now_ms(void) { return esp_timer_get_time() / 1000; }

// The display's turn: taken only while nobody holds the display off the bus.
static void turn_take(void)
{
    for (;;) {
        xSemaphoreTake(bus_turn, portMAX_DELAY);
        if (!atomic_load(&holders)) return;
        xSemaphoreGive(bus_turn);
        vTaskDelay(1);
    }
}
// One transfer, then a tick for whoever waits for the bus.
static bool write_bytes(void *ctx, const uint8_t *bytes, size_t length)
{
    (void)ctx;
    turn_take();
    const esp_err_t err = i2c_master_transmit(device, bytes, length, I2C_TIMEOUT_MS);
    xSemaphoreGive(bus_turn);
    vTaskDelay(1);
    return err == ESP_OK;
}
// The status register: any read from the controller returns it (SSD1306 table 9-2).
static bool read_status(uint8_t *value)
{
    turn_take();
    const esp_err_t err = i2c_master_receive(device, value, 1, I2C_TIMEOUT_MS);
    xSemaphoreGive(bus_turn);
    vTaskDelay(1);
    return err == ESP_OK;
}
static bool answers(void)
{
    turn_take();
    const esp_err_t err = i2c_master_probe(config.bus, config.address, I2C_TIMEOUT_MS);
    xSemaphoreGive(bus_turn);
    return err == ESP_OK;
}

// Fills the model and draws the screen that is due into the frame. Returns the contrast the
// model asks for.
static uint8_t draw(int64_t now)
{
    xSemaphoreTake(state_lock, portMAX_DELAY);
    memset(&model, 0, sizeof model);
    config.model(&model);
    if (atomic_exchange(&next_requested, false)) oled_director_next(&director, &model, now);
    next_frame_ms = now_ms() + (model.time_valid ? 1000 - model.utc_millis % 1000 : 1000) + 2;
    oled_page_t page;
    const oled_screen_t screen = oled_director_step(&director, &model, now, &page);
    oled_clear(&frame);
    oled_shift(now, &frame.dx, &frame.dy);
    if (screen == OLED_SCREEN_SPLASH) oled_draw_splash(&frame, &model);
    else if (screen == OLED_SCREEN_RESET_ARMED) oled_draw_reset_armed(&frame);
    else oled_draw_page(&frame, &model, page);
    frame_drawn = true;
    status.screen = screen;
    status.page = page;
    const uint8_t contrast = oled_contrast(model.brightness ? model.brightness : 100);
    xSemaphoreGive(state_lock);
    return contrast;
}

static void count(const ssd1306_traffic_t *traffic, bool failed)
{
    xSemaphoreTake(state_lock, portMAX_DELAY);
    status.writes += traffic->writes;
    status.bytes += traffic->bytes;
    status.failures += failed;
    xSemaphoreGive(state_lock);
}

// Configures a module that answers and shows the first frame on it. The display is turned
// on only once its memory holds the frame, so the noise the memory starts with is never seen.
// *applied is the contrast it was given.
static bool attach(int64_t now, bool rotated, uint8_t *applied)
{
    if (!answers()) return false;
    uint8_t bytes[SSD1306_SETUP_MAX], off = 0, on = 0;
    ssd1306_traffic_t traffic = {0};
    const uint8_t contrast = draw(now);
    bool ok = write_bytes(NULL, bytes, ssd1306_setup(bytes, true, rotated, contrast));
    const bool off_read = ok && read_status(&off);
    shadow.valid = false;
    ok = ok && ssd1306_flush(&shadow, &frame, write_bytes, NULL, &traffic);
    ok = ok && write_bytes(NULL, bytes, ssd1306_power(bytes, true));
    const bool on_read = ok && read_status(&on);
    count(&traffic, !ok);
    if (!ok) {
        ESP_LOGW(TAG, "SSD1306 at 0x%02x answered, but its configuration was not accepted; trying again in %d s",
                 config.address, PROBE_MS / 1000);
        return false;
    }
    // A controller whose status follows the display going off and on can be watched for a
    // reset. One that does not is still written to; it is refreshed whole every few minutes.
    const bool tracks = off_read && on_read && (off & SSD1306_STATUS_OFF) && !(on & SSD1306_STATUS_OFF);
    *applied = contrast;
    xSemaphoreTake(state_lock, portMAX_DELAY);
    status.present = true;
    status.powered = true;
    status.rotated = rotated;
    status.status_tracks = tracks;
    status.status = on;
    status.contrast = contrast;
    status.attached++;
    status.frames++;
    xSemaphoreGive(state_lock);
    if (tracks)
        ESP_LOGI(TAG, "SSD1306 at 0x%02x configured: display on, %s, contrast %u; status 0x%02x off and 0x%02x on, "
                 "watched for a reset", config.address, rotated ? "rotated" : "not rotated", contrast, off, on);
    else
        ESP_LOGW(TAG, "SSD1306 at 0x%02x configured: display on, %s, contrast %u; its status does not follow the "
                 "display (%s 0x%02x off, %s 0x%02x on), so a reset is not seen until the %d-minute refresh",
                 config.address, rotated ? "rotated" : "not rotated", contrast, off_read ? "read" : "unread", off,
                 on_read ? "read" : "unread", on, REFRESH_MS / 60000);
    return true;
}

static void set_present(bool present, bool lost)
{
    xSemaphoreTake(state_lock, portMAX_DELAY);
    status.present = present;
    status.lost += lost;
    xSemaphoreGive(state_lock);
}

// The wait to the next frame. A frame that took past its successor's time is followed at
// once, and a notification (a page step, a rotation, the power) ends the wait early.
static void wait_for_next_frame(void)
{
    const int64_t wait = next_frame_ms - now_ms();
    (void)ulTaskNotifyTake(pdTRUE, wait > 1000 ? pdMS_TO_TICKS(1000) : wait > 0 ? pdMS_TO_TICKS(wait) : 1);
}

static void display_task(void *arg)
{
    (void)arg;
    bool present = false, absence_logged = false, stack_logged = false, left_first_page = false, powered = true;
    bool rotated = atomic_load(&rotation_wanted);
    uint8_t contrast = 0;
    int64_t next_probe = 0, next_refresh = 0;
    for (;;) {
        int64_t now = now_ms();
        if (!present) {
            if (now >= next_probe) {
                rotated = atomic_load(&rotation_wanted);
                atomic_store(&rotation_requested, false);
                present = attach(now, rotated, &contrast);
                next_probe = now + PROBE_MS;
                next_refresh = now + REFRESH_MS;
                if (!present && !absence_logged) {
                    absence_logged = true;
                    ESP_LOGI(TAG, "no display answers at 0x%02x on the display header; looking for one every %d s",
                             config.address, PROBE_MS / 1000);
                }
            }
            if (!present) {
                (void)ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(PROBE_MS));
                continue;
            }
            wait_for_next_frame();
            continue;
        }

        uint8_t bytes[SSD1306_SETUP_MAX];
        ssd1306_traffic_t traffic = {0};
        bool ok = true, reset = false;
        // Asked to go dark: the panel off, and nothing more on the bus. The frames are still
        // drawn, for the console, and the module is configured from the start when asked back.
        if (!atomic_load(&power_wanted)) {
            if (powered) {
                powered = false;
                ok = write_bytes(NULL, bytes, ssd1306_power(bytes, false));
                xSemaphoreTake(state_lock, portMAX_DELAY);
                status.powered = false;
                xSemaphoreGive(state_lock);
                ESP_LOGI(TAG, "display at 0x%02x turned off%s; it stays off the bus until it is turned on", config.address,
                         ok ? "" : " (the command was not acknowledged)");
            }
            (void)draw(now);
            wait_for_next_frame();
            continue;
        }
        if (!powered) {
            powered = true;
            present = false; // attach() configures it and turns it on
            next_probe = now;
            ESP_LOGI(TAG, "display at 0x%02x turned on again", config.address);
            continue;
        }
        // A module that was unplugged and plugged in again, or lost its supply, is on the bus
        // with its display off and noise in its memory: it is configured again from the start.
        xSemaphoreTake(state_lock, portMAX_DELAY);
        const bool tracks = status.status_tracks;
        xSemaphoreGive(state_lock);
        if (tracks) {
            uint8_t value = 0;
            ok = read_status(&value);
            reset = ok && (value & SSD1306_STATUS_OFF);
            if (ok) {
                xSemaphoreTake(state_lock, portMAX_DELAY);
                status.status = value;
                status.resets += reset;
                xSemaphoreGive(state_lock);
            }
        }
        if (reset) {
            ESP_LOGW(TAG, "SSD1306 at 0x%02x found with its display off: it was reset or plugged in again; configuring it",
                     config.address);
            present = attach(now, rotated, &contrast);
            next_refresh = now + REFRESH_MS;
            if (!present) {
                set_present(false, true);
                next_probe = now + PROBE_MS;
            }
            wait_for_next_frame();
            continue;
        }
        if (ok && atomic_exchange(&rotation_requested, false)) {
            rotated = atomic_load(&rotation_wanted);
            next_refresh = now; // the settings and every byte again, the other way up
            xSemaphoreTake(state_lock, portMAX_DELAY);
            status.rotated = rotated;
            xSemaphoreGive(state_lock);
        }
        const uint8_t wanted = draw(now);
        if (ok && now >= next_refresh) {
            ok = write_bytes(NULL, bytes, ssd1306_setup(bytes, false, rotated, wanted));
            contrast = wanted;
            shadow.valid = false;
            next_refresh = now + REFRESH_MS;
        }
        if (ok && wanted != contrast) {
            ok = write_bytes(NULL, bytes, ssd1306_contrast(bytes, wanted));
            contrast = wanted;
        }
        ok = ok && ssd1306_flush(&shadow, &frame, write_bytes, NULL, &traffic);
        count(&traffic, !ok);
        xSemaphoreTake(state_lock, portMAX_DELAY);
        status.frames += ok;
        status.contrast = contrast;
        xSemaphoreGive(state_lock);
        if (!ok) {
            // A transfer failed. A module that still answers its address is written whole at
            // the next frame; one that does not is gone, and looked for.
            shadow.valid = false;
            contrast = 0;
            if (!answers()) {
                present = false;
                set_present(false, true);
                next_probe = now_ms() + PROBE_MS;
                ESP_LOGW(TAG, "the display at 0x%02x stopped answering; looking for it every %d s", config.address,
                         PROBE_MS / 1000);
                continue;
            }
        }
        // Once the pages have come round to the first again, every page has been drawn,
        // formatted and written: this task's deepest paths.
        xSemaphoreTake(state_lock, portMAX_DELAY);
        const bool on_first_page = status.screen == OLED_SCREEN_PAGE && status.page == OLED_PAGE_NOW;
        const bool on_another = status.screen == OLED_SCREEN_PAGE && status.page != OLED_PAGE_NOW;
        xSemaphoreGive(state_lock);
        left_first_page = left_first_page || on_another;
        if (!stack_logged && left_first_page && on_first_page) {
            stack_logged = true;
            ESP_LOGI(TAG, "stack minimum free=%u bytes after every page was drawn",
                     (unsigned)uxTaskGetStackHighWaterMark(NULL));
        }
        wait_for_next_frame();
    }
}

// The saved rotation; false when none is saved or it cannot be read.
static bool rotation_load(void)
{
    nvs_handle_t handle;
    uint8_t stored = 0;
    esp_err_t err = nvs_open(NVS_NAMESPACE, NVS_READONLY, &handle);
    if (err == ESP_ERR_NVS_NOT_FOUND) return false;
    if (err == ESP_OK) {
        err = nvs_get_u8(handle, NVS_ROTATED, &stored);
        nvs_close(handle);
    }
    if (err != ESP_OK && err != ESP_ERR_NVS_NOT_FOUND)
        ESP_LOGW(TAG, "could not read the display's saved rotation: %s; not rotated", esp_err_to_name(err));
    return err == ESP_OK && stored == 1;
}

esp_err_t oled_service_start(const oled_service_config_t *c)
{
    if (!c || !c->bus || !c->model || c->address > 0x7f) return ESP_ERR_INVALID_ARG;
    if (task) return ESP_ERR_INVALID_STATE;
    config = *c;
    bus_turn = xSemaphoreCreateMutex();
    state_lock = xSemaphoreCreateMutex();
    const i2c_device_config_t device_config = {.dev_addr_length = I2C_ADDR_BIT_LEN_7,
        .device_address = c->address, .scl_speed_hz = OLED_I2C_HZ};
    esp_err_t err = bus_turn && state_lock ? i2c_master_bus_add_device(c->bus, &device_config, &device) : ESP_ERR_NO_MEM;
    if (err == ESP_OK) {
        atomic_store(&rotation_wanted, rotation_load());
        status = (oled_service_status_t){.started = true, .address = c->address};
        // Priority 4, with the panel's refresh: drawing takes well under a millisecond, and a
        // task below the bus's other users could hold it while they wait. 4 KiB: text
        // formatting without floats, the frame and the model are static.
        if (xTaskCreate(display_task, "oled", 4096, NULL, 4, &task) != pdPASS) err = ESP_ERR_NO_MEM;
    }
    if (err == ESP_OK) atomic_store(&running, true);
    if (err != ESP_OK) {
        // Nothing is left half-started: every other call stays a no-op.
        if (device) (void)i2c_master_bus_rm_device(device);
        device = NULL;
        task = NULL;
        if (bus_turn) vSemaphoreDelete(bus_turn);
        if (state_lock) vSemaphoreDelete(state_lock);
        bus_turn = state_lock = NULL;
        status = (oled_service_status_t){0};
    }
    return err;
}

bool oled_service_hold(void)
{
    if (!atomic_load(&running)) return false;
    atomic_fetch_add(&holders, 1);
    // A transfer the display had already begun is waited out; it begins no other.
    xSemaphoreTake(bus_turn, portMAX_DELAY);
    xSemaphoreGive(bus_turn);
    return true;
}
void oled_service_release(void) { atomic_fetch_sub(&holders, 1); }

void oled_service_next_page(void)
{
    if (!atomic_load(&running)) return;
    atomic_store(&next_requested, true);
    xTaskNotifyGive(task);
}

esp_err_t oled_service_set_rotated(bool rotated)
{
    if (!atomic_load(&running)) return ESP_ERR_INVALID_STATE;
    nvs_handle_t handle;
    esp_err_t err = nvs_open(NVS_NAMESPACE, NVS_READWRITE, &handle);
    if (err != ESP_OK) return err;
    err = nvs_set_u8(handle, NVS_ROTATED, rotated ? 1 : 0);
    if (err == ESP_OK) err = nvs_commit(handle);
    nvs_close(handle);
    if (err != ESP_OK) return err;
    atomic_store(&rotation_wanted, rotated);
    atomic_store(&rotation_requested, true);
    xTaskNotifyGive(task);
    return ESP_OK;
}

void oled_service_set_power(bool on)
{
    if (!atomic_load(&running)) return;
    atomic_store(&power_wanted, on);
    xTaskNotifyGive(task);
}

void oled_service_status(oled_service_status_t *out)
{
    *out = (oled_service_status_t){0};
    if (!atomic_load(&running)) return;
    xSemaphoreTake(state_lock, portMAX_DELAY);
    *out = status;
    xSemaphoreGive(state_lock);
    // A rotation asked for and not yet applied is still the unit's setting.
    out->rotated = atomic_load(&rotation_wanted);
}

bool oled_service_frame(const oled_page_t *page, oled_canvas_t *out)
{
    if (!atomic_load(&running)) return false;
    xSemaphoreTake(state_lock, portMAX_DELAY);
    const bool drawn = frame_drawn;
    if (drawn && !page) {
        *out = frame;
    } else if (drawn) {
        oled_clear(out);
        out->dx = out->dy = 0;
        oled_draw_page(out, &model, *page);
    }
    xSemaphoreGive(state_lock);
    return drawn;
}
