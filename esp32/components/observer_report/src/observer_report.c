#include "observer_report.h"
#include "gnf1.h"
#include <stdlib.h>
#include <string.h>
size_t observer_timing_encode(uint8_t *out, size_t cap, const report_timing_t *r, uint64_t uptime)
{
    size_t length=24+3+TIMING_WIRE_SIZE;
    if (!out || !r->present || cap < length) return 0;
    memset(out,0,length); out[0]=1; out[1]=REPORT_CHECKIN; gnf1_be64(out+2,uptime);
    out[24]=8; gnf1_be16(out+25,TIMING_WIRE_SIZE); uint8_t *b=out+27;
    b[0]=1; b[1]=r->clock; b[2]=r->rtc_state; b[3]=r->rtc_control; b[4]=r->rtc_trim;
    b[5]=r->flags; b[6]=r->tp_flags; b[7]=r->tp_ref;
    gnf1_be32(b+8,r->resolution_hz); gnf1_be32(b+12,r->queue_dropped);
    gnf1_be64(b+16,r->started_ms); gnf1_be32(b+24,r->rtc_minus_gnss_ticks); gnf1_be64(b+28,r->tp_ms);
    for (unsigned i=0;i<2;i++) {
        uint8_t *p=b+36+80*i; const timing_channel_report_t *c=&r->channel[i];
        gnf1_be32(p,c->flags); gnf1_be32(p+4,c->period_ticks); gnf1_be32(p+8,c->width_ticks);
        gnf1_be32(p+12,c->min_ticks); gnf1_be32(p+16,c->max_ticks); gnf1_be32(p+20,c->discontinuities);
        gnf1_be64(p+24,c->missing_estimate); gnf1_be64(p+32,c->captured); gnf1_be64(p+40,c->physical);
        gnf1_be64(p+48,c->span_ticks); gnf1_be64(p+56,c->span_intervals); gnf1_be64(p+64,c->last_rise_ms);
        gnf1_be32(p+72,c->counter_discontinuities);
    }
    return length;
}
uint8_t observer_report_due(const report_policy_t *p, const observer_report_t *r)
{
    if (!p->sent) return REPORT_BOOT;
    if (r->uptime_ms < p->sent_ms) return 0;
    uint64_t elapsed = r->uptime_ms - p->sent_ms;
    if (r->event_count != p->last.event_count && elapsed >= 5000) return REPORT_INTERFERENCE;
    if (elapsed >= 300000) return REPORT_CHECKIN;
    if (elapsed < 60000) return 0;
    const report_environment_t *a = &p->last.environment, *b = &r->environment;
    bool changed = a->valid != b->valid || a->ready != b->ready ||
        ((b->valid & 1) && abs(a->mcp_centi_c - b->mcp_centi_c) >= 50) ||
        ((b->valid & 2) && (abs(a->hdc_centi_c - b->hdc_centi_c) >= 50 ||
                            abs(a->rh_centi_percent - b->rh_centi_percent) >= 200)) ||
        ((b->valid & 4) && (abs(a->bmp_centi_c - b->bmp_centi_c) >= 50 ||
                            llabs((long long)a->pressure_pa - b->pressure_pa) >= 100)) ||
        p->last.heater.state != r->heater.state || p->last.heater.runs != r->heater.runs ||
        p->last.rtc.flags != r->rtc.flags;
    const report_barometer_t *ba = &p->last.barometer, *bb = &r->barometer;
    changed = changed || ba->present != bb->present || ba->state != bb->state || ba->valid != bb->valid ||
        ba->flags != bb->flags || (bb->valid && (abs(ba->centi_c - bb->centi_c) >= 50 ||
                                                 llabs((long long)ba->pressure_pa - bb->pressure_pa) >= 100));
    const report_thermocouple_t *ta = &p->last.thermocouple, *tb = &r->thermocouple;
    changed = changed || ta->present != tb->present || ta->state != tb->state || ta->valid != tb->valid ||
        ta->fault != tb->fault || ((tb->valid & 1) && llabs((long long)ta->tc_centi_c - tb->tc_centi_c) >= 100) ||
        ((tb->valid & 2) && abs(ta->cj_centi_c - tb->cj_centi_c) >= 50);
    const report_motion_t *ma = &p->last.motion, *mb = &r->motion;
    changed = changed || ma->present != mb->present || ma->imu_state != mb->imu_state ||
        ma->mag_state != mb->mag_state || (ma->valid & 5) != (mb->valid & 5) || ma->moving != mb->moving ||
        ma->overflows != mb->overflows || ma->resyncs != mb->resyncs;
    return changed ? REPORT_CHANGE : 0;
}
void observer_report_sent(report_policy_t *p, const observer_report_t *r)
{ p->sent = true; p->sent_ms = r->uptime_ms; p->last = *r; }
size_t observer_report_encode(uint8_t *out, size_t cap, const observer_report_t *r)
{
    size_t fwlen = 0;
    while (fwlen < 32 && r->firmware[fwlen]) fwlen++;
    // Header 24; six fixed TLVs (14,17,16,40,29,29); firmware TLV; optional heater (58),
    // barometer (10), thermocouple (12), motion (95) and humidity-sensor (2) TLVs.
    size_t length = 24 + 18 + 14 + 17 + 16 + 5 + NVF_BOARD_UID_SIZE + 29 + 29 + 3 + fwlen +
                    (r->heater.present ? 3 + 58 : 0) + (r->barometer.present ? 3 + 10 : 0) +
                    (r->thermocouple.present ? 3 + 12 : 0) + (r->motion.present ? 3 + 95 : 0) +
                    (r->humidity.present ? 3 + 2 : 0);
    if (!out || cap < length) return 0;
    memset(out, 0, length);
    out[0] = 1; out[1] = r->reason; gnf1_be64(out+2, r->uptime_ms);
    gnf1_be32(out+10, r->event_count); gnf1_be64(out+14, r->event_ms);
    out[22] = r->event_flags; out[23] = r->event_states;
    size_t offset = 24;
#define TLV(tag, n) out[offset] = (tag); gnf1_be16(out+offset+1, (n)); uint8_t *b = out+offset+3; offset += 3+(n)
    { TLV(1, 14); const report_environment_t *e = &r->environment;
      b[0]=e->valid; b[1]=e->ready; gnf1_be16(b+2,e->mcp_centi_c);
      gnf1_be16(b+4,e->hdc_centi_c); gnf1_be16(b+6,e->bmp_centi_c);
      gnf1_be16(b+8,e->rh_centi_percent); gnf1_be32(b+10,e->pressure_pa); }
    { TLV(2, 17); b[0]=r->rtc.flags; gnf1_be64(b+1,r->rtc.epoch); gnf1_be64(b+9,r->rtc.sampled_ms); }
    { TLV(3, 16); const report_crypto_t *c=&r->crypto;
      gnf1_be64(b,c->checked_ms); b[8]=c->revision_valid; memcpy(b+9,c->revision,4);
      b[13]=c->config_lock; b[14]=c->data_lock; b[15]=c->rng; }
    { TLV(4, 5+NVF_BOARD_UID_SIZE); const report_manifest_t *m=&r->manifest;
      b[0]=m->action; b[1]=m->uid_valid; memcpy(b+2,m->board_uid,NVF_BOARD_UID_SIZE);
      b[2+NVF_BOARD_UID_SIZE]=m->capabilities_valid; b[3+NVF_BOARD_UID_SIZE]=m->revision;
      b[4+NVF_BOARD_UID_SIZE]=m->component_count; }
    { TLV(5, 29); const report_resources_t *s=&r->resources;
      b[0]=s->psram; gnf1_be32(b+1,s->used); gnf1_be32(b+5,s->capacity);
      gnf1_be32(b+9,s->queued); gnf1_be64(b+13,s->dropped);
      gnf1_be32(b+21,s->internal_free); gnf1_be32(b+25,s->psram_free); }
    { TLV(6, 29); const report_receiver_t *s=&r->receiver;
      b[0]=s->supported; b[1]=s->expected; memcpy(b+2,s->tracked,8);
      b[10]=s->valid; b[11]=s->jam; b[12]=s->spoof;
      gnf1_be64(b+13,s->rf_ms); gnf1_be64(b+21,s->status_ms); }
    { TLV(7, fwlen); memcpy(b,r->firmware,fwlen); }
    if (r->heater.present) { TLV(10, 58); const report_heater_t *h=&r->heater;
      b[0]=1; b[1]=h->state; b[2]=h->flags; b[3]=h->runs;
      gnf1_be32(b+4,h->rh95_s); gnf1_be32(b+8,h->rh98_s); gnf1_be32(b+12,h->streak_s);
      gnf1_be64(b+16,h->last_utc); gnf1_be64(b+24,h->start_ms);
      gnf1_be32(b+32,h->on_ms); gnf1_be32(b+36,h->recovery_ms); b[40]=h->stop; b[41]=h->valid;
      gnf1_be16(b+42,h->rh_before); gnf1_be16(b+44,h->rh_stop);
      gnf1_be16(b+46,h->hdc_before); gnf1_be16(b+48,h->hdc_peak); gnf1_be16(b+50,h->hdc_end);
      gnf1_be16(b+52,h->mcp_before); gnf1_be16(b+54,h->mcp_peak); gnf1_be16(b+56,h->mcp_end); }
    if (r->barometer.present) { TLV(11, 10); const report_barometer_t *m=&r->barometer;
      b[0]=1; b[1]=m->state; b[2]=m->valid; b[3]=m->flags;
      gnf1_be16(b+4,m->centi_c); gnf1_be32(b+6,m->pressure_pa); }
    if (r->thermocouple.present) { TLV(12, 12); const report_thermocouple_t *t=&r->thermocouple;
      b[0]=1; b[1]=t->state; b[2]=t->valid; b[3]=t->flags; b[4]=t->fault; b[5]=t->config;
      gnf1_be32(b+6,t->tc_centi_c); gnf1_be16(b+10,t->cj_centi_c); }
    if (r->motion.present) { TLV(13, 95); const report_motion_t *m=&r->motion;
      b[0]=1; b[1]=m->imu_state; b[2]=m->mag_state; b[3]=m->valid; b[4]=m->profile; b[5]=m->moving;
      gnf1_be16(b+6,m->rate_decihz); b[8]=m->accel_fs_g; gnf1_be16(b+9,m->gyro_fs_dps);
      for (unsigned i=0;i<3;i++) { gnf1_be16(b+11+2*i,m->accel[i]); gnf1_be16(b+17+2*i,m->gyro[i]); }
      gnf1_be16(b+23,m->imu_centi_c);
      gnf1_be32(b+25,m->packets); gnf1_be32(b+29,m->overflows); gnf1_be32(b+33,m->resyncs);
      gnf1_be32(b+37,m->rate_changes); gnf1_be32(b+41,m->window_samples); gnf1_be32(b+45,m->window_ms);
      for (unsigned i=0;i<3;i++) { gnf1_be16(b+49+2*i,m->accel_mean[i]); gnf1_be16(b+55+2*i,m->gyro_mean[i]); }
      gnf1_be16(b+61,m->accel_min_mg); gnf1_be16(b+63,m->accel_max_mg); gnf1_be16(b+65,m->gyro_max_decidps);
      gnf1_be64(b+67,m->imu_ms);
      for (unsigned i=0;i<3;i++) { gnf1_be16(b+75+2*i,m->mag[i]); gnf1_be16(b+81+2*i,m->mag_offset[i]); }
      gnf1_be64(b+87,m->mag_ms); }
    if (r->humidity.present) { TLV(14, 2); b[0]=1; b[1]=r->humidity.part; }
#undef TLV
    return offset;
}
