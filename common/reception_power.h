#ifndef NVF_RECEPTION_POWER_H
#define NVF_RECEPTION_POWER_H

#include "reception.h"

#define NRP_VERSION 1u
#define NRP_HEADER 40u
#define NRP_ENTRY_SIZE 16u
#define NRP_MAX_WIRE (NRP_HEADER + NRP_ENTRY_SIZE*NR_MAX_ENTRIES)
#define NRP_SAMPLE_SIZE 256u
#define NRP_EVENT_SIZE 68u
#define NRP_MAX_MIN_SUPPORT 4u
#define NRP_F_EXPECTATION 0x0du
#define NRP_FLAG_LOCAL 1u
#define NRP_FLAG_REMOTE 2u

typedef struct {
    uint8_t valid,expected[NR_SLOTS],mad[NR_SLOTS],support[NR_SLOTS];
} nrp_entry_t;
typedef struct {
    uint64_t expectation_id,model_id,issued,site_id;
    uint8_t min_deviation,mad_multiplier,min_support,count;
    nrp_entry_t entries[NR_MAX_ENTRIES];
} nrp_expectation_t;
typedef struct { uint8_t valid[16],bad[16]; } nrp_assessment_t;
typedef struct {
    uint64_t expectation_id,remote_model_id,local_model_id,utc,uptime_ms;
    uint8_t count,flags,local_valid,local_alarm,remote_valid,remote_alarm;
    uint8_t observed_valid[16],observed[NR_MAX_ENTRIES];
    nrp_assessment_t local,remote;
} nrp_sample_t;
typedef struct {
    uint64_t expectation_id,remote_model_id,local_model_id,utc,uptime_ms,boot,event;
    uint8_t flags,local_valid,local_alarm,remote_valid,remote_alarm,joint_valid,joint_alarm;
    uint8_t local_abnormal,remote_abnormal,joint_abnormal,model_conflict;
} nrp_event_t;

static inline bool nrp_tail_clear(const uint8_t bits[16],uint8_t count)
{
    for(unsigned i=count;i<NR_MAX_ENTRIES;i++) if(bits[i/8]&(1u<<(i%8))) return false;
    return true;
}

static inline bool nrp_decode(nrp_expectation_t *p,const uint8_t *b,size_t n)
{
    if(n<NRP_HEADER || b[0]!=NRP_VERSION || b[1]!=NR_SLOTS || b[2]!=NR_SLOT_S ||
       b[3]>NR_MAX_ENTRIES || n!=NRP_HEADER+NRP_ENTRY_SIZE*b[3] || b[39] ||
       !nr_get(b+4,8) || !nr_get(b+12,8) || !nr_get(b+28,8) || nr_get(b+20,8)<946684800 ||
       nr_get(b+20,8)>=4102444800ULL || b[36]<3 || b[36]>30 || !b[37] ||
       b[37]>16 || b[38]<2 || b[38]>NRP_MAX_MIN_SUPPORT) return false;
    nrp_expectation_t v={.expectation_id=nr_get(b+4,8),.model_id=nr_get(b+12,8),
        .issued=nr_get(b+20,8),.site_id=nr_get(b+28,8),.min_deviation=b[36],.mad_multiplier=b[37],
        .min_support=b[38],.count=b[3]};
    for(unsigned i=0;i<v.count;i++) {
        const uint8_t *q=b+NRP_HEADER+NRP_ENTRY_SIZE*i;nrp_entry_t *e=&v.entries[i];
        e->valid=q[0];memcpy(e->expected,q+1,NR_SLOTS);memcpy(e->mad,q+6,NR_SLOTS);
        memcpy(e->support,q+11,NR_SLOTS);
        if(e->valid>>NR_SLOTS)return false;
        for(unsigned s=0;s<NR_SLOTS;s++) {
            bool valid=e->valid&(1u<<s);
            if(valid!=(e->support[s]>=v.min_support) || (valid && (!e->expected[s] || e->expected[s]>99)))return false;
        }
    }
    *p=v;return true;
}

