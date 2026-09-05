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
#include "netcfg_check.h" // netcfg_t + netcfg_validate (pure, host-testable)

#ifdef __cplusplus
extern "C" {
#endif

// netcfg_load reads the complete versioned NVS record, including its reset marker.
// Before the first versioned save only, legacy keys override compiled defaults.
// Unreadable/malformed records fail closed instead of exposing legacy/default
// credentials. Returns the shared netcfg_validate rule, with a reason in err.
bool netcfg_load(netcfg_t *out, char *err, size_t errcap);

// netcfg_save validates and replaces one versioned blob in namespace "navfeeder".
// An error may leave the complete old OR new record durable, never mixed fields.
// A successful save retires the reset marker in the same atomic replacement.
esp_err_t netcfg_save(const netcfg_t *cfg);

// netcfg_reset_provisioning replaces configuration with a persistent reset unit
// that suppresses legacy keys and compiled development defaults on the next boot.
// It is a logical reset, not secure flash erasure.
// Hardware identity and enrollment material must live outside this namespace.
esp_err_t netcfg_reset_provisioning(void);

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
