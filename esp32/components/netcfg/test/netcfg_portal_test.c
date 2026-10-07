// netcfg_portal_test — host unit test for the browser portal's form-to-record decoder.
//
// save_post used to seed its record from netcfg_load(), whose fallback is the compiled
// Kconfig credentials, so a POST that omitted `token` (or `host`, or `pass`) committed the
// bench token and the compiled NVF_INSECURE default reached NVS as a stored TLS-downgrade
// bit. The decoder now starts from a zeroed record; this test is compiled with
// CONFIG_NVF_INSECURE=1 to show the compiled default can no longer leak, once without the
// portal checkbox (the shipped default) and once with CONFIG_NVF_ALLOW_INSECURE_PORTAL=1.
//
//   make -C esp32/components/netcfg/test

#include <stdio.h>
#include <string.h>

#include "netcfg_portal.h"

#ifndef CONFIG_NVF_ALLOW_INSECURE_PORTAL
#define CONFIG_NVF_ALLOW_INSECURE_PORTAL 0
#endif

static int failures;

#define CHECK(cond, ...)                                  \
    do {                                                  \
        if (!(cond)) {                                    \
            fprintf(stderr, "FAIL: " __VA_ARGS__);        \
            fputc('\n', stderr);                          \
            failures++;                                   \
        }                                                 \
    } while (0)

static const char FULL[] = "ssid=fieldnet&pass=correct+horse&host=collector.host.invalid&port=5580"
                           "&station=navfeeder-AABBCC&token=5tKq8yWvNxrPmZbJhGdF";

// A wg-quick profile as a browser posts a textarea: CRLF line ends, '+' for spaces.
static const char WG[] = "wg=%5BInterface%5D%0D%0APrivateKey+%3D+AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8%3D%0D%0A"
                         "Address+%3D+10.77.0.12%2F24%0D%0A%0D%0A%5BPeer%5D%0D%0A"
                         "PublicKey+%3D+ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8%3D%0D%0A"
                         "Endpoint+%3D+wg.collector.invalid%3A51820%0D%0AAllowedIPs+%3D+10.77.0.1%2F32%0D%0A";

static bool zeroed(const netcfg_t *c)
{
    static const netcfg_t empty;
    return memcmp(c, &empty, sizeof empty) == 0;
}

static void expect_refused(const char *name, const char *body, const netcfg_tunnel_t *stored,
                           const char *want_substr)
{
    netcfg_t out;
    memset(&out, 0xAA, sizeof out);
    char err[NETCFG_PORTAL_ERR_CAP] = "unset";
    CHECK(!netcfg_portal_decode(body, stored, &out, err, sizeof err), "%s: accepted, want refused", name);
    CHECK(strstr(err, want_substr) != NULL, "%s: reason \"%s\" does not mention \"%s\"", name, err, want_substr);
    CHECK(zeroed(&out), "%s: refused record was not zeroed", name);
}

static netcfg_t expect_accepted(const char *name, const char *body, const netcfg_tunnel_t *stored)
{
    netcfg_t out;
    memset(&out, 0xAA, sizeof out);
    char err[NETCFG_PORTAL_ERR_CAP] = "unset";
    CHECK(netcfg_portal_decode(body, stored, &out, err, sizeof err), "%s: refused with \"%s\", want accepted", name, err);
    CHECK(err[0] == '\0', "%s: accepted but left reason \"%s\"", name, err);
    return out;
}

