#include "reception_runtime.h"
#include "power_history.h"
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
static uint8_t incoming[NR_MAX_WIRE],incoming_power[NRP_MAX_WIRE],incoming_request[20];
static size_t incoming_length,incoming_power_length;
static bool have_request;
static nr_expectation_t expectation;
static nrp_expectation_t power_expectation;
static nr_machine_t machine;
static nr_machine_t local_power_machine,remote_power_machine,joint_power_machine;
static nr_sample_t latest;
static nrp_event_t latest_power;
static bool started,was_online;
static uint64_t accepted_ms,accepted_utc,next_report,last_sample_ms,last_power_sample_ms,upload_event,upload_power_event;
static uint64_t request_id,request_deadline,request_ms,completed_request;
static uint8_t request_scopes;
static uint8_t completed_result[12],prepared_result[12];
static bool snapshot_needed;

void reception_control(uint8_t type,const uint8_t *b,size_t n)
{
    // The network task only queues bounded bytes; the board task owns analysis and I2C.
    taskENTER_CRITICAL(&control_lock);
    if(type==NR_F_EXPECTATION && n>=NR_HEADER && n<=sizeof incoming) {memcpy(incoming,b,n);incoming_length=n;}
    if(type==NRP_F_EXPECTATION && n>=NRP_HEADER && n<=sizeof incoming_power) {memcpy(incoming_power,b,n);incoming_power_length=n;}
    if(type==NR_F_SNAPSHOT && n==sizeof incoming_request) {memcpy(incoming_request,b,n);have_request=true;}
    taskEXIT_CRITICAL(&control_lock);
}
static uint64_t emit(unsigned tag,const uint8_t *bytes,size_t n,uint64_t now,uint64_t (*now_ns)(void))
{
    uint8_t body[384]={1,REPORT_CHANGE},record[384+GNF1_RECORD_HDR];
    if(n+27>sizeof body)return 0;
    nr_put(body+2,now,8);body[24]=tag;nr_put(body+25,n,2);memcpy(body+27,bytes,n);
    size_t length=gnf1_encode_telem(record,now_ns ? now_ns() : 0,GNF1_T_OBSERVER,body,n+27);
    return length ? spool_append(record,length) : 0;
}
static uint8_t assessment_constellations(const nr_expectation_t *base,const uint8_t bits[16])
{
    uint8_t out=0;for(unsigned i=0;i<base->count;i++)if(bits[i/8]&(1u<<(i%8)))out|=1u<<base->entries[i].gnss;
    return out;
}
static bool power_event_changed(const nrp_event_t *a,const nrp_event_t *b)
{
    // Model and expectation IDs are captured with a material state transition,
    // but routine reference refreshes must not consume the flash event FIFO.
    return a->flags!=b->flags||a->local_valid!=b->local_valid||a->local_alarm!=b->local_alarm||
        a->remote_valid!=b->remote_valid||a->remote_alarm!=b->remote_alarm||a->joint_valid!=b->joint_valid||
        a->joint_alarm!=b->joint_alarm||a->model_conflict!=b->model_conflict;
}
static nrp_sample_t assess_power(const gnss_status_t *status,uint64_t utc,uint64_t now,
                                 uint64_t measurement_ms,bool fresh_measurement)
{
    nrp_sample_t sample={.expectation_id=expectation.id,.utc=utc,.uptime_ms=measurement_ms};
    bool remote=power_expectation.expectation_id==expectation.id&&power_expectation.issued==expectation.issued&&
        power_expectation.count==expectation.count;
    if(remote){sample.flags|=NRP_FLAG_REMOTE;sample.remote_model_id=power_expectation.model_id;}
    power_history_assess(&expectation,status,utc,now,fresh_measurement,&sample);
    if(remote)sample.remote=nrp_compare(&power_expectation,&sample);
    uint8_t modeled[8],anomalous[8],local_bad=0,remote_bad=0,joint_bad=0;
    if(sample.flags&NRP_FLAG_LOCAL)local_bad=nrp_counts(&expectation,sample.count,&sample.local,modeled,anomalous,&sample.local_valid);
    if(sample.flags&NRP_FLAG_REMOTE)remote_bad=nrp_counts(&expectation,sample.count,&sample.remote,modeled,anomalous,&sample.remote_valid);
    nrp_assessment_t joint={0},conflict={0};
    if((sample.flags&(NRP_FLAG_LOCAL|NRP_FLAG_REMOTE))==(NRP_FLAG_LOCAL|NRP_FLAG_REMOTE))for(unsigned i=0;i<16;i++) {
        joint.valid[i]=sample.local.valid[i]&sample.remote.valid[i];
        joint.bad[i]=sample.local.bad[i]&sample.remote.bad[i]&joint.valid[i];
        conflict.bad[i]=(sample.local.bad[i]^sample.remote.bad[i])&joint.valid[i];
    }
    uint8_t joint_valid=0;joint_bad=nrp_counts(&expectation,sample.count,&joint,modeled,anomalous,&joint_valid);
    local_power_machine.pending&=sample.local_valid;remote_power_machine.pending&=sample.remote_valid;
    joint_power_machine.pending&=joint_valid;
    if(fresh_measurement) {
        nr_step(&local_power_machine,sample.local_valid,local_bad,now,expectation.alarm_s,expectation.clear_s);
        nr_step(&remote_power_machine,sample.remote_valid,remote_bad,now,expectation.alarm_s,expectation.clear_s);
        nr_step(&joint_power_machine,joint_valid,joint_bad,now,expectation.alarm_s,expectation.clear_s);
    }
    sample.local_alarm=sample.flags&NRP_FLAG_LOCAL?local_power_machine.alarm:0;
    sample.remote_alarm=sample.flags&NRP_FLAG_REMOTE?remote_power_machine.alarm:0;
    nrp_event_t event={.flags=sample.flags,.local_valid=sample.local_valid,.local_alarm=sample.local_alarm,
        .remote_valid=sample.remote_valid,.remote_alarm=sample.remote_alarm,.joint_valid=joint_valid,
        .joint_alarm=joint_power_machine.alarm,.expectation_id=sample.expectation_id,
        .remote_model_id=sample.remote_model_id,.local_model_id=sample.local_model_id,.utc=utc,.uptime_ms=sample.uptime_ms,
        .local_abnormal=local_bad,.remote_abnormal=remote_bad,.joint_abnormal=joint_bad,
        .model_conflict=assessment_constellations(&expectation,conflict.bad)};
    if(sample.flags&&power_event_changed(&event,&latest_power)) {
        bool alarm_changed=event.joint_alarm!=latest_power.joint_alarm;
        bool saved=journal_power_save(&event);latest_power=event;
        ESP_LOGW("reception","power alarm=0x%02x local=0x%02x remote=0x%02x conflict=0x%02x journal=%s",
            event.joint_alarm,event.local_alarm,event.remote_alarm,event.model_conflict,saved?"saved":"unavailable");
        if(alarm_changed&&!snapshot_needed){snapshot_needed=true;request_ms=now;request_id=0;request_scopes=7;request_deadline=now+5000;receiver_request_snapshot();}
    }
    return sample;
}
uint8_t reception_poll(const gnss_status_t *status,uint64_t now,uint64_t (*now_ns)(void))
{
    if(!started) {
        if(journal_reception_latest(&latest))machine.alarm=latest.alarm;
        if(journal_power_latest(&latest_power)){local_power_machine.alarm=latest_power.local_alarm;
            remote_power_machine.alarm=latest_power.remote_alarm;joint_power_machine.alarm=latest_power.joint_alarm;}
        started=true;
    }
    // Static: the board task is the only caller. The wire copies and the two decoded
    // candidates below are 5.3 KB together, more than a task stack should carry; the
    // decoders' own temporaries (nrp_decode alone is 2 KB on xtensa) stay on the stack.
    static uint8_t bytes[NR_MAX_WIRE],power_bytes[NRP_MAX_WIRE],request[20];size_t n,power_n;bool pending;
    taskENTER_CRITICAL(&control_lock);
    n=incoming_length;if(n)memcpy(bytes,incoming,n);incoming_length=0;
    power_n=incoming_power_length;if(power_n)memcpy(power_bytes,incoming_power,power_n);incoming_power_length=0;
    pending=have_request;if(pending)memcpy(request,incoming_request,20);have_request=false;
    taskEXIT_CRITICAL(&control_lock);
    uint64_t utc=(uint64_t)time(NULL);
    if(n) {
        static nr_expectation_t candidate;
        if(nr_decode(&candidate,bytes,n) && candidate.id!=expectation.id && candidate.issued>=expectation.issued &&
           utc>=candidate.issued && utc-candidate.issued<NR_SLOTS*NR_SLOT_S) {
            expectation=candidate;accepted_ms=now;accepted_utc=utc;
            if(power_expectation.expectation_id!=candidate.id)power_expectation=(nrp_expectation_t){0};
            ESP_LOGI("reception","forecast=%llu entries=%u expires=%llu",(unsigned long long)expectation.id,
                expectation.count,(unsigned long long)(expectation.issued+NR_SLOTS*NR_SLOT_S));
        }
    }
    if(power_n) {
        static nrp_expectation_t candidate;
        if(nrp_decode(&candidate,power_bytes,power_n)&&candidate.expectation_id==expectation.id&&
           candidate.issued==expectation.issued&&candidate.count==expectation.count&&utc>=candidate.issued&&
           utc-candidate.issued<NR_SLOTS*NR_SLOT_S) {
            power_expectation=candidate;(void)power_history_bind(candidate.site_id,candidate.min_deviation,
                candidate.mad_multiplier,candidate.min_support);
            ESP_LOGI("reception","power model=%llu site=%llu entries=%u",(unsigned long long)candidate.model_id,
                (unsigned long long)candidate.site_id,candidate.count);
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
    bool power_measurement=measured_ms!=last_power_sample_ms;nrp_sample_t power={0};
    if(expectation.id&&(power_expectation.expectation_id==expectation.id||power_history_model_id())) {
        power=assess_power(status,sample.utc,now,measured_ms,power_measurement);if(power_measurement)last_power_sample_ms=measured_ms;
    }
    power_history_checkpoint(now);
    bool online=pusher_connected();if(online&&!was_online){upload_event=0;upload_power_event=0;}was_online=online;
    if(now>=next_report) {
        next_report=now+1000;uint8_t wire[NR_SAMPLE_SIZE];
        if(expectation.id || latest.event || latest.alarm) {nr_encode_sample(wire,&latest);emit(17,wire,sizeof wire,now,now_ns);}
        if(power.flags) {uint8_t power_wire[NRP_SAMPLE_SIZE];nrp_encode_sample(power_wire,&power);emit(20,power_wire,sizeof power_wire,now,now_ns);}
        nr_sample_t event;
        if(online && journal_reception_next(upload_event,&event)) {
            nr_encode_sample(wire,&event);if(emit(18,wire,sizeof wire,now,now_ns))upload_event=event.event;
        }
        nrp_event_t power_event;
        if(online&&journal_power_next(upload_power_event,&power_event)) {
            uint8_t event_wire[NRP_EVENT_SIZE];nrp_encode_event(event_wire,&power_event);
            if(emit(21,event_wire,sizeof event_wire,now,now_ns))upload_power_event=power_event.event;
        }
    }
    return machine.alarm|joint_power_machine.alarm;
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
