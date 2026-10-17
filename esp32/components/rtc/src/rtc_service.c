#include "rtc_service.h"
#include "rtc_max31328.h"
#include <string.h>

static bool snapshot(const rtc_io_t *io,bool tcxo,uint8_t calendar[7],uint8_t *control,uint8_t *status,int64_t *epoch,bool *valid)
{
    if (!io->read(io->ctx,0,calendar,7)) return false;
    if (tcxo) {
        uint8_t state[2];if(!io->read(io->ctx,MAX31328_CONTROL,state,2))return false;
        *control=state[0];*status=state[1];*valid=max31328_running(calendar,*control,*status,epoch);
        if(!*valid)(void)max31328_decode(calendar,epoch);
    } else {
        if (!io->read(io->ctx,7,control,1))return false;
        *status=calendar[3];*valid=rtc_running(calendar,epoch);
        if(!*valid)(void)rtc_decode(calendar,epoch);
    }
    return true;
}
unsigned rtc_service_execute(const rtc_io_t *io,bool tcxo,const rtc_service_request_t *req,rtc_service_save_t save,void *ctx,rtc_service_record_t *r)
{
    *r=(rtc_service_record_t){.version=1,.request=*req,.started_ms=io->now_ms(io->ctx)};
    r->old_read=snapshot(io,tcxo,r->old_calendar,&r->old_control,&r->old_status,&r->old_epoch,&r->old_valid);
    bool fresh=req->reference.sampled_ms>=0 && r->started_ms>=req->reference.sampled_ms && r->started_ms-req->reference.sampled_ms<=2000;
    uint8_t encoded[7];
    bool valid_reference=req->verify_only ? req->source==OBS_TIME_RTC :
        ((req->source==OBS_TIME_GNSS || req->source==OBS_TIME_FACTORY) && fresh && req->reference.utc_ns>=946684800LL*1000000000LL && req->reference.utc_ns<4102444800LL*1000000000LL && req->uncertainty_ns<=100000000 && rtc_encode(rtc_ref_second(&req->reference,r->started_ms),encoded));
    if (!valid_reference || !req->reason[0] || !memchr(req->reason,0,sizeof req->reason) || !req->reference_id[0] || !memchr(req->reference_id,0,sizeof req->reference_id)) r->result=RTC_SERVICE_BAD_REFERENCE;
    else if (!r->old_read) r->result=RTC_SERVICE_READ_FAILED;
    if (!save || !save(ctx,r)) {r->result=RTC_SERVICE_AUDIT_FAILED;return r->result;}
    if (r->result==RTC_SERVICE_PENDING) {
        if(req->verify_only) {
            r->result=r->old_valid?RTC_SERVICE_OK:RTC_SERVICE_VERIFY_FAILED;
            if(r->old_valid)io->delay(io->ctx,1200);
        } else if (tcxo) {
            unsigned result=max31328_set_verified(io,&req->reference,&r->written_epoch);
            r->result=result==MAX31328_SET_OK?RTC_SERVICE_OK:result==MAX31328_SET_OSF_STUCK?RTC_SERVICE_OSF_STUCK:RTC_SERVICE_WRITE_FAILED;
        } else r->result=rtc_set_verified(io,&req->reference,&r->written_epoch)?RTC_SERVICE_OK:RTC_SERVICE_WRITE_FAILED;
    }
    r->readback_read=snapshot(io,tcxo,r->readback_calendar,&r->readback_control,&r->readback_status,&r->readback_epoch,&r->readback_valid);
    if (r->result==RTC_SERVICE_OK && (!r->readback_read || !r->readback_valid)) r->result=req->verify_only?RTC_SERVICE_VERIFY_FAILED:RTC_SERVICE_WRITE_FAILED;
    if(req->verify_only && r->result==RTC_SERVICE_OK && (r->readback_epoch<=r->old_epoch || r->readback_epoch>r->old_epoch+3))r->result=RTC_SERVICE_VERIFY_FAILED;
    r->completed_ms=io->now_ms(io->ctx);r->audit_complete=true;
    if (!save(ctx,r)) {r->audit_complete=false;r->result=RTC_SERVICE_AUDIT_FAILED;}
    return r->result;
}
