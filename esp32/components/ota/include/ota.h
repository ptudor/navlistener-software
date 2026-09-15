#pragma once
#include "esp_err.h"
#include "esp_http_server.h"
// Only the protected provisioning AP may register the pairing endpoint.
esp_err_t nvf_ota_register_pairing(httpd_handle_t server);
// Start the authenticated station-mode control endpoint (requires pairing key).
esp_err_t nvf_ota_start(void);
// Call after local boot checks, including the receiver task heartbeat. No
// EEPROM, satellite fix or collector connectivity is required for confirmation.
esp_err_t nvf_ota_confirm_boot(void);