static inline bool nrp_decode_sample(nrp_sample_t *s,const uint8_t *b,size_t n)
{
    if(n!=NRP_SAMPLE_SIZE || b[0]!=NRP_VERSION || b[1]>NR_MAX_ENTRIES || b[2]&~3u || b[7] ||
       !nr_get(b+8,8) || nr_get(b+32,8)<946684800 || nr_get(b+32,8)>=4102444800ULL)return false;
    nrp_sample_t v={.count=b[1],.flags=b[2],.local_valid=b[3],.local_alarm=b[4],
        .remote_valid=b[5],.remote_alarm=b[6],.expectation_id=nr_get(b+8,8),
        .remote_model_id=nr_get(b+16,8),.local_model_id=nr_get(b+24,8),
        .utc=nr_get(b+32,8),.uptime_ms=nr_get(b+40,8)};
    memcpy(v.observed_valid,b+48,16);memcpy(v.observed,b+64,NR_MAX_ENTRIES);
    memcpy(v.local.valid,b+192,16);memcpy(v.local.bad,b+208,16);
    memcpy(v.remote.valid,b+224,16);memcpy(v.remote.bad,b+240,16);
    if(!nrp_tail_clear(v.observed_valid,v.count)||!nrp_tail_clear(v.local.valid,v.count)||
       !nrp_tail_clear(v.local.bad,v.count)||!nrp_tail_clear(v.remote.valid,v.count)||
       !nrp_tail_clear(v.remote.bad,v.count))return false;
    for(unsigned i=0;i<v.count;i++) {
        uint8_t bit=1u<<(i%8);
        if((v.local.bad[i/8]&~v.local.valid[i/8]&bit)||(v.remote.bad[i/8]&~v.remote.valid[i/8]&bit)||
           ((v.observed_valid[i/8]&bit)&&(!v.observed[i]||v.observed[i]>99)))return false;
    }
    nrp_assessment_t zero={0};
    if(((v.flags&NRP_FLAG_LOCAL)&&!v.local_model_id)||((v.flags&NRP_FLAG_REMOTE)&&!v.remote_model_id))return false;
    if(!(v.flags&NRP_FLAG_LOCAL)&&(v.local_model_id||v.local_valid||v.local_alarm||memcmp(&v.local,&zero,sizeof zero)))return false;
    if(!(v.flags&NRP_FLAG_REMOTE)&&(v.remote_model_id||v.remote_valid||v.remote_alarm||memcmp(&v.remote,&zero,sizeof zero)))return false;
    *s=v;return true;
}

static inline void nrp_encode_sample(uint8_t b[NRP_SAMPLE_SIZE],const nrp_sample_t *s)
{
    memset(b,0,NRP_SAMPLE_SIZE);b[0]=NRP_VERSION;b[1]=s->count;b[2]=s->flags;
    b[3]=s->local_valid;b[4]=s->local_alarm;b[5]=s->remote_valid;b[6]=s->remote_alarm;
    nr_put(b+8,s->expectation_id,8);nr_put(b+16,s->remote_model_id,8);
    nr_put(b+24,s->local_model_id,8);nr_put(b+32,s->utc,8);nr_put(b+40,s->uptime_ms,8);
    memcpy(b+48,s->observed_valid,16);memcpy(b+64,s->observed,NR_MAX_ENTRIES);
    memcpy(b+192,s->local.valid,16);memcpy(b+208,s->local.bad,16);
    memcpy(b+224,s->remote.valid,16);memcpy(b+240,s->remote.bad,16);
}

static inline nrp_assessment_t nrp_compare(const nrp_expectation_t *p,const nrp_sample_t *s)
{
    nrp_assessment_t a={0};
    if(s->expectation_id!=p->expectation_id || s->remote_model_id!=p->model_id ||
       s->count!=p->count || s->utc<p->issued || s->utc-p->issued>=NR_SLOTS*NR_SLOT_S)return a;
    unsigned slot=(unsigned)((s->utc-p->issued)/NR_SLOT_S);
    for(unsigned i=0;i<p->count;i++) {
        uint8_t bit=1u<<(i%8);const nrp_entry_t *e=&p->entries[i];
        if(!(e->valid&(1u<<slot)) || !(s->observed_valid[i/8]&bit))continue;
        a.valid[i/8]|=bit;unsigned d=s->observed[i]>e->expected[slot]?
            s->observed[i]-e->expected[slot]:e->expected[slot]-s->observed[i];
        unsigned threshold=p->mad_multiplier*e->mad[slot];
        if(threshold<p->min_deviation)threshold=p->min_deviation;
        if(d>=threshold)a.bad[i/8]|=bit;
    }
    return a;
}

