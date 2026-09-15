// netcfg_tunnel — see include/netcfg_tunnel.h. Pure C: no ESP-IDF, host-testable
// (test/netcfg_tunnel_test.c).

#include "netcfg_tunnel.h"

#include <stdio.h>
#include <string.h>

// --- shared helpers -----------------------------------------------------------------------

static bool fail(char *err, size_t errcap, const char *msg)
{
    if (err && errcap) snprintf(err, errcap, "%s", msg);
    return false;
}

static bool all_zero(const uint8_t *p, size_t n)
{
    uint8_t acc = 0;
    for (size_t i = 0; i < n; i++) acc |= p[i];
    return acc == 0;
}

static bool host_char(char c)
{
    return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
           c == '-' || c == '.';
}

// host_ok accepts a DNS name or IPv4 literal: 1-63 bytes of [A-Za-z0-9.-], no leading or
// trailing dot. That is the whole character set the endpoint resolver ever needs, so the
// stored value can never carry a delimiter into a log line or a later text format.
static bool host_ok(const char *s, size_t len)
{
    if (len == 0 || len > 63 || s[0] == '.' || s[len - 1] == '.') return false;
    for (size_t i = 0; i < len; i++)
        if (!host_char(s[i])) return false;
    return true;
}

static bool parse_uint(const char *s, size_t len, unsigned max, unsigned *out)
{
    if (len == 0 || len > 5) return false;
    unsigned v = 0;
    for (size_t i = 0; i < len; i++) {
        if (s[i] < '0' || s[i] > '9') return false;
        if (i > 0 && v == 0) return false; // leading zero
        v = v * 10 + (unsigned)(s[i] - '0');
        if (v > max) return false;
    }
    *out = v;
    return true;
}

// --- base64 (RFC 4648, canonical) --------------------------------------------------------

