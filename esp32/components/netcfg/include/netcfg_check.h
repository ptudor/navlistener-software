// netcfg_check — the device config record and its validation rule.
//
// Deliberately split out of netcfg.h and kept PURE (no ESP-IDF, no NVS, no httpd, no
// FreeRTOS): validation is the one part of netcfg that can be exhaustively unit-tested on a
// host, and regression fix is exactly the class of bug that happens when the rule lives inline in
// two places instead of one. netcfg.h includes this; nothing else changes for callers.

#ifndef NETCFG_CHECK_H
#define NETCFG_CHECK_H

#include <stdbool.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct {
    char wifi_ssid[33];
    char wifi_pass[65];  // may be empty: an open network is unusual but legal
    char host[64];       // collector hostname
    int  port;           // collector [push] port
    char token[129];     // bearer token
    char station[33];    // observer/station id
    bool insecure;       // skip TLS verification (dev only)
} netcfg_t;

// NETCFG_ERR_CAP bounds the reason buffer netcfg_validate fills. Messages are kept short
// enough (< 26 chars) to render on one line of the 320 px LCD at STATUS_SCALE 2, because the
// boot-time failure path shows the reason on the panel — the operator of a headless board
// otherwise has no way to tell "wrong password" from "no token was ever provisioned".
#define NETCFG_ERR_CAP 48

// netcfg_validate reports whether cfg is complete enough to run in station mode: non-empty
// SSID, collector host, bearer token, and station id, and a port in [1, 65535]. The WiFi
// password is NOT required (open networks are legal).
//
// On failure it writes a short human-readable reason into err (NUL-terminated, truncated to
// errcap) when err is non-NULL and errcap > 0. This is THE definition of "provisioned" —
// netcfg_load returns it, and the portal checks it before saving, so the two can no longer
// disagree. It is a completeness check, not a reachability check: a valid config
// can still hold the wrong password or an unreachable host, which is a runtime failure and
// deliberately not a reason to drop out of station mode.
bool netcfg_validate(const netcfg_t *cfg, char *err, size_t errcap);

#ifdef __cplusplus
}
#endif

#endif // NETCFG_CHECK_H
