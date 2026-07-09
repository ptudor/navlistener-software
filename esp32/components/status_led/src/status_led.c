// status_led — WS2812 on GPIO8 over RMT. See include/status_led.h.

#include "status_led.h"

#include <string.h>
#include "driver/rmt_tx.h"
#include "esp_check.h"
#include "esp_log.h"

static const char *TAG = "led";

#define LED_GPIO        8
#define RMT_RES_HZ      10000000            // 10 MHz -> 0.1 us per tick
#define LED_BRIGHTNESS  24                  // scale 0..255 down; the onboard WS2812 is bright

static rmt_channel_handle_t s_chan = NULL;
static rmt_encoder_handle_t s_encoder = NULL;
static bool s_ready = false;

esp_err_t status_led_init(void)
{
    rmt_tx_channel_config_t chan_cfg = {
        .clk_src = RMT_CLK_SRC_DEFAULT,
        .gpio_num = LED_GPIO,
        .mem_block_symbols = 64,
        .resolution_hz = RMT_RES_HZ,
        .trans_queue_depth = 4,
    };
    ESP_RETURN_ON_ERROR(rmt_new_tx_channel(&chan_cfg, &s_chan), TAG, "rmt channel");

    // WS2812 bit timings at 0.1 us/tick: '0' = 0.3us high + 0.9us low; '1' = 0.9us high +
    // 0.3us low. WS2812 clocks the most-significant bit first.
    rmt_bytes_encoder_config_t enc_cfg = {
        .bit0 = { .level0 = 1, .duration0 = 3, .level1 = 0, .duration1 = 9 },
        .bit1 = { .level0 = 1, .duration0 = 9, .level1 = 0, .duration1 = 3 },
        .flags.msb_first = 1,
    };
    ESP_RETURN_ON_ERROR(rmt_new_bytes_encoder(&enc_cfg, &s_encoder), TAG, "rmt encoder");
    ESP_RETURN_ON_ERROR(rmt_enable(s_chan), TAG, "rmt enable");
    s_ready = true;
    status_led_state(LED_BOOT);
    return ESP_OK;
}

void status_led_set_rgb(uint8_t r, uint8_t g, uint8_t b)
{
    if (!s_ready) return;
    // Scale for comfort, then pack GRB (WS2812 wire order).
    uint8_t grb[3] = {
        (uint8_t)((uint16_t)g * LED_BRIGHTNESS / 255),
        (uint8_t)((uint16_t)r * LED_BRIGHTNESS / 255),
        (uint8_t)((uint16_t)b * LED_BRIGHTNESS / 255),
    };
    rmt_transmit_config_t tx = { .loop_count = 0 };
    rmt_transmit(s_chan, s_encoder, grb, sizeof grb, &tx);
    rmt_tx_wait_all_done(s_chan, 100);
}

void status_led_state(led_state_t s)
{
    switch (s) {
    case LED_NO_WIFI:       status_led_set_rgb(255, 0,   0);   break; // red
    case LED_NO_LINK:       status_led_set_rgb(255, 180, 0);   break; // yellow
    case LED_SPOOL_FILLING: status_led_set_rgb(255, 90,  0);   break; // orange
    case LED_STREAMING:     status_led_set_rgb(0,   255, 0);   break; // green
    case LED_BOOT:
    default:                status_led_set_rgb(0,   40,  255); break; // blue
    }
}
