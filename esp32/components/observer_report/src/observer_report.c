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
    return changed ? REPORT_CHANGE : 0;
}
void observer_report_sent(report_policy_t *p, const observer_report_t *r)
{ p->sent = true; p->sent_ms = r->uptime_ms; p->last = *r; }
size_t observer_report_encode(uint8_t *out, size_t cap, const observer_report_t *r)
{
    size_t fwlen = 0;
    while (fwlen < 32 && r->firmware[fwlen]) fwlen++;
    // Header 24; six fixed TLVs (14,17,16,40,29,29); firmware TLV; optional heater TLV (58).
    size_t length = 24 + 18 + 14 + 17 + 16 + 5 + NVF_BOARD_UID_SIZE + 29 + 29 + 3 + fwlen +
                    (r->heater.present ? 3 + 58 : 0);
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
#undef TLV
    return offset;
}
