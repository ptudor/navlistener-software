#include "spool_ring.h"
#include <string.h>
void spool_ring_init(spool_ring_t *r, spool_slot_t *slots, size_t capacity,
                     uint8_t *bytes, size_t byte_capacity)
{
    *r = (spool_ring_t){.slots = slots, .bytes = bytes, .capacity = capacity,
                        .byte_capacity = byte_capacity};
}
static void remove_oldest(spool_ring_t *r)
{
    r->used -= r->slots[r->head].len;
    r->head = (r->head + 1) % r->capacity;
    r->count--;
}
uint64_t spool_ring_append(spool_ring_t *r, const uint8_t *data, uint32_t len)
{
    if (!data || !len || len > r->byte_capacity || !r->capacity ||
        r->byte_capacity > UINT32_MAX || r->seq == UINT64_MAX) {
        r->dropped++;
        return 0;
    }
    while (r->count == r->capacity || r->byte_capacity - r->used < len) {
        remove_oldest(r);
        r->dropped++;
    }
    spool_slot_t *slot = &r->slots[(r->head + r->count) % r->capacity];
    *slot = (spool_slot_t){.seq = ++r->seq, .len = len, .offset = r->write_offset};
    size_t first = r->byte_capacity - r->write_offset;
    if (first > len) first = len;
    memcpy(r->bytes + r->write_offset, data, first);
    memcpy(r->bytes, data + first, len - first);
    r->write_offset = (r->write_offset + len) % r->byte_capacity;
    r->used += len;
    r->count++;
    return slot->seq;
}
void spool_ring_ack(spool_ring_t *r, uint64_t seq)
{
    if (seq > r->seq) seq = r->seq;
    while (r->count && r->slots[r->head].seq <= seq) remove_oldest(r);
    if (seq > r->acked) r->acked = seq;
}
const spool_slot_t *spool_ring_after(const spool_ring_t *r, uint64_t after, size_t index)
{
    if (!r->count || after >= r->seq) return NULL;
    // Eviction and ACK remove only a prefix; rejected appends assign no sequence.
    // Direct indexing avoids rescanning a large backlog for each transport batch.
    uint64_t first = r->slots[r->head].seq;
    size_t skip = after < first ? 0 : (size_t)(after - first + 1);
    if (skip >= r->count || index >= r->count - skip) return NULL;
    return &r->slots[(r->head + skip + index) % r->capacity];
}
void spool_ring_copy(const spool_ring_t *r, const spool_slot_t *slot, uint8_t *out)
{
    size_t first = r->byte_capacity - slot->offset;
    if (first > slot->len) first = slot->len;
    memcpy(out, r->bytes + slot->offset, first);
    memcpy(out + first, r->bytes, slot->len - first);
}
