#include "power_history.h"

#include <stdlib.h>
#include <string.h>

#ifdef ESP_PLATFORM
#include "esp_err.h"
#include "esp_log.h"
#include "nvs.h"
#endif

#define POWER_HISTORY_MAGIC "NPH1"
#define POWER_HISTORY_VERSION 1u
#define POWER_HISTORY_HEADER 32u
#define POWER_HISTORY_CELL 16u
#define POWER_HISTORY_SIDEREAL 86164u
#define POWER_HISTORY_SAVE_MS (6u*60u*60u*1000u)

typedef struct {
    uint16_t bin,cycle,sum,count;
    uint8_t gnss,sv,signal,meta,history[POWER_HISTORY_DAYS];
} power_cell_t;
_Static_assert(sizeof(power_cell_t)==POWER_HISTORY_CELL,"power history cell must stay compact");

typedef struct {
    power_cell_t *cells;
    uint64_t site_id;
    uint32_t model_generation,checkpoint_generation;
    uint64_t last_save_ms,last_attempt_ms;
    uint8_t min_deviation,mad_multiplier,min_support;
    bool dirty,force_save,load_attempted;
#ifdef ESP_PLATFORM
    nvs_handle_t storage;
    bool storage_ready;
#endif
} power_model_t;
static power_model_t model;

