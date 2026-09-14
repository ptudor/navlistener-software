// netcfg_validate_test — host unit test for the station-mode config rule.
//
// netcfg_validate is the single definition of "provisioned enough to run": netcfg_load
// returns it at boot and the provisioning portal checks it before writing NVS. Before the
// split, boot checked two fields and the portal checked a different, larger set, so a config
// populated by Kconfig, a partial NVS write, or external NVS tooling could pass the boot gate
// and then fail forever at WiFi/TLS/auth with no runtime portal fallback — a serial-cable
// recovery on a board whose GPIO9 strapping makes that unusually awkward.
//
// netcfg_check.c is pure C (no ESP-IDF), so this runs on the host:
//
//   make -C esp32/components/netcfg/test
//
// Covers the review's verification list: empty token, empty station, zero/negative/
// out-of-range port, and truncated values.

#include <stdio.h>
#include <string.h>

#include "netcfg_check.h"

static int failures;

#define CHECK(cond, ...)                                  \
    do {                                                  \
        if (!(cond)) {                                    \
            fprintf(stderr, "FAIL: " __VA_ARGS__);        \
            fputc('\n', stderr);                          \
            failures++;                                   \
        }                                                 \
    } while (0)

// valid_cfg returns a fully provisioned config; each case below breaks exactly one field, so
// a failure identifies the rule that changed rather than "something is wrong".
static netcfg_t valid_cfg(void)
{
    netcfg_t c;
    memset(&c, 0, sizeof c);
    snprintf(c.wifi_ssid, sizeof c.wifi_ssid, "%s", "fieldnet");
    snprintf(c.wifi_pass, sizeof c.wifi_pass, "%s", "correct-horse-battery");
    snprintf(c.host, sizeof c.host, "%s", "collector.host.invalid");
    c.port = 5580;
    snprintf(c.token, sizeof c.token, "%s", "5tKq8yWvNxrPmZbJhGdF");
    snprintf(c.station, sizeof c.station, "%s", "navfeeder-AABBCC");
    return c;
}

static void expect_valid(const char *name, netcfg_t c)
{
    char err[NETCFG_ERR_CAP] = "unset";
    CHECK(netcfg_validate(&c, err, sizeof err), "%s: rejected with \"%s\", want accepted", name, err);
    CHECK(err[0] == '\0', "%s: accepted but left reason \"%s\", want it cleared", name, err);
}

static void expect_invalid(const char *name, netcfg_t c, const char *want_substr)
{
    char err[NETCFG_ERR_CAP] = "unset";
    CHECK(!netcfg_validate(&c, err, sizeof err), "%s: accepted, want rejected", name);
    CHECK(strstr(err, want_substr) != NULL, "%s: reason \"%s\" does not mention \"%s\"", name, err,
          want_substr);
    // The reason is rendered on one LCD line at STATUS_SCALE 2 (~26 chars); a message that
    // silently overflows would be worse than none on a headless board.
    CHECK(strlen(err) < 26, "%s: reason \"%s\" is %zu chars, want < 26 to fit the display", name,
          err, strlen(err));
}

int main(void)
{
    expect_valid("fully provisioned", valid_cfg());

    // An open WiFi network is legal — the password is the one field that may be empty.
    netcfg_t open_net = valid_cfg();
    open_net.wifi_pass[0] = '\0';
    expect_valid("open wifi (no password)", open_net);

    netcfg_t c;

    c = valid_cfg();
    c.wifi_ssid[0] = '\0';
    expect_invalid("empty ssid", c, "ssid");

    c = valid_cfg();
    c.host[0] = '\0';
    expect_invalid("empty host", c, "host");

    c = valid_cfg();
    c.token[0] = '\0';
    expect_invalid("empty token", c, "token");

    c = valid_cfg();
    c.station[0] = '\0';
    expect_invalid("empty station", c, "station");

    // Whitespace-only is empty: a form POST of "   " would otherwise satisfy a bare
    // [0] != '\0' check and produce a station id the collector matches to no observer.
    c = valid_cfg();
    snprintf(c.station, sizeof c.station, "%s", "   ");
    expect_invalid("whitespace-only station", c, "station");

    c = valid_cfg();
    snprintf(c.token, sizeof c.token, "%s", "\t ");
    expect_invalid("whitespace-only token", c, "token");

    // Port: 0 is the "NVS key never written" value, negatives come from a sign-flipped or
    // garbage i32, and > 65535 from a typo in the portal form.
    const int bad_ports[] = { 0, -1, -5580, 65536, 99999, 1 << 30 };
    for (size_t i = 0; i < sizeof bad_ports / sizeof bad_ports[0]; i++) {
        char name[48];
        snprintf(name, sizeof name, "port %d", bad_ports[i]);
        c = valid_cfg();
        c.port = bad_ports[i];
        expect_invalid(name, c, "port");
    }
    const int ok_ports[] = { 1, 80, 5580, 65535 };
    for (size_t i = 0; i < sizeof ok_ports / sizeof ok_ports[0]; i++) {
        char name[48];
        snprintf(name, sizeof name, "port %d", ok_ports[i]);
        c = valid_cfg();
        c.port = ok_ports[i];
        expect_valid(name, c);
    }

    // Truncated NVS values: a value cut short is still a value. The rule is completeness,
    // not plausibility — a one-character host is accepted here and fails at DNS, which is a
    // runtime condition and deliberately not a reason to drop out of station mode.
    c = valid_cfg();
    snprintf(c.host, sizeof c.host, "%s", "c");
    expect_valid("single-character host", c);

    // Full-width fields must not trip any length assumption: fill each buffer to capacity-1.
    c = valid_cfg();
    memset(c.wifi_ssid, 'a', sizeof c.wifi_ssid - 1);
    c.wifi_ssid[sizeof c.wifi_ssid - 1] = '\0';
    memset(c.token, 'b', sizeof c.token - 1);
    c.token[sizeof c.token - 1] = '\0';
    memset(c.station, 'c', sizeof c.station - 1);
    c.station[sizeof c.station - 1] = '\0';
    expect_valid("maximum-length fields", c);

    // A NULL config must be rejected, not dereferenced.
    char err[NETCFG_ERR_CAP] = "unset";
    CHECK(!netcfg_validate(NULL, err, sizeof err), "NULL config: accepted, want rejected");

    // The reason buffer is optional: callers that only want the boolean pass NULL/0.
    netcfg_t bad = valid_cfg();
    bad.token[0] = '\0';
    CHECK(!netcfg_validate(&bad, NULL, 0), "NULL reason buffer: accepted, want rejected");

    // A too-small reason buffer must truncate, never overflow.
    char tiny[4] = { 'x', 'x', 'x', 'x' };
    netcfg_validate(&bad, tiny, sizeof tiny);
    CHECK(tiny[3] == '\0', "tiny reason buffer was not NUL-terminated");

    if (failures) {
        fprintf(stderr, "netcfg_validate_test: %d failure(s)\n", failures);
        return 1;
    }
    printf("netcfg_validate_test: OK\n");
    return 0;
}
