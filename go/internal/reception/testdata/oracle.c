#include "../../../../common/reception.h"
#include <assert.h>
#include <stdio.h>
int main(void)
{
    uint8_t b[NR_MAX_WIRE];size_t n=fread(b,1,sizeof b,stdin);nr_expectation_t e;
    assert(nr_decode(&e,b,n));nr_machine_t m={0};
    for(unsigned sec=1;sec<=70;sec++) {
        nr_sample_t s={.expectation_id=e.id,.utc=e.issued+sec,.uptime_ms=sec*1000,.valid=1};
        s.matched[0]=3;if(sec>=40){s.matched[0]=255;s.matched[1]=15;}
        uint8_t bad=nr_counts(&e,&s);s.alarm=nr_step(&m,s.valid,bad,s.uptime_ms,e.alarm_s,e.clear_s);
        if(sec==31||sec==70){uint8_t out[NR_SAMPLE_SIZE];nr_encode_sample(out,&s);assert(fwrite(out,1,sizeof out,stdout)==sizeof out);}
    }
    uint8_t g=0x82,y=0x04;nr_alarm_leds(1,249,&g,&y);assert(g==0x83&&y==0x04);
    nr_alarm_leds(1,250,&g,&y);assert(g==0x82&&y==0x05);
    nr_alarm_leds(1,500,&g,&y);assert(g==0x83&&y==0x04);
    for(size_t i=0;i<n;i++)assert(!nr_decode(&e,b,i));
    return 0;
}
