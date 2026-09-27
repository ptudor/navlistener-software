#ifndef NVF_RECEPTION_H
#define NVF_RECEPTION_H
// Station expectation v1. All multi-byte fields are big endian; see docs/RECEPTION.md.
#include <stdbool.h>
#include <stdint.h>
#include <stddef.h>
#include <string.h>

#define NR_MAX_ENTRIES 128u
#define NR_HEADER 40u
#define NR_MAX_WIRE (NR_HEADER + 4u*NR_MAX_ENTRIES)
#define NR_SAMPLE_SIZE 76u
#define NR_SLOTS 5u
#define NR_SLOT_S 60u
#define NR_SATELLITE 255u
#define NR_F_EXPECTATION 0x0bu
#define NR_F_SNAPSHOT 0x0cu

typedef struct { uint8_t gnss, sv, signal, slots; } nr_entry_t;
typedef struct {
    uint64_t id, issued;
    int32_t latitude, longitude;
    uint16_t radius_m, alarm_s, clear_s;
    uint8_t min_expected, min_missing, missing_percent, count;
    nr_entry_t entries[NR_MAX_ENTRIES];
} nr_expectation_t;
typedef struct {
    uint64_t expectation_id, utc, uptime_ms, boot, event;
    uint8_t valid, alarm, matched[16], expected[8], observed[8];
} nr_sample_t;
typedef struct {
    uint8_t alarm, target, pending;
    uint64_t since[8], last_ms;
} nr_machine_t;

static inline uint64_t nr_get(const uint8_t *p, unsigned n)
{ uint64_t v=0; for(unsigned i=0;i<n;i++) v=(v<<8)|p[i]; return v; }
static inline void nr_put(uint8_t *p,uint64_t v,unsigned n)
{ for(unsigned i=n;i>0;i--) { p[i-1]=(uint8_t)v;v>>=8; } }

static inline bool nr_decode(nr_expectation_t *e,const uint8_t *b,size_t n)
{
    if(n<NR_HEADER || b[0]!=1 || b[1]!=NR_SLOTS || b[2]!=NR_SLOT_S || b[3]>NR_MAX_ENTRIES ||
       n!=NR_HEADER+4u*b[3] || b[37] || b[38] || b[39]) return false;
    nr_expectation_t v={.id=nr_get(b+4,8),.issued=nr_get(b+12,8),.latitude=(int32_t)nr_get(b+20,4),
        .longitude=(int32_t)nr_get(b+24,4),.radius_m=(uint16_t)nr_get(b+28,2),
        .alarm_s=(uint16_t)nr_get(b+30,2),.clear_s=(uint16_t)nr_get(b+32,2),
        .min_expected=b[34],.min_missing=b[35],.missing_percent=b[36],.count=b[3]};
    if(!v.id || v.issued<946684800 || v.issued>=4102444800ULL || v.latitude < -900000000 ||
       v.latitude>900000000 || v.longitude < -1800000000 || v.longitude>1800000000 || !v.radius_m ||
       v.alarm_s<5 || v.alarm_s>120 || v.clear_s<5 || v.clear_s>120 || !v.min_expected ||
       !v.min_missing || !v.missing_percent || v.missing_percent>100) return false;
    for(unsigned i=0;i<v.count;i++) {
        const uint8_t *p=b+NR_HEADER+4*i;
        nr_entry_t x={p[0],p[1],p[2],p[3]};
        if(x.gnss>7 || x.gnss==4 || !x.sv || !x.slots || x.slots>>NR_SLOTS ||
           (x.signal!=NR_SATELLITE && x.signal>31)) return false;
        for(unsigned j=0;j<i;j++) if(x.gnss==v.entries[j].gnss && x.sv==v.entries[j].sv &&
            x.signal==v.entries[j].signal) return false;
        v.entries[i]=x;
    }
    *e=v;return true;
}

