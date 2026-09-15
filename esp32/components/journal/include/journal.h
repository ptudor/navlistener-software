#pragma once
#include "journal_store.h"
#include "gnss_status.h"
#include "observer_report.h"
// Call first in app_main, before default NVS and peripheral initialization.
void journal_start(void);
// Single board-task caller; UTC is GNSS-qualified or a labeled running RTC.
void journal_poll(const gnss_status_t *gnss, const observer_report_t *report, uint64_t now_ms);
// Thread-safe best-effort lifecycle write. Never aborts normal firmware work.
void journal_event(uint8_t event, int32_t error);
// Paged diagnostic read; lane 0 lifecycle, 1 checkpoints. before=0 starts newest.
// Returns <=8 records newest-first and a next cursor (0 at end).
esp_err_t journal_page(unsigned lane, uint64_t before, journal_record_t out[8],
                        unsigned *count, uint64_t *next);