static uint16_t be16(const uint8_t *p){return (uint16_t)((uint16_t)p[0]<<8)|p[1];}
static uint32_t be32(const uint8_t *p){return (uint32_t)nr_get(p,4);}
static void put16(uint8_t *p,uint16_t v){nr_put(p,v,2);}
static void put32(uint8_t *p,uint32_t v){nr_put(p,v,4);}
static uint32_t crc32(const uint8_t *p,size_t n)
{
    uint32_t v=UINT32_MAX;
    for(size_t i=0;i<n;i++){v^=p[i];for(unsigned b=0;b<8;b++)v=(v>>1)^(0xedb88320u&(0u-(v&1u)));}
    return ~v;
}
static bool ensure_cells(void)
{
    if(model.cells)return true;
    model.cells=calloc(POWER_HISTORY_CAPACITY,sizeof *model.cells);
    if(!model.cells)return false;
    for(unsigned i=0;i<POWER_HISTORY_CAPACITY;i++)model.cells[i].gnss=UINT8_MAX;
    return true;
}
static void clear_cells(void)
{
    if(!ensure_cells())return;
    memset(model.cells,0,POWER_HISTORY_CAPACITY*sizeof *model.cells);
    for(unsigned i=0;i<POWER_HISTORY_CAPACITY;i++)model.cells[i].gnss=UINT8_MAX;
}
static unsigned hash_key(uint8_t g,uint8_t sv,uint8_t signal,uint16_t bin)
{
    uint32_t h=2166136261u;
    h=(h^g)*16777619u;h=(h^sv)*16777619u;h=(h^signal)*16777619u;
    h=(h^(uint8_t)(bin>>8))*16777619u;h=(h^(uint8_t)bin)*16777619u;
    return h&(POWER_HISTORY_CAPACITY-1u);
}
static power_cell_t *find_cell(uint8_t g,uint8_t sv,uint8_t signal,uint16_t bin,bool create)
{
    if(!model.cells)return NULL;
    unsigned start=hash_key(g,sv,signal,bin);
    for(unsigned step=0;step<POWER_HISTORY_CAPACITY;step++) {
        power_cell_t *c=&model.cells[(start+step)&(POWER_HISTORY_CAPACITY-1u)];
        if(c->gnss==UINT8_MAX) {
            if(!create)return NULL;
            *c=(power_cell_t){.gnss=g,.sv=sv,.signal=signal,.bin=bin};return c;
        }
        if(c->gnss==g&&c->sv==sv&&c->signal==signal&&c->bin==bin)return c;
    }
    return NULL; // preserve mature cells rather than churn an over-capacity model
}
static uint8_t history_len(const power_cell_t *c){return c->meta&7u;}
static uint8_t history_next(const power_cell_t *c){return (c->meta>>3)&7u;}
static void history_meta(power_cell_t *c,uint8_t len,uint8_t next){c->meta=(uint8_t)(len|(next<<3));}
static void sort_bytes(uint8_t *v,unsigned n)
{for(unsigned i=1;i<n;i++){uint8_t x=v[i];unsigned j=i;while(j&&v[j-1]>x){v[j]=v[j-1];j--;}v[j]=x;}}
static uint8_t median(uint8_t *v,unsigned n)
{
    sort_bytes(v,n);return n&1u?v[n/2]:(uint8_t)(((unsigned)v[n/2-1]+v[n/2]+1)/2);
}
static bool reference(const power_cell_t *c,uint8_t *expected,uint8_t *mad)
{
    uint8_t n=history_len(c);if(n<model.min_support||n>POWER_HISTORY_DAYS)return false;
    uint8_t values[POWER_HISTORY_DAYS],deviation[POWER_HISTORY_DAYS];memcpy(values,c->history,n);
    *expected=median(values,n);
    for(unsigned i=0;i<n;i++)deviation[i]=c->history[i]>*expected?c->history[i]-*expected:*expected-c->history[i];
    *mad=median(deviation,n);return true;
}
static uint64_t model_id(void)
{
    uint64_t id=model.site_id^((uint64_t)model.model_generation*0x9e3779b97f4a7c15ULL);
    return id?id:1;
}
uint64_t power_history_model_id(void){return model.site_id&&model.cells?model_id():0;}
static void finalize(power_cell_t *c)
{
    if(!c->count)return;
    uint8_t value=(uint8_t)((c->sum+c->count/2u)/c->count),len=history_len(c),next=history_next(c);
    c->history[next]=value;if(len<POWER_HISTORY_DAYS)len++;next=(next+1u)%POWER_HISTORY_DAYS;
    history_meta(c,len,next);model.model_generation++;if(!model.model_generation)model.model_generation=1;
    model.dirty=true;
}
static bool accept(const power_cell_t *c,uint8_t cno)
{
    uint8_t expected,mad;if(!reference(c,&expected,&mad))return true;
    unsigned d=cno>expected?cno-expected:expected-cno;
    unsigned threshold=model.mad_multiplier*mad;if(threshold<model.min_deviation)threshold=model.min_deviation;
    return d<threshold;
}
static void observe(uint8_t g,uint8_t sv,uint8_t signal,uint64_t utc,uint8_t cno)
{
    if(g!=0||!sv||!cno||cno>99)return; // v1 edge model is deliberately GPS-only
    uint32_t cycle=(uint32_t)(utc/POWER_HISTORY_SIDEREAL),phase=(uint32_t)(utc%POWER_HISTORY_SIDEREAL);
    if(cycle>UINT16_MAX)return;
    power_cell_t *c=find_cell(g,sv,signal,(uint16_t)(phase/POWER_HISTORY_PHASE_SECONDS),true);if(!c)return;
    if(!c->count)c->cycle=(uint16_t)cycle;
    else if(cycle<c->cycle)return;
    else if(cycle>c->cycle){finalize(c);c->cycle=(uint16_t)cycle;c->sum=0;c->count=0;}
    if(!accept(c,cno)||c->count==UINT16_MAX||UINT16_MAX-c->sum<cno)return;
    c->sum=(uint16_t)(c->sum+cno);c->count++;model.dirty=true;
}
static const gnss_observation_t *observation(const gnss_status_t *s,const nr_entry_t *e,uint64_t now)
{
    bool satellite=e->signal==NR_SATELLITE;
    int64_t measured=satellite?s->satellites_ms:s->signals_ms;
    bool valid=satellite?s->satellites_valid:s->signals_valid;
    if(!valid||measured<0||now<(uint64_t)measured||now-(uint64_t)measured>15000)return NULL;
    const gnss_observation_t *v=satellite?s->satellites:s->signals;
    unsigned n=satellite?s->satellite_count:s->signal_count;
    for(unsigned i=0;i<n;i++)if(v[i].gnss==e->gnss&&v[i].sv==e->sv&&v[i].signal==e->signal&&v[i].cno)return &v[i];
    return NULL;
}
void power_history_assess(const nr_expectation_t *base,const gnss_status_t *status,
                          uint64_t utc,uint64_t now,bool learn,nrp_sample_t *sample)
{
    memset(sample->observed_valid,0,sizeof sample->observed_valid);memset(sample->observed,0,sizeof sample->observed);
    memset(&sample->local,0,sizeof sample->local);sample->flags&=~NRP_FLAG_LOCAL;
    sample->local_model_id=0;sample->local_valid=0;sample->local_alarm=0;sample->count=base->count;
    bool local=model.site_id&&model.cells;
    if(local){sample->flags|=NRP_FLAG_LOCAL;sample->local_model_id=model_id();}
    if(sample->expectation_id!=base->id||utc<base->issued||utc-base->issued>=NR_SLOTS*NR_SLOT_S)return;
    unsigned slot=(unsigned)((utc-base->issued)/NR_SLOT_S);
    uint32_t phase=(uint32_t)(utc%POWER_HISTORY_SIDEREAL);uint16_t bin=(uint16_t)(phase/POWER_HISTORY_PHASE_SECONDS);
    for(unsigned i=0;i<base->count;i++) {
        const nr_entry_t *entry=&base->entries[i];if(!(entry->slots&(1u<<slot)))continue;
        const gnss_observation_t *obs=observation(status,entry,now);if(!obs||obs->cno>99)continue;
        uint8_t bit=1u<<(i%8);sample->observed_valid[i/8]|=bit;sample->observed[i]=obs->cno;
        power_cell_t *cell=local?find_cell(entry->gnss,entry->sv,entry->signal,bin,false):NULL;uint8_t expected,mad;
        if(cell&&reference(cell,&expected,&mad)) {
            sample->local.valid[i/8]|=bit;unsigned d=obs->cno>expected?obs->cno-expected:expected-obs->cno;
            unsigned threshold=model.mad_multiplier*mad;if(threshold<model.min_deviation)threshold=model.min_deviation;
            if(d>=threshold)sample->local.bad[i/8]|=bit;
        }
        if(local&&learn)observe(entry->gnss,entry->sv,entry->signal,utc,obs->cno);
    }
}

