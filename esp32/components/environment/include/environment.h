#ifndef NVF_ENVIRONMENT_H
#define NVF_ENVIRONMENT_H
#include <stdint.h>
#include "env_sensors.h"
#include "driver/i2c_master.h"
// Call from the board task, which owns sensor/RTC transactions.
void environment_sample(i2c_master_bus_handle_t bus, env_sample_t *sample, uint8_t *ready);
#endif
