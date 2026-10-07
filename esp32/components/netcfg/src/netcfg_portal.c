// netcfg_portal — see include/netcfg_portal.h. Pure C: no ESP-IDF, host-testable
// (test/netcfg_portal_test.c, built with and without the insecure checkbox).

#include "netcfg_portal.h"
#include "netcfg_form.h"
#include "netcfg_tunnel.h"
#include "sdkconfig.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// A Kconfig bool left at 'n' emits no #define.
#ifndef CONFIG_NVF_WIREGUARD
#define CONFIG_NVF_WIREGUARD 0
#endif
#ifndef CONFIG_NVF_ALLOW_INSECURE_PORTAL
#define CONFIG_NVF_ALLOW_INSECURE_PORTAL 0
#endif

// refuse zeroes the record so a rejected POST leaves no half-decoded credential behind, and
// hands back a fixed reason that never contains submitted bytes.
static bool refuse(netcfg_t *out, char *err, size_t errcap, const char *why)
{
    memset(out, 0, sizeof *out);
    if (err && errcap) snprintf(err, errcap, "%s", why);
    return false;
}

bool netcfg_portal_decode(const char *body, const netcfg_tunnel_t *stored, netcfg_t *out,
                          char *err, size_t errcap)
{
    if (!out) {
        if (err && errcap) snprintf(err, errcap, "%s", "no record");
        return false;
    }
    memset(out, 0, sizeof *out);
    if (!body) return refuse(out, err, errcap, "no form");

    // Every negative result — too long, or malformed percent-encoding / a control byte —
    // refuses the whole POST. An absent field leaves its zeroed destination alone.
    const struct { const char *name; char *dst; size_t cap; } text_fields[] = {
        { "ssid", out->wifi_ssid, sizeof out->wifi_ssid },
        { "pass", out->wifi_pass, sizeof out->wifi_pass },
        { "host", out->host, sizeof out->host },
        { "token", out->token, sizeof out->token },
    };
    for (size_t i = 0; i < sizeof text_fields / sizeof *text_fields; i++) {
        form_result_t r = netcfg_form_field(body, text_fields[i].name, text_fields[i].dst,
                                            text_fields[i].cap);
        if (r < 0) return refuse(out, err, errcap, netcfg_form_error(r));
    }
    char port[8] = {0};
    form_result_t port_result = netcfg_form_field(body, "port", port, sizeof port);
    if (port_result < 0) return refuse(out, err, errcap, netcfg_form_error(port_result));
    // atoi's 0-on-garbage lands in the range netcfg_validate rejects, as does an absent
    // port, so one rule below catches a 99999/negative/garbage/missing port alike.
    if (port_result == FORM_OK && port[0]) out->port = atoi(port);

    // The station is decoded into its own buffer: on a commissioned board a submitted one is
    // checked, never stored, and an absent one must not be confused with a stored value.
    char station[sizeof out->station] = {0};
    form_result_t station_result = netcfg_form_field(body, "station", station, sizeof station);
    if (station_result < 0) return refuse(out, err, errcap, netcfg_form_error(station_result));
    if (netcfg_commissioned()) {
        // The form shows no station field here, so one arriving is a stale page or a crafted
        // request. The record carries no station: the board names itself.
        char why[NETCFG_ERR_CAP];
        if (!netcfg_station_input_ok(station, why, sizeof why)) return refuse(out, err, errcap, why);
    } else if (station_result == FORM_OK) {
        memcpy(out->station, station, sizeof out->station);
    }

#if CONFIG_NVF_WIREGUARD
    // The pasted profile is the one multi-line field. Absent keeps the stored profile (a
    // partial POST), present-but-blank turns the tunnel off and drops its keys, and
    // anything else must parse and validate whole, or the POST fails with the parser's
    // fixed reason — never the submitted bytes, which include a private key.
    char profile[NETCFG_TUNNEL_CONF_CAP];
    form_result_t wg_result = netcfg_form_field_text(body, "wg", profile, sizeof profile);
    if (wg_result < 0) return refuse(out, err, errcap, netcfg_form_error(wg_result));
    if (wg_result == FORM_ABSENT) {
        if (stored) out->tunnel = *stored;
    } else if (strspn(profile, " \t\r\n") != strlen(profile)) {
        char why[NETCFG_ERR_CAP];
        bool parsed = netcfg_tunnel_parse_conf(profile, &out->tunnel, why, sizeof why);
        memset(profile, 0, sizeof profile); // the pasted text carries the private key
        if (!parsed) return refuse(out, err, errcap, why);
    }
#else
    (void)stored;
#endif

#if CONFIG_NVF_ALLOW_INSECURE_PORTAL
    // The checkbox exists only in a dev/bench build; present means on.
    char ins[8] = {0};
    form_result_t ins_result = netcfg_form_field(body, "insecure", ins, sizeof ins);
    if (ins_result < 0) return refuse(out, err, errcap, netcfg_form_error(ins_result));
    out->insecure = ins[0] != '\0';
#endif
    // With the control compiled out (the shipped default) insecure stays false whatever the
    // body or the compiled NVF_INSECURE default says: the portal cannot store a downgrade.

    // The rule netcfg_load applies at boot. Saving a config the boot path would reject is how
    // a unit ends up unprovisionable without a serial cable — refuse it here, with the field
    // named, while the operator still has the portal open.
    char reason[NETCFG_ERR_CAP];
    if (!netcfg_validate(out, reason, sizeof reason)) return refuse(out, err, errcap, reason);
    if (err && errcap) err[0] = '\0';
    return true;
}
