#pragma once
#include <stddef.h>
#include "esp_err.h"
typedef void *i2c_master_bus_handle_t;
typedef void *i2c_master_dev_handle_t;
#define I2C_ADDR_BIT_LEN_7 7
typedef struct { int dev_addr_length, device_address, scl_speed_hz; } i2c_device_config_t;
esp_err_t i2c_master_bus_add_device(i2c_master_bus_handle_t bus,
                                  const i2c_device_config_t *config, i2c_master_dev_handle_t *device);
esp_err_t i2c_master_transmit_receive(i2c_master_dev_handle_t device, const void *tx,
                                    size_t tx_length, void *rx, size_t rx_length, int timeout);
esp_err_t i2c_master_transmit(i2c_master_dev_handle_t device, const void *tx, size_t length, int timeout);
