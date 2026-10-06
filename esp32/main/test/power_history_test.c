#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "power_history.h"

static uint64_t day(unsigned n,uint64_t phase){return (uint64_t)(21000u+n)*86164u+phase;}
static nr_expectation_t expectation(uint64_t id,uint64_t utc)
{
    nr_expectation_t e={.id=id,.issued=utc,.radius_m=1000,.alarm_s=5,.clear_s=5,
        .min_expected=1,.min_missing=1,.missing_percent=100,.count=1,
        .entries={{.gnss=0,.sv=12,.signal=NR_SATELLITE,.slots=31}}};
    return e;
}
static nrp_sample_t assess(nr_expectation_t *e,gnss_status_t *status,uint64_t utc,uint64_t now,bool learn)
{
    nrp_sample_t sample={.expectation_id=e->id,.utc=utc,.uptime_ms=now};
    power_history_assess(e,status,utc,now,learn,&sample);return sample;
}
int main(void)
{
    const uint64_t site=0x1122334455667788ULL,phase=123*600+60;
    assert(power_history_bind(site,6,4,3));
    gnss_status_t status={.satellites_valid=true,.satellite_count=1,
        .satellites={{0,12,NR_SATELLITE,40,45}}};
    for(unsigned d=0;d<4;d++) {
        uint64_t utc=day(d,phase),now=1000+d*1000;status.satellites_ms=(int64_t)now;
        status.satellites[0].cno=(uint8_t[]){40,42,41,41}[d];
        nr_expectation_t e=expectation(10+d,utc);nrp_sample_t sample=assess(&e,&status,utc,now,true);
        assert(sample.observed_valid[0]==1&&sample.observed[0]==status.satellites[0].cno);
        if(d<3)assert(!(sample.local.valid[0]&1));
    }
    uint64_t utc=day(3,phase),now=5000;status.satellites_ms=(int64_t)now;
    nr_expectation_t e=expectation(20,utc);nrp_sample_t mature=assess(&e,&status,utc,now,false);
    assert((mature.flags&NRP_FLAG_LOCAL)&&mature.local_model_id&&mature.local.valid[0]==1&&!mature.local.bad[0]);
    uint64_t model_id=mature.local_model_id;

    status.satellites[0].cno=60;status.satellites_ms=6000;
    nrp_sample_t anomaly=assess(&e,&status,utc+1,6000,true);
    assert(anomaly.local.valid[0]==1&&anomaly.local.bad[0]==1&&power_history_model_id()==model_id);

    size_t n=power_history_export(NULL,0,1);assert(n>36&&n<=POWER_HISTORY_WIRE_MAX);
    uint8_t *wire=malloc(n);assert(wire&&power_history_export(wire,n,1)==n);
    power_history_test_reset();assert(power_history_bind(site,6,4,3));
    assert(power_history_import(wire,n,site)&&power_history_model_id()==model_id);
    wire[n-1]^=1;assert(!power_history_import(wire,n,site));wire[n-1]^=1;
    assert(!power_history_import(wire,n,site+1));free(wire);
    assert(power_history_bind(site+1,6,4,3)&&power_history_model_id()!=model_id);
    power_history_test_reset();
    puts("edge power history: distinct passes, robust outlier rejection, CRC restore and site reset passed");
}
