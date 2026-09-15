#include "spool.h"
#include "spool_ring.h"
#include <stdlib.h>
#include "sdkconfig.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
static spool_ring_t g;
static SemaphoreHandle_t mu;
static SemaphoreHandle_t producer_gate;
static bool external;
static bool allocate(size_t frames, size_t bytes, uint32_t caps)
{
    if (!frames || frames > SIZE_MAX / sizeof(spool_slot_t) || !bytes || bytes > UINT32_MAX)
        return false;
    spool_slot_t *slots = heap_caps_malloc(frames * sizeof(*slots), caps);
    uint8_t *arena = heap_caps_malloc(bytes, caps);
    if (!slots || !arena) { free(slots); free(arena); return false; }
    spool_ring_init(&g, slots, frames, arena, bytes);
    return true;
}
bool spool_init(size_t cap)
{
    if (mu) return false;
    mu = xSemaphoreCreateMutex();
    if (!mu) return false;
    producer_gate = xSemaphoreCreateMutex();
    if (!producer_gate) { vSemaphoreDelete(mu); mu = 0; return false; }
#if CONFIG_NVF_SPOOL_PSRAM
    external = allocate(CONFIG_NVF_SPOOL_PSRAM_FRAMES, CONFIG_NVF_SPOOL_PSRAM_BYTES,
                        MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (!external) ESP_LOGW("spool", "PSRAM spool unavailable; using internal RAM budget");
#endif
    if (!external && !allocate(cap, CONFIG_NVF_SPOOL_BYTES, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT)) {
        vSemaphoreDelete(mu);
        vSemaphoreDelete(producer_gate);
        producer_gate = 0;
        mu = 0;
        return false;
    }
    ESP_LOGI("spool", "%s: %u frames, %u payload bytes; volatile across reboot",
             external ? "PSRAM" : "internal RAM", (unsigned)g.capacity, (unsigned)g.byte_capacity);
    return true;
}
uint64_t spool_append(const uint8_t *data, uint32_t len)
{
    xSemaphoreTake(producer_gate, portMAX_DELAY);
    xSemaphoreTake(mu, portMAX_DELAY);
    uint64_t seq = spool_ring_append(&g, data, len);
    xSemaphoreGive(mu);
    xSemaphoreGive(producer_gate);
    return seq;
}
bool spool_pause_producers(uint64_t *final)
{
    if (!producer_gate || !final || xSemaphoreTake(producer_gate, pdMS_TO_TICKS(500)) != pdTRUE) return false;
    spool_stats(final, NULL, NULL);
    return true;
}
void spool_resume_producers(void) { xSemaphoreGive(producer_gate); }
void spool_ack(uint64_t seq)
{
    xSemaphoreTake(mu, portMAX_DELAY);
    spool_ring_ack(&g, seq);
    xSemaphoreGive(mu);
}
size_t spool_collect(uint64_t after, spool_frame_t *out, size_t max)
{
    xSemaphoreTake(mu, portMAX_DELAY);
    size_t n = 0;
    const spool_slot_t *slot;
    while (out && n < max && (slot = spool_ring_after(&g, after, n))) {
        // Transport copies use internal RAM. The persistent backlog stays in
        // its fixed arena; appends never allocate or fragment the heap.
        uint8_t *copy = heap_caps_malloc(slot->len, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
        if (!copy) break;
        spool_ring_copy(&g, slot, copy);
        out[n++] = (spool_frame_t){.seq = slot->seq, .len = slot->len, .data = copy};
    }
    xSemaphoreGive(mu);
    return n;
}
uint64_t spool_acked(void)
{
    xSemaphoreTake(mu, portMAX_DELAY);
    uint64_t seq = g.acked;
    xSemaphoreGive(mu);
    return seq;
}
void spool_stats(uint64_t *last_seq, uint64_t *dropped, size_t *count)
{
    xSemaphoreTake(mu, portMAX_DELAY);
    if (last_seq) *last_seq = g.seq;
    if (dropped) *dropped = g.dropped;
    if (count) *count = g.count;
    xSemaphoreGive(mu);
}
void spool_memory_stats(size_t *used, size_t *capacity, bool *psram)
{
    xSemaphoreTake(mu, portMAX_DELAY);
    if (used) *used = g.used;
    if (capacity) *capacity = g.byte_capacity;
    if (psram) *psram = external;
    xSemaphoreGive(mu);
}
