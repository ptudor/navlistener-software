// Bounded RAM storage, independent of allocation and task synchronization.
#ifndef SPOOL_RING_H
#define SPOOL_RING_H
#include <stddef.h>
#include <stdint.h>
typedef struct {
    uint64_t seq;
    uint32_t len, offset;
} spool_slot_t;
typedef struct {
    spool_slot_t *slots;
    uint8_t *bytes;
    size_t capacity, byte_capacity, head, count, used, write_offset;
    uint64_t seq, acked, dropped;
} spool_ring_t;
void spool_ring_init(spool_ring_t *r, spool_slot_t *slots, size_t capacity,
                     uint8_t *bytes, size_t byte_capacity);
uint64_t spool_ring_append(spool_ring_t *r, const uint8_t *data, uint32_t len);
void spool_ring_ack(spool_ring_t *r, uint64_t seq);
const spool_slot_t *spool_ring_after(const spool_ring_t *r, uint64_t after, size_t index);
void spool_ring_copy(const spool_ring_t *r, const spool_slot_t *slot, uint8_t *out);
#endif
