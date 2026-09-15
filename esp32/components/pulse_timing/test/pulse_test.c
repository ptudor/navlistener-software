#include "pulse_stats.h"
#include <assert.h>
#include <stdio.h>

static void daily(void)
{
    pulse_stats_t s={.hz=80000000}; report_timing_t out={0};
    for (unsigned c=0;c<2;c++) s.channel[c].report.flags=TIMING_ENABLED|TIMING_COUNT_VALID;
    for (uint64_t second=1;second<=86400;second++) {
        // +1 ppm and +2 ppm period errors relative to APB. More than 1,600
        // capture-counter wraps and two hardware pulse-counter wraps per day.
        for (unsigned c=0;c<2;c++) {
            uint64_t tick=UINT64_C(4000000000)+second*(80000080u+80u*c)+c*8000000u;
            uint64_t us=second*1000000+c*100000;
            pulse_stats_edge(&s,c,true,(uint32_t)tick,us);
            pulse_stats_edge(&s,c,false,(uint32_t)(tick+40000000),us+500000);
            pulse_stats_counter(&s,c,second%PULSE_COUNTER_MODULUS,us+500001);
        }
    }
    pulse_stats_snapshot(&s,UINT64_C(86400)*1000000+600001,&out);
    for (unsigned c=0;c<2;c++) {
        assert(out.channel[c].captured==86400 && out.channel[c].physical==86400);
        assert(out.channel[c].span_intervals==86399);
        assert(out.channel[c].span_ticks==UINT64_C(86399)*(80000080u+80u*c));
        assert(out.channel[c].width_ticks==40000000);
        assert(out.channel[c].discontinuities==0 && out.channel[c].missing_estimate==0);
        assert(out.channel[c].flags==63);
    }
    assert((out.flags&1) && out.rtc_minus_gnss_ticks==8000000+86400*80);
}
static void gaps(void)
{
    pulse_stats_t s={.hz=80000000}; report_timing_t out={0};
    s.channel[0].report.flags=TIMING_ENABLED|TIMING_COUNT_VALID;
    pulse_stats_edge(&s,0,true,4000000000u,1000000);
    pulse_stats_edge(&s,0,false,4040000000u,1500000);
    pulse_stats_edge(&s,0,true,4080000000u,2000000);
    pulse_stats_snapshot(&s,2100000,&out);
    assert(out.channel[0].period_ticks==80000000);
    pulse_stats_snapshot(&s,4000000,&out);
    assert(!(out.channel[0].flags&TIMING_FRESH) && out.channel[0].period_ticks==0 && out.channel[0].width_ticks==0);
    pulse_stats_edge(&s,0,true,(uint32_t)UINT64_C(4320000000),5000000);
    assert(s.channel[0].report.captured==3 && s.channel[0].report.missing_estimate==2);
    assert(s.channel[0].report.discontinuities==1 && s.channel[0].report.span_intervals==0);
    // Multi-wrap outages, duplicated timestamps, noise and queue loss break
    // phase continuity instead of producing a fabricated drift measurement.
    pulse_stats_edge(&s,0,true,100,120000000);
    assert(s.channel[0].report.discontinuities==2);
    pulse_stats_edge(&s,0,true,200,120000000);
    assert(s.channel[0].report.discontinuities==3);
    pulse_stats_loss(&s);
    assert(!s.channel[0].have_rise && s.channel[0].report.discontinuities==4);
    pulse_stats_counter(&s,0,10,1000000);
    pulse_stats_counter(&s,0,20,UINT64_C(30001000000));
    assert(!(s.channel[0].report.flags&TIMING_COUNT_VALID) && s.channel[0].report.counter_discontinuities==1);
    s.channel[1].report.flags=TIMING_ENABLED|TIMING_COUNT_VALID;
    pulse_stats_counter(&s,1,UINT32_MAX,1000000);
    pulse_stats_counter(&s,1,UINT32_MAX,2000000);
    assert(!(s.channel[1].report.flags&TIMING_COUNT_VALID) && s.channel[1].report.counter_discontinuities==1);
}
static void negative_phase(void)
{
    pulse_stats_t s={.hz=80000000}; report_timing_t out={0};
    for (unsigned n=1;n<=2;n++) for (unsigned c=0;c<2;c++)
        pulse_stats_edge(&s,c,true,(uint32_t)(UINT64_C(4200000000)+n*80000000+c*60000000),n*1000000+c*750000);
    pulse_stats_snapshot(&s,2800000,&out);
    assert((out.flags&1) && out.rtc_minus_gnss_ticks==-20000000);
}
static void adjacent_cycles(void)
{
    pulse_stats_t s={.hz=80000000}; report_timing_t first={0}, next={0};
    const uint32_t period=79999600; // -5 ppm relative to the ESP
    for (unsigned n=1;n<=2;n++) for (unsigned c=0;c<2;c++)
        pulse_stats_edge(&s,c,true,n*period+c*8000000,n*1000000+c*100000);
    pulse_stats_snapshot(&s,2200000,&first);
    // The next GNSS edge arrives before the next RTC edge. The reported RTC
    // phase must not jump by the 400-tick reference-period error at that point.
    pulse_stats_edge(&s,0,true,3*period,3000000);
    pulse_stats_snapshot(&s,3050000,&next);
    assert((first.flags&1) && (next.flags&1));
    assert(first.rtc_minus_gnss_ticks==8000000 && next.rtc_minus_gnss_ticks==8000000);
}
int main(void) { daily(); gaps(); negative_phase(); adjacent_cycles(); puts("pulse daily counts, wraparound, gaps, stale data and phase tests passed"); }
