#include "observer_time_policy.h"
#include <assert.h>
#include <stdio.h>
#define UTC (1760000000LL*1000000000LL)
int main(void)
{
    observer_time_state_t s={0};observer_time_sample_t out;
    assert(!observer_time_bootstrap(&s,0,0,&out)); // fresh board, no RTC/GNSS/network
    s.independent=(observer_time_sample_t){.source=OBS_TIME_RTC,.utc_ns=UTC,.sampled_ms=1000};
    assert(!observer_time_fresh(&s.independent,999));
    assert(observer_time_bootstrap(&s,0,31000,&out) && !out.uncertainty_known);
    assert(!observer_time_bootstrap(&s,0,31001,&out)); // stale RTC
    assert(observer_time_bootstrap(&s,0,2000,&out)); // no network, no fix
    observer_time_changed(&s,&out,0,UTC+1000000000LL,2000);
    assert(s.changes==1 && s.old_utc_ns==0 && s.system.source==OBS_TIME_RTC);
    assert(!observer_time_bootstrap(&s,UTC,2000,&out)); // retained, even if wrong
    s.independent=(observer_time_sample_t){.source=OBS_TIME_GNSS,.utc_ns=UTC+86400LL*1000000000LL,.sampled_ms=2000,.uncertainty_known=true,.uncertainty_ns=100000000};
    assert(observer_time_bootstrap(&s,UTC,2000,&out)); // promote RTC to qualified GNSS
    observer_time_changed(&s,&out,UTC,out.utc_ns,2000);
    assert(s.changes==2 && s.new_utc_ns-s.old_utc_ns==86400LL*1000000000LL);
    assert(!observer_time_bootstrap(&s,out.utc_ns,2001,&out));
    assert(!observer_time_bootstrap(&s,0,4001,&out)); // stale GNSS
    s.system.source=OBS_TIME_NETWORK;
    assert(!observer_time_bootstrap(&s,UTC,2000,&out)); // availability is not independent proof
    s.independent.utc_ns=0;
    assert(!observer_time_bootstrap(&s,0,2000,&out));
    puts("time policy: unavailable, stale, RTC continuity, GNSS promotion and network source passed");
}