static inline uint8_t nrp_counts(const nr_expectation_t *base,uint8_t count,const nrp_assessment_t *a,
                                 uint8_t modeled[8],uint8_t anomalous[8],uint8_t *valid)
{
    memset(modeled,0,8);memset(anomalous,0,8);*valid=0;
    if(count!=base->count)return 0;
    for(unsigned i=0;i<count;i++) {
        uint8_t bit=1u<<(i%8),g=base->entries[i].gnss;
        if(!(a->valid[i/8]&bit))continue;
        modeled[g]++;if(a->bad[i/8]&bit)anomalous[g]++;
    }
    uint8_t bad=0;
    for(unsigned g=0;g<8;g++) if(modeled[g]>=base->min_expected) {
        *valid|=1u<<g;
        if(anomalous[g]>=base->min_missing && anomalous[g]*100u>=base->missing_percent*modeled[g])bad|=1u<<g;
    }
    return bad;
}

static inline void nrp_encode_event(uint8_t b[NRP_EVENT_SIZE],const nrp_event_t *e)
{
    memset(b,0,NRP_EVENT_SIZE);b[0]=NRP_VERSION;b[1]=e->flags;b[2]=e->local_valid;b[3]=e->local_alarm;
    b[4]=e->remote_valid;b[5]=e->remote_alarm;b[6]=e->joint_valid;b[7]=e->joint_alarm;
    nr_put(b+8,e->expectation_id,8);nr_put(b+16,e->remote_model_id,8);nr_put(b+24,e->local_model_id,8);
    nr_put(b+32,e->utc,8);nr_put(b+40,e->uptime_ms,8);nr_put(b+48,e->boot,8);nr_put(b+56,e->event,8);
    b[64]=e->local_abnormal;b[65]=e->remote_abnormal;b[66]=e->joint_abnormal;b[67]=e->model_conflict;
}

static inline bool nrp_decode_event(nrp_event_t *e,const uint8_t *b,size_t n)
{
    if(n!=NRP_EVENT_SIZE || b[0]!=NRP_VERSION || b[1]&~3u || (b[2]&16) || (b[3]&16) ||
       (b[4]&16) || (b[5]&16) || (b[6]&16) || (b[7]&16) || !nr_get(b+8,8) ||
       nr_get(b+32,8)<946684800 || nr_get(b+32,8)>=4102444800ULL || !nr_get(b+48,8) || !nr_get(b+56,8) ||
       (b[64]&16) || (b[65]&16) || (b[66]&16) || (b[67]&16))return false;
    *e=(nrp_event_t){.flags=b[1],.local_valid=b[2],.local_alarm=b[3],.remote_valid=b[4],.remote_alarm=b[5],
        .joint_valid=b[6],.joint_alarm=b[7],.expectation_id=nr_get(b+8,8),.remote_model_id=nr_get(b+16,8),
        .local_model_id=nr_get(b+24,8),.utc=nr_get(b+32,8),.uptime_ms=nr_get(b+40,8),
        .boot=nr_get(b+48,8),.event=nr_get(b+56,8),.local_abnormal=b[64],.remote_abnormal=b[65],
        .joint_abnormal=b[66],.model_conflict=b[67]};
    if((!(e->flags&NRP_FLAG_LOCAL)&&(e->local_model_id||e->local_valid||e->local_alarm||e->local_abnormal)) ||
       (!(e->flags&NRP_FLAG_REMOTE)&&(e->remote_model_id||e->remote_valid||e->remote_alarm||e->remote_abnormal)))return false;
    if(((e->flags&NRP_FLAG_LOCAL)&&!e->local_model_id)||((e->flags&NRP_FLAG_REMOTE)&&!e->remote_model_id))return false;
    if(e->flags!=(NRP_FLAG_LOCAL|NRP_FLAG_REMOTE) &&
       (e->joint_valid||e->joint_alarm||e->joint_abnormal||e->model_conflict))return false;
    return true;
}

#endif
