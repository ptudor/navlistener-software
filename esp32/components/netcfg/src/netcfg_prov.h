#ifndef NETCFG_PROV_H
#define NETCFG_PROV_H

#include "esp_err.h"
#include "esp_http_server.h"
#include "netcfg_setup.h"

// Starts the Station app's setup: ESP-IDF Unified Provisioning with Security 2 and the
// nav-config, nav-identity and nav-tunnel endpoints. Wi-Fi must already be initialized,
// with the setup AP configured. Over the SoftAP, the endpoints share the setup portal's
// server, whose handle must outlive provisioning; the scheme raises the AP itself. Over
// BLE (NETCFG_BLE_PROVISIONING) the portal is unused, Wi-Fi starts station-only, and the
// caller then adds the AP. On failure Wi-Fi is stopped, and endpoints the SoftAP transport
// registered on the portal are left behind: the caller must replace that server.
esp_err_t netcfg_prov_start(const netcfg_setup_credentials_t *credentials,
                            httpd_handle_t *portal);

#endif
