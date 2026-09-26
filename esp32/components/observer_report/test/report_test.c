#include "observer_report.h"
#include "panel_control.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>
int main(int argc, char **argv)
{
    assert(argc == 4);
    observer_report_t r = {.uptime_ms=1000000, .event_count=3, .event_ms=999000,
        .reason=8, .event_flags=1, .event_states=6,
        .environment={7,7,2550,4212,2500,5000,100000},
        .rtc={23,1789433627,998000}, .crypto={1000,1,{0,0,0x60,5},1,1,2},
        .manifest={.action=1, .uid_valid=1, .board_uid={0,3,16, 0x02,0,0,0,0,0,0,0,0,0,0,0,0,0,0,1}},
        .resources={1,1024,4194304,3,2,65536,3145728},
        .receiver={0x6f,0x4f,{6,2,1,1,0,0,1,0},15,2,1,999500,999600},
        .firmware="test-v1",
        .heater={.present=true, .state=0, .flags=3, .runs=1, .stop=1, .valid=0xff,
            .rh95_s=20000, .rh98_s=14460, .last_utc=1789400000, .start_ms=250000, .on_ms=95000,
            .recovery_ms=630000, .rh_before=9912, .rh_stop=430, .hdc_before=-150, .hdc_peak=7310,
            .hdc_end=1310, .mcp_before=-210, .mcp_peak=2475, .mcp_end=1150}};
    uint8_t actual[OBSERVER_REPORT_MAX], expected[OBSERVER_REPORT_MAX]; size_t count=0;
    FILE *f=fopen(argv[1],"r"); assert(f); unsigned byte;
    while (fscanf(f,"%2x",&byte)==1) { assert(count<sizeof expected); expected[count++]=byte; }
    fclose(f);
    assert(observer_report_encode(actual,sizeof actual,&r)==count);
    assert(!memcmp(actual,expected,count));
    const observer_report_t golden=r;
    // Without an HDC policy the heater component is omitted, not sent as zeros.
    observer_report_t absent=r; absent.heater.present=false;
    assert(observer_report_encode(actual,sizeof actual,&absent)==count-61 && !memcmp(actual,expected,count-61));
    report_timing_t timing={.present=true,.clock=1,.rtc_state=1,.rtc_control=0xc0,.flags=3,
        .tp_flags=3,.tp_ms=999500,.resolution_hz=80000000,.started_ms=1000,.rtc_minus_gnss_ticks=-80};
    for (unsigned i=0;i<2;i++) timing.channel[i]=(timing_channel_report_t){
        .flags=63,.period_ticks=80000080+i*80,.width_ticks=40000000,.min_ticks=80000000,.max_ticks=80000160,
        .captured=999,.physical=999,.span_ticks=UINT64_C(998)*(80000080+i*80),.span_intervals=998,.last_rise_ms=999600};
    f=fopen(argv[2],"r"); assert(f); size_t timing_count=0;
    while (fscanf(f,"%2x",&byte)==1) { assert(timing_count<sizeof expected); expected[timing_count++]=byte; }
    fclose(f);
    assert(observer_timing_encode(actual,sizeof actual,&timing,1000000)==timing_count);
    assert(!memcmp(actual,expected,timing_count));
    memset(actual,0xa5,sizeof actual);
    assert(!observer_timing_encode(actual,timing_count-1,&timing,1000000) && actual[0]==0xa5);
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
    observer_report_sent(&p,&r); r.uptime_ms+=60000; r.heater.rh95_s+=60;
    assert(!observer_report_due(&p,&r)); // dwell counters ride along with other reports
    r.heater.state=1; r.heater.runs=2;
    assert(observer_report_due(&p,&r)==REPORT_CHANGE); // heater start
    observer_report_sent(&p,&r); r.uptime_ms+=60000; r.heater.state=3;
    assert(observer_report_due(&p,&r)==REPORT_CHANGE); // heater off, recovering

    // The MAX board's barometer, thermocouple and motion tags follow the heater.
    observer_report_t m=golden;
    m.barometer=(report_barometer_t){.present=true,.state=2,.valid=1,.centi_c=2215,.pressure_pa=101325};
    m.thermocouple=(report_thermocouple_t){.present=true,.state=1,.valid=3,.flags=1,.config=3,
        .tc_centi_c=23456,.cj_centi_c=2437};
    m.motion=(report_motion_t){.present=true,.imu_state=1,.mag_state=1,.valid=7,.profile=0,.moving=1,
        .rate_decihz=500,.gyro_fs_dps=1000,.accel_fs_g=8,.accel={10,-20,4096},.gyro={3,-2,1},
        .imu_centi_c=2750,.packets=3000,.overflows=1,.rate_changes=4,.window_samples=900,
        .window_ms=30000,.accel_mean={410,-12,4075},.gyro_mean={2,-1,33},.accel_min_mg=980,
        .accel_max_mg=1530,.gyro_max_decidps=125,.imu_ms=999900,.mag={512,-205,922},
        .mag_offset={32808,32751,32771},.mag_ms=999800};
    f=fopen(argv[3],"r"); assert(f); size_t max_count=0;
    while (fscanf(f,"%2x",&byte)==1) { assert(max_count<sizeof expected); expected[max_count++]=byte; }
    fclose(f);
    assert(observer_report_encode(actual,sizeof actual,&m)==max_count && !memcmp(actual,expected,max_count));
    // The fullest report, with a 32-character firmware version, fits the buffer.
    memset(m.firmware,'v',32); m.firmware[32]=0;
    assert(observer_report_encode(actual,sizeof actual,&m)==max_count+25);
    memcpy(m.firmware,"test-v1",8);
    p=(report_policy_t){0}; observer_report_sent(&p,&m);
    m.uptime_ms+=60000; m.motion.accel_max_mg=4000; m.motion.packets+=6000; m.barometer.pressure_pa+=99;
    assert(!observer_report_due(&p,&m)); // motion extremes and small drift ride along
    m.barometer.pressure_pa+=1;
    assert(observer_report_due(&p,&m)==REPORT_CHANGE); // 1 hPa
    observer_report_sent(&p,&m); m.uptime_ms+=60000; m.thermocouple.tc_centi_c+=100;
    assert(observer_report_due(&p,&m)==REPORT_CHANGE); // 1 C at the probe
    observer_report_sent(&p,&m); m.uptime_ms+=60000; m.thermocouple.fault=1; m.thermocouple.valid=2;
    assert(observer_report_due(&p,&m)==REPORT_CHANGE); // an open thermocouple
    observer_report_sent(&p,&m); m.uptime_ms+=60000; m.motion.overflows++;
    assert(observer_report_due(&p,&m)==REPORT_CHANGE); // lost IMU samples
    observer_report_sent(&p,&m); m.uptime_ms+=60000; m.motion.valid&=~4;
    assert(observer_report_due(&p,&m)==REPORT_CHANGE); // the magnetometer stopped answering
    observer_report_sent(&p,&m); m.uptime_ms+=60000; m.motion.rate_changes++;
    assert(!observer_report_due(&p,&m)); // the counter rides along
    m.motion.moving=0;
    assert(observer_report_due(&p,&m)==REPORT_CHANGE); // the unit came to rest
    assert(panel_pwm_off_ticks(50)==512 && panel_pwm_off_ticks(20)==819 && panel_pwm_off_ticks(10)==922);
    assert(panel_pwm_off_ticks(100)==0 && panel_pwm_off_ticks(0)==1024);
    assert(panel_next_brightness(20)==10 && panel_next_brightness(10)==50 && panel_next_brightness(50)==20);
    // Trimmer: 0 V is the dimmest setting, full travel is 100%, an open wiper is invalid.
    assert(panel_trimmer_percent(0,3000)==1 && panel_trimmer_percent(1250,3000)==51);
    assert(panel_trimmer_percent(2500,3000)==100 && panel_trimmer_percent(2860,3000)==100);
    assert(!panel_trimmer_percent(3000,3000) && !panel_trimmer_percent(3300,3000) && !panel_trimmer_percent(-1,3000));
    panel_brightness_t lamp;
    panel_brightness_boot(&lamp,20,0,70);          // first boot with a trimmer: it sets the level
    assert(lamp.percent==70 && lamp.reference==70);
    panel_brightness_boot(&lamp,50,70,72);         // unmoved within the deadband: saved preset stands
    assert(lamp.percent==50 && lamp.reference==72);
    panel_brightness_boot(&lamp,50,70,30);         // moved while off: the trimmer wins
    assert(lamp.percent==30 && lamp.reference==30);
    panel_brightness_boot(&lamp,50,70,0);          // open wiper: saved setting and reference stand
    assert(lamp.percent==50 && lamp.reference==70);
    assert(!panel_brightness_trimmer(&lamp,0) && lamp.percent==50);
    assert(!panel_brightness_trimmer(&lamp,73) && lamp.percent==50);  // jitter inside the deadband
    assert(panel_brightness_trimmer(&lamp,74) && lamp.percent==74 && lamp.reference==74);
    panel_brightness_preset(&lamp,74);             // a press (74 -> 20) wins until the trimmer moves again
    assert(lamp.percent==20 && lamp.reference==74);
    assert(!panel_brightness_trimmer(&lamp,76) && lamp.percent==20);
    assert(panel_brightness_trimmer(&lamp,40) && lamp.percent==40);
    panel_brightness_preset(&lamp,0);              // a press with the trimmer unreadable keeps its reference
    assert(lamp.percent==20 && lamp.reference==40);
    lamp=(panel_brightness_t){.percent=20};        // the trimmer becomes readable after an open wiper
    assert(panel_brightness_trimmer(&lamp,60) && lamp.percent==60 && lamp.reference==60);
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
