#include "journal_store.h"
#include "journal_policy.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

static struct { bool present; uint8_t bytes[JOURNAL_RECORD_SIZE]; } slots[2][JOURNAL_HEALTH_CAP];
static unsigned sets, commits;
// NVS provides atomic blobs. Model an error either before or after the blob
// becomes durable, including commit failure after a successful set.
static enum { GOOD, FAIL_BEFORE_SET, FAIL_AFTER_SET, FAIL_COMMIT } fault;
static void key_parts(const char *key, unsigned *lane, unsigned *slot)
{ *lane=key[0]=='h'; assert(sscanf(key+1,"%u",slot)==1); assert(*slot<(*lane ? JOURNAL_HEALTH_CAP : JOURNAL_LIFE_CAP)); }
esp_err_t nvs_open_from_partition(const char *part,const char *ns,int mode,nvs_handle_t *h)
{ assert(!strcmp(part,"journal") && !strcmp(ns,"nvf_journal") && mode==NVS_READWRITE); *h=1; return ESP_OK; }
void nvs_close(nvs_handle_t h) { assert(h==1); }
esp_err_t nvs_get_blob(nvs_handle_t h,const char *key,void *out,size_t *size)
{
    assert(h==1); unsigned lane,slot; key_parts(key,&lane,&slot);
    if (!slots[lane][slot].present) return ESP_ERR_NVS_NOT_FOUND;
    assert(*size==JOURNAL_RECORD_SIZE); memcpy(out,slots[lane][slot].bytes,*size); return ESP_OK;
}
esp_err_t nvs_set_blob(nvs_handle_t h,const char *key,const void *data,size_t size)
{
    assert(h==1 && size==JOURNAL_RECORD_SIZE); sets++;
    if (fault==FAIL_BEFORE_SET) return ESP_ERR_NVS_NOT_ENOUGH_SPACE;
    unsigned lane,slot; key_parts(key,&lane,&slot);
    memcpy(slots[lane][slot].bytes,data,size); slots[lane][slot].present=true;
    return fault==FAIL_AFTER_SET ? ESP_FAIL : ESP_OK;
}
esp_err_t nvs_commit(nvs_handle_t h) { assert(h==1); commits++; return fault==FAIL_COMMIT ? ESP_FAIL : ESP_OK; }
static journal_record_t record(uint8_t event)
{
    journal_record_t r={.event=event,.boot=1,.utc=1800000000,.time_source=JOURNAL_TIME_GNSS,
        .uptime_ms=UINT64_C(100000000000),.dropped=UINT64_C(9007199254740993),
        .flags=JOURNAL_SAMPLED|JOURNAL_RECEIVER,.reset_reason=7,.queued=123,.internal_free=456,
        .environment=7,.rng=2,.manifest=6,
        .timing_flags=15,.timing_elapsed_s=86400,.gnss_pulses=86400,.rtc_pulses=86401,
        .timing_dropped=3,.timing_phase_ticks=-80,.timing_hz=80000000};
    strcpy(r.firmware,"revision-one"); strcpy(r.partition,"ota_0"); memset(r.elf_sha256,0xa5,32);
    return r;
}
static void reset(void) { memset(slots,0,sizeof slots); sets=commits=0; fault=GOOD; }
static void fifo(void)
{
    reset(); journal_store_t s; assert(journal_store_open(&s)==ESP_OK);
    journal_record_t r=record(JOURNAL_BOOT), got;
    assert(journal_store_append(&s,&r)==ESP_OK);
    // Many complete wraps never increase the live key count or fail when full.
    for (unsigned i=0;i<JOURNAL_HEALTH_CAP*4+17;i++) {
        r=record(JOURNAL_CHECKPOINT); r.uptime_ms=i*JOURNAL_CHECKPOINT_MS;
        assert(journal_store_append(&s,&r)==ESP_OK);
    }
    assert(journal_store_read(&s,0,1,&got)==ESP_OK && got.event==JOURNAL_BOOT);
    assert(got.dropped==UINT64_C(9007199254740993) && got.elf_sha256[31]==0xa5);
    assert(got.timing_flags==15 && got.timing_elapsed_s==86400 && got.gnss_pulses==86400 && got.rtc_pulses==86401);
    assert(got.timing_dropped==3 && got.timing_phase_ticks==-80 && got.timing_hz==80000000);
    uint64_t last=s.latest[1];
    assert(journal_store_read(&s,1,last-JOURNAL_HEALTH_CAP,&got)==ESP_ERR_NVS_NOT_FOUND);
    for (uint64_t seq=last-JOURNAL_HEALTH_CAP+1;seq<=last;seq++)
        assert(journal_store_read(&s,1,seq,&got)==ESP_OK && got.sequence==seq);
    for (unsigned i=0;i<JOURNAL_LIFE_CAP*3;i++) {
        r=record(JOURNAL_BOOT); r.boot=s.latest[0]+1;
        assert(journal_store_append(&s,&r)==ESP_OK);
    }
    assert(journal_store_read(&s,0,1,&got)==ESP_ERR_NVS_NOT_FOUND);
    journal_store_close(&s); assert(journal_store_open(&s)==ESP_OK);
    assert(s.latest[1]==last && s.latest[0]==JOURNAL_LIFE_CAP*3+1);
    assert(journal_store_read(&s,0,s.latest[0],&got)==ESP_OK && got.boot==s.latest[0]);
    // Export the whole wrapped ring exactly once, newest-first.
    journal_record_t page[8]; uint64_t cursor=0, expected=last; unsigned count,total=0;
    do {
        assert(journal_store_page(&s,1,cursor,page,&count,&cursor)==ESP_OK);
        for (unsigned i=0;i<count;i++) { assert(page[i].sequence==expected--); total++; }
    } while (cursor);
    assert(total==JOURNAL_HEALTH_CAP);
    assert(journal_store_page(&s,1,2,page,&count,&cursor)==ESP_OK && !count && !cursor);
    assert(journal_store_page(&s,2,0,page,&count,&cursor)!=ESP_OK);
    assert(sets==commits); journal_store_close(&s);
}
static void faults(void)
{
    for (int f=FAIL_BEFORE_SET;f<=FAIL_COMMIT;f++) {
        reset(); journal_store_t s; assert(journal_store_open(&s)==ESP_OK);
        journal_record_t r=record(JOURNAL_BOOT), got;
        assert(journal_store_append(&s,&r)==ESP_OK);
        fault=f; r=record(JOURNAL_CONFIRMED);
        assert(journal_store_append(&s,&r)!=ESP_OK && !s.ready);
        unsigned attempts=sets;
        // A full/broken log gets no unbounded retries and cannot block callers.
        assert(journal_store_append(&s,&r)!=ESP_OK && sets==attempts);
        fault=GOOD; assert(journal_store_open(&s)==ESP_OK);
        assert(s.latest[0]==(f==FAIL_BEFORE_SET ? 1u : 2u));
        assert(journal_store_read(&s,0,1,&got)==ESP_OK && got.event==JOURNAL_BOOT);
        r=record(JOURNAL_BOOT); r.boot=s.latest[0]+1;
        assert(journal_store_append(&s,&r)==ESP_OK && r.sequence==r.boot);
        journal_store_close(&s);
    }
    // Corrupt/unknown-format evidence is not silently erased or reused.
    slots[0][0].bytes[68]^=1;
    journal_store_t s; unsigned attempts=sets;
    assert(journal_store_open(&s)!=ESP_OK && !s.ready && sets==attempts);
}
static void cadence(void)
{
    journal_policy_t p={0};
    assert(!journal_checkpoint_due(&p,59999)); assert(journal_checkpoint_due(&p,60000));
    journal_checkpoint_saved(&p,60000);
    assert(!journal_checkpoint_due(&p,3659999)); assert(journal_checkpoint_due(&p,3660000));
    // A long gap creates one current checkpoint, no flood of fabricated ones.
    uint64_t years=UINT64_C(10)*365*24*3600000;
    assert(journal_checkpoint_due(&p,years)); journal_checkpoint_saved(&p,years);
    assert(!journal_checkpoint_due(&p,years+1));
}
static void previous_format(void)
{
    reset(); journal_store_t s; assert(journal_store_open(&s)==ESP_OK);
    journal_record_t r=record(JOURNAL_BOOT), got;
    assert(journal_store_append(&s,&r)==ESP_OK);
    // Original NVJ1 rows had zero reserved bytes here. Recreate a valid old
    // record and reopen it; absent timing must not mean zero clock error.
    uint8_t *b=slots[0][0].bytes;
    memset(b+154,0,34); uint32_t crc=UINT32_MAX;
    for (unsigned i=0;i<188;i++) {
        crc^=b[i];
        for (unsigned bit=0;bit<8;bit++) crc=(crc>>1)^(0xedb88320u & (0u-(crc&1)));
    }
    crc=~crc; for (unsigned i=0;i<4;i++) b[188+i]=crc>>(8*i);
    journal_store_close(&s); assert(journal_store_open(&s)==ESP_OK);
    assert(journal_store_read(&s,0,1,&got)==ESP_OK && got.boot==1 && got.timing_flags==0 && got.timing_hz==0);
    journal_store_close(&s);
}
int main(void) { fifo(); faults(); cadence(); previous_format(); puts("journal FIFO, recovery, full-store, compatibility and cadence tests passed"); }
