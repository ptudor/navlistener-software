#pragma once
#include <stdbool.h>
#include <stdint.h>
#include "esp_err.h"
#include "nvs.h"

#define JOURNAL_LIFE_CAP 256u
#define JOURNAL_HEALTH_CAP 1024u
#define JOURNAL_RECORD_SIZE 192u
enum { JOURNAL_BOOT=1, JOURNAL_TIME=2, JOURNAL_CONFIRMED=3,
       JOURNAL_OTA_READY=4, JOURNAL_OTA_FAILED=5, JOURNAL_CHECKPOINT=6,
       // Lifecycle lane. HW_TRUST is written when the collector's hardware-trust verdict
       // changes, never per connection; COMMISSION records a bench operation. Their
       // `error` encodings are in docs/JOURNAL.md.
       JOURNAL_HW_TRUST=7, JOURNAL_COMMISSION=8 };
enum { JOURNAL_TIME_UNKNOWN, JOURNAL_TIME_RTC, JOURNAL_TIME_GNSS };
enum { JOURNAL_RECEIVER=1, JOURNAL_FIX=2, JOURNAL_LINK=4, JOURNAL_WIFI=8,
       JOURNAL_PSRAM=16, JOURNAL_SAMPLED=32 };
typedef struct {
    uint64_t sequence, boot, uptime_ms, utc, dropped;
    uint32_t flags, reset_reason, queued, internal_free, error;
    char firmware[32], partition[16];
    uint8_t elf_sha256[32];
    uint8_t event, time_source, environment, rtc, rng, manifest;
    uint8_t timing_flags;
    uint32_t timing_elapsed_s, timing_dropped, timing_hz;
    uint64_t gnss_pulses, rtc_pulses;
    int32_t timing_phase_ticks;
} journal_record_t;
typedef struct { nvs_handle_t handle; uint64_t latest[2]; bool ready; } journal_store_t;
// Separate FIFO lanes: lifecycle events cannot be evicted by hourly checkpoints.
// No erase-on-error, heap queue, retry loop, or mandatory dependency on logging.
esp_err_t journal_store_open(journal_store_t *store);
void journal_store_close(journal_store_t *store);
esp_err_t journal_store_append(journal_store_t *store, journal_record_t *record);
esp_err_t journal_store_read(journal_store_t *store, unsigned lane, uint64_t sequence,
                             journal_record_t *record);
esp_err_t journal_store_page(journal_store_t *store, unsigned lane, uint64_t before,
                             journal_record_t out[8], unsigned *count, uint64_t *next);
