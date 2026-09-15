// netcfg — persisted device config (NVS) + first-boot provisioning.
//
// Config precedence: NVS (field-provisioned) over the compiled Kconfig defaults (dev bench).
// A factory-fresh S3 offers encrypted BLE provisioning and a protected browser fallback.
// Both capture WiFi + collector + token, write one NVS transaction, and reboot into station
// mode. The C6 development board retains the browser path.
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

#define NETCFG_PROVISIONING_NAME_CAP 24
#define NETCFG_PROVISIONING_PASSWORD_CAP 16
#define NETCFG_PROVISIONING_QR_CAP 192

typedef struct {
    char name[NETCFG_PROVISIONING_NAME_CAP];
    char password[NETCFG_PROVISIONING_PASSWORD_CAP];
    char qr_payload[NETCFG_PROVISIONING_QR_CAP];
    bool credential_created; // true only on the boot that minted the label secret
    bool ble_active;         // false on C6 or if BLE initialization failed
} netcfg_provisioning_info_t;

// Loads or creates the persistent per-device setup credential, starts BLE with
// Security 2 on the S3, and starts the password-protected SoftAP browser portal
// as an immediate fallback. A successful path saves one complete netcfg record
// and schedules a reboot. The setup credential lives in an independent NVS
// namespace and survives netcfg_reset_provisioning(). Non-blocking.
esp_err_t netcfg_start_provisioning(netcfg_provisioning_info_t *info);

#ifdef __cplusplus
}
#endif

#endif // NETCFG_H
