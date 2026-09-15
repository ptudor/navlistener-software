// netcfg_tunnel_test — host unit test for the WireGuard profile parser, the nav-tunnel
// binary frame, the canonical key codec and the shared validation rule.
//
//   make -C esp32/components/netcfg/test
//
// The profile is untrusted input from the browser portal (pasted text) and the Station
// app (BLE frame). Every rejection here is a record that must never reach NVS.

#include <assert.h>
#include <stdio.h>
#include <string.h>

#include "netcfg_tunnel.h"

static int failures;

#define CHECK(cond, ...)                           \
    do {                                           \
        if (!(cond)) {                             \
            fprintf(stderr, "FAIL: " __VA_ARGS__); \
            fputc('\n', stderr);                   \
            failures++;                            \
        }                                          \
    } while (0)

// Test vectors: any 32 bytes are a valid X25519 private key once clamped, and the parser
// never derives anything from them, so fixed patterns are fine. These are not real keys.
static const char PRIVATE[] = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=";
static const char PUBLIC[]  = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=";
static const char PSK[]     = "QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8=";

static const char PROFILE[] =
    "[Interface]\n"
    "PrivateKey = AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"
    "Address = 10.77.0.12/24\n"
    "\n"
    "[Peer]\n"
    "PublicKey = ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=\n"
    "PresharedKey = QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8=\n"
    "Endpoint = wg.collector.invalid:51820\n"
    "AllowedIPs = 10.77.0.1/32\n"
    "PersistentKeepalive = 25\n";

static void expect_profile(const netcfg_tunnel_t *t, bool psk)
{
    uint8_t key[32];
    CHECK(t->enabled, "profile not enabled");
    CHECK(netcfg_tunnel_key_decode(PRIVATE, 44, key) && !memcmp(key, t->private_key, 32), "private key");
    CHECK(netcfg_tunnel_key_decode(PUBLIC, 44, key) && !memcmp(key, t->peer_public_key, 32), "public key");
    if (psk) CHECK(netcfg_tunnel_key_decode(PSK, 44, key) && !memcmp(key, t->preshared_key, 32), "psk");
    else { uint8_t z[32] = {0}; CHECK(!memcmp(t->preshared_key, z, 32), "psk should be absent"); }
    CHECK(!strcmp(t->endpoint_host, "wg.collector.invalid"), "endpoint host '%s'", t->endpoint_host);
    CHECK(t->endpoint_port == 51820, "endpoint port %d", t->endpoint_port);
    CHECK(t->address[0] == 10 && t->address[1] == 77 && t->address[2] == 0 && t->address[3] == 12, "address");
    CHECK(t->prefix == 24, "prefix %d", t->prefix);
    CHECK(t->collector[0] == 10 && t->collector[1] == 77 && t->collector[2] == 0 && t->collector[3] == 1, "collector");
    CHECK(t->keepalive == 25, "keepalive %d", t->keepalive);
}

static void expect_rejected(const char *name, const char *text, const char *want)
{
    netcfg_tunnel_t t;
    char err[NETCFG_ERR_CAP] = "unset";
    CHECK(!netcfg_tunnel_parse_conf(text, &t, err, sizeof err), "%s: accepted, want rejected", name);
    CHECK(strstr(err, want) != NULL, "%s: reason \"%s\" does not mention \"%s\"", name, err, want);
    CHECK(strlen(err) < NETCFG_ERR_CAP - 1, "%s: reason overflowed", name);
    CHECK(!t.enabled, "%s: rejected profile left enabled", name);
    uint8_t z[32] = {0};
    CHECK(!memcmp(t.private_key, z, 32), "%s: rejected profile kept key material", name);
}

// replace returns PROFILE with one line swapped, for single-fault cases.
static const char *replace(char *buf, size_t cap, const char *from, const char *to)
{
    const char *at = strstr(PROFILE, from);
    assert(at);
    size_t head = (size_t)(at - PROFILE);
    assert(head + strlen(to) + strlen(at + strlen(from)) < cap);
    memcpy(buf, PROFILE, head);
    strcpy(buf + head, to);
    strcat(buf, at + strlen(from));
    return buf;
}

