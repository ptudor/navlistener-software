// Run the actual service task and driver against a simulated IMU and scheduler.
#include "icm45686_mock.h"
#include <setjmp.h>
#include "../src/motion.c"

static chip_t chip;
static int64_t clock_ms;
static bool bad_rate_read, partial_fifo_read;
static void (*task_entry)(void *);
static void (*after_iteration)(unsigned);
static unsigned iteration;
static jmp_buf finished;

int64_t esp_timer_get_time(void) { return clock_ms * 1000; }
void vTaskDelay(TickType_t ticks) { clock_ms += ticks; }
unsigned ulTaskNotifyTake(BaseType_t clear, TickType_t ticks)
{
    assert(clear == pdTRUE);
    after_iteration(iteration++);
    assert(iteration < 20); // every scenario must recover within bounded time
    clock_ms += ticks;
    return 0; // fallback polling must work even without interrupts
}
void vTaskNotifyGiveFromISR(TaskHandle_t t, BaseType_t *woken) { assert(t); *woken = pdFALSE; }
BaseType_t xTaskCreate(void (*entry)(void *), const char *name, unsigned stack,
                       void *arg, unsigned priority, TaskHandle_t *out)
{
    (void)name; (void)stack; (void)arg; (void)priority;
    task_entry = entry; *out = &chip; return pdPASS;
}
SemaphoreHandle_t xSemaphoreCreateMutex(void) { return &chip; }
BaseType_t xSemaphoreTake(SemaphoreHandle_t semaphore, TickType_t ticks)
{ (void)ticks; assert(semaphore); return pdTRUE; }
BaseType_t xSemaphoreGive(SemaphoreHandle_t semaphore) { assert(semaphore); return pdTRUE; }
esp_err_t gpio_config(const gpio_config_t *config) { (void)config; return ESP_OK; }
esp_err_t gpio_install_isr_service(int flags) { (void)flags; return ESP_OK; }
esp_err_t gpio_set_intr_type(int pin, int type) { (void)pin; (void)type; return ESP_OK; }
esp_err_t gpio_isr_handler_add(int pin, void (*handler)(void *), void *arg)
{ (void)pin; (void)handler; (void)arg; return ESP_OK; }
esp_err_t i2c_master_bus_add_device(i2c_master_bus_handle_t bus,
                                  const i2c_device_config_t *config, i2c_master_dev_handle_t *out)
{
    assert(bus && config->device_address == ICM45686_ADDRESS);
    *out = &chip; return ESP_OK;
}
esp_err_t i2c_master_transmit_receive(i2c_master_dev_handle_t dev, const void *tx,
                                    size_t tx_length, void *rx, size_t rx_length, int timeout)
{
    (void)timeout; assert(dev == &chip && tx_length == 1);
    uint8_t reg = *(const uint8_t *)tx;
    if (reg == 0x1b && bad_rate_read && chip.rate_writes > 1) return ESP_FAIL;
    if (reg == 0x14 && partial_fifo_read) {
        // A timed-out transfer can consume bytes before the driver sees the error.
        assert(chip.fifo_bytes > 7);
        memmove(chip.fifo, chip.fifo + 7, chip.fifo_bytes - 7);
        chip.fifo_bytes -= 7;
        return ESP_FAIL;
    }
    return chip_read(&chip, reg, rx, rx_length) ? ESP_OK : ESP_FAIL;
}
esp_err_t i2c_master_transmit(i2c_master_dev_handle_t dev, const void *tx, size_t length, int timeout)
{
    (void)timeout; assert(dev == &chip && length >= 2);
    const uint8_t *data = tx;
    return chip_write(&chip, data[0], data + 1, length - 1) ? ESP_OK : ESP_FAIL;
}

static void push(bool valid)
{
    const int16_t accel[3] = {0, 0, valid ? 8192 : INT16_MIN}, gyro[3] = {0};
    push_packet(&chip, 0x68, accel, gyro, 5);
}
static void rate_failure(unsigned step)
{
    motion_status_t snapshot;
    motion_snapshot(&snapshot);
    if (step == 0) {
        assert(snapshot.ready && chip.resets == 1);
        push(true); push(true); bad_rate_read = true;
    } else if (step == 1) {
        assert(chip.regs[0x1b] == 0x2a); // write took effect before read-back failed
        assert(!snapshot.ready && !snapshot.stats.moving);
        bad_rate_read = false;
    } else {
        assert(step == 2 && snapshot.ready && chip.resets == 2);
        assert(!snapshot.stats.latest_valid && snapshot.stats.packets == 1);
        assert(chip.regs[0x1b] == 0x2c && snapshot.stats.period_ms == 80);
        longjmp(finished, 1);
    }
}
static void fifo_failure(unsigned step)
{
    motion_status_t snapshot;
    motion_snapshot(&snapshot);
    if (step == 0) {
        push(true); push(true); partial_fifo_read = true;
    } else if (step == 1) {
        assert(!snapshot.ready && chip.fifo_bytes == 25 && snapshot.stats.packets == 0);
        partial_fifo_read = false;
    } else if (step == 2) {
        assert(snapshot.ready && chip.resets == 2 && !snapshot.stats.latest_valid && !chip.fifo_bytes);
        push(true); push(true);
    } else {
        assert(step == 3 && snapshot.ready && snapshot.stats.latest_valid && snapshot.stats.packets == 1);
        longjmp(finished, 1);
    }
}
static void invalid_stream(unsigned step)
{
    (void)step;
    motion_status_t snapshot;
    motion_snapshot(&snapshot);
    if (chip.resets >= 2) {
        assert(snapshot.ready && snapshot.stats.packets && !snapshot.stats.latest_valid);
        longjmp(finished, 1);
    }
    // Packet traffic alone must not prevent recovery when every sample is invalid.
    for (unsigned i = 0; i < 16; i++) push(false);
}
static void run(void (*scenario)(unsigned))
{
    (void)chip_io(&chip);
    status = (motion_status_t){0};
    clock_ms = 0; iteration = 0;
    bad_rate_read = partial_fifo_read = false;
    after_iteration = scenario;
    assert(motion_start(&chip, 16, 17, &ICM45686_SURFACE) == ESP_OK);
    if (!setjmp(finished)) task_entry(NULL);
}
int main(void)
{
    run(rate_failure); run(fifo_failure); run(invalid_stream);
    puts("Motion task: unverified rate writes, partial FIFO transfers and invalid-sample stalls recover");
    return 0;
}