static const char B64[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

static int b64_value(char c)
{
    if (c >= 'A' && c <= 'Z') return c - 'A';
    if (c >= 'a' && c <= 'z') return c - 'a' + 26;
    if (c >= '0' && c <= '9') return c - '0' + 52;
    if (c == '+') return 62;
    if (c == '/') return 63;
    return -1;
}

bool netcfg_tunnel_key_decode(const char *text, size_t len, uint8_t key[NETCFG_TUNNEL_KEY_LEN])
{
    if (!text || !key || len != NETCFG_TUNNEL_KEY_B64 || text[NETCFG_TUNNEL_KEY_B64 - 1] != '=')
        return false;
    uint32_t acc = 0;
    int bits = 0;
    size_t o = 0;
    for (size_t i = 0; i < NETCFG_TUNNEL_KEY_B64 - 1; i++) {
        int v = b64_value(text[i]);
        if (v < 0) return false;
        acc = (acc << 6) | (uint32_t)v;
        bits += 6;
        if (bits >= 8) {
            bits -= 8;
            if (o < NETCFG_TUNNEL_KEY_LEN) key[o++] = (uint8_t)(acc >> bits);
            acc &= (1u << bits) - 1u;
        }
    }
    // 43 sextets carry 258 bits; the two past the key must be zero or the encoding is not
    // canonical (a second spelling of the same key), which wg(8) also refuses.
    return o == NETCFG_TUNNEL_KEY_LEN && bits == 2 && acc == 0;
}

void netcfg_tunnel_key_encode(const uint8_t key[NETCFG_TUNNEL_KEY_LEN],
                              char out[NETCFG_TUNNEL_KEY_B64 + 1])
{
    size_t o = 0;
    for (size_t i = 0; i + 3 <= NETCFG_TUNNEL_KEY_LEN; i += 3) {
        uint32_t v = (uint32_t)key[i] << 16 | (uint32_t)key[i + 1] << 8 | key[i + 2];
        out[o++] = B64[(v >> 18) & 63];
        out[o++] = B64[(v >> 12) & 63];
        out[o++] = B64[(v >> 6) & 63];
        out[o++] = B64[v & 63];
    }
    // 30 bytes done; the last two bytes become three characters and one pad.
    uint32_t v = (uint32_t)key[30] << 16 | (uint32_t)key[31] << 8;
    out[o++] = B64[(v >> 18) & 63];
    out[o++] = B64[(v >> 12) & 63];
    out[o++] = B64[(v >> 6) & 63];
    out[o++] = '=';
    out[o] = '\0';
}

// --- IPv4 ---------------------------------------------------------------------------------

bool netcfg_tunnel_ip4_parse(const char *s, size_t len, uint8_t ip[4])
{
    size_t i = 0;
    for (int octet = 0; octet < 4; octet++) {
        unsigned v = 0;
        size_t digits = 0;
        while (i < len && s[i] >= '0' && s[i] <= '9') {
            if (digits > 0 && v == 0) return false; // "01": leading zero (octal ambiguity)
            if (++digits > 3) return false;
            v = v * 10 + (unsigned)(s[i] - '0');
            if (v > 255) return false;
            i++;
        }
        if (digits == 0) return false;
        ip[octet] = (uint8_t)v;
        if (octet < 3) {
            if (i >= len || s[i] != '.') return false;
            i++;
        }
    }
    return i == len;
}

void netcfg_tunnel_ip4_format(const uint8_t ip[4], char out[16])
{
    snprintf(out, 16, "%u.%u.%u.%u", ip[0], ip[1], ip[2], ip[3]);
}

void netcfg_tunnel_prefix_mask(int prefix, char out[16])
{
    uint32_t mask = prefix <= 0 ? 0u : prefix >= 32 ? 0xffffffffu : ~((1u << (32 - prefix)) - 1u);
    uint8_t ip[4] = { (uint8_t)(mask >> 24), (uint8_t)(mask >> 16), (uint8_t)(mask >> 8), (uint8_t)mask };
    netcfg_tunnel_ip4_format(ip, out);
}

// parse_cidr reads "a.b.c.d" or "a.b.c.d/n". A missing prefix yields default_prefix.
static bool parse_cidr(const char *s, size_t len, uint8_t ip[4], int *prefix, int default_prefix)
{
    const char *slash = memchr(s, '/', len);
    size_t iplen = slash ? (size_t)(slash - s) : len;
    if (!netcfg_tunnel_ip4_parse(s, iplen, ip)) return false;
    if (!slash) {
        *prefix = default_prefix;
        return true;
    }
    unsigned n;
    if (!parse_uint(slash + 1, len - iplen - 1, 32, &n)) return false;
    *prefix = (int)n;
    return true;
}

// --- validation ---------------------------------------------------------------------------

bool netcfg_tunnel_validate(const netcfg_tunnel_t *t, char *err, size_t errcap)
{
    if (!t) return fail(err, errcap, "no tunnel profile");
    if (all_zero(t->private_key, sizeof t->private_key)) return fail(err, errcap, "tunnel key is empty");
    if (all_zero(t->peer_public_key, sizeof t->peer_public_key))
        return fail(err, errcap, "tunnel peer key is empty");
    size_t hostlen = strnlen(t->endpoint_host, sizeof t->endpoint_host);
    if (hostlen >= sizeof t->endpoint_host || !host_ok(t->endpoint_host, hostlen))
        return fail(err, errcap, "tunnel endpoint invalid");
    if (t->endpoint_port < 1 || t->endpoint_port > 65535)
        return fail(err, errcap, "tunnel port out of range");
    if (all_zero(t->address, 4) || t->prefix < 1 || t->prefix > 32)
        return fail(err, errcap, "tunnel address invalid");
    if (all_zero(t->collector, 4) || memcmp(t->collector, t->address, 4) == 0)
        return fail(err, errcap, "tunnel collector invalid");
    if (t->keepalive < 0 || t->keepalive > 65535) return fail(err, errcap, "tunnel keepalive invalid");
    fail(err, errcap, "");
    return true;
}

// --- wg-quick(8) profile text -------------------------------------------------------------

static bool space(char c) { return c == ' ' || c == '\t' || c == '\r'; }

static void trim(const char **b, const char **e)
{
    while (*b < *e && space(**b)) (*b)++;
    while (*e > *b && space((*e)[-1])) (*e)--;
}

// ieq compares a range with a literal, ASCII case-insensitively (wg(8) does the same for
// section names and keys).
static bool ieq(const char *b, const char *e, const char *lit)
{
    size_t n = strlen(lit);
    if ((size_t)(e - b) != n) return false;
    for (size_t i = 0; i < n; i++) {
        char c = b[i], l = lit[i];
        if (c >= 'A' && c <= 'Z') c = (char)(c - 'A' + 'a');
        if (l >= 'A' && l <= 'Z') l = (char)(l - 'A' + 'a');
        if (c != l) return false;
    }
    return true;
}

static bool one_of(const char *b, const char *e, const char *const *lits, size_t n)
{
    for (size_t i = 0; i < n; i++)
        if (ieq(b, e, lits[i])) return true;
    return false;
}

static bool parse_endpoint(const char *b, const char *e, netcfg_tunnel_t *out)
{
    const char *colon = NULL;
    for (const char *p = b; p < e; p++)
        if (*p == ':') colon = p;
    if (!colon) return false;
    size_t hostlen = (size_t)(colon - b);
    // A second ':' means an IPv6 literal (bracketed or not); the port cannot follow it.
    if (memchr(b, ':', hostlen) || !host_ok(b, hostlen)) return false;
    unsigned port;
    if (!parse_uint(colon + 1, (size_t)(e - colon - 1), 65535, &port) || port == 0) return false;
    memcpy(out->endpoint_host, b, hostlen);
    out->endpoint_host[hostlen] = '\0';
    out->endpoint_port = (int)port;
    return true;
}

typedef enum { SEC_NONE, SEC_INTERFACE, SEC_PEER } section_t;

static bool parse_conf(const char *text, netcfg_tunnel_t *out, char *err, size_t errcap)
{
    if (!text) return fail(err, errcap, "profile missing");
    size_t total = strnlen(text, NETCFG_TUNNEL_CONF_CAP);
    if (total >= NETCFG_TUNNEL_CONF_CAP) return fail(err, errcap, "profile too long");

    static const char *const ignored_interface[] = {
        "ListenPort", "DNS", "MTU", "Table", "FwMark", "PreUp", "PostUp", "PreDown",
        "PostDown", "SaveConfig",
    };
    section_t sec = SEC_NONE;
    int interfaces = 0, peers = 0;
    bool have_private = false, have_address = false, have_public = false, have_psk = false;
    bool have_endpoint = false, have_allowed = false, have_keepalive = false;

    const char *p = text, *end = text + total;
    while (p < end) {
        const char *nl = memchr(p, '\n', (size_t)(end - p));
        const char *next = nl ? nl + 1 : end;
        const char *line_end = nl ? nl : end;
        const char *hash = memchr(p, '#', (size_t)(line_end - p));
        if (hash) line_end = hash;
        const char *b = p, *e = line_end;
        trim(&b, &e);
        p = next;
        if (b == e) continue;

        if (*b == '[') {
            if (e[-1] != ']') return fail(err, errcap, "malformed section header");
            if (ieq(b + 1, e - 1, "Interface")) {
                if (interfaces++) return fail(err, errcap, "only one [Interface] section");
                sec = SEC_INTERFACE;
            } else if (ieq(b + 1, e - 1, "Peer")) {
                if (peers++) return fail(err, errcap, "only one [Peer] section");
                sec = SEC_PEER;
            } else {
                return fail(err, errcap, "unknown section in profile");
            }
            continue;
        }

        const char *eq = memchr(b, '=', (size_t)(e - b));
        if (!eq) return fail(err, errcap, "expected Key = Value");
        const char *kb = b, *ke = eq, *vb = eq + 1, *ve = e;
        trim(&kb, &ke);
        trim(&vb, &ve);
        if (kb == ke || vb == ve) return fail(err, errcap, "expected Key = Value");

        if (sec == SEC_INTERFACE) {
            if (ieq(kb, ke, "PrivateKey")) {
                if (have_private) return fail(err, errcap, "duplicate key in profile");
                if (!netcfg_tunnel_key_decode(vb, (size_t)(ve - vb), out->private_key))
                    return fail(err, errcap, "PrivateKey is not a valid key");
                have_private = true;
            } else if (ieq(kb, ke, "Address")) {
                // wg-quick allows a list and IPv6 here; this tunnel carries one IPv4 host.
                if (have_address || memchr(vb, ',', (size_t)(ve - vb)) ||
                    !parse_cidr(vb, (size_t)(ve - vb), out->address, &out->prefix, 32))
                    return fail(err, errcap, "Address must be one IPv4/prefix");
                have_address = true;
            } else if (one_of(kb, ke, ignored_interface,
                              sizeof ignored_interface / sizeof ignored_interface[0])) {
                // Host-side wg-quick settings: harmless in a pasted profile, not applicable here.
            } else {
                return fail(err, errcap, "unknown [Interface] key");
            }
        } else if (sec == SEC_PEER) {
            if (ieq(kb, ke, "PublicKey")) {
                if (have_public) return fail(err, errcap, "duplicate key in profile");
                if (!netcfg_tunnel_key_decode(vb, (size_t)(ve - vb), out->peer_public_key))
                    return fail(err, errcap, "PublicKey is not a valid key");
                have_public = true;
            } else if (ieq(kb, ke, "PresharedKey")) {
                if (have_psk) return fail(err, errcap, "duplicate key in profile");
                if (!netcfg_tunnel_key_decode(vb, (size_t)(ve - vb), out->preshared_key))
                    return fail(err, errcap, "PresharedKey is not a valid key");
                have_psk = true;
            } else if (ieq(kb, ke, "Endpoint")) {
                if (have_endpoint) return fail(err, errcap, "duplicate key in profile");
                if (!parse_endpoint(vb, ve, out))
                    return fail(err, errcap, "Endpoint must be host:port (IPv4/DNS)");
                have_endpoint = true;
            } else if (ieq(kb, ke, "AllowedIPs")) {
                int prefix;
                if (have_allowed || memchr(vb, ',', (size_t)(ve - vb)) ||
                    !parse_cidr(vb, (size_t)(ve - vb), out->collector, &prefix, 32) || prefix != 32)
                    return fail(err, errcap, "AllowedIPs must be one /32 (collector)");
                have_allowed = true;
            } else if (ieq(kb, ke, "PersistentKeepalive")) {
                unsigned seconds = 0;
                if (have_keepalive ||
                    (!ieq(vb, ve, "off") && !parse_uint(vb, (size_t)(ve - vb), 65535, &seconds)))
                    return fail(err, errcap, "PersistentKeepalive must be 0-65535");
                out->keepalive = (int)seconds;
                have_keepalive = true;
            } else {
                return fail(err, errcap, "unknown [Peer] key");
            }
        } else {
            return fail(err, errcap, "key outside [Interface]/[Peer]");
        }
    }

    if (!interfaces) return fail(err, errcap, "[Interface] section missing");
    if (!peers) return fail(err, errcap, "[Peer] section missing");
    if (!have_private) return fail(err, errcap, "PrivateKey missing");
    if (!have_address) return fail(err, errcap, "Address missing");
    if (!have_public) return fail(err, errcap, "PublicKey missing");
    if (!have_endpoint) return fail(err, errcap, "Endpoint missing");
    if (!have_allowed) return fail(err, errcap, "AllowedIPs missing");
    out->enabled = true;
    return netcfg_tunnel_validate(out, err, errcap);
}

bool netcfg_tunnel_parse_conf(const char *text, netcfg_tunnel_t *out, char *err, size_t errcap)
{
    if (!out) return fail(err, errcap, "no tunnel profile");
    memset(out, 0, sizeof *out);
    // A refused profile leaves nothing behind: a caller that seeds the record from the
    // current config and then rejects the POST must not carry a half-decoded key onward.
    if (parse_conf(text, out, err, errcap)) return true;
    memset(out, 0, sizeof *out);
    return false;
}

// --- nav-tunnel v1 binary request (BLE) ---------------------------------------------------

#define NVT1_HEADER 115

static bool decode_frame(const uint8_t *data, size_t size, netcfg_tunnel_t *out,
                         char *err, size_t errcap)
{
    if (!data) return fail(err, errcap, "missing request");
    if (size < NVT1_HEADER || memcmp(data, "NVT1", 4)) return fail(err, errcap, "bad request header");
    if (data[4] != 0) return fail(err, errcap, "unsupported flags");

    const size_t hostlen = data[114];
    if (!hostlen || hostlen >= sizeof out->endpoint_host) return fail(err, errcap, "invalid field length");
    if (size != NVT1_HEADER + hostlen) return fail(err, errcap, "request length mismatch");
    const char *host = (const char *)data + NVT1_HEADER;
    if (!host_ok(host, hostlen)) return fail(err, errcap, "endpoint host is invalid");

    memcpy(out->private_key, data + 5, NETCFG_TUNNEL_KEY_LEN);
    memcpy(out->peer_public_key, data + 37, NETCFG_TUNNEL_KEY_LEN);
    memcpy(out->preshared_key, data + 69, NETCFG_TUNNEL_KEY_LEN);
    memcpy(out->address, data + 101, 4);
    out->prefix = data[105];
    memcpy(out->collector, data + 106, 4);
    out->endpoint_port = (int)((unsigned)data[110] << 8 | data[111]);
    out->keepalive = (int)((unsigned)data[112] << 8 | data[113]);
    memcpy(out->endpoint_host, host, hostlen);
    out->enabled = true;
    return netcfg_tunnel_validate(out, err, errcap);
}

bool netcfg_tunnel_decode(const uint8_t *data, size_t size, netcfg_tunnel_t *out,
                          char *err, size_t errcap)
{
    if (!out) return fail(err, errcap, "no tunnel profile");
    memset(out, 0, sizeof *out);
    if (decode_frame(data, size, out, err, errcap)) return true;
    memset(out, 0, sizeof *out);
    return false;
}
