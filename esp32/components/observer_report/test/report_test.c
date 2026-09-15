#include "observer_report.h"
#include "panel_control.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>
int main(int argc, char **argv)
{
    assert(argc == 2);
    observer_report_t r = {.uptime_ms=1000000, .event_count=3, .event_ms=999000,
        .reason=8, .event_flags=1, .event_states=6,
        .environment={7,7,2550,4212,2500,5000,100000},
        .rtc={23,1789433627,998000}, .crypto={1000,1,{0,0,0x60,5},1,1,2},
        .manifest={1,1,{2,0,0,0,0,0,0,1},0,0,0},
        .resources={1,1024,4194304,3,2,65536,3145728},
        .receiver={0x6f,0x4f,{6,2,1,1,0,0,1,0},15,2,1,999500,999600},
        .firmware="test-v1"};
    uint8_t actual[OBSERVER_REPORT_MAX], expected[OBSERVER_REPORT_MAX]; size_t count=0;
    FILE *f=fopen(argv[1],"r"); assert(f); unsigned byte;
    while (fscanf(f,"%2x",&byte)==1) { assert(count<sizeof expected); expected[count++]=byte; }
    fclose(f);
    assert(observer_report_encode(actual,sizeof actual,&r)==count);
    assert(!memcmp(actual,expected,count));
    memset(actual,0xa5,sizeof actual);
    assert(observer_report_encode(actual,count-1,&r)==0 && actual[0]==0xa5);
    report_policy_t p={0};
    assert(observer_report_due(&p,&r)==REPORT_BOOT); observer_report_sent(&p,&r);
    r.uptime_ms+=30000; r.environment.mcp_centi_c+=25;
    assert(!observer_report_due(&p,&r)); // no noisy small-change reporting
    r.uptime_ms+=30000; r.environment.mcp_centi_c+=25;
    assert(observer_report_due(&p,&r)==REPORT_CHANGE); // accumulated drift
    observer_report_sent(&p,&r); r.uptime_ms+=299999;
    assert(!observer_report_due(&p,&r)); r.uptime_ms++;
    assert(observer_report_due(&p,&r)==REPORT_CHECKIN);
    observer_report_sent(&p,&r); r.event_count++; r.uptime_ms+=4999;
    assert(!observer_report_due(&p,&r)); r.uptime_ms++;
    assert(observer_report_due(&p,&r)==REPORT_INTERFERENCE);
    observer_report_sent(&p,&r); r.uptime_ms+=60000; r.environment.valid&=~2;
    assert(observer_report_due(&p,&r)==REPORT_CHANGE); // unavailable is a change
    observer_report_sent(&p,&r); r.uptime_ms+=60000; r.environment.valid|=2;
    assert(observer_report_due(&p,&r)==REPORT_CHANGE); // recovery is a change
    assert(panel_pwm_off_ticks(33)==686 && panel_pwm_off_ticks(100)==0 && panel_pwm_off_ticks(0)==1024);
    assert(panel_next_brightness(33)==10 && panel_next_brightness(10)==100 && panel_next_brightness(100)==33);
    panel_button_t button={0};
    assert(!panel_button_short_press(&button,true,200));
    assert(!panel_button_short_press(&button,false,80));
    assert(panel_button_short_press(&button,false,20));
    assert(!panel_button_short_press(&button,false,200));
    for (int i=0;i<400;i++) assert(!panel_button_short_press(&button,true,20));
    assert(!panel_button_short_press(&button,false,100)); // eight-second reset hold never dims
    assert(!panel_button_short_press(&button,true,20));
    assert(!panel_button_short_press(&button,false,100)); // short contact noise
    const unsigned presses[] = {500, 1500, 2000, 3000, 3020, 7900, 8000};
    for (unsigned i = 0; i < sizeof presses / sizeof presses[0]; i++) {
        button = (panel_button_t){0};
        unsigned remaining = presses[i];
        while (remaining) {
            unsigned step = remaining < 20 ? remaining : 20;
            assert(!panel_button_short_press(&button, true, step));
            remaining -= step;
        }
        assert(!panel_button_short_press(&button, false, 80));
        assert(panel_button_short_press(&button, false, 20) == (presses[i] <= 3000));
        assert(!panel_button_short_press(&button, false, 100));
    }
    puts("observer report golden/cadence tests passed");
}