int main(void)
{
    netcfg_t full = expect_accepted("full form", FULL, NULL);
    CHECK(!strcmp(full.wifi_ssid, "fieldnet") && !strcmp(full.wifi_pass, "correct horse") &&
          !strcmp(full.host, "collector.host.invalid") && full.port == 5580 &&
          !strcmp(full.station, "navfeeder-AABBCC") && !strcmp(full.token, "5tKq8yWvNxrPmZbJhGdF"),
          "full form: fields were not decoded as posted");
    CHECK(!full.insecure, "full form: insecure is set although the form never asked for it");
    CHECK(!full.tunnel.enabled, "full form: a tunnel appeared from nowhere");

    // An omitted field is empty, never the compiled bench credential: the reason names the
    // field the boot path would reject.
    expect_refused("omitted token", "ssid=fieldnet&pass=x&host=collector.host.invalid&port=5580&station=s", NULL,
                   "bearer token is empty");
    expect_refused("omitted host", "ssid=fieldnet&pass=x&port=5580&station=s&token=t", NULL, "collector host is empty");
    expect_refused("omitted ssid", "pass=x&host=h&port=5580&station=s&token=t", NULL, "wifi ssid is empty");
    expect_refused("omitted station", "ssid=fieldnet&pass=x&host=h&port=5580&token=t", NULL, "station id is empty");
    expect_refused("omitted port", "ssid=fieldnet&pass=x&host=h&station=s&token=t", NULL, "port out of range");
    expect_refused("garbage port", "ssid=fieldnet&pass=x&host=h&port=lots&station=s&token=t", NULL, "port out of range");
    expect_refused("port 99999", "ssid=fieldnet&pass=x&host=h&port=99999&station=s&token=t", NULL, "port out of range");
    expect_refused("empty body", "", NULL, "ssid");
    expect_refused("no body", NULL, NULL, "no form");
    // An omitted password is an open network, which is legal.
    netcfg_t open = expect_accepted("omitted pass", "ssid=fieldnet&host=h&port=5580&station=s&token=t", NULL);
    CHECK(open.wifi_pass[0] == '\0', "omitted pass: password is \"%s\", want empty", open.wifi_pass);

    // Malformed or over-long fields refuse the whole POST with a fixed reason.
    expect_refused("malformed token", "ssid=a&host=h&port=1&station=s&token=t%00k", NULL, "malformed");
    expect_refused("control byte in host", "ssid=a&host=h%0Aost&port=1&station=s&token=t", NULL, "malformed");
    char long_body[256];
    snprintf(long_body, sizeof long_body, "ssid=%s&host=h&port=1&station=s&token=t",
             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); // 40 > 32
    expect_refused("over-long ssid", long_body, NULL, "too long");

    // The insecure bit: compiled out (shipped default) a posted checkbox is ignored; compiled
    // in, present means on and absent means off. In neither build does the compiled
    // NVF_INSECURE=1 this test is built with reach the record.
    char with_checkbox[sizeof FULL + 16];
    snprintf(with_checkbox, sizeof with_checkbox, "%s&insecure=on", FULL);
    netcfg_t boxed = expect_accepted("insecure posted", with_checkbox, NULL);
    CHECK(boxed.insecure == (CONFIG_NVF_ALLOW_INSECURE_PORTAL != 0),
          "insecure posted: stored %d, want %d in this build", boxed.insecure, CONFIG_NVF_ALLOW_INSECURE_PORTAL);
    CHECK(!expect_accepted("insecure absent", FULL, NULL).insecure, "insecure absent: stored as on");

    // A commissioned board names itself: a posted station id is refused, an absent or empty
    // one leaves the record without a station, and the record still validates.
    netcfg_board_t commissioned = {0};
    snprintf(commissioned.observer, sizeof commissioned.observer, "%s", "board-0003-00112233445566778899aabbccddeeff");
    netcfg_set_board(commissioned);
    expect_refused("commissioned with station", FULL, NULL, "station");
    netcfg_t named = expect_accepted("commissioned without station", "ssid=fieldnet&host=h&port=5580&token=t", NULL);
    CHECK(named.station[0] == '\0', "commissioned: a station was stored");
    netcfg_t blank = expect_accepted("commissioned with empty station", "ssid=fieldnet&host=h&port=5580&station=&token=t", NULL);
    CHECK(blank.station[0] == '\0', "commissioned: an empty station input was stored as something");
    netcfg_set_board((netcfg_board_t){0});
    // A wired board needs no network; the rule follows netcfg_set_board as everywhere else.
    expect_refused("wireless without ssid", "host=h&port=5580&station=s&token=t", NULL, "ssid");
    netcfg_set_board((netcfg_board_t){.wired_uplink = true});
    expect_accepted("wired without ssid", "host=h&port=5580&station=s&token=t", NULL);
    netcfg_set_board((netcfg_board_t){0});

    // The tunnel profile is the one stored value the form may keep: absent keeps it,
    // present-but-blank turns it off, a pasted profile replaces it and a bad one refuses.
    netcfg_tunnel_t stored = {.enabled = true, .endpoint_port = 51820, .prefix = 24, .keepalive = 25};
    memset(stored.private_key, 1, sizeof stored.private_key);
    memset(stored.peer_public_key, 2, sizeof stored.peer_public_key);
    snprintf(stored.endpoint_host, sizeof stored.endpoint_host, "%s", "old.collector.invalid");
    stored.address[0] = 10; stored.address[3] = 9; stored.collector[0] = 10; stored.collector[3] = 1;
    netcfg_t kept = expect_accepted("wg absent", FULL, &stored);
    CHECK(kept.tunnel.enabled && !strcmp(kept.tunnel.endpoint_host, "old.collector.invalid") &&
          !memcmp(kept.tunnel.private_key, stored.private_key, sizeof stored.private_key),
          "wg absent: the stored profile was not kept");
    CHECK(!expect_accepted("wg absent, none stored", FULL, NULL).tunnel.enabled, "no stored profile: tunnel enabled");
    char body[sizeof FULL + sizeof WG + 32];
    snprintf(body, sizeof body, "%s&wg=+%%0D%%0A", FULL);
    netcfg_t off = expect_accepted("wg blank", body, &stored);
    CHECK(!off.tunnel.enabled && zeroed(&(netcfg_t){.tunnel = off.tunnel}),
          "wg blank: the tunnel was not turned off and cleared");
    snprintf(body, sizeof body, "%s&%s", FULL, WG);
    netcfg_t pasted = expect_accepted("wg pasted", body, &stored);
    CHECK(pasted.tunnel.enabled && !strcmp(pasted.tunnel.endpoint_host, "wg.collector.invalid") &&
          pasted.tunnel.endpoint_port == 51820 && pasted.tunnel.private_key[0] == 0 && pasted.tunnel.private_key[1] == 1,
          "wg pasted: profile not parsed from the form");
    snprintf(body, sizeof body, "%s&wg=%%5BInterface%%5D%%0APrivateKey+%%3D+not-a-key%%0AAddress+%%3D+10.77.0.12%%2F24%%0A"
                                "%%5BPeer%%5D%%0APublicKey+%%3D+ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8%%3D%%0A"
                                "Endpoint+%%3D+wg.collector.invalid%%3A51820%%0AAllowedIPs+%%3D+10.77.0.1%%2F32%%0A", FULL);
    expect_refused("wg bad", body, &stored, "PrivateKey");
    // A stored profile never rescues a refused form.
    expect_refused("wg absent, token omitted", "ssid=fieldnet&host=h&port=5580&station=s", &stored, "token");

    // The reason buffer is optional and a short one truncates cleanly.
    netcfg_t out;
    CHECK(!netcfg_portal_decode("ssid=a", NULL, &out, NULL, 0), "NULL reason buffer: accepted");
    char tiny[4] = {'x', 'x', 'x', 'x'};
    netcfg_portal_decode("ssid=a", NULL, &out, tiny, sizeof tiny);
    CHECK(tiny[3] == '\0', "tiny reason buffer was not NUL-terminated");

    if (failures) {
        fprintf(stderr, "netcfg_portal_test (insecure checkbox %s): %d failure(s)\n",
                CONFIG_NVF_ALLOW_INSECURE_PORTAL ? "compiled in" : "compiled out", failures);
        return 1;
    }
    printf("netcfg_portal_test (insecure checkbox %s): OK\n",
           CONFIG_NVF_ALLOW_INSECURE_PORTAL ? "compiled in" : "compiled out");
    return 0;
}
