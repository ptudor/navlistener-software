#ifndef NETCFG_SETUP_H
#define NETCFG_SETUP_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

#define NETCFG_SETUP_NAME_CAP 24
#define NETCFG_SETUP_PASSWORD_CAP 16
#define NETCFG_SETUP_QR_CAP 192

typedef struct {
    char name[NETCFG_SETUP_NAME_CAP];
    char username[NETCFG_SETUP_NAME_CAP];
    char password[NETCFG_SETUP_PASSWORD_CAP];
} netcfg_setup_credentials_t;

// Loads the independent setup credential, or creates it exactly once. The MAC
// and RNG are parameters so persistence and corruption behavior remain
// host-testable without ESP-IDF radio hardware.
esp_err_t netcfg_setup_load_or_create(netcfg_setup_credentials_t *out,
                                      const uint8_t mac[6],
                                      uint32_t (*random_word)(void),
                                      bool *created);

// Official ESP Provisioning QR payload for BLE + Security 2.
esp_err_t netcfg_setup_qr_payload(const netcfg_setup_credentials_t *credentials,
                                  char *out, size_t out_cap);

#endif
