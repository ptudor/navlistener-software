// tunnel — WireGuard uplink to the collector (ESP32-S3, NVF_WIREGUARD).
//
// Brings one WireGuard peer up over the observer's uplink and reports whether it
// holds a valid session. The pusher chooses per connection: the collector's address inside
// the tunnel while tunnel_up() holds, the public collector endpoint otherwise. GNF1/TLS
// is unchanged either way — the tunnel only carries it — so the collector still
// authenticates the bearer token and the observer still verifies the collector's
// certificate against its configured name. Nothing but the collector's tunnel address is
// routed through the tunnel; SNTP, OTA downloads and DNS keep using the ordinary uplink.
//
// Builds without NVF_WIREGUARD keep the same API: tunnel_start
// reports ESP_ERR_NOT_SUPPORTED, tunnel_up() is always false and
// tunnel_collector_address() is NULL, so app_main and the pusher need no #ifdefs.

#ifndef TUNNEL_H
#define TUNNEL_H

#include <stdbool.h>

#include "esp_err.h"
#include "netcfg_check.h"

#ifdef __cplusplus
extern "C" {
#endif

// tunnel_start copies the validated profile and spawns the tunnel task. Call once per boot,
// after the network interfaces have been started and before the pusher, so
// tunnel_collector_address() is valid when the pusher is configured. uplink_up reports
// whether any link (Wi-Fi or Ethernet) has an address; the tunnel waits for it, tears down
// when it fails, and follows the default route when the link in use changes. It must be
// safe to call from any task. The first handshake waits for a plausible wall clock.
// ESP_ERR_INVALID_ARG on a disabled or incomplete profile or a NULL uplink_up;
// ESP_ERR_NOT_SUPPORTED on builds without NVF_WIREGUARD.
esp_err_t tunnel_start(const netcfg_tunnel_t *cfg, bool (*uplink_up)(void));

// tunnel_up reports whether the peer currently holds a valid session: a handshake completed
// and has not expired. Cheap and safe from any task; false before tunnel_start.
bool tunnel_up(void);

// tunnel_collector_address returns the collector's dotted IPv4 inside the tunnel, valid for
// the rest of the boot once tunnel_start succeeded, otherwise NULL.
const char *tunnel_collector_address(void);

#ifdef __cplusplus
}
#endif

#endif // TUNNEL_H