static inline void nr_encode_sample(uint8_t b[NR_SAMPLE_SIZE],const nr_sample_t *s)
{
    memset(b,0,NR_SAMPLE_SIZE);b[0]=1;b[1]=s->valid;b[2]=s->alarm;
    nr_put(b+4,s->expectation_id,8);nr_put(b+12,s->utc,8);nr_put(b+20,s->uptime_ms,8);
    nr_put(b+28,s->boot,8);nr_put(b+36,s->event,8);memcpy(b+44,s->matched,16);
    memcpy(b+60,s->expected,8);memcpy(b+68,s->observed,8);
}
static inline bool nr_decode_sample(nr_sample_t *s,const uint8_t *b,size_t n)
{
    if(n!=NR_SAMPLE_SIZE || b[0]!=1 || b[3] || (b[1]&16) || (b[2]&16)) return false;
    *s=(nr_sample_t){.valid=b[1],.alarm=b[2],.expectation_id=nr_get(b+4,8),.utc=nr_get(b+12,8),
        .uptime_ms=nr_get(b+20,8),.boot=nr_get(b+28,8),.event=nr_get(b+36,8)};
    memcpy(s->matched,b+44,16);memcpy(s->expected,b+60,8);memcpy(s->observed,b+68,8);return true;
}

static inline uint8_t nr_counts(const nr_expectation_t *e,nr_sample_t *s)
{
    memset(s->expected,0,8);memset(s->observed,0,8);
    if(s->expectation_id!=e->id || s->utc<e->issued || s->utc-e->issued>=NR_SLOTS*NR_SLOT_S) {
        s->valid=0;return 0;
    }
    unsigned slot=1u<<((s->utc-e->issued)/NR_SLOT_S);
    for(unsigned i=0;i<e->count;i++) {
        const nr_entry_t *v=&e->entries[i];if(!(v->slots&slot)) continue;
        s->expected[v->gnss]++;if(s->matched[i/8]&(1u<<(i%8))) s->observed[v->gnss]++;
    }
    uint8_t bad=0;
    for(unsigned g=0;g<8;g++) {
        if(s->expected[g]<e->min_expected) {s->valid&=~(1u<<g);continue;}
        unsigned missing=s->expected[g]-s->observed[g];
        if(missing>=e->min_missing && missing*100>=e->missing_percent*s->expected[g]) bad|=1u<<g;
    }
    return bad&s->valid;
}

static inline uint8_t nr_step(nr_machine_t *m,uint8_t valid,uint8_t bad,uint64_t now,
                               uint16_t alarm_s,uint16_t clear_s)
{
    if(m->last_ms && (now<=m->last_ms || now-m->last_ms>15000)) m->pending=0;
    if(now<=m->last_ms) return m->alarm;
    m->last_ms=now;
    for(unsigned g=0;g<8;g++) {
        uint8_t bit=1u<<g,want=bad&bit;
        if(!(valid&bit) || want==(m->alarm&bit)) {m->pending&=~bit;continue;}
        if(!(m->pending&bit) || (m->target&bit)!=want) {
            m->pending|=bit;m->target=(m->target&~bit)|want;m->since[g]=now;
        }
        uint64_t dwell=(uint64_t)(want ? alarm_s : clear_s)*1000;
        if(now-m->since[g]>=dwell) {m->alarm=(m->alarm&~bit)|want;m->pending&=~bit;}
    }
    return m->alarm;
}

// Canonical data/pilot pairs share a reception expectation. No inference for unknown IDs.
static inline uint8_t nr_signal(uint8_t g,uint8_t s)
{
    if(g==0) {if(s==4)return 3;if(s==7)return 6;}
    if(g==2) {if(s==1)return 0;if(s==4)return 3;if(s==6)return 5;}
    if(g==3) {if(s==1)return 0;if(s==3)return 2;if(s==6)return 5;if(s==7)return 8;}
    if(g==5) {if(s==5)return 4;if(s==9)return 8;}
    return s;
}

// 500 ms full cycle: green for 250 ms, yellow for 250 ms, affected columns only.
static inline void nr_alarm_leds(uint8_t alarm,uint64_t now,uint8_t *green,uint8_t *yellow)
{
    const uint8_t gnss[7]={0,1,2,3,5,6,7};
    for(unsigned i=0;i<7;i++) if(alarm&(1u<<gnss[i])) {
        *green&=~(1u<<i);*yellow&=~(1u<<i);
        if((now/250)%2) *yellow|=1u<<i;else *green|=1u<<i;
    }
}
#endif
