#include "pulse_stats.h"
#include <stdlib.h>

static void discontinuity(pulse_channel_t *c)
{
    c->report.discontinuities++;
    c->report.span_ticks=0; c->report.span_intervals=0;
    c->report.flags &= ~(TIMING_PERIOD_VALID|TIMING_WIDTH_VALID|TIMING_SPAN_VALID);
    c->have_rise=false; c->high=false;
}
void pulse_stats_loss(pulse_stats_t *s)
{ for (unsigned i=0;i<2;i++) discontinuity(&s->channel[i]); }
static bool delta(const pulse_stats_t *s, uint32_t tick, uint32_t previous,
                  uint64_t now, uint64_t then, uint32_t *ticks)
{
    // Unsigned hardware subtraction handles one wrap. The coarse ISR arrival
    // clock rejects ambiguous multi-wrap gaps and excessive delivery latency;
    // it is never used as the precision edge timestamp.
    if (!s->hz || now <= then || now-then > 10000000) return false;
    *ticks=tick-previous;
    uint64_t nominal=(now-then)*s->hz/1000000;
    return nominal < UINT32_MAX && llabs((long long)*ticks-(long long)nominal) <= s->hz/100;
}
void pulse_stats_edge(pulse_stats_t *s, unsigned channel, bool rising, uint32_t tick, uint64_t now)
{
    if (channel > 1 || !s->hz) return;
    pulse_channel_t *c=&s->channel[channel]; uint32_t ticks;
    if (!rising) {
        if (c->high && delta(s,tick,c->high_tick,now,c->high_us,&ticks) && ticks && ticks < s->hz*3u/2u) {
            c->report.width_ticks=ticks; c->report.flags |= TIMING_WIDTH_VALID;
        } else c->report.flags &= ~TIMING_WIDTH_VALID;
        c->high=false; return;
    }
    c->report.captured++;
    if (c->have_rise) {
        bool valid=delta(s,tick,c->rise_tick,now,c->rise_us,&ticks);
        uint64_t gap=now > c->rise_us ? now-c->rise_us : 0;
        if (gap >= 1500000) c->report.missing_estimate+=(gap+500000)/1000000-1;
        if (valid && ticks >= s->hz/2 && ticks < s->hz*3u/2u) {
            c->report.period_ticks=ticks; c->report.flags |= TIMING_PERIOD_VALID|TIMING_SPAN_VALID;
            c->report.span_ticks+=ticks; c->report.span_intervals++;
            if (!c->report.min_ticks || ticks < c->report.min_ticks) c->report.min_ticks=ticks;
            if (ticks > c->report.max_ticks) c->report.max_ticks=ticks;
        } else discontinuity(c);
    }
    c->rise_tick=tick; c->rise_us=now; c->have_rise=true;
    c->high_tick=tick; c->high_us=now; c->high=true;
    c->report.last_rise_ms=now/1000;
}
void pulse_stats_counter(pulse_stats_t *s, unsigned channel, uint32_t raw, uint64_t now)
{
    if (channel > 1) return;
    pulse_channel_t *c=&s->channel[channel];
    if (raw >= PULSE_COUNTER_MODULUS) {
        if (c->report.flags & TIMING_COUNT_VALID) c->report.counter_discontinuities++;
        c->report.flags &= ~TIMING_COUNT_VALID;
        return;
    }
    // Polling does not clear a live hardware counter. At nominal 1 Hz the
    // 30,000-count hardware rollover is far longer than the board-task period.
    if (c->counter_started && (now < c->counter_us || now-c->counter_us >= UINT64_C(30000000000))) {
        c->report.counter_discontinuities++; c->report.flags &= ~TIMING_COUNT_VALID;
    } else c->report.physical+=(raw+PULSE_COUNTER_MODULUS-c->counter_raw)%PULSE_COUNTER_MODULUS;
    c->counter_raw=raw; c->counter_us=now; c->counter_started=true;
}
void pulse_stats_snapshot(const pulse_stats_t *s, uint64_t now, report_timing_t *out)
{
    out->flags &= ~1u; out->rtc_minus_gnss_ticks=0;
    for (unsigned i=0;i<2;i++) {
        const pulse_channel_t *c=&s->channel[i]; out->channel[i]=c->report;
        if (c->have_rise && now >= c->rise_us && now-c->rise_us <= 1500000)
            out->channel[i].flags |= TIMING_FRESH;
        else out->channel[i].flags &= ~(TIMING_FRESH|TIMING_PERIOD_VALID|TIMING_WIDTH_VALID|TIMING_SPAN_VALID);
        if (!(out->channel[i].flags & TIMING_PERIOD_VALID)) out->channel[i].period_ticks=0;
        if (!(out->channel[i].flags & TIMING_WIDTH_VALID)) out->channel[i].width_ticks=0;
    }
    if (s->hz && (out->channel[0].flags & (TIMING_FRESH|TIMING_PERIOD_VALID)) == (TIMING_FRESH|TIMING_PERIOD_VALID) &&
        (out->channel[1].flags & (TIMING_FRESH|TIMING_PERIOD_VALID)) == (TIMING_FRESH|TIMING_PERIOD_VALID)) {
        int64_t phase=(int32_t)(s->channel[1].rise_tick-s->channel[0].rise_tick);
        phase %= s->hz;
        if (phase >= (int64_t)s->hz/2) phase-=s->hz;
        if (phase < -(int64_t)s->hz/2) phase+=s->hz;
        out->rtc_minus_gnss_ticks=phase; out->flags |= 1;
    }
}
