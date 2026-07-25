// netcfg_check — see include/netcfg_check.h. Pure C: no ESP-IDF, host-testable
// (test/netcfg_validate_test.c).

#include "netcfg_check.h"

#include <stdio.h>
#include <string.h>

// set_err copies a reason, truncating cleanly. A NULL/zero-cap buffer is legal — callers that
// only want the boolean pass NULL.
static void set_err(char *err, size_t errcap, const char *msg)
{
    if (!err || errcap == 0) return;
    snprintf(err, errcap, "%s", msg);
}

// nonempty treats a field of only spaces as empty: a form POST of "   " for the station id
// would otherwise satisfy a bare [0] != '\0' check and produce a station the collector
// cannot match to any observer record.
static bool nonempty(const char *s)
{
    if (!s) return false;
    for (; *s; s++) {
        if (*s != ' ' && *s != '\t') return true;
    }
    return false;
}

bool netcfg_validate(const netcfg_t *cfg, char *err, size_t errcap)
{
    if (!cfg) {
        set_err(err, errcap, "no config");
        return false;
    }
    // Order matters only for which reason the operator sees first; it follows the
    // provisioning form's field order so the message points at the field to fix.
    if (!nonempty(cfg->wifi_ssid)) {
        set_err(err, errcap, "wifi ssid is empty");
        return false;
    }
    if (!nonempty(cfg->host)) {
        set_err(err, errcap, "collector host is empty");
        return false;
    }
    if (cfg->port < 1 || cfg->port > 65535) {
        // NVS stores the port as i32 and Kconfig can carry any int, so this catches 0
        // (the "key absent, never set" value), negatives, and 99999-style typos that
        // would otherwise produce a permanently unconnectable unit.
        set_err(err, errcap, "port out of range 1-65535");
        return false;
    }
    if (!nonempty(cfg->station)) {
        set_err(err, errcap, "station id is empty");
        return false;
    }
    if (!nonempty(cfg->token)) {
        set_err(err, errcap, "bearer token is empty");
        return false;
    }
    set_err(err, errcap, "");
    return true;
}
