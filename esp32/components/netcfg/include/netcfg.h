// netcfg — persisted device config (NVS) + first-boot SoftAP provisioning portal.
//
// Config precedence: NVS (field-provisioned) over the compiled Kconfig defaults (dev bench).
// A factory-fresh board with no WiFi SSID raises its own AP and serves a small HTTP form to
// capture WiFi + collector + token, writes them to NVS, and reboots into station mode — so a
// unit is provisioned with no serial console (the provisioning pattern from lightshow/shepherd).
// Secrets live only in NVS, never in a committed sdkconfig (docs: config is NVS, never .env).

#ifndef NETCFG_H
#define NETCFG_H

#include <stdbool.h>
#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

typedef struct {
    char wifi_ssid[33];
    char wifi_pass[65];
    char host[64];       // collector hostname
    int  port;           // collector [push] port
    char token[129];     // bearer token
    char station[33];    // observer/station id
    bool insecure;       // skip TLS verification (dev only)
} netcfg_t;

// netcfg_load fills out from NVS, falling back to the compiled Kconfig defaults for any key
// absent from NVS. Returns true when SSID + host are set. It deliberately does not validate
// token/station/port; a partial or externally-written NVS record can therefore leave station
// mode repeatedly failing authentication rather than re-entering the portal.
bool netcfg_load(netcfg_t *out);

// netcfg_save persists cfg to NVS (namespace "navfeeder"). Returns ESP_OK on commit.
esp_err_t netcfg_save(const netcfg_t *cfg);

// netcfg_start_portal brings up a SoftAP + an HTTP config form for an unprovisioned board.
// It generates a strong one-time AP password (never a placeholder) and copies the AP SSID +
// password into ap_ssid/ap_pass so the caller can show them on the LCD. On a successful form
// submit it saves to NVS and reboots into station mode. Non-blocking (the HTTP server runs on
// its own task); returns ESP_OK once the AP + server are up.
esp_err_t netcfg_start_portal(char ap_ssid[33], char ap_pass[16]);

#ifdef __cplusplus
}
#endif

#endif // NETCFG_H
