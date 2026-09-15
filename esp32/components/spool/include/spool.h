// Bounded RAM spool between the UBX producer and GNF1 TLS pusher.
// Metadata and variable-length records occupy fixed arenas: PSRAM when enabled
// and available, otherwise internal RAM. Reaching either the byte or record
// limit evicts the oldest unacked records. ACK removes a prefix; reconnect
// replays the retained suffix in order. Collector durability depends on its
// historian configuration, as specified in docs/DESIGN.md.
// All records are lost on ANY reboot, including OTA. Never persist the GNF1
// session across boots. The flash spool partition remains unused.
// Task context only: PSRAM cannot be accessed by a cache-disabled ISR.

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

// Allocate storage once at boot; cap is the internal-RAM fallback record limit.
// PSRAM uses its configured frame/byte limits. Returns false on allocation failure.
bool spool_init(size_t cap);

// spool_append copies a record in, assigns the next sequence, and returns it. On overflow it
// drops (and counts) the oldest records. Invalid/oversized records return 0 and
// increment dropped without assigning a sequence. Thread-safe.
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

void spool_memory_stats(size_t *used, size_t *capacity, bool *psram);

#ifdef __cplusplus
}
#endif

#endif // SPOOL_H