static size_t frame(uint8_t *out, const netcfg_tunnel_t *t, const char *host)
{
    memcpy(out, "NVT1", 4);
    out[4] = 0;
    memcpy(out + 5, t->private_key, 32);
    memcpy(out + 37, t->peer_public_key, 32);
    memcpy(out + 69, t->preshared_key, 32);
    memcpy(out + 101, t->address, 4);
    out[105] = (uint8_t)t->prefix;
    memcpy(out + 106, t->collector, 4);
    out[110] = (uint8_t)(t->endpoint_port >> 8);
    out[111] = (uint8_t)t->endpoint_port;
    out[112] = (uint8_t)(t->keepalive >> 8);
    out[113] = (uint8_t)t->keepalive;
    out[114] = (uint8_t)strlen(host);
    memcpy(out + 115, host, strlen(host));
    return 115 + strlen(host);
}

int main(void)
{
    // --- key codec ---
    uint8_t key[32], back[32];
    char text[45];
    for (int i = 0; i < 32; i++) key[i] = (uint8_t)(i * 37 + 11);
    netcfg_tunnel_key_encode(key, text);
    CHECK(strlen(text) == 44 && text[43] == '=', "encode shape '%s'", text);
    CHECK(netcfg_tunnel_key_decode(text, 44, back) && !memcmp(key, back, 32), "round trip");
    CHECK(netcfg_tunnel_key_decode(PRIVATE, 44, back) && back[0] == 0 && back[31] == 31, "vector");
    // Non-canonical: the 43rd character carries two trailing bits that must be zero.
    char noncanon[45];
    strcpy(noncanon, PRIVATE);
    noncanon[42] = 'B'; // 'A'(0) -> 'B'(1): low bits set
    CHECK(!netcfg_tunnel_key_decode(noncanon, 44, back), "non-canonical accepted");
    CHECK(!netcfg_tunnel_key_decode(PRIVATE, 43, back), "short accepted");
    strcpy(noncanon, PRIVATE); noncanon[43] = 'A';
    CHECK(!netcfg_tunnel_key_decode(noncanon, 44, back), "missing pad accepted");
    strcpy(noncanon, PRIVATE); noncanon[5] = '-';
    CHECK(!netcfg_tunnel_key_decode(noncanon, 44, back), "url-safe alphabet accepted");
    strcpy(noncanon, PRIVATE); noncanon[5] = ' ';
    CHECK(!netcfg_tunnel_key_decode(noncanon, 44, back), "space accepted");

    // --- IPv4 ---
    uint8_t ip[4];
    CHECK(netcfg_tunnel_ip4_parse("10.77.0.1", 9, ip) && ip[0] == 10 && ip[3] == 1, "ip parse");
    CHECK(netcfg_tunnel_ip4_parse("0.0.0.0", 7, ip), "zero ip parses (validation rejects it)");
    CHECK(netcfg_tunnel_ip4_parse("255.255.255.255", 15, ip) && ip[2] == 255, "max ip");
    const char *bad_ips[] = { "10.77.0", "10.77.0.1.2", "10.077.0.1", "256.0.0.1", "10.77.0.1 ", " 10.77.0.1",
                              "10..0.1", "a.b.c.d", "", "1234.0.0.1", "10.77.0.-1" };
    for (size_t i = 0; i < sizeof bad_ips / sizeof bad_ips[0]; i++)
        CHECK(!netcfg_tunnel_ip4_parse(bad_ips[i], strlen(bad_ips[i]), ip), "bad ip '%s' accepted", bad_ips[i]);
    char dotted[16];
    uint8_t q[4] = { 10, 77, 0, 12 };
    netcfg_tunnel_ip4_format(q, dotted);
    CHECK(!strcmp(dotted, "10.77.0.12"), "format '%s'", dotted);
    netcfg_tunnel_prefix_mask(24, dotted); CHECK(!strcmp(dotted, "255.255.255.0"), "/24 mask '%s'", dotted);
    netcfg_tunnel_prefix_mask(32, dotted); CHECK(!strcmp(dotted, "255.255.255.255"), "/32 mask '%s'", dotted);
    netcfg_tunnel_prefix_mask(1, dotted);  CHECK(!strcmp(dotted, "128.0.0.0"), "/1 mask '%s'", dotted);
    netcfg_tunnel_prefix_mask(0, dotted);  CHECK(!strcmp(dotted, "0.0.0.0"), "/0 mask '%s'", dotted);

    // --- profile text ---
    netcfg_tunnel_t t;
    char err[NETCFG_ERR_CAP] = "unset";
    CHECK(netcfg_tunnel_parse_conf(PROFILE, &t, err, sizeof err), "canonical profile rejected: %s", err);
    CHECK(err[0] == '\0', "accepted profile left reason '%s'", err);
    expect_profile(&t, true);

    // Operators paste from many editors: CRLF, tabs, comments (full-line and trailing),
    // lower-case keys, spacing around '=', a trailing dot-less bare address and "off".
    const char messy[] =
        "# generated for observer roof\r\n"
        "[interface]\r\n"
        "privatekey=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=   # observer key\r\n"
        "\tAddress\t=\t10.77.0.12/24\r\n"
        "DNS = 9.9.9.9\r\n"
        "MTU = 1420\r\n"
        "ListenPort = 51820\r\n"
        "[PEER]\r\n"
        "PublicKey = ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=\r\n"
        "Endpoint = wg.collector.invalid:51820\r\n"
        "AllowedIPs = 10.77.0.1\r\n"
        "PersistentKeepalive = off\r\n";
    CHECK(netcfg_tunnel_parse_conf(messy, &t, err, sizeof err), "messy profile rejected: %s", err);
    CHECK(t.enabled && t.keepalive == 0 && t.prefix == 24 && t.collector[3] == 1, "messy fields");
    { uint8_t z[32] = {0}; CHECK(!memcmp(t.preshared_key, z, 32), "messy psk should be absent"); }

    // Keepalive absent means off, as wg(8) treats it; a bare Address is a /32 host.
    char buf[1024];
    replace(buf, sizeof buf, "PersistentKeepalive = 25\n", "");
    CHECK(netcfg_tunnel_parse_conf(buf, &t, err, sizeof err) && t.keepalive == 0, "keepalive default: %s", err);
    replace(buf, sizeof buf, "Address = 10.77.0.12/24\n", "Address = 10.77.0.12\n");
    CHECK(netcfg_tunnel_parse_conf(buf, &t, err, sizeof err) && t.prefix == 32, "bare address: %s", err);
    // An IPv4 literal endpoint is fine; the resolver is never consulted for it.
    replace(buf, sizeof buf, "Endpoint = wg.collector.invalid:51820\n", "Endpoint = 203.0.113.5:443\n");
    CHECK(netcfg_tunnel_parse_conf(buf, &t, err, sizeof err) && t.endpoint_port == 443 &&
          !strcmp(t.endpoint_host, "203.0.113.5"), "ip endpoint: %s", err);

    // Rejections, one fault each.
    expect_rejected("no interface", "[Peer]\nPublicKey = ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=\n", "[Interface]");
    expect_rejected("no peer", "[Interface]\nPrivateKey = AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\nAddress = 10.77.0.12\n", "[Peer]");
    expect_rejected("empty", "", "[Interface]");
    expect_rejected("key before section", "PrivateKey = x\n[Interface]\n", "outside");
    expect_rejected("bad header", "[Interface\n", "section header");
    expect_rejected("unknown section", "[Tunnel]\n", "unknown section");
    expect_rejected("no equals", "[Interface]\nPrivateKey\n", "Key = Value");
    expect_rejected("empty value", "[Interface]\nPrivateKey =\n", "Key = Value");
    expect_rejected("missing private", replace(buf, sizeof buf, "PrivateKey = AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n", ""), "PrivateKey missing");
    expect_rejected("missing address", replace(buf, sizeof buf, "Address = 10.77.0.12/24\n", ""), "Address missing");
    expect_rejected("missing public", replace(buf, sizeof buf, "PublicKey = ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=\n", ""), "PublicKey missing");
    expect_rejected("missing endpoint", replace(buf, sizeof buf, "Endpoint = wg.collector.invalid:51820\n", ""), "Endpoint missing");
    expect_rejected("missing allowed", replace(buf, sizeof buf, "AllowedIPs = 10.77.0.1/32\n", ""), "AllowedIPs missing");
    expect_rejected("bad private", replace(buf, sizeof buf, "PrivateKey = AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n", "PrivateKey = not-a-key\n"), "PrivateKey");
    expect_rejected("bad public", replace(buf, sizeof buf, "PublicKey = ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=\n", "PublicKey = ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8\n"), "PublicKey");
    expect_rejected("bad psk", replace(buf, sizeof buf, "PresharedKey = QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8=\n", "PresharedKey = QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl9=\n"), "PresharedKey");
    expect_rejected("ipv6 address", replace(buf, sizeof buf, "Address = 10.77.0.12/24\n", "Address = fd00::12/64\n"), "Address");
    expect_rejected("two addresses", replace(buf, sizeof buf, "Address = 10.77.0.12/24\n", "Address = 10.77.0.12/24, 10.77.1.12/24\n"), "Address");
    expect_rejected("address /33", replace(buf, sizeof buf, "Address = 10.77.0.12/24\n", "Address = 10.77.0.12/33\n"), "Address");
    expect_rejected("address /0", replace(buf, sizeof buf, "Address = 10.77.0.12/24\n", "Address = 10.77.0.12/0\n"), "address");
    expect_rejected("zero address", replace(buf, sizeof buf, "Address = 10.77.0.12/24\n", "Address = 0.0.0.0/24\n"), "address");
    expect_rejected("ipv6 endpoint", replace(buf, sizeof buf, "Endpoint = wg.collector.invalid:51820\n", "Endpoint = [2001:db8::1]:51820\n"), "Endpoint");
    expect_rejected("no port", replace(buf, sizeof buf, "Endpoint = wg.collector.invalid:51820\n", "Endpoint = wg.collector.invalid\n"), "Endpoint");
    expect_rejected("port 0", replace(buf, sizeof buf, "Endpoint = wg.collector.invalid:51820\n", "Endpoint = wg.collector.invalid:0\n"), "Endpoint");
    expect_rejected("port 65536", replace(buf, sizeof buf, "Endpoint = wg.collector.invalid:51820\n", "Endpoint = wg.collector.invalid:65536\n"), "Endpoint");
    expect_rejected("host chars", replace(buf, sizeof buf, "Endpoint = wg.collector.invalid:51820\n", "Endpoint = wg_collector.invalid:51820\n"), "Endpoint");
    expect_rejected("host too long", replace(buf, sizeof buf, "Endpoint = wg.collector.invalid:51820\n",
        "Endpoint = aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.invalid:51820\n"), "Endpoint");
    expect_rejected("allowed subnet", replace(buf, sizeof buf, "AllowedIPs = 10.77.0.1/32\n", "AllowedIPs = 10.77.0.0/24\n"), "AllowedIPs");
    expect_rejected("allowed default route", replace(buf, sizeof buf, "AllowedIPs = 10.77.0.1/32\n", "AllowedIPs = 0.0.0.0/0\n"), "AllowedIPs");
    expect_rejected("allowed list", replace(buf, sizeof buf, "AllowedIPs = 10.77.0.1/32\n", "AllowedIPs = 10.77.0.1/32, 10.77.0.2/32\n"), "AllowedIPs");
    expect_rejected("allowed ipv6", replace(buf, sizeof buf, "AllowedIPs = 10.77.0.1/32\n", "AllowedIPs = fd00::1/128\n"), "AllowedIPs");
    expect_rejected("allowed is self", replace(buf, sizeof buf, "AllowedIPs = 10.77.0.1/32\n", "AllowedIPs = 10.77.0.12/32\n"), "collector");
    expect_rejected("keepalive text", replace(buf, sizeof buf, "PersistentKeepalive = 25\n", "PersistentKeepalive = soon\n"), "PersistentKeepalive");
    expect_rejected("keepalive big", replace(buf, sizeof buf, "PersistentKeepalive = 25\n", "PersistentKeepalive = 65536\n"), "PersistentKeepalive");
    expect_rejected("unknown interface key", replace(buf, sizeof buf, "Address = 10.77.0.12/24\n", "Address = 10.77.0.12/24\nColour = blue\n"), "unknown [Interface]");
    expect_rejected("unknown peer key", replace(buf, sizeof buf, "PersistentKeepalive = 25\n", "PersistentKeepalive = 25\nRoute = all\n"), "unknown [Peer]");
    expect_rejected("duplicate key", replace(buf, sizeof buf, "PersistentKeepalive = 25\n", "PersistentKeepalive = 25\nPersistentKeepalive = 30\n"), "PersistentKeepalive");
    expect_rejected("duplicate endpoint", replace(buf, sizeof buf, "PersistentKeepalive = 25\n", "PersistentKeepalive = 25\nEndpoint = other.invalid:1\n"), "duplicate");
    expect_rejected("second peer", replace(buf, sizeof buf, "PersistentKeepalive = 25\n", "PersistentKeepalive = 25\n[Peer]\n"), "one [Peer]");
    expect_rejected("second interface", replace(buf, sizeof buf, "[Peer]\n", "[Interface]\n[Peer]\n"), "one [Interface]");
    // A key in the wrong section is an unknown key there, never silently accepted.
    expect_rejected("public key in interface", replace(buf, sizeof buf, "Address = 10.77.0.12/24\n", "Address = 10.77.0.12/24\nPublicKey = ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=\n"), "unknown [Interface]");
    // Oversized text is refused before parsing, and a NULL is not dereferenced.
    static char huge[NETCFG_TUNNEL_CONF_CAP + 8];
    memset(huge, '#', sizeof huge - 1);
    expect_rejected("too long", huge, "too long");
    CHECK(!netcfg_tunnel_parse_conf(NULL, &t, err, sizeof err), "NULL text accepted");
    CHECK(!netcfg_tunnel_parse_conf(PROFILE, NULL, err, sizeof err), "NULL out accepted");
    // Reason buffers are optional and never overflowed.
    CHECK(!netcfg_tunnel_parse_conf("", &t, NULL, 0), "NULL reason: accepted");
    char tiny[4] = { 'x', 'x', 'x', 'x' };
    netcfg_tunnel_parse_conf("", &t, tiny, sizeof tiny);
    CHECK(tiny[3] == '\0', "tiny reason not terminated");

    // --- nav-tunnel v1 frame ---
    netcfg_tunnel_t ref;
    CHECK(netcfg_tunnel_parse_conf(PROFILE, &ref, err, sizeof err), "reference profile");
    uint8_t data[256];
    size_t size = frame(data, &ref, "wg.collector.invalid");
    CHECK(size == 135, "frame size %zu", size);
    CHECK(netcfg_tunnel_decode(data, size, &t, err, sizeof err), "frame rejected: %s", err);
    expect_profile(&t, true);
    // Longest legal frame fits the documented maximum.
    char longest[64];
    memset(longest, 'h', 63); longest[63] = '\0';
    size = frame(data, &ref, longest);
    CHECK(size == NETCFG_TUNNEL_REQUEST_MAX, "longest frame %zu", size);
    CHECK(netcfg_tunnel_decode(data, size, &t, err, sizeof err) && !strcmp(t.endpoint_host, longest), "longest frame: %s", err);
    size = frame(data, &ref, "wg.collector.invalid");
    // Every truncated prefix is refused; the length byte can never point past the frame.
    for (size_t n = 0; n < size; n++)
        CHECK(!netcfg_tunnel_decode(data, n, &t, err, sizeof err), "prefix %zu accepted", n);
    CHECK(!netcfg_tunnel_decode(data, size + 1, &t, err, sizeof err), "trailing byte accepted");
    uint8_t saved;
    saved = data[0]; data[0] = 'X';
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "bad magic accepted"); data[0] = saved;
    saved = data[4]; data[4] = 1;
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "flag accepted"); data[4] = saved;
    saved = data[114]; data[114] = 0;
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "empty host accepted"); data[114] = saved;
    saved = data[115]; data[115] = ' ';
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "space in host accepted"); data[115] = saved;
    saved = data[115]; data[115] = 0x80;
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "high byte in host accepted"); data[115] = saved;
    saved = data[105]; data[105] = 0;
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "prefix 0 accepted"); data[105] = saved;
    saved = data[105]; data[105] = 33;
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "prefix 33 accepted"); data[105] = saved;
    saved = data[110]; data[110] = 0; data[111] = 0;
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "port 0 accepted");
    data[110] = saved; data[111] = (uint8_t)51820;
    memset(data + 5, 0, 32);
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "zero private key accepted");
    memcpy(data + 5, ref.private_key, 32);
    memcpy(data + 106, data + 101, 4);
    CHECK(!netcfg_tunnel_decode(data, size, &t, err, sizeof err), "collector == address accepted");
    memcpy(data + 106, ref.collector, 4);
    // A zero preshared key is simply "none" and still a valid frame.
    memset(data + 69, 0, 32);
    CHECK(netcfg_tunnel_decode(data, size, &t, err, sizeof err), "no-psk frame rejected: %s", err);
    expect_profile(&t, false);
    CHECK(!netcfg_tunnel_decode(NULL, size, &t, err, sizeof err), "NULL frame accepted");

    // --- validation rule used by netcfg_validate ---
    netcfg_tunnel_t v = ref;
    CHECK(netcfg_tunnel_validate(&v, err, sizeof err), "reference invalid: %s", err);
    v = ref; memset(v.private_key, 0, 32);
    CHECK(!netcfg_tunnel_validate(&v, err, sizeof err) && strstr(err, "key"), "zero private: %s", err);
    v = ref; memset(v.peer_public_key, 0, 32);
    CHECK(!netcfg_tunnel_validate(&v, err, sizeof err) && strstr(err, "peer"), "zero public: %s", err);
    v = ref; v.endpoint_host[0] = '\0';
    CHECK(!netcfg_tunnel_validate(&v, err, sizeof err) && strstr(err, "endpoint"), "empty host: %s", err);
    v = ref; memset(v.endpoint_host, 'h', sizeof v.endpoint_host); // unterminated
    CHECK(!netcfg_tunnel_validate(&v, err, sizeof err), "unterminated host accepted");
    v = ref; v.endpoint_port = 65536;
    CHECK(!netcfg_tunnel_validate(&v, err, sizeof err) && strstr(err, "port"), "port: %s", err);
    v = ref; v.prefix = 0;
    CHECK(!netcfg_tunnel_validate(&v, err, sizeof err) && strstr(err, "address"), "prefix: %s", err);
    v = ref; memset(v.collector, 0, 4);
    CHECK(!netcfg_tunnel_validate(&v, err, sizeof err) && strstr(err, "collector"), "collector: %s", err);
    v = ref; v.keepalive = -1;
    CHECK(!netcfg_tunnel_validate(&v, err, sizeof err) && strstr(err, "keepalive"), "keepalive: %s", err);
    CHECK(!netcfg_tunnel_validate(NULL, err, sizeof err), "NULL accepted");

    if (failures) {
        fprintf(stderr, "netcfg_tunnel_test: %d failure(s)\n", failures);
        return 1;
    }
    printf("netcfg_tunnel_test: OK\n");
    return 0;
}
