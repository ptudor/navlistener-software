#include "observer_time_policy.h"
#define NS 1000000000LL
bool observer_time_fresh(const observer_time_sample_t *s, int64_t now)
{
    if (s->source == OBS_TIME_UNKNOWN || s->utc_ns < 946684800LL*NS || s->utc_ns >= 4102444800LL*NS || s->sampled_ms<0 || now < s->sampled_ms) return false;
    int64_t age=now-s->sampled_ms;
    switch (s->source) {
    case OBS_TIME_GNSS: return age<=2000;
    case OBS_TIME_RTC: return age<=30000;
    case OBS_TIME_FACTORY: return age<=2000;
    case OBS_TIME_NETWORK: return age<=3600000;
    default: return false;
    }
}
int64_t observer_time_project(const observer_time_sample_t *s,int64_t now)
{ return s->utc_ns+(now-s->sampled_ms)*1000000LL; }
bool observer_time_bootstrap(const observer_time_state_t *s,int64_t wall,int64_t now,observer_time_sample_t *out)
{
    if (!observer_time_fresh(&s->independent,now)) return false;
    int64_t target=observer_time_project(&s->independent,now);
    if (target<1704067200LL*NS || target>=4102444800LL*NS) return false;
    bool plausible=wall>=1704067200LL*NS && wall<4102444800LL*NS;
    bool promote=s->system.source==OBS_TIME_RTC && s->independent.source==OBS_TIME_GNSS;
    if (plausible && !promote) return false;
    *out=s->independent;return true;
}
void observer_time_changed(observer_time_state_t *s,const observer_time_sample_t *sample,int64_t old,int64_t new,int64_t now)
{ s->system=*sample;s->old_utc_ns=old;s->new_utc_ns=new;s->changed_ms=now;s->changes++; }
const char *observer_time_source_name(observer_time_source_t s)
{ static const char *const names[]={"unknown","rtc_continuity","gnss_qualified","network","factory_reference"};return s<=OBS_TIME_FACTORY?names[s]:"unknown"; }
