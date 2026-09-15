#ifndef HARDWARE_MANIFEST_H
#define HARDWARE_MANIFEST_H

#include <stdbool.h>
#include <stdint.h>

#include "driver/i2c_master.h"
#include "esp_err.h"
#include "esp_hardware_discovery.h"
#include "hardware_manifest_policy.h"

#ifdef __cplusplus
extern "C" {
#endif

#define OBSERVER_MANIFEST_I2C_ADDRESS EEPROM_I2C_ADDR_0

typedef struct {
    hardware_manifest_action_t action;
    uint8_t eui64[EEPROM_UNIQUE_ID_SIZE];
    bool eui64_valid;
    eeprom_capabilities_t capabilities;
    bool capabilities_valid;
} hardware_manifest_result_t;

// Bring up the custom observer's shared I2C bus, inspect its manifest EEPROM,
// apply the safe first-boot policy, and optionally install the compiled board
// defaults when allow_factory_init is true. A successful call means the bus
// and inspection completed; consult result->action before using capabilities.
esp_err_t hardware_manifest_boot(bool allow_factory_init,
                                 hardware_manifest_result_t *result);

// The shared bus is owned by this component so later RTC/sensor/ATECC drivers
// can add devices without creating a second controller on GPIO6/GPIO7.
i2c_master_bus_handle_t hardware_manifest_i2c_bus(void);

const char *hardware_manifest_action_name(hardware_manifest_action_t action);

#ifdef __cplusplus
}
#endif

#endif
