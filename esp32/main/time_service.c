#include "observer_time.h"
#include "rtc_policy.h"
#include "esp_timer.h"
#include "esp_sntp.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "journal.h"
#include "lwip/tcpip.h"
#include <sys/time.h>
#include <limits.h>
#include <stdlib.h>

static SemaphoreHandle_t lock;
static observer_time_state_t state;
static rtc_candidate_t candidate;
static int64_t wall_ns(void) {struct timeval t;gettimeofday(&t,NULL);return (int64_t)t.tv_sec*1000000000LL+t.tv_usec*1000LL;}
static void record_change(const observer_time_sample_t *sample,int64_t old,int64_t new,int64_t now)
{
    observer_time_changed(&state,sample,old,new,now);
}
void observer_time_network_sync(struct timeval *t)
{
    int64_t now=esp_timer_get_time()/1000,new=(int64_t)t->tv_sec*1000000000LL+t->tv_usec*1000LL;
    if(!lock || xSemaphoreTake(lock,portMAX_DELAY)!=pdTRUE)return;
    int64_t old=state.system.source ? observer_time_project(&state.system,now) : 0;
    observer_time_sample_t sample={.source=OBS_TIME_NETWORK,.utc_ns=new,.sampled_ms=now};
    record_change(&sample,old,new,now);
    xSemaphoreGive(lock);
    journal_clock_change(sample.source,old,new,sample.sampled_ms,sample.uncertainty_known,sample.uncertainty_ns);
}
void observer_time_start(void) {lock=xSemaphoreCreateMutex();}
observer_time_state_t observer_time_status(void) {observer_time_state_t s={0};if(lock && xSemaphoreTake(lock,portMAX_DELAY)==pdTRUE){s=state;xSemaphoreGive(lock);}return s;}
uint64_t observer_time_capture(uint8_t *source)
{
    *source=1;
    if(!lock || xSemaphoreTake(lock,portMAX_DELAY)!=pdTRUE)return 0;
    int64_t ns=wall_ns();*source=(uint8_t)state.system.source+1;
    if((state.system.source==OBS_TIME_NETWORK || state.system.source==OBS_TIME_FACTORY) && !observer_time_fresh(&state.system,esp_timer_get_time()/1000))*source=1;
    xSemaphoreGive(lock);
    return ns>=1704067200LL*1000000000LL && ns<4102444800LL*1000000000LL?(uint64_t)ns:0;
}
static void service_clock(void *arg)
{
    observer_time_sample_t sample=*(observer_time_sample_t *)arg;free(arg);
    int64_t now=esp_timer_get_time()/1000;
    if(now<sample.sampled_ms || now-sample.sampled_ms>15000)return;
    if(!lock || xSemaphoreTake(lock,portMAX_DELAY)!=pdTRUE)return;
    int64_t old=wall_ns(),target=observer_time_project(&sample,now);
    struct timeval tv={.tv_sec=target/1000000000LL,.tv_usec=target%1000000000LL/1000};
    if(settimeofday(&tv,NULL)!=0){xSemaphoreGive(lock);return;}
    record_change(&sample,old,target,now);xSemaphoreGive(lock);
    journal_clock_change(sample.source,old,target,sample.sampled_ms,sample.uncertainty_known,sample.uncertainty_ns);
}
void observer_time_service_applied(const rtc_service_record_t *r)
{
    if(r->result!=RTC_SERVICE_OK || !r->audit_complete || r->request.verify_only)return;
    observer_time_sample_t *sample=malloc(sizeof *sample);if(!sample)return;
    *sample=(observer_time_sample_t){.source=r->request.source,.utc_ns=r->request.reference.utc_ns,
        .sampled_ms=r->request.reference.sampled_ms,.uncertainty_known=true,.uncertainty_ns=r->request.uncertainty_ns};
    if(tcpip_callback(service_clock,sample)!=ERR_OK)free(sample);
}
static void bootstrap(void *arg)
{
    (void)arg;
    int64_t now=esp_timer_get_time()/1000;
    observer_time_state_t before=observer_time_status();
    observer_time_sample_t selected;
    if(!lock || xSemaphoreTake(lock,portMAX_DELAY)!=pdTRUE)return;
    int64_t old=wall_ns();
    if (!observer_time_bootstrap(&before,old,now,&selected)) {xSemaphoreGive(lock);return;}
    int64_t target=observer_time_project(&selected,now);
    struct timeval tv={.tv_sec=target/1000000000LL,.tv_usec=(target%1000000000LL)/1000};
    if (settimeofday(&tv,NULL)!=0) {xSemaphoreGive(lock);return;}
    record_change(&selected,old,target,now);xSemaphoreGive(lock);
    journal_clock_change(selected.source,old,target,selected.sampled_ms,selected.uncertainty_known,selected.uncertainty_ns);
}
void observer_time_poll(const gnss_status_t *g,report_rtc_t rtc,int64_t now)
{
    int64_t utc;
    unsigned source=rtc_trusted_utc(&candidate,g,rtc.flags,(int64_t)rtc.epoch,(int64_t)rtc.sampled_ms,now,&utc);
    observer_time_sample_t sample={0};
    if (source==RTC_UTC_GNSS) {
        const uint8_t *p=g->pvt_utc+8;
        sample=(observer_time_sample_t){.source=OBS_TIME_GNSS,.utc_ns=candidate.last_utc_ns,.sampled_ms=candidate.last_sample_ms,
            .uncertainty_known=true,.uncertainty_ns=p[0]|((uint32_t)p[1]<<8)|((uint32_t)p[2]<<16)|((uint32_t)p[3]<<24)};
    } else if (source==RTC_UTC_RTC) sample=(observer_time_sample_t){.source=OBS_TIME_RTC,.utc_ns=(int64_t)rtc.epoch*1000000000LL,.sampled_ms=(int64_t)rtc.sampled_ms};
    if(!lock || xSemaphoreTake(lock,portMAX_DELAY)!=pdTRUE)return;
    state.independent=sample;xSemaphoreGive(lock);
    // Both setters run on the TCP/IP thread, including SNTP's notification.
    // This prevents a retained RTC bootstrap from racing a network update.
    observer_time_state_t before=observer_time_status();observer_time_sample_t selected;
    if (observer_time_bootstrap(&before,wall_ns(),now,&selected)) (void)tcpip_callback(bootstrap,NULL);
}
