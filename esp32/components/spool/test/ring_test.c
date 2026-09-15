#include "spool_ring.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
typedef struct { uint64_t seq; unsigned len; uint8_t data[300]; } record;
int main(void)
{
    for (unsigned frames = 1; frames < 20; frames += 3) {
        spool_slot_t slots[20]; uint8_t bytes[97]; spool_ring_t ring;
        record model[20]; size_t count = 0, used = 0; uint64_t seq = 0, dropped = 0, ack = 0;
        spool_ring_init(&ring, slots, frames, bytes, sizeof bytes);
        srand(451 + frames);
        for (unsigned step = 0; step < 10000; step++) {
            if (rand() % 4) {
                record next = {.seq = seq + 1, .len = rand() % 120};
                for (unsigned i = 0; i < next.len; i++) next.data[i] = (uint8_t)(step + i);
                uint64_t got = spool_ring_append(&ring, next.data, next.len);
                if (!next.len || next.len > sizeof bytes) { dropped++; assert(got == 0); }
                else {
                    while (count == frames || used + next.len > sizeof bytes) {
                        used -= model[0].len; memmove(model, model+1, (--count) * sizeof *model); dropped++;
                    }
                    model[count++] = next; used += next.len; assert(got == ++seq);
                }
            } else {
                uint64_t n = seq > 5 ? seq - 5 + (unsigned)rand() % 10 : seq + 4;
                spool_ring_ack(&ring, n); if (n > seq) n = seq;
                while (count && model[0].seq <= n) { used -= model[0].len; memmove(model, model+1, (--count) * sizeof *model); }
                if (n > ack) ack = n;
            }
            assert(ring.seq == seq && ring.dropped == dropped && ring.acked == ack);
            assert(ring.count == count && ring.used == used);
            for (size_t i = 0; i < count; i++) {
                const spool_slot_t *s = spool_ring_after(&ring, 0, i); uint8_t out[300];
                assert(s && s->seq == model[i].seq && s->len == model[i].len);
                spool_ring_copy(&ring, s, out); assert(!memcmp(out, model[i].data, s->len));
                assert(spool_ring_after(&ring, s->seq - 1, 0) == s);
            }
            assert(!spool_ring_after(&ring, seq, 0));
            assert(!spool_ring_after(&ring, 0, count));
        }
    }
    puts("spool ring: randomized byte/frame eviction, wrap, ACK and replay passed");
}