static bool valid_policy(uint8_t deviation,uint8_t multiplier,uint8_t support)
{return deviation>=3&&deviation<=30&&multiplier&&multiplier<=16&&support>=2&&support<=POWER_HISTORY_DAYS;}

static bool blob_valid(const uint8_t *b,size_t n,uint64_t site)
{
    if(n<POWER_HISTORY_HEADER+4||memcmp(b,POWER_HISTORY_MAGIC,4)||b[4]!=POWER_HISTORY_VERSION||b[5]!=POWER_HISTORY_HEADER||
       b[6]!=POWER_HISTORY_CELL||b[7]||nr_get(b+8,8)!=site||!be32(b+16)||!be32(b+20)||
       !valid_policy(b[26],b[27],b[28])||b[29]||b[30]||b[31])return false;
    unsigned count=be16(b+24);return count<=POWER_HISTORY_CAPACITY&&n==POWER_HISTORY_HEADER+count*POWER_HISTORY_CELL+4&&
        be32(b+n-4)==crc32(b,n-4);
}
size_t power_history_export(uint8_t *b,size_t capacity,uint32_t checkpoint_generation)
{
    if(!model.cells||!model.site_id||!checkpoint_generation)return 0;
    unsigned count=0;for(unsigned i=0;i<POWER_HISTORY_CAPACITY;i++)if(model.cells[i].gnss!=UINT8_MAX)count++;
    size_t n=POWER_HISTORY_HEADER+count*POWER_HISTORY_CELL+4;if(!b||capacity<n)return n;
    memset(b,0,n);memcpy(b,POWER_HISTORY_MAGIC,4);b[4]=POWER_HISTORY_VERSION;b[5]=POWER_HISTORY_HEADER;b[6]=POWER_HISTORY_CELL;
    nr_put(b+8,model.site_id,8);put32(b+16,model.model_generation);put32(b+20,checkpoint_generation);put16(b+24,(uint16_t)count);
    b[26]=model.min_deviation;b[27]=model.mad_multiplier;b[28]=model.min_support;
    unsigned row=0;
    for(unsigned i=0;i<POWER_HISTORY_CAPACITY;i++)if(model.cells[i].gnss!=UINT8_MAX) {
        const power_cell_t *c=&model.cells[i];uint8_t *p=b+POWER_HISTORY_HEADER+row++*POWER_HISTORY_CELL;
        p[0]=c->gnss;p[1]=c->sv;p[2]=c->signal;put16(p+3,c->bin);put16(p+5,c->cycle);put16(p+7,c->sum);
        put16(p+9,c->count);p[11]=c->meta;memcpy(p+12,c->history,POWER_HISTORY_DAYS);
    }
    put32(b+n-4,crc32(b,n-4));return n;
}
bool power_history_import(const uint8_t *b,size_t n,uint64_t expected_site)
{
    if(!ensure_cells()||!blob_valid(b,n,expected_site))return false;
    clear_cells();model.site_id=expected_site;model.model_generation=be32(b+16);model.checkpoint_generation=be32(b+20);
    model.min_deviation=b[26];model.mad_multiplier=b[27];model.min_support=b[28];
    unsigned count=be16(b+24);
    for(unsigned row=0;row<count;row++) {
        const uint8_t *p=b+POWER_HISTORY_HEADER+row*POWER_HISTORY_CELL;
        uint8_t g=p[0],sv=p[1],signal=p[2],meta=p[11],len=meta&7u,next=(meta>>3)&7u;
        uint16_t bin=be16(p+3),cycle=be16(p+5),sum=be16(p+7),samples=be16(p+9);
        if(g>7||g==4||!sv||(signal!=NR_SATELLITE&&signal>31)||(uint32_t)bin*POWER_HISTORY_PHASE_SECONDS>=POWER_HISTORY_SIDEREAL||
           (meta&0xc0u)||len>POWER_HISTORY_DAYS||next>=POWER_HISTORY_DAYS||
           (len<POWER_HISTORY_DAYS&&next!=len)||(samples==0)!=(sum==0)||
           (samples&&((uint32_t)sum<samples||(uint32_t)sum>99u*samples))) {clear_cells();model.site_id=0;return false;}
        for(unsigned j=0;j<POWER_HISTORY_DAYS;j++)if((j<len&&(p[12+j]==0||p[12+j]>99))||(j>=len&&p[12+j]&&len<POWER_HISTORY_DAYS)){
            clear_cells();model.site_id=0;return false;
        }
        if(find_cell(g,sv,signal,bin,false)){clear_cells();model.site_id=0;return false;}
        power_cell_t *c=find_cell(g,sv,signal,bin,true);if(!c){clear_cells();model.site_id=0;return false;}
        c->cycle=cycle;c->sum=sum;c->count=samples;c->meta=meta;memcpy(c->history,p+12,POWER_HISTORY_DAYS);
    }
    model.dirty=false;model.force_save=false;return true;
}

