#ifndef NETCFG_BLE_H
#define NETCFG_BLE_H

#include "esp_err.h"
#include "netcfg_setup.h"

// Wi-Fi must already be initialized. On success the provisioning manager has
// started Wi-Fi in STA mode; the caller may then add the fallback SoftAP.
esp_err_t netcfg_ble_start(const netcfg_setup_credentials_t *credentials);

#endif
