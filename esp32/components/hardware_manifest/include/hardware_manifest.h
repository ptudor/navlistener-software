#ifndef HARDWARE_MANIFEST_H
#define HARDWARE_MANIFEST_H

#include <stdbool.h>
#include <stdint.h>

#include "driver/i2c_master.h"
#include "esp_err.h"
#include "esp_hardware_discovery.h"
#include "hardware_manifest_policy.h"
#include "manifest_board.h"
#include "../../../../common/board_identity.h"

#ifdef __cplusplus
extern "C" {
#endif


typedef struct {
    hardware_manifest_action_t action;
    nvf_board_identity_t identity; // eeprom_valid: the manifest EEPROM's 128-bit serial was read
    eeprom_capabilities_t capabilities;
    bool capabilities_valid;
    board_model_t board; // the board the manifest names; BOARD_MODEL_NONE unless used
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

// A device handle on the shared bus for one 7-bit address at 100 kHz, for the occasional
// reads that have no driver of their own: identity registers, the ATECC, the manifest
// EEPROM's serial. It is added at first use and kept. ESP-IDF 5.5 refuses to remove a device
// while any transfer is in flight on the bus, whichever task's and whichever device's, and
// leaves it allocated, so a handle added and removed around each read fails, and leaks,
// whenever another part is being spoken to at that moment. NULL when the bus is not up, the
// address is not a 7-bit one, or the handle cannot be added. From any task.
i2c_master_dev_handle_t hardware_manifest_i2c_device(uint8_t address);

esp_err_t hardware_manifest_read_identity(nvf_board_identity_t *out);

const char *hardware_manifest_action_name(hardware_manifest_action_t action);

#ifdef __cplusplus
}
#endif

#endif
