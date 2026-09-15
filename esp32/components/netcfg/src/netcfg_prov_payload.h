#ifndef NETCFG_PROV_PAYLOAD_H
#define NETCFG_PROV_PAYLOAD_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "netcfg_check.h"

#define NETCFG_PROV_RESPONSE_SIZE 5

typedef enum {
    NETCFG_PROV_WAITING_FOR_WIFI = 0,
    NETCFG_PROV_SAVED_REBOOTING = 1,
    NETCFG_PROV_INVALID_REQUEST = 2,
    NETCFG_PROV_STORAGE_FAILED = 3,
    NETCFG_PROV_SAVED_RESTART_REQUIRED = 4,
    // nav-tunnel only: the complete record was already saved (Wi-Fi verified first), so
    // the profile was not applied. The app must reset and provision again, tunnel first.
    NETCFG_PROV_ALREADY_SAVED = 5,
} netcfg_prov_status_t;

// Decodes the nav-config v1 binary request. Wi-Fi fields remain empty and are
// supplied later by the standard ESP provisioning endpoint.
bool netcfg_prov_decode(const uint8_t *data, size_t size, netcfg_t *out,
                        char *error, size_t error_cap);

void netcfg_prov_encode_response(uint8_t out[NETCFG_PROV_RESPONSE_SIZE],
                                 netcfg_prov_status_t status);

#endif
