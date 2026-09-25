#include "motion.h"
#include <string.h>
#include "driver/gpio.h"
#include "esp_check.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"

static const char *TAG = "motion";
#define IMU_I2C_HZ       400000
#define I2C_TIMEOUT_MS   50
// Without INT1 the FIFO is still drained 4 times a second: 25 of its 128 packets.
#define SERVICE_MS       250
#define RETRY_MS         10000
#define STALL_MS         2000 // no packet in 20 sample periods: the IMU lost its configuration
#define FAILURE_LIMIT    3

static i2c_master_dev_handle_t imu;
static TaskHandle_t task;
static SemaphoreHandle_t lock;     // guards status and serializes the IMU transfers
static motion_status_t status;

static bool imu_read(void *ctx, uint8_t reg, uint8_t *data, size_t length)
{
    (void)ctx;
    return i2c_master_transmit_receive(imu, &reg, 1, data, length, I2C_TIMEOUT_MS) == ESP_OK;
}
static bool imu_write(void *ctx, uint8_t reg, const uint8_t *data, size_t length)
{
    (void)ctx;
    uint8_t buffer[9];
    if (length > sizeof buffer - 1) return false;
    buffer[0] = reg;
    memcpy(buffer + 1, data, length);
    return i2c_master_transmit(imu, buffer, length + 1, I2C_TIMEOUT_MS) == ESP_OK;
}
static void imu_delay(void *ctx, unsigned ms) { (void)ctx; vTaskDelay(pdMS_TO_TICKS(ms) + 1); }
static const icm45686_io_t io = {.read = imu_read, .write = imu_write, .delay_ms = imu_delay};

static void IRAM_ATTR int1_isr(void *arg)
{
    (void)arg;
    BaseType_t woken = pdFALSE;
    vTaskNotifyGiveFromISR(task, &woken);
    portYIELD_FROM_ISR(woken);
}

static void motion_task(void *arg)
{
    (void)arg;
    unsigned failures = 0;
    bool announced = false;
    uint32_t last_packets = 0;
    int64_t last_progress = 0, next_attempt = 0;
    for (;;) {
        int64_t now = esp_timer_get_time() / 1000;
        xSemaphoreTake(lock, portMAX_DELAY);
        if (!status.ready && now >= next_attempt) {
            status.ready = icm45686_configure(&io);
            if (status.ready) {
                ESP_LOGI(TAG, "ICM-45686 configured: %d Hz, +/-%d g, +/-%d dps, FIFO stream mode on INT1",
                         ICM45686_ODR_HZ, ICM45686_ACCEL_FS_G, ICM45686_GYRO_FS_DPS);
                failures = 0; last_packets = status.stats.packets; last_progress = now;
            } else {
                if (!announced) ESP_LOGW(TAG, "ICM-45686 not answering; retrying every %d s", RETRY_MS / 1000);
                next_attempt = now + RETRY_MS;
            }
            announced = true;
        } else if (status.ready) {
            uint32_t before = status.stats.latest_packet;
            if (!icm45686_service(&io, &status.stats)) {
                if (++failures >= FAILURE_LIMIT) {
                    ESP_LOGW(TAG, "ICM-45686 transfers failing; configuring again");
                    status.ready = false;
                }
            } else {
                failures = 0;
                if (status.stats.latest_packet != before) status.latest_ms = now;
            }
            if (status.stats.packets != last_packets) {
                last_packets = status.stats.packets; last_progress = now;
            } else if (status.ready && now - last_progress >= STALL_MS) {
                ESP_LOGW(TAG, "ICM-45686 FIFO produced nothing for %d ms; configuring again", STALL_MS);
                status.ready = false;
            }
        }
        bool ready = status.ready;
        xSemaphoreGive(lock);
        // INT1 wakes the task at the watermark; the timeout keeps draining without it.
        ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(ready ? SERVICE_MS : 1000));
    }
}

esp_err_t motion_start(i2c_master_bus_handle_t bus, int int1_gpio, int int2_gpio)
{
    if (!bus) return ESP_ERR_INVALID_ARG;
    if (!(lock = xSemaphoreCreateMutex())) return ESP_ERR_NO_MEM;
    const i2c_device_config_t device = {.dev_addr_length = I2C_ADDR_BIT_LEN_7,
        .device_address = ICM45686_ADDRESS, .scl_speed_hz = IMU_I2C_HZ};
    ESP_RETURN_ON_ERROR(i2c_master_bus_add_device(bus, &device, &imu), TAG, "add IMU");
    const gpio_config_t inputs = {.pin_bit_mask = (1ULL << int1_gpio) | (1ULL << int2_gpio),
        .mode = GPIO_MODE_INPUT, .pull_up_en = GPIO_PULLUP_DISABLE, .pull_down_en = GPIO_PULLDOWN_ENABLE,
        .intr_type = GPIO_INTR_DISABLE};
    ESP_RETURN_ON_ERROR(gpio_config(&inputs), TAG, "interrupt inputs");
    if (xTaskCreate(motion_task, "motion", 3072, NULL, 4, &task) != pdPASS) return ESP_ERR_NO_MEM;
    esp_err_t err = gpio_install_isr_service(0);
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) return err;
    ESP_RETURN_ON_ERROR(gpio_set_intr_type(int1_gpio, GPIO_INTR_POSEDGE), TAG, "INT1 edge");
    ESP_RETURN_ON_ERROR(gpio_isr_handler_add(int1_gpio, int1_isr, NULL), TAG, "INT1 handler");
    return ESP_OK;
}

void motion_snapshot(motion_status_t *out)
{
    if (!lock) { *out = (motion_status_t){0}; return; }
    xSemaphoreTake(lock, portMAX_DELAY);
    *out = status;
    icm45686_window_snapshot(&status.stats);
    xSemaphoreGive(lock);
}

void motion_report_sent(void)
{
    if (!lock) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    icm45686_window_sent(&status.stats);
    xSemaphoreGive(lock);
}
