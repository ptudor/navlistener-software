#include "thermocouple.h"
#include <string.h>
#include "driver/gpio.h"
#include "driver/spi_master.h"
#include "esp_check.h"
#include "esp_log.h"

static const char *TAG = "thermocouple";
// SPI mode 1 (CPHA 1, datasheet Table 5); 1 MHz is well inside the 5 MHz limit.
#define TC_SPI_HOST SPI2_HOST
#define TC_SPI_HZ   (1000 * 1000)
#define TC_MAX_TRANSFER 17

static spi_device_handle_t device;
static int drdy_pin = -1;
static bool filter, configured, reported;

static bool transfer(void *ctx, const uint8_t *tx, uint8_t *rx, size_t length)
{
    (void)ctx;
    if (length > TC_MAX_TRANSFER) return false;
    spi_transaction_t t = {.length = length * 8, .tx_buffer = tx, .rx_buffer = rx};
    return spi_device_polling_transmit(device, &t) == ESP_OK;
}
static bool ready(void *ctx) { (void)ctx; return gpio_get_level(drdy_pin) == 0; }
static const max31856_io_t io = {.transfer = transfer, .ready = ready};

static void configure(void)
{
    configured = max31856_configure(&io, filter);
    if (configured) ESP_LOGI(TAG, "MAX31856 configured: K type, %s Hz notch, 4-sample averaging, continuous",
                             filter ? "50" : "60");
    else if (!reported) ESP_LOGW(TAG, "MAX31856 not answering; retrying at each sample");
    reported = true;
}

esp_err_t thermocouple_start(int sck, int mosi, int miso, int cs, int drdy, bool filter_50hz)
{
    // DRDY_N and CS_N have 10 kOhm pull-ups to 3V3_SENS on the board.
    const gpio_config_t input = {.pin_bit_mask = 1ULL << drdy, .mode = GPIO_MODE_INPUT,
        .pull_up_en = GPIO_PULLUP_DISABLE, .pull_down_en = GPIO_PULLDOWN_DISABLE, .intr_type = GPIO_INTR_DISABLE};
    ESP_RETURN_ON_ERROR(gpio_config(&input), TAG, "DRDY_N input");
    const spi_bus_config_t bus = {.sclk_io_num = sck, .mosi_io_num = mosi, .miso_io_num = miso,
        .quadwp_io_num = -1, .quadhd_io_num = -1, .max_transfer_sz = TC_MAX_TRANSFER};
    ESP_RETURN_ON_ERROR(spi_bus_initialize(TC_SPI_HOST, &bus, SPI_DMA_DISABLED), TAG, "SPI bus");
    const spi_device_interface_config_t config = {.mode = 1, .clock_speed_hz = TC_SPI_HZ,
        .spics_io_num = cs, .queue_size = 1};
    esp_err_t err = spi_bus_add_device(TC_SPI_HOST, &config, &device);
    if (err != ESP_OK) {
        spi_bus_free(TC_SPI_HOST);
        ESP_LOGE(TAG, "SPI device: %s", esp_err_to_name(err));
        return err;
    }
    drdy_pin = drdy;
    filter = filter_50hz;
    configure();
    return ESP_OK;
}

void thermocouple_sample(env_thermocouple_t *out)
{
    memset(out, 0, sizeof *out);
    out->filter_50hz = filter;
    if (!device) return;
    if (!configured) configure();
    if (configured && !max31856_read(&io, filter, &out->sample)) {
        // Reset or power-cycled since it was configured: configure it and read next time.
        ESP_LOGW(TAG, "MAX31856 configuration lost; configuring again");
        memset(&out->sample, 0, sizeof out->sample);
        configure();
        out->configured = false;
        return;
    }
    out->configured = configured;
    if (!configured) return;
    const max31856_sample_t *s = &out->sample;
    if (s->valid & MAX31856_TC_VALID)
        ESP_LOGI(TAG, "thermocouple=%.2f C cold junction=%.2f C", s->tc_centi_c / 100.0, s->cj_centi_c / 100.0);
    else
        ESP_LOGW(TAG, "thermocouple unavailable: %s fault=0x%02x%s%s", s->fresh ? "conversion" : "no new conversion",
                 s->fault, s->fault & MAX31856_OPEN ? " (open circuit)" : "",
                 s->fault & MAX31856_OVUV ? " (input over/under voltage)" : "");
}