#ifdef ESP_PLATFORM
static const char *TAG="power_history";
static bool read_slot(const char *key,uint64_t site,uint8_t **out,size_t *length,uint32_t *generation)
{
    size_t n=0;if(nvs_get_blob(model.storage,key,NULL,&n)!=ESP_OK||n>POWER_HISTORY_WIRE_MAX)return false;
    uint8_t *b=malloc(n);if(!b)return false;
    if(nvs_get_blob(model.storage,key,b,&n)!=ESP_OK||!blob_valid(b,n,site)){free(b);return false;}
    *out=b;*length=n;*generation=be32(b+20);return true;
}
static bool load_storage(uint64_t site)
{
    if(nvs_open_from_partition("journal","nvf_power",NVS_READWRITE,&model.storage)!=ESP_OK)return false;
    model.storage_ready=true;uint8_t *best=NULL;size_t best_n=0;uint32_t best_gen=0;
    for(unsigned i=0;i<2;i++){uint8_t *b=NULL;size_t n=0;uint32_t gen=0;if(read_slot(i?"model_b":"model_a",site,&b,&n,&gen)){
        if(!best||gen>best_gen){free(best);best=b;best_n=n;best_gen=gen;}else free(b);
    }}
    bool ok=best&&power_history_import(best,best_n,site);free(best);return ok;
}
#else
static bool load_storage(uint64_t site){(void)site;return false;}
#endif

bool power_history_bind(uint64_t site,uint8_t deviation,uint8_t multiplier,uint8_t support)
{
    if(!site||!valid_policy(deviation,multiplier,support)||!ensure_cells())return false;
    if(!model.load_attempted){model.load_attempted=true;(void)load_storage(site);}
    if(model.site_id!=site||model.min_deviation!=deviation||model.mad_multiplier!=multiplier||model.min_support!=support) {
        clear_cells();model.site_id=site;model.model_generation=1;model.checkpoint_generation=0;
        model.min_deviation=deviation;model.mad_multiplier=multiplier;model.min_support=support;
        model.dirty=true;model.force_save=true;
    }
    return true;
}
void power_history_checkpoint(uint64_t now)
{
#ifdef ESP_PLATFORM
    if(!model.storage_ready||!model.dirty||(!model.force_save&&model.last_save_ms&&now-model.last_save_ms<POWER_HISTORY_SAVE_MS)||
       (model.last_attempt_ms&&now-model.last_attempt_ms<60000))return;
    model.last_attempt_ms=now;uint32_t generation=model.checkpoint_generation+1;if(!generation)return;
    size_t n=power_history_export(NULL,0,generation);uint8_t *b=malloc(n);if(!b)return;
    if(power_history_export(b,n,generation)!=n){free(b);return;}
    const char *key=(generation&1u)?"model_a":"model_b";esp_err_t err=nvs_set_blob(model.storage,key,b,n);free(b);
    if(err==ESP_OK)err=nvs_commit(model.storage);
    if(err==ESP_OK){model.checkpoint_generation=generation;model.last_save_ms=now;model.dirty=false;model.force_save=false;}
    else ESP_LOGW(TAG,"checkpoint failed: %s",esp_err_to_name(err));
#else
    (void)now;
#endif
}
void power_history_test_reset(void)
{
#ifdef ESP_PLATFORM
    if(model.storage_ready)nvs_close(model.storage);
#endif
    free(model.cells);model=(power_model_t){0};
}
