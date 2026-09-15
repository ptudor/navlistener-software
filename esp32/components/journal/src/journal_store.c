#include "journal_store.h"
#include <stdio.h>
#include <string.h>

static unsigned capacity(unsigned lane) { return lane ? JOURNAL_HEALTH_CAP : JOURNAL_LIFE_CAP; }
static void key_for(char key[16], unsigned lane, uint64_t sequence)
{ snprintf(key, 16, "%c%04u", lane ? 'h' : 'b', (unsigned)((sequence-1) % capacity(lane))); }
static void put(uint8_t *p, uint64_t v, unsigned n)
{ for (unsigned i=0; i<n; i++) { p[i] = v; v >>= 8; } }
static uint64_t get(const uint8_t *p, unsigned n)
{ uint64_t v=0; for (unsigned i=0; i<n; i++) v |= (uint64_t)p[i] << (8*i); return v; }
static uint32_t crc(const uint8_t *p, unsigned n)
{
    uint32_t v = UINT32_MAX;
    for (unsigned i=0; i<n; i++) {
        v ^= p[i];
        for (unsigned b=0; b<8; b++) v = (v >> 1) ^ (0xedb88320u & (0u-(v&1)));
    }
    return ~v;
}
static void encode(uint8_t b[JOURNAL_RECORD_SIZE], const journal_record_t *r)
{
    memset(b, 0, JOURNAL_RECORD_SIZE); memcpy(b, "NVJ1", 4);
    put(b+8,r->sequence,8); put(b+16,r->boot,8); put(b+24,r->uptime_ms,8);
    put(b+32,r->utc,8); put(b+40,r->dropped,8);
    put(b+48,r->flags,4); put(b+52,r->reset_reason,4); put(b+56,r->queued,4);
    put(b+60,r->internal_free,4); put(b+64,r->error,4);
    memcpy(b+68,r->firmware,32); memcpy(b+100,r->partition,16);
    memcpy(b+116,r->elf_sha256,32);
    b[148]=r->event; b[149]=r->time_source; b[150]=r->environment;
    b[151]=r->rtc; b[152]=r->rng; b[153]=r->manifest;
    put(b+188,crc(b,188),4);
}
static bool decode(journal_record_t *r, const uint8_t b[JOURNAL_RECORD_SIZE])
{
    if (memcmp(b,"NVJ1",4) || get(b+188,4) != crc(b,188) || !get(b+8,8) ||
        b[148] < JOURNAL_BOOT || b[148] > JOURNAL_CHECKPOINT ||
        b[149] > JOURNAL_TIME_GNSS || !memchr(b+68,0,32) || !memchr(b+100,0,16)) return false;
    *r = (journal_record_t){.sequence=get(b+8,8), .boot=get(b+16,8),
        .uptime_ms=get(b+24,8), .utc=get(b+32,8), .dropped=get(b+40,8),
        .flags=get(b+48,4), .reset_reason=get(b+52,4), .queued=get(b+56,4),
        .internal_free=get(b+60,4), .error=get(b+64,4), .event=b[148],
        .time_source=b[149], .environment=b[150], .rtc=b[151], .rng=b[152], .manifest=b[153]};
    memcpy(r->firmware,b+68,32); memcpy(r->partition,b+100,16); memcpy(r->elf_sha256,b+116,32);
    return true;
}
static esp_err_t read_key(journal_store_t *s, const char *key, journal_record_t *r)
{
    uint8_t bytes[JOURNAL_RECORD_SIZE]; size_t n=sizeof bytes;
    esp_err_t err=nvs_get_blob(s->handle,key,bytes,&n);
    if (err != ESP_OK) return err;
    return n == sizeof bytes && decode(r,bytes) ? ESP_OK : ESP_ERR_INVALID_STATE;
}
esp_err_t journal_store_open(journal_store_t *s)
{
    *s=(journal_store_t){0};
    esp_err_t err=nvs_open_from_partition("journal","nvf_journal",NVS_READWRITE,&s->handle);
    if (err != ESP_OK) return err;
    for (unsigned lane=0; lane<2; lane++) for (unsigned slot=0; slot<capacity(lane); slot++) {
        char key[16]; journal_record_t r; key_for(key,lane,slot+1);
        err=read_key(s,key,&r);
        if (err == ESP_ERR_NVS_NOT_FOUND) continue;
        if (err == ESP_OK && ((r.sequence-1)%capacity(lane) != slot ||
            (r.event == JOURNAL_CHECKPOINT) != (lane == 1))) err=ESP_ERR_INVALID_STATE;
        if (err != ESP_OK) { nvs_close(s->handle); return err; }
        if (r.sequence > s->latest[lane]) s->latest[lane]=r.sequence;
    }
    s->ready=true; return ESP_OK;
}
void journal_store_close(journal_store_t *s)
{ if (s->ready) nvs_close(s->handle); s->ready=false; }
esp_err_t journal_store_read(journal_store_t *s, unsigned lane, uint64_t sequence, journal_record_t *r)
{
    if (!s->ready || lane > 1 || !sequence) return ESP_ERR_INVALID_STATE;
    char key[16]; key_for(key,lane,sequence);
    esp_err_t err=read_key(s,key,r);
    return err == ESP_OK && r->sequence != sequence ? ESP_ERR_NVS_NOT_FOUND : err;
}
esp_err_t journal_store_append(journal_store_t *s, journal_record_t *r)
{
    unsigned lane=r->event == JOURNAL_CHECKPOINT;
    if (!s->ready) return ESP_ERR_INVALID_STATE;
    if (s->latest[lane] == UINT64_MAX) { journal_store_close(s); return ESP_ERR_INVALID_STATE; }
    r->sequence=s->latest[lane]+1;
    uint8_t bytes[JOURNAL_RECORD_SIZE]; char key[16];
    encode(bytes,r); key_for(key,lane,r->sequence);
    // Replacing one atomic NVS blob also advances the sequence. There is no
    // separate head pointer that a reset could leave ahead of its record.
    esp_err_t err=nvs_set_blob(s->handle,key,bytes,sizeof bytes);
    if (err == ESP_OK) err=nvs_commit(s->handle);
    if (err == ESP_OK) s->latest[lane]=r->sequence;
    // A failed set/commit may nevertheless persist. Stop writes for this boot;
    // the next open rescans records instead of reusing an uncertain sequence.
    else journal_store_close(s);
    return err;
}
esp_err_t journal_store_page(journal_store_t *s, unsigned lane, uint64_t before,
                             journal_record_t out[8], unsigned *count, uint64_t *next)
{
    *count=0; *next=0;
    if (!s->ready || lane > 1) return ESP_ERR_INVALID_STATE;
    uint64_t seq=before ? before-1 : s->latest[lane];
    if (seq > s->latest[lane]) seq=s->latest[lane];
    unsigned cap=capacity(lane);
    uint64_t first=s->latest[lane] >= cap ? s->latest[lane]-cap+1 : 1;
    for (; seq >= first && *count < 8; seq--) {
        esp_err_t err=journal_store_read(s,lane,seq,&out[*count]);
        if (err == ESP_ERR_NVS_NOT_FOUND) continue;
        if (err != ESP_OK) return err;
        (*count)++;
    }
    if (seq >= first) *next=seq+1;
    return ESP_OK;
}
