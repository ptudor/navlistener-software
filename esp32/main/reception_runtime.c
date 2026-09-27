#include "reception_runtime.h"
#include "receiver.h"
#include "gnf1.h"
#include "journal.h"
#include "spool.h"
#include "pusher.h"
#include "esp_timer.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include <time.h>
#include <stdlib.h>

static portMUX_TYPE control_lock=portMUX_INITIALIZER_UNLOCKED;
static uint8_t incoming[NR_MAX_WIRE],incoming_request[20];
static size_t incoming_length;
static bool have_request;
static nr_expectation_t expectation;
static nr_machine_t machine;
static nr_sample_t latest;
static bool started,was_online;
static uint64_t accepted_ms,accepted_utc,next_report,last_sample_ms,upload_event;
static uint64_t request_id,request_deadline,request_ms,completed_request;
static uint8_t request_scopes;
static uint8_t completed_result[12],prepared_result[12];
static bool snapshot_needed;

void reception_control(uint8_t type,const uint8_t *b,size_t n)
{
    // The network task only queues bounded bytes; the board task owns analysis and I2C.
    taskENTER_CRITICAL(&control_lock);
    if(type==NR_F_EXPECTATION && n>=NR_HEADER && n<=sizeof incoming) {memcpy(incoming,b,n);incoming_length=n;}
    if(type==NR_F_SNAPSHOT && n==sizeof incoming_request) {memcpy(incoming_request,b,n);have_request=true;}
    taskEXIT_CRITICAL(&control_lock);
}
static uint64_t emit(unsigned tag,const uint8_t *bytes,size_t n,uint64_t now,uint64_t (*now_ns)(void))
{
    uint8_t body[128]={1,REPORT_CHANGE},record[128+GNF1_RECORD_HDR];
    if(n+27>sizeof body)return 0;
    nr_put(body+2,now,8);body[24]=tag;nr_put(body+25,n,2);memcpy(body+27,bytes,n);
    size_t length=gnf1_encode_telem(record,now_ns ? now_ns() : 0,GNF1_T_OBSERVER,body,n+27);
    return length ? spool_append(record,length) : 0;
}
uint8_t reception_poll(const gnss_status_t *status,uint64_t now,uint64_t (*now_ns)(void))
{
    if(!started) {if(journal_reception_latest(&latest)) machine.alarm=latest.alarm;started=true;}
    uint8_t bytes[NR_MAX_WIRE],request[20];size_t n;bool pending;
    taskENTER_CRITICAL(&control_lock);
    n=incoming_length;if(n)memcpy(bytes,incoming,n);incoming_length=0;
    pending=have_request;if(pending)memcpy(request,incoming_request,20);have_request=false;
    taskEXIT_CRITICAL(&control_lock);
    uint64_t utc=(uint64_t)time(NULL);
    if(n) {
        nr_expectation_t candidate;
        if(nr_decode(&candidate,bytes,n) && candidate.id!=expectation.id && candidate.issued>=expectation.issued &&
           utc>=candidate.issued && utc-candidate.issued<NR_SLOTS*NR_SLOT_S) {
            expectation=candidate;accepted_ms=now;accepted_utc=utc;
            ESP_LOGI("reception","forecast=%llu entries=%u expires=%llu",(unsigned long long)expectation.id,
                expectation.count,(unsigned long long)(expectation.issued+NR_SLOTS*NR_SLOT_S));
        }
    }
    if(pending && request[0]==1 && !request[2] && !request[3] && request[1] && !(request[1]&~7u)) {
        uint64_t id=nr_get(request+4,8),deadline=nr_get(request+12,8);
        if(id && id==completed_request && nr_get(completed_result+4,8)==id)
            emit(19,completed_result,sizeof completed_result,now,now_ns);
        if(id && id!=completed_request && id!=request_id && !snapshot_needed && deadline>utc && deadline-utc<=30) {
            request_id=id;request_deadline=now+(deadline-utc)*1000;request_scopes=request[1];request_ms=now;
            snapshot_needed=true;receiver_request_snapshot();
        }
    }
    if(snapshot_needed && request_id && now>=request_deadline) {
        uint8_t result[12]={1,3};nr_put(result+4,request_id,8);
        if(emit(19,result,sizeof result,now,now_ns)) {
            memcpy(prepared_result,result,sizeof result);reception_snapshot_queued();
        }
    }
    nr_sample_t sample={.expectation_id=expectation.id,.utc=accepted_utc+(now-accepted_ms)/1000,.uptime_ms=now,
        .boot=latest.boot,.event=latest.event};
    if(!expectation.id)sample.utc=utc>=946684800 ? utc : 0;
    gnss_status_compare(status,&expectation,&sample,(int64_t)now);
    // Monotonic elapsed time owns validity after receipt. A large clock change invalidates the model.
    if(!expectation.id || llabs((long long)sample.utc-(long long)utc)>5) sample.valid=0;
    uint8_t bad=nr_counts(&expectation,&sample);
    uint64_t measured_ms=(uint64_t)status->satellites_ms;
    for(unsigned i=0;i<expectation.count;i++) if(expectation.entries[i].signal!=NR_SATELLITE) {measured_ms=(uint64_t)status->signals_ms;break;}
    // The collector can distinguish a new measurement from a repeated status report.
    sample.uptime_ms=measured_ms;
    machine.pending&=sample.valid;
    if(!sample.valid) nr_step(&machine,0,0,now,expectation.alarm_s,expectation.clear_s);
    else if(measured_ms!=last_sample_ms) {nr_step(&machine,sample.valid,bad,now,expectation.alarm_s,expectation.clear_s);last_sample_ms=measured_ms;}
    sample.alarm=machine.alarm;
    if(sample.alarm!=latest.alarm || sample.valid!=latest.valid) {
        if(sample.alarm!=latest.alarm && !snapshot_needed) {snapshot_needed=true;request_ms=now;request_id=0;request_scopes=7;request_deadline=now+5000;receiver_request_snapshot();}
        bool saved=journal_reception_save(&sample);
        ESP_LOGW("reception","alarm=0x%02x coverage=0x%02x GPS=%u/%u forecast=%llu journal=%s",
            sample.alarm,sample.valid,sample.observed[0],sample.expected[0],(unsigned long long)sample.expectation_id,saved ? "saved" : "unavailable");
    }
    latest=sample;
    bool online=pusher_connected();if(online&&!was_online)upload_event=0;was_online=online;
    if(now>=next_report) {
        next_report=now+1000;uint8_t wire[NR_SAMPLE_SIZE];
        if(expectation.id || latest.event || latest.alarm) {nr_encode_sample(wire,&latest);emit(17,wire,sizeof wire,now,now_ns);}
        nr_sample_t event;
        if(online && journal_reception_next(upload_event,&event)) {
            nr_encode_sample(wire,&event);if(emit(18,wire,sizeof wire,now,now_ns))upload_event=event.event;
        }
    }
    return machine.alarm;
}
bool reception_snapshot_due(uint64_t now)
{return snapshot_needed && now>=request_ms+1000 && (!request_id || now<request_deadline);}
size_t reception_snapshot_append(uint8_t *body,size_t length,size_t cap,const gnss_status_t *status,uint64_t now)
{
    if(!snapshot_needed || !request_id || !length)return length;
    if(length+15>cap)return 0;
    uint8_t complete=request_scopes&1; // a fresh board conversion was attempted; validity remains in its sensor fields
    if(status->satellites_valid && (uint64_t)status->satellites_ms>=request_ms && now>=(uint64_t)status->satellites_ms && now-status->satellites_ms<=15000)complete|=request_scopes&2;
    if(status->rf_valid && (uint64_t)status->rf_ms>=request_ms && now>=(uint64_t)status->rf_ms && now-status->rf_ms<=15000)complete|=request_scopes&4;
    body[length]=19;nr_put(body+length+1,12,2);uint8_t *v=body+length+3;memset(v,0,12);v[0]=1;
    v[1]=now>request_deadline ? 3 : complete==request_scopes ? 1 : 2;v[2]=complete;nr_put(v+4,request_id,8);
    memcpy(prepared_result,v,12);return length+15;
}
void reception_snapshot_queued(void)
{
    if(snapshot_needed) {
        if(request_id){completed_request=request_id;memcpy(completed_result,prepared_result,12);}
        snapshot_needed=false;request_id=0;
    }
}
