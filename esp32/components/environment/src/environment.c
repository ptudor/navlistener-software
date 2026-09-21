#include "environment.h"
#include "sdkconfig.h"
#include <string.h>
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
#include <string.h>
#include "env_sensors.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
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
void environment_sample(i2c_master_bus_handle_t bus, env_sample_t *out, uint8_t *ready)
{
    memset(out, 0, sizeof *out); *ready = 0;
    if (!bus) return;
    if (!initialized) {
        for (unsigned i = 0; i < 3; i++) {
            if (i2c_master_probe(bus, addresses[i], 100) != ESP_OK) continue;
            i2c_device_config_t config = {.dev_addr_length = I2C_ADDR_BIT_LEN_7,
                .device_address = addresses[i], .scl_speed_hz = 100000};
            (void)i2c_master_bus_add_device(bus, &config, &devices[i]);
        }
        env_io_t io = {.read = read_register, .write = write_register, .delay_ms = delay_ms};
        env_sensors_init(&sensors, &io, hdc_variant); initialized = true;
        ESP_LOGI(TAG, "configured humidity sensor: %s (%s)", hdc_name, hdc_conversion);
        ESP_LOGI(TAG, "measurement setup: MCP9808=%s %s=%s BMP388/BMP384=%s",
            sensors.mcp_ready ? "ready" : "unavailable", hdc_name, sensors.hdc_ready ? "ready" : "unavailable",
            sensors.bmp_ready ? "ready" : "unavailable");
    }
    env_sample_t sample;
    env_sensors_read(&sensors, &sample);
    *out = sample;
    *ready = (sensors.mcp_ready ? 1 : 0) | (sensors.hdc_ready ? 2 : 0) | (sensors.bmp_ready ? 4 : 0);
    if (sample.mcp_valid) ESP_LOGI(TAG, "MCP9808 temperature=%.2f C", sample.mcp_c);
    else ESP_LOGW(TAG, "MCP9808 measurement unavailable");
    if (sample.hdc_valid) ESP_LOGI(TAG, "%s temperature=%.2f C humidity=%.2f %%RH (%s)", hdc_name, sample.hdc_c, sample.rh_percent, hdc_conversion);
    else ESP_LOGW(TAG, "%s measurement unavailable", hdc_name);
    if (sample.bmp_valid) ESP_LOGI(TAG, "BMP388/BMP384 temperature=%.2f C pressure=%.2f hPa (local absolute)", sample.bmp_c, sample.pressure_pa / 100.0);
    else ESP_LOGW(TAG, "BMP388/BMP384 measurement unavailable");
}
#else
void environment_sample(i2c_master_bus_handle_t bus, env_sample_t *out, uint8_t *ready)
{ (void)bus; memset(out, 0, sizeof *out); *ready = 0; }
#endif
