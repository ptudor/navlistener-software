#include "journal.h"
#include "journal_policy.h"
#include "sdkconfig.h"
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
#include <stdio.h>
#include <string.h>
#include "esp_app_desc.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_ota_ops.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "nvs_flash.h"
#include "rtc_policy.h"
#include "spool.h"
#include "pusher.h"

static const char *TAG="journal";
static journal_store_t store;
static SemaphoreHandle_t mutex;
static journal_record_t current;
static journal_policy_t policy;
static rtc_candidate_t candidate;
static uint8_t best_time;

static void log_record(const char *label, const journal_record_t *r)
{
    ESP_LOGI(TAG, "%s event=%u seq=%llu boot=%llu uptime=%llus UTC=%llu source=%u version=%s reset=%lu flags=0x%02lx drop=%llu",
        label,r->event,(unsigned long long)r->sequence,(unsigned long long)r->boot,
        (unsigned long long)(r->uptime_ms/1000),(unsigned long long)r->utc,r->time_source,
        r->firmware,(unsigned long)r->reset_reason,(unsigned long)r->flags,(unsigned long long)r->dropped);
}
static bool append(journal_record_t *r)
{
    if (!store.ready) return false;
    esp_err_t err=journal_store_append(&store,r);
    if (err != ESP_OK) ESP_LOGE(TAG,"storage disabled for this boot: %s; GNSS continues",esp_err_to_name(err));
    else log_record("saved",r);
    return err == ESP_OK;
}
void journal_start(void)
{
    mutex=xSemaphoreCreateMutex();
    if (!mutex) { ESP_LOGE(TAG,"mutex unavailable; GNSS continues without journal"); return; }
    // This is a dedicated partition. Never erase configuration or historical
    // evidence automatically if NVS cannot recover a damaged/full partition.
    esp_err_t err=nvs_flash_init_partition("journal");
    if (err == ESP_OK) err=journal_store_open(&store);
    if (err != ESP_OK) {
        ESP_LOGW(TAG,"unavailable: %s; GNSS continues (older layouts need USB partition-table update)",esp_err_to_name(err));
        return;
    }
    for (unsigned lane=0; lane<2; lane++) if (store.latest[lane]) {
        journal_record_t previous;
        if (journal_store_read(&store,lane,store.latest[lane],&previous) == ESP_OK)
            log_record(lane ? "previous checkpoint" : "previous lifecycle",&previous);
    }
    const esp_app_desc_t *app=esp_app_get_description();
    const esp_partition_t *part=esp_ota_get_running_partition();
    current=(journal_record_t){.boot=store.latest[0]+1,.event=JOURNAL_BOOT,
        .uptime_ms=esp_timer_get_time()/1000,.reset_reason=esp_reset_reason()};
    snprintf(current.firmware,sizeof current.firmware,"%.*s",31,app->version);
    snprintf(current.partition,sizeof current.partition,"%.15s",part ? part->label : "unknown");
    memcpy(current.elf_sha256,app->app_elf_sha256,32);
    append(&current);
}
void journal_event(uint8_t event, int32_t error)
{
    if (!mutex || xSemaphoreTake(mutex,pdMS_TO_TICKS(100)) != pdTRUE) return;
    journal_record_t r=current;
    r.event=event; r.error=(uint32_t)error;
    uint64_t now=esp_timer_get_time()/1000;
    // Lifecycle calls can occur before the first board sample or after a task
    // fault. Do not attach stale health or extrapolate an old wall clock.
    uint64_t age=now >= r.uptime_ms ? now-r.uptime_ms : UINT64_MAX;
    if (age > 30000) { r.flags=0; r.environment=0; r.rtc=0; }
    if (age > (r.time_source == JOURNAL_TIME_GNSS ? 2000u : 30000u)) {
        r.time_source=JOURNAL_TIME_UNKNOWN; r.utc=0;
    } else if (r.time_source) r.utc+=age/1000;
    r.uptime_ms=now;
    append(&r);
    xSemaphoreGive(mutex);
}
void journal_poll(const gnss_status_t *g, const observer_report_t *report, uint64_t now)
{
    if (!mutex || xSemaphoreTake(mutex,0) != pdTRUE) return;
    if (!store.ready) { xSemaphoreGive(mutex); return; }
    current.uptime_ms=now; current.utc=0; current.time_source=JOURNAL_TIME_UNKNOWN;
    int64_t epoch;
    (void)rtc_gnss_candidate(&candidate,g,(int64_t)now,&epoch);
    if (candidate.samples >= 3 && now >= (uint64_t)candidate.last_sample_ms &&
        now-(uint64_t)candidate.last_sample_ms <= 2000) {
        current.utc=(candidate.last_utc_ns+(now-candidate.last_sample_ms)*1000000)/1000000000;
        current.time_source=JOURNAL_TIME_GNSS;
    } else if ((report->rtc.flags & 19) == 19 && now >= report->rtc.sampled_ms &&
               now-report->rtc.sampled_ms <= 30000) {
        current.utc=report->rtc.epoch+(now-report->rtc.sampled_ms)/1000;
        current.time_source=JOURNAL_TIME_RTC;
    }
    current.flags=JOURNAL_SAMPLED;
    if (g->satellites_valid && now >= (uint64_t)g->satellites_ms && now-g->satellites_ms <= 15000)
        current.flags |= JOURNAL_RECEIVER;
    if (gnss_status_position_fresh(g,(int64_t)now)) current.flags |= JOURNAL_FIX;
    if (pusher_connected()) current.flags |= JOURNAL_LINK;
    wifi_ap_record_t ap;
    if (esp_wifi_sta_get_ap_info(&ap) == ESP_OK) current.flags |= JOURNAL_WIFI;
    size_t queued; bool psram;
    spool_stats(NULL,&current.dropped,&queued); current.queued=queued;
    spool_memory_stats(NULL,NULL,&psram);
    if (psram) current.flags |= JOURNAL_PSRAM;
    current.internal_free=heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    current.environment=now >= report->uptime_ms && now-report->uptime_ms <= 60000 ? report->environment.valid : 0;
    current.rtc=report->rtc.flags; current.rng=report->crypto.rng; current.manifest=report->manifest.action;
    current.error=0;
    if (current.time_source > best_time) {
        current.event=JOURNAL_TIME;
        if (append(&current)) best_time=current.time_source;
    }
    if (journal_checkpoint_due(&policy,now)) {
        current.event=JOURNAL_CHECKPOINT;
        if (append(&current)) journal_checkpoint_saved(&policy,now);
    }
    xSemaphoreGive(mutex);
}
esp_err_t journal_page(unsigned lane, uint64_t before, journal_record_t out[8],
                        unsigned *count, uint64_t *next)
{
    *count=0; *next=0;
    if (lane > 1 || !mutex || xSemaphoreTake(mutex,pdMS_TO_TICKS(100)) != pdTRUE)
        return ESP_ERR_INVALID_STATE;
    esp_err_t err=journal_store_page(&store,lane,before,out,count,next);
    xSemaphoreGive(mutex); return err;
}
#else
void journal_start(void) {}
void journal_poll(const gnss_status_t *g, const observer_report_t *r, uint64_t now)
{ (void)g; (void)r; (void)now; }
void journal_event(uint8_t event, int32_t error) { (void)event; (void)error; }
esp_err_t journal_page(unsigned lane, uint64_t before, journal_record_t out[8], unsigned *count, uint64_t *next)
{ (void)lane; (void)before; (void)out; *count=0; *next=0; return ESP_ERR_NOT_SUPPORTED; }
#endif
