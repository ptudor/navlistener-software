// spool — a bounded, sequence-numbered ring of unacked GNF1 records.
//
// The store-and-forward buffer between the UART producer (ubx -> record) and the TLS pusher.
// Reading and sending are decoupled, so a collector restart or a network blip does not lose
// data while the unacked backlog fits the ring and this process stays alive: on every
// reconnect the pusher replays all unacked records, and the collector acks the highest
// sequence it has received this connection — not "stored"; the ack makes no durability
// guarantee, replay-on-reconnect is what makes duplicates harmless (regression fix; see
// ../go/internal/wire/wire.go). The ack prunes the ring (navfeeder.c's spool, ported to
// FreeRTOS).
//
// ⚠ DURABILITY ENVELOPE — THIS SPOOL IS RAM-ONLY, BY DECISION. There is no flash
// tier and none is planned for this board. The partition table reserves 1.5 MiB for one
// (`spool`, partitions.csv) and docs/PLAN.md §P-spool records the design, but nothing mounts
// or writes it. This firmware is non-durable across reboots; a flash-backed
// spool remains a planned feature. Two consequences an operator must plan around, not discover:
//
//   1. Outage depth is the ring, and only the ring. On overflow the OLDEST unacked record is
//      dropped and counted. At the 1024-frame default that is roughly 100 s of a multi-GNSS
//      receiver's output — see the arithmetic in ../../../README.md ("Durability envelope").
//   2. A reboot or power cut loses EVERY unacked record, however short the outage. Records
//      live in malloc'd RAM; nothing survives esp_restart(), a brownout, or a watchdog.
//
// The acceptance in (2) is bounded at "the ring, and nothing else" ONLY because of // main.c mints a fresh GNF1 session every boot, so the sequence space this ring
// restarts at 0 is a NEW space at the collector. Before that, a reboot also poisoned the
// frames captured afterwards — they collided with the durable ledger's rows from the previous
// run and were silently dropped as replays. Removing or persisting the per-boot session mint
// therefore voids this whole envelope, it does not merely change the handshake.
//
// The standalone C feeder (../../../feeder/navfeeder.c --spool-file) does have a disk tier,
// so the deployed router-based fleet is unaffected by this. Treat every navfeeder-esp unit as
// loss-tolerant-only by explicit design: fine as an additional observer, not as the sole
// witness of an event you need forensically complete.
//
// Records are variable length (a nav frame is typically ~40-60 B), so each is malloc'd; `cap`
// bounds the frame count, not the bytes — size it against the C6 SRAM budget with WiFi+TLS up
// (see docs/PLAN.md sizing note).

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
