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

#ifdef __cplusplus
extern "C" {
#endif

typedef struct {
    const char *host;     // collector hostname
    int port;             // collector [push] port
    const char *token;    // bearer token (NVS-first; Kconfig is the development fallback)
    const char *station;  // observer/station id
    const char *feed;     // "ubx"
    const char *ca_pem;   // PEM CA to verify the collector; NULL => Mozilla bundle
    bool insecure;        // skip TLS verification (dev only)
} pusher_cfg_t;

// pusher_start copies cfg and spawns the push task. The spool must already be initialised.
// Returns false if the task could not be created.
bool pusher_start(const pusher_cfg_t *cfg);

// pusher_connected reports whether the push link is currently up (for the status display/LED).
bool pusher_connected(void);

#ifdef __cplusplus
}
#endif

#endif // PUSHER_H
