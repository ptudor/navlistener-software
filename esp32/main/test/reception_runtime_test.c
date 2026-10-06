#include <assert.h>
#include <stdio.h>
#include <time.h>
static time_t fake_utc=1800000000;
static time_t test_time(time_t *out) {if(out)*out=fake_utc;return fake_utc;}
#define time test_time
#include "../reception_runtime.c"
#undef time

static nr_sample_t persisted[128];
static nrp_event_t persisted_power[128];
static unsigned persisted_count,persisted_power_count,receiver_polls,records;
static bool connected;
static uint8_t last_tag,last_value[NRP_SAMPLE_SIZE];
void receiver_request_snapshot(void) {receiver_polls++;}
bool pusher_connected(void) {return connected;}
uint64_t spool_append(const uint8_t *b,uint32_t n)
{
    assert(n>=GNF1_RECORD_HDR+27);
    last_tag=b[GNF1_RECORD_HDR+24];size_t length=nr_get(b+GNF1_RECORD_HDR+25,2);
    assert(length<=sizeof last_value);memcpy(last_value,b+GNF1_RECORD_HDR+27,length);
    return ++records;
}
bool journal_reception_save(nr_sample_t *s)
{
    assert(persisted_count<128);s->boot=1;s->event=persisted_count+1;
    persisted[persisted_count++]=*s;return true;
}
bool journal_reception_latest(nr_sample_t *s)
{if(!persisted_count)return false;*s=persisted[persisted_count-1];return true;}
bool journal_reception_next(uint64_t after,nr_sample_t *s)
{if(after>=persisted_count)return false;*s=persisted[after];return true;}
bool journal_power_save(nrp_event_t *s)
{assert(persisted_power_count<128);s->boot=1;s->event=persisted_power_count+1;persisted_power[persisted_power_count++]=*s;return true;}
bool journal_power_latest(nrp_event_t *s)
{if(!persisted_power_count)return false;*s=persisted_power[persisted_power_count-1];return true;}
bool journal_power_next(uint64_t after,nrp_event_t *s)
{if(after>=persisted_power_count)return false;*s=persisted_power[after];return true;}

