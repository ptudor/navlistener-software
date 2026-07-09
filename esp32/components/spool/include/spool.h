// spool — a bounded, sequence-numbered ring of unacked GNF1 records.
//
// The store-and-forward buffer between the UART producer (ubx -> record) and the TLS pusher.
// Reading and sending are decoupled, so a collector restart or a network blip does not lose
// data: on every reconnect the pusher replays all unacked records, and the collector acks the
// highest sequence it has stored, pruning the ring (navfeeder.c's spool, ported to FreeRTOS).
//
// On overflow the OLDEST record is dropped and counted (a littlefs disk tier — the
// reboot-surviving equivalent of navfeeder's --spool-file — is a later phase; until then the
// ring is the whole buffer). Records are variable length (a nav frame is typically ~40-60 B),
// so each is malloc'd; `cap` bounds the frame count, not the bytes — size it against the C6
// SRAM budget with WiFi+TLS up (see docs/PLAN.md sizing note).

#ifndef SPOOL_H
#define SPOOL_H

#include <stddef.h>
#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

// spool_frame is one collected record: the caller owns `data` and must free() it.
typedef struct {
    uint64_t seq;
    uint32_t len;
    uint8_t *data;
} spool_frame_t;

// spool_init allocates the ring (cap frames). Returns false on OOM. Call once at boot.
bool spool_init(size_t cap);

// spool_append copies a record in, assigns the next sequence, and returns it. On overflow it
// drops (and counts) the oldest record. Thread-safe.
uint64_t spool_append(const uint8_t *data, uint32_t len);

// spool_ack drops every record with seq <= n and advances the ack watermark.
void spool_ack(uint64_t n);

// spool_collect copies up to `max` records with seq > `after` into out (oldest first). The
// caller frees each out[i].data. Returns the count.
size_t spool_collect(uint64_t after, spool_frame_t *out, size_t max);

// spool_acked returns the highest sequence the collector has acked.
uint64_t spool_acked(void);

// spool_stats reads the counters (any pointer may be NULL).
void spool_stats(uint64_t *last_seq, uint64_t *dropped, size_t *count);

#ifdef __cplusplus
}
#endif

#endif // SPOOL_H
