#ifndef NVF_THERMOCOUPLE_H
#define NVF_THERMOCOUPLE_H
// The MAX board's external K-type thermocouple input: a MAX31856 on its own SPI bus
// (the hardware repository's pcb/gnss-color-max/THERMOCOUPLE.md). Call from one task.
#include <stdbool.h>
#include "esp_err.h"
#include "max31856.h"

typedef struct {
    bool configured;          // CR0/CR1 hold the configuration
    bool filter_50hz;         // the notch in use
    max31856_sample_t sample; // cleared unless the registers were read
} env_thermocouple_t;

// Claims the SPI bus and the DRDY_N input and configures the converter. The converter
// not answering is not an error here; thermocouple_sample keeps trying.
esp_err_t thermocouple_start(int sck, int mosi, int miso, int cs, int drdy, bool filter_50hz);
// Reads the latest conversion; a converter found unconfigured is configured again.
void thermocouple_sample(env_thermocouple_t *out);
#endif
