// netcfg — persisted device config (NVS) + first-boot provisioning.
//
// Config precedence: NVS (field-provisioned) over the compiled Kconfig defaults (dev bench).
// A factory-fresh S3 raises a WPA2 setup SoftAP with a browser form. An image built with
// NVF_BLE_PROVISIONING also offers encrypted BLE provisioning beside it. Both capture WiFi +
// collector + token, write one NVS transaction, and reboot into station mode.
// Secrets live only in NVS, never in a committed sdkconfig (docs: config is NVS, never .env).

#ifndef NETCFG_H
#define NETCFG_H

#include <stdbool.h>
#include "esp_err.h"
#include "sdkconfig.h"
#include "netcfg_check.h" // netcfg_t + netcfg_validate (pure, host-testable)

// BLE provisioning is built only with NVF_BLE_PROVISIONING. The S3 image leaves Bluetooth
// off: beside the SoftAP the BLE stack does not fit in internal RAM.
#if defined(CONFIG_NVF_BLE_PROVISIONING)
#define NETCFG_BLE_PROVISIONING 1
#else
#define NETCFG_BLE_PROVISIONING 0
#endif

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
    bool ble_active;         // false without NETCFG_BLE_PROVISIONING or if BLE failed to start
} netcfg_provisioning_info_t;

// Loads or creates the persistent per-device setup credential and starts the
// password-protected SoftAP browser portal, plus BLE with Security 2 when built
// with NETCFG_BLE_PROVISIONING. qr_payload is the setup label: a Wi-Fi network
// QR, or the ESP Provisioning BLE payload on a BLE build. A successful path saves
// one complete netcfg record and schedules a reboot. The setup credential lives
// in an independent NVS namespace and survives netcfg_reset_provisioning().
// Non-blocking.
esp_err_t netcfg_start_provisioning(netcfg_provisioning_info_t *info);

#ifdef __cplusplus
}
#endif

#endif // NETCFG_H
