// pusher — the GNF1/TLS push consumer task.
//
// Connects out to the navlistener collector's authenticated push endpoint, does the GNF1
// handshake (HELLO -> WELCOME), then drains the spool: sends DATA frames, applies ACKs
// (pruning the spool), sends PING keepalives, and reconnects forever with backoff — replaying
// all unacked records from the last ack on every reconnect. Mirrors navfeeder.c's
// serve_collector(), but single-task: instead of a reader thread racing a writer on one SSL
// object (why the C feeder pins TLS 1.2), we select() the socket for readable ACKs between
// sends, so exactly one code path touches the TLS connection.

#ifndef PUSHER_H
#define PUSHER_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct {
    const char *host;     // collector hostname
    int port;             // collector [push] port
    const char *token;    // bearer token (NVS-first; Kconfig is the development fallback)
    const char *station;  // observer/station id
    const char *feed;     // "ubx"
    // session is the GNF1 boot/session identity sent in every HELLO, REQUIRED:
    // the collector's replay-dedup key is (observer, session, seq) and it rejects a
    // sessionless HELLO outright. It is minted once per boot by app_main (never operator-
    // supplied, never stored) because this feeder's spool is RAM-only — see main.c and
    // spool.h. Must satisfy gnf1_session_valid().
    const char *session;
    const char *ca_pem;   // PEM CA to verify the collector; NULL => Mozilla bundle
    bool insecure;        // skip TLS verification (dev only)
    // tunnel_host is the collector's address inside the WireGuard tunnel, or NULL when no
    // tunnel is configured, and tunnel_up reports whether the peer session is valid (NULL
    // means never). While it is, connections go to tunnel_host and TLS still verifies
    // `host`; otherwise the public `host` is used, so the tunnel is an upgrade to the
    // uplink and never a single point of failure. A direct session moves onto the tunnel
    // once it comes up (see serve()).
    const char *tunnel_host;
    bool (*tunnel_up)(void);
    void (*update_control)(const uint8_t *,size_t); // authenticated GNF1 control frames
    // evidence, when set, builds the GNF1 EVIDENCE payload for one TLS session
    // (docs/COMMISSIONING.md §6). exported is that session's GNF1_EVIDENCE_EXPORTED_SIZE
    // bytes of keying material, or NULL when it could not be derived; out holds
    // GNF1_EVIDENCE_MAX bytes. It returns the payload length, or 0 to present nothing, and
    // only a nonzero return puts `"evidence":true` in the HELLO. hardware_trust receives the
    // collector's answer from WELCOME whenever evidence was sent: the verdict and, when the
    // evidence was rejected, the reason. Either string is NULL when the collector sent none.
    size_t (*evidence)(const uint8_t *exported, uint8_t *out, size_t cap);
    void (*hardware_trust)(const char *trust, const char *error);
} pusher_cfg_t;

// pusher_start copies cfg and spawns the push task. The spool must already be initialised,
// and host/token/station/session must point to valid strings. Call once per boot; there is no
// stop/reconfigure path. Returns false on allocation failure, an invalid session, or if the
// task could not be created.
bool pusher_start(const pusher_cfg_t *cfg);

// pusher_connected reports whether the push link is currently up (for the status display/LED).
bool pusher_connected(void);

// pusher_via_tunnel reports whether the current push link runs inside the WireGuard tunnel.
// False whenever pusher_connected() is false.
bool pusher_via_tunnel(void);
bool pusher_durable_connected(void);

#ifdef __cplusplus
}
#endif

#endif // PUSHER_H
