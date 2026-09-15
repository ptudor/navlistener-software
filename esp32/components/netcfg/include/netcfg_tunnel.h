// netcfg_tunnel — the optional WireGuard profile: parsing, validation and key encoding.
//
// Pure C (no ESP-IDF, no lwIP), for the same reason as netcfg_check and netcfg_form: the
// profile is untrusted input. It arrives from the browser portal as pasted wg-quick(8)
// text and from the Station app as the compact `nav-tunnel` BLE frame, and both must be
// refused before anything reaches NVS. Everything here is exhaustively host-tested
// (test/netcfg_tunnel_test.c). The firmware-side `tunnel` component consumes only the
// validated netcfg_tunnel_t and never re-parses text.
//
// Scope, deliberately narrow: one [Interface], one [Peer], IPv4 only, and AllowedIPs
// naming exactly one /32 — the collector's address inside the tunnel. That single
// address is the only destination the tunnel carries; SNTP, OTA downloads and the public
// collector endpoint keep using the station's ordinary uplink.

#ifndef NETCFG_TUNNEL_H
#define NETCFG_TUNNEL_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "netcfg_check.h"

#ifdef __cplusplus
extern "C" {
#endif

// NETCFG_TUNNEL_CONF_CAP bounds the pasted profile text, NUL included. A real profile is
// well under 400 bytes; the cap only has to leave room for comments and CRLF endings.
#define NETCFG_TUNNEL_CONF_CAP 1024

// NETCFG_TUNNEL_KEY_B64 is the base64 length of one 32-byte key, NUL excluded.
#define NETCFG_TUNNEL_KEY_B64 44

// NETCFG_TUNNEL_REQUEST_MAX is the largest legal nav-tunnel (NVT1) frame: the fixed
// 115-byte header plus a 63-byte endpoint host. See docs/PROVISIONING.md.
#define NETCFG_TUNNEL_REQUEST_MAX 178

// netcfg_tunnel_validate reports whether an enabled profile is complete: non-zero keys, an
// endpoint host and port, an IPv4 address with a 1-32 prefix, and a collector address that
// differs from it. netcfg_validate calls this whenever cfg->tunnel.enabled is set, so the
// portal, the BLE endpoint and the boot path apply one rule. Reasons stay under 26 chars.
bool netcfg_tunnel_validate(const netcfg_tunnel_t *t, char *err, size_t errcap);

// netcfg_tunnel_parse_conf parses wg-quick(8) profile text into out (zeroed first).
// Accepted: `[Interface]` PrivateKey, Address (one IPv4, optional /prefix) and the wg-quick
// host-side keys, which are ignored (ListenPort, DNS, MTU, Table, FwMark, Pre/PostUp,
// Pre/PostDown, SaveConfig); `[Peer]` PublicKey, PresharedKey, Endpoint (host:port, IPv4
// or DNS), AllowedIPs (exactly one /32: the collector) and PersistentKeepalive (seconds
// or "off"). `#` starts a comment anywhere on a line; CRLF is tolerated. Any unknown key,
// duplicate key, second section or IPv6 literal is refused. On success out->enabled is set
// and the result already passed netcfg_tunnel_validate.
bool netcfg_tunnel_parse_conf(const char *text, netcfg_tunnel_t *out, char *err, size_t errcap);

// netcfg_tunnel_decode parses the nav-tunnel v1 binary request (NVT1) into out (zeroed
// first) and validates it. Truncated, oversized, non-printable and unknown-flag frames are
// refused; the frame must end exactly after the endpoint host.
bool netcfg_tunnel_decode(const uint8_t *data, size_t size, netcfg_tunnel_t *out,
                          char *err, size_t errcap);

// netcfg_tunnel_key_decode decodes one canonical base64 key (44 chars, '=' padded, zero
// trailing bits) into 32 bytes. Non-canonical encodings are refused, as wg(8) does.
bool netcfg_tunnel_key_decode(const char *text, size_t len, uint8_t key[NETCFG_TUNNEL_KEY_LEN]);

// netcfg_tunnel_key_encode writes the canonical 44-char base64 form plus NUL.
void netcfg_tunnel_key_encode(const uint8_t key[NETCFG_TUNNEL_KEY_LEN],
                              char out[NETCFG_TUNNEL_KEY_B64 + 1]);

// netcfg_tunnel_ip4_parse parses a strict dotted quad (no leading zeros, no whitespace).
bool netcfg_tunnel_ip4_parse(const char *text, size_t len, uint8_t ip[4]);

// netcfg_tunnel_ip4_format writes the dotted quad plus NUL (at most 16 bytes).
void netcfg_tunnel_ip4_format(const uint8_t ip[4], char out[16]);

// netcfg_tunnel_prefix_mask writes the dotted netmask for a 0-32 prefix plus NUL.
void netcfg_tunnel_prefix_mask(int prefix, char out[16]);

#ifdef __cplusplus
}
#endif

#endif // NETCFG_TUNNEL_H
