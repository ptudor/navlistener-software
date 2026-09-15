#include "pulse_timing.h"
#include "pulse_stats.h"
#include "sdkconfig.h"
#if CONFIG_NVF_BOARD_GNSS_COLOR_NEO
#include "driver/mcpwm_cap.h"
#include "driver/pulse_cnt.h"
#include "driver/gpio.h"
#include "esp_attr.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#if !CONFIG_MCPWM_ISR_CACHE_SAFE
#error "PPS capture must remain available while flash caches are disabled"
#endif
typedef struct { uint64_t us; uint32_t tick; uint8_t channel, rising; } edge_t;
static const char *TAG="pulse_timing";
static QueueHandle_t edges;
static portMUX_TYPE lock=portMUX_INITIALIZER_UNLOCKED;
static uint32_t dropped, seen_dropped;
static unsigned indices[]={0,1};
static pulse_stats_t stats;
static uint64_t started_ms;
static bool ready;
static mcpwm_cap_timer_handle_t timer;
static mcpwm_cap_channel_handle_t captures[2];
static pcnt_unit_handle_t counters[2];
static pcnt_channel_handle_t count_channels[2];
static bool IRAM_ATTR capture(mcpwm_cap_channel_handle_t channel, const mcpwm_capture_event_data_t *data, void *ctx)
{
    (void)channel;
    edge_t edge={.us=esp_timer_get_time(),.tick=data->cap_value,
        .channel=*(unsigned *)ctx,.rising=data->cap_edge==MCPWM_CAP_EDGE_POS};
    BaseType_t wake=pdFALSE;
    if (xQueueSendFromISR(edges,&edge,&wake) != pdTRUE) {
        portENTER_CRITICAL_ISR(&lock); if (dropped != UINT32_MAX) dropped++; portEXIT_CRITICAL_ISR(&lock);
    }
    return wake==pdTRUE;
}
esp_err_t pulse_timing_start(void)
{
    edges=xQueueCreate(128,sizeof(edge_t));
    if (!edges) return ESP_ERR_NO_MEM;
    mcpwm_capture_timer_config_t config={.group_id=0,.clk_src=MCPWM_CAPTURE_CLK_SRC_APB};
    esp_err_t err=mcpwm_new_capture_timer(&config,&timer);
    if (err != ESP_OK) goto failed;
    err=mcpwm_capture_timer_get_resolution(timer,&stats.hz);
    if (err != ESP_OK) goto failed;
    const int pins[]={10,15};
    for (unsigned i=0;i<2;i++) {
        mcpwm_capture_channel_config_t ch={.gpio_num=pins[i],.prescale=1,
            .flags={.pos_edge=true,.neg_edge=true}};
        err=mcpwm_new_capture_channel(timer,&ch,&captures[i]);
        if (err != ESP_OK) goto failed;
        mcpwm_capture_event_callbacks_t cb={.on_cap=capture};
        err=mcpwm_capture_channel_register_event_callbacks(captures[i],&cb,&indices[i]);
        if (err != ESP_OK) goto failed;
        pcnt_unit_config_t count={.low_limit=-1,.high_limit=PULSE_COUNTER_MODULUS};
        err=pcnt_new_unit(&count,&counters[i]); if (err != ESP_OK) goto failed;
        pcnt_chan_config_t input={.edge_gpio_num=pins[i],.level_gpio_num=-1};
        err=pcnt_new_channel(counters[i],&input,&count_channels[i]); if (err != ESP_OK) goto failed;
        // The PCB supplies its pull resistors. PCNT enables internal pull-ups by default.
        gpio_set_pull_mode(pins[i],GPIO_FLOATING);
        err=pcnt_channel_set_edge_action(count_channels[i],PCNT_CHANNEL_EDGE_ACTION_INCREASE,PCNT_CHANNEL_EDGE_ACTION_HOLD);
        if (err != ESP_OK) goto failed;
        err=pcnt_channel_set_level_action(count_channels[i],PCNT_CHANNEL_LEVEL_ACTION_KEEP,PCNT_CHANNEL_LEVEL_ACTION_KEEP);
        if (err != ESP_OK) goto failed;
        err=pcnt_unit_enable(counters[i]); if (err != ESP_OK) goto failed;
        err=pcnt_unit_clear_count(counters[i]); if (err != ESP_OK) goto failed;
        err=pcnt_unit_start(counters[i]); if (err != ESP_OK) goto failed;
        err=mcpwm_capture_channel_enable(captures[i]); if (err != ESP_OK) goto failed;
        stats.channel[i].report.flags=TIMING_ENABLED|TIMING_COUNT_VALID;
    }
    err=mcpwm_capture_timer_enable(timer); if (err != ESP_OK) goto failed;
    started_ms=esp_timer_get_time()/1000;
    err=mcpwm_capture_timer_start(timer); if (err != ESP_OK) goto failed;
    ready=true;
    ESP_LOGI(TAG,"hardware capture GPIO10=GNSS GPIO15=RTC; APB=%lu Hz; independent pulse counters enabled",(unsigned long)stats.hz);
    return ESP_OK;
failed:
    // Cleanup is best effort and must not compromise receiver startup.
    for (unsigned i=0;i<2;i++) {
        if (captures[i]) { (void)mcpwm_capture_channel_disable(captures[i]); (void)mcpwm_del_capture_channel(captures[i]); }
        if (counters[i]) { (void)pcnt_unit_stop(counters[i]); (void)pcnt_unit_disable(counters[i]); }
        if (count_channels[i]) (void)pcnt_del_channel(count_channels[i]);
        if (counters[i]) (void)pcnt_del_unit(counters[i]);
    }
    if (timer) { (void)mcpwm_capture_timer_disable(timer); (void)mcpwm_del_capture_timer(timer); }
    // Keep the small queue allocated on a partial driver-cleanup failure so
    // a delayed interrupt can never access freed storage. No retry loop.
    return err;
}
void pulse_timing_poll(uint64_t now, report_timing_t *out)
{
    *out=(report_timing_t){.present=true,.clock=1,.resolution_hz=stats.hz,.started_ms=started_ms};
    if (!ready) return;
    edge_t edge;
    for (unsigned i=0;i<128 && xQueueReceive(edges,&edge,0)==pdTRUE;i++)
        pulse_stats_edge(&stats,edge.channel,edge.rising,edge.tick,edge.us);
    // Retain the delivered-edge count, but do not rebuild a current continuous
    // span from queued edges preceding a loss. Resume on subsequent edges.
    portENTER_CRITICAL(&lock); uint32_t lost=dropped; portEXIT_CRITICAL(&lock);
    if (lost != seen_dropped) { pulse_stats_loss(&stats); seen_dropped=lost; }
    out->queue_dropped=lost;
    for (unsigned i=0;i<2;i++) {
        int raw;
        if (pcnt_unit_get_count(counters[i],&raw)==ESP_OK)
            pulse_stats_counter(&stats,i,raw,now);
        else pulse_stats_counter(&stats,i,UINT32_MAX,now);
    }
    // Sample time after draining edges, which may have arrived during I2C work.
    pulse_stats_snapshot(&stats,esp_timer_get_time(),out);
}
#else
esp_err_t pulse_timing_start(void) { return ESP_ERR_NOT_SUPPORTED; }
void pulse_timing_poll(uint64_t now, report_timing_t *out) { (void)now; *out=(report_timing_t){0}; }
#endif
