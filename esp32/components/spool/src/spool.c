// spool — bounded sequence-numbered record ring. See include/spool.h.
// Ported from ../../../feeder/navfeeder.c's spool (pthread mutex -> FreeRTOS mutex).

#include "spool.h"

#include <inttypes.h>
#include <stdlib.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "esp_log.h"

static const char *TAG = "spool";

typedef struct {
    uint64_t seq;
    uint32_t len;
    uint8_t *data;
} slot_t;

static struct {
    slot_t *ring;
    size_t cap, head, count;
    uint64_t seq;     // last assigned sequence
    uint64_t acked;   // last sequence acked by the collector
    uint64_t dropped; // records lost to overflow
    SemaphoreHandle_t mu;
} g;

static inline void lock(void)   { xSemaphoreTake(g.mu, portMAX_DELAY); }
static inline void unlock(void) { xSemaphoreGive(g.mu); }

bool spool_init(size_t cap)
{
    if (cap < 1) cap = 1;
    g.ring = calloc(cap, sizeof *g.ring);
    if (!g.ring) return false;
    g.cap = cap;
    g.head = g.count = 0;
    g.seq = g.acked = g.dropped = 0;
    g.mu = xSemaphoreCreateMutex();
    if (!g.mu) { free(g.ring); g.ring = NULL; return false; }
    return true;
}

uint64_t spool_append(const uint8_t *data, uint32_t len)
{
    uint8_t *copy = malloc(len);
    if (!copy) { // out of memory: drop this record rather than crash the producer
        lock(); g.dropped++; unlock();
        ESP_LOGW(TAG, "append OOM (%" PRIu32 " B), dropped", len);
        return 0;
    }
    memcpy(copy, data, len);

    lock();
    if (g.count == g.cap) { // overflow: evict the oldest
        free(g.ring[g.head].data);
        g.head = (g.head + 1) % g.cap;
        g.count--;
        g.dropped++;
    }
    uint64_t seq = ++g.seq;
    size_t idx = (g.head + g.count) % g.cap;
    g.ring[idx].seq = seq;
    g.ring[idx].len = len;
    g.ring[idx].data = copy;
    g.count++;
    unlock();
    return seq;
}

void spool_ack(uint64_t n)
{
    lock();
    while (g.count > 0 && g.ring[g.head].seq <= n) {
        free(g.ring[g.head].data);
        g.ring[g.head].data = NULL;
        g.head = (g.head + 1) % g.cap;
        g.count--;
    }
    if (n > g.acked) g.acked = n;
    unlock();
}

size_t spool_collect(uint64_t after, spool_frame_t *out, size_t max)
{
    lock();
    size_t n = 0;
    for (size_t i = 0; i < g.count && n < max; i++) {
        slot_t *f = &g.ring[(g.head + i) % g.cap];
        if (f->seq <= after) continue;
        uint8_t *copy = malloc(f->len);
        if (!copy) break; // caller sends what we have; the rest replays next round
        memcpy(copy, f->data, f->len);
        out[n].seq = f->seq;
        out[n].len = f->len;
        out[n].data = copy;
        n++;
    }
    unlock();
    return n;
}

uint64_t spool_acked(void)
{
    lock();
    uint64_t a = g.acked;
    unlock();
    return a;
}

void spool_stats(uint64_t *last_seq, uint64_t *dropped, size_t *count)
{
    lock();
    if (last_seq) *last_seq = g.seq;
    if (dropped)  *dropped = g.dropped;
    if (count)    *count = g.count;
    unlock();
}