static void forecast(uint64_t id)
{
    uint8_t b[40+12*4]={1,5,60,12};nr_put(b+4,id,8);nr_put(b+12,fake_utc,8);
    nr_put(b+28,1000,2);nr_put(b+30,5,2);nr_put(b+32,5,2);b[34]=4;b[35]=3;b[36]=50;
    for(unsigned i=0;i<12;i++){b[40+4*i+1]=i+1;b[40+4*i+2]=255;b[40+4*i+3]=31;}
    reception_control(NR_F_EXPECTATION,b,sizeof b);
}
static void power_forecast(uint64_t expectation_id,uint64_t model_id,uint64_t site_id)
{
    uint8_t b[NRP_HEADER+12*NRP_ENTRY_SIZE]={1,NR_SLOTS,NR_SLOT_S,12};
    nr_put(b+4,expectation_id,8);nr_put(b+12,model_id,8);nr_put(b+20,fake_utc,8);nr_put(b+28,site_id,8);
    b[36]=6;b[37]=4;b[38]=3;
    for(unsigned i=0;i<12;i++){uint8_t *e=b+NRP_HEADER+i*NRP_ENTRY_SIZE;e[0]=31;
        memset(e+1,40,NR_SLOTS);memset(e+6,1,NR_SLOTS);memset(e+11,3,NR_SLOTS);}
    reception_control(NRP_F_EXPECTATION,b,sizeof b);
}
static uint8_t poll(gnss_status_t *s,unsigned second,bool new_measurement)
{
    fake_utc=1800000000+second;
    if(new_measurement)s->satellites_ms=(int64_t)second*1000;
    return reception_poll(s,(uint64_t)second*1000,NULL);
}
int main(void)
{
    gnss_status_t s={.supported=1,.satellites_valid=true,.satellite_count=2,
        .satellites={{0,1,255,40,30},{0,2,255,41,35}}};
    forecast(7);
    for(unsigned t=1;t<6;t++)assert(poll(&s,t,true)==0);
    assert(poll(&s,6,true)==1 && !connected && receiver_polls==1);
    assert(persisted_count==2 && persisted[1].observed[0]==2 && persisted[1].expected[0]==12);
    assert(last_tag==17 && last_value[2]==1);
    reception_snapshot_queued();
    // Expiry holds the alarm, interrupts recovery, and records unknown coverage.
    assert(poll(&s,301,true)==1 && latest.valid==0 && persisted[persisted_count-1].alarm==1);
    // A reboot with no forecast restores the persisted local alarm.
    started=false;expectation=(nr_expectation_t){0};machine=(nr_machine_t){0};latest=(nr_sample_t){0};
    assert(poll(&s,302,true)==1);
    forecast(8);s.satellite_count=12;
    for(unsigned i=0;i<12;i++)s.satellites[i]=(gnss_observation_t){0,i+1,255,40,30};
    for(unsigned t=303;t<308;t++)assert(poll(&s,t,true)==1);
    assert(poll(&s,308,true)==0);
    reception_snapshot_queued();
    // A repeated receiver report cannot start and finish another alarm.
    s.satellite_count=2;
    assert(poll(&s,309,true)==0);
    for(unsigned t=310;t<320;t++)assert(poll(&s,t,false)==0);
    assert(poll(&s,330,true)==0);
    // A command polls once; a retransmission of its completed ID resends its result.
    uint8_t request[20]={1,7};nr_put(request+4,42,8);nr_put(request+12,fake_utc+30,8);
    unsigned previous_polls=receiver_polls;
    reception_control(NR_F_SNAPSHOT,request,sizeof request);poll(&s,331,true);
    assert(receiver_polls==previous_polls+1 && !reception_snapshot_due(331000));
    poll(&s,332,true);s.rf_valid=true;s.rf_ms=332000;
    assert(reception_snapshot_due(332000));
    uint8_t body[128]={0};size_t length=reception_snapshot_append(body,24,sizeof body,&s,332000);
    assert(length==39 && body[24]==19 && body[28]==1 && body[29]==7);
    reception_snapshot_queued();reception_control(NR_F_SNAPSHOT,request,sizeof request);
    next_report=UINT64_MAX;poll(&s,333,true);
    assert(receiver_polls==previous_polls+1 && last_tag==19 && last_value[1]==1 && nr_get(last_value+4,8)==42);
    // Reconnect uploads retained history without deleting the local evidence.
    connected=true;next_report=0;poll(&s,334,true);
    assert(last_tag==18 && persisted_count>=4 && upload_event==persisted[0].event);
    // A delayed board pass reports expiration without starting late conversions.
    reception_snapshot_queued();nr_put(request+4,43,8);nr_put(request+12,fake_utc+2,8);
    reception_control(NR_F_SNAPSHOT,request,sizeof request);poll(&s,335,true);
    assert(!reception_snapshot_due(337000));
    next_report=UINT64_MAX;poll(&s,337,true);
    assert(last_tag==19 && last_value[1]==3 && completed_request==43 && !snapshot_needed);

    // A mature local history and the delivered reference must agree before a
    // received-power anomaly reaches the panel. The live report contains the
    // actual aligned C/N0 values for collector recomputation.
    power_history_test_reset();const uint64_t site_id=0x1122334455667788ULL;
    assert(power_history_bind(site_id,6,4,3));
    gnss_status_t p={.supported=1,.satellites_valid=true,.satellite_count=12};
    for(unsigned i=0;i<12;i++)p.satellites[i]=(gnss_observation_t){0,i+1,NR_SATELLITE,40,40};
    const uint64_t phase=1800000000ULL%86164ULL;
    for(unsigned d=0;d<4;d++) {
        uint64_t training=(1800000000ULL/86164ULL-3+d)*86164ULL+phase;
        nr_expectation_t base={.id=100+d,.issued=training,.radius_m=1000,.alarm_s=5,.clear_s=5,
            .min_expected=4,.min_missing=3,.missing_percent=50,.count=12};
        for(unsigned i=0;i<12;i++)base.entries[i]=(nr_entry_t){0,i+1,NR_SATELLITE,31};
        nrp_sample_t sample={.expectation_id=base.id,.utc=training};p.satellites_ms=1000+d;
        power_history_assess(&base,&p,training,1000+d,true,&sample);
    }
    persisted_count=persisted_power_count=0;connected=false;started=false;was_online=false;
    expectation=(nr_expectation_t){0};power_expectation=(nrp_expectation_t){0};machine=(nr_machine_t){0};
    local_power_machine=remote_power_machine=joint_power_machine=(nr_machine_t){0};
    latest=(nr_sample_t){0};latest_power=(nrp_event_t){0};next_report=last_sample_ms=last_power_sample_ms=0;
    fake_utc=1800000000;forecast(50);power_forecast(50,60,site_id);p.satellites_ms=1000;
    assert(reception_poll(&p,1000,NULL)==0&&last_tag==20);
    nrp_sample_t power_sample;assert(nrp_decode_sample(&power_sample,last_value,NRP_SAMPLE_SIZE));
    assert(power_sample.flags==3&&power_sample.local.valid[0]==0xff&&power_sample.remote.valid[0]==0xff);
    for(unsigned i=0;i<12;i++)p.satellites[i].cno=60;
    for(unsigned second=2;second<7;second++){fake_utc=1800000000+second-1;p.satellites_ms=second*1000;
        assert(reception_poll(&p,second*1000,NULL)==0);}
    fake_utc=1800000006;p.satellites_ms=7000;assert(reception_poll(&p,7000,NULL)==1);
    assert(last_tag==20&&nrp_decode_sample(&power_sample,last_value,NRP_SAMPLE_SIZE));
    assert(power_sample.observed[0]==60&&power_sample.local_alarm==1&&power_sample.remote_alarm==1&&
           persisted_power_count>=2&&persisted_power[persisted_power_count-1].joint_alarm==1);
    puts("edge reception: offline alarm, persistence, expiry, fresh recovery, snapshot retry and reconnect passed");
}
