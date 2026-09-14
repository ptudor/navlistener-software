// display_st7789 — ST7789 status display for the Waveshare ESP32-C6-LCD-1.47.
// Ported from apps/shepherdprotocol/esp32's driver for this exact panel (same wiring, same
// 34-px dead-offset), with a navfeeder dashboard. esp_lcd + embedded 8x8 font, no LVGL.

#include "display_st7789.h"
#include "font8x8.h"

#include "esp_log.h"
#include "esp_check.h"
#include "esp_heap_caps.h"
#include "esp_app_desc.h"
#include "driver/spi_master.h"
#include "driver/ledc.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_panel_vendor.h"
#include "esp_lcd_panel_ops.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/semphr.h"
#include <string.h>
#include <stdio.h>

static const char *TAG = "display";

// ---- Board wiring (Waveshare ESP32-C6-LCD-1.47, fixed) ----------------------
#define LCD_HOST   SPI2_HOST
#define PIN_SCLK   7
#define PIN_MOSI   6
#define PIN_CS     14
#define PIN_DC     15
#define PIN_RST    21
#define PIN_BL     22

// ---- Geometry: landscape 320x172, 34-px dead offset on the short (vertical) axis --------
#define LCD_H_RES        320
#define LCD_V_RES        172
#define LCD_X_GAP        0
#define LCD_Y_GAP        34
#define LCD_SWAP_XY      true
#define LCD_MIRROR_X     true
#define LCD_MIRROR_Y     false
#define LCD_INVERT_COLOR true   // IPS needs INVON for correct colors
#define LCD_PCLK_HZ      (40 * 1000 * 1000)
#define LCD_CMD_BITS     8
#define LCD_PARAM_BITS   8
#define LCD_BAND_ROWS    48

// ---- Backlight (LEDC) — hard-capped at 50% (panel overheats above) ----------------------
#define BL_LEDC_MODE       LEDC_LOW_SPEED_MODE
#define BL_LEDC_TIMER      LEDC_TIMER_1
#define BL_LEDC_CHANNEL    LEDC_CHANNEL_2
#define BL_LEDC_RES_BITS   LEDC_TIMER_8_BIT
#define BL_LEDC_FREQ_HZ    5000
#define BL_MAX_PERCENT     50
#define BL_DEFAULT_PERCENT 40

// ---- Layout -----------------------------------------------------------------------------
#define TXT_MARGIN_X   4
#define TITLE_SCALE    2
#define STATUS_SCALE   2
#define VER_SCALE      1
#define TOP_BANNER_H   (8 * TITLE_SCALE + 4)   // 20
#define BOT_BANNER_H   (8 * VER_SCALE + 4)     // 12
#define STATUS_Y0      (TOP_BANNER_H + 2)
#define STATUS_Y1      (LCD_V_RES - BOT_BANNER_H - 2)
#define STATUS_LINE_H  (8 * STATUS_SCALE + 2)  // 18
#define GLYPH_ADV(s)   (8 * (s) + (s))
#define TEXT_PX(n, s)  ((n) * GLYPH_ADV(s) - (s))

// ---- Palette ----------------------------------------------------------------------------
#define COL_TOP_FG     DISPLAY_RGB(60,  100, 255)   // blue title
#define COL_TOP_BG     DISPLAY_RGB(20,  40,  120)   // deep navy
#define COL_BOT_FG     DISPLAY_RGB(200, 20,  20)
#define COL_BOT_BG     DISPLAY_RGB(255, 225, 0)
#define COL_STATUS_FG  DISPLAY_RGB(255, 255, 150)   // light yellow
#define COL_STATUS_BG  DISPLAY_RGB(20,  30,  40)    // near-black slate
#define COL_UP         DISPLAY_RGB(80,  255, 80)    // link up (bright green)
#define COL_DOWN       DISPLAY_RGB(255, 70,  70)    // link/wifi down (red)

static esp_lcd_panel_handle_t    s_panel = NULL;
static esp_lcd_panel_io_handle_t s_io = NULL;
static bool s_ready = false;
// signaled by on_color_trans_done whenever any esp_lcd_panel_draw_bitmap() transfer
// finishes (framebuffer path included). The fallback (direct-draw) path drains any stale
// signal before issuing its own draw_bitmap and then blocks on this until ITS transfer
// completes, before freeing the DMA buffer it just handed to the panel -- draw_bitmap
// queues async SPI/DMA transactions (trans_queue_depth 10) and returns before they've
// necessarily finished, so freeing right after issuing one races the in-flight DMA read.
// Safe because display_* calls are only ever made serially from one render task (never
// concurrently), so there is no other source of a draw_bitmap in flight to confuse the
// drain-then-wait pairing.
static SemaphoreHandle_t s_trans_sem = NULL;
// Full-frame off-screen buffer (320x172 RGB565). The dashboard is rendered into this and
// pushed in one esp_lcd_panel_draw_bitmap, so text is never corrupted by per-glyph DMA-buffer
// reuse (the panel IO queues transfers async) and updates don't flicker. NULL => fall back to
// direct banded draws.
static uint16_t *s_fb = NULL;

static inline uint16_t swab16(uint16_t c) { return (uint16_t)((c >> 8) | (c << 8)); }
static void fill_rect(int x, int y, int w, int h, uint16_t color);
static void display_flush(void);

// trans_done_cb runs in ISR context (the SPI DMA completion interrupt) and simply signals
// s_trans_sem -- see the comment on s_trans_sem above.
static bool trans_done_cb(esp_lcd_panel_io_handle_t io, esp_lcd_panel_io_event_data_t *edata,
                           void *user_ctx)
{
    (void)io; (void)edata; (void)user_ctx;
    BaseType_t hp_task_woken = pdFALSE;
    xSemaphoreGiveFromISR(s_trans_sem, &hp_task_woken);
    return hp_task_woken == pdTRUE;
}

// wait_trans_done blocks until the panel confirms the most recently issued draw_bitmap has
// fully completed. Call immediately after issuing exactly one draw_bitmap and
// before touching/freeing the buffer it was given. drain_stale_trans_signal must be called
// first, immediately before that draw_bitmap call, to clear any leftover signal from an
// earlier, unrelated transfer (e.g. the framebuffer path's display_flush(), which never
// waits on this semaphore itself).
static void drain_stale_trans_signal(void) { xSemaphoreTake(s_trans_sem, 0); }
static void wait_trans_done(void) { xSemaphoreTake(s_trans_sem, portMAX_DELAY); }

void display_backlight_set_percent(uint8_t percent)
{
    if (percent > BL_MAX_PERCENT) percent = BL_MAX_PERCENT;
    uint32_t duty = (255u * percent) / 100u;
    ledc_set_duty(BL_LEDC_MODE, BL_LEDC_CHANNEL, duty);
    ledc_update_duty(BL_LEDC_MODE, BL_LEDC_CHANNEL);
}

static esp_err_t backlight_init(void)
{
    ledc_timer_config_t tcfg = {
        .speed_mode = BL_LEDC_MODE, .timer_num = BL_LEDC_TIMER,
        .duty_resolution = BL_LEDC_RES_BITS, .freq_hz = BL_LEDC_FREQ_HZ, .clk_cfg = LEDC_AUTO_CLK,
    };
    ESP_RETURN_ON_ERROR(ledc_timer_config(&tcfg), TAG, "BL timer");
    ledc_channel_config_t ccfg = {
        .gpio_num = PIN_BL, .speed_mode = BL_LEDC_MODE, .channel = BL_LEDC_CHANNEL,
        .timer_sel = BL_LEDC_TIMER, .duty = 0, .hpoint = 0,
    };
    ESP_RETURN_ON_ERROR(ledc_channel_config(&ccfg), TAG, "BL channel");
    return ESP_OK;
}

static void draw_banner(int y, int h, const char *s, int scale, uint16_t fg, uint16_t bg)
{
    if (!s_ready || !s) return;
    fill_rect(0, y, LCD_H_RES, h, bg);
    int textw = TEXT_PX((int)strlen(s), scale);
    int tx = (LCD_H_RES - textw) / 2;
    if (tx < TXT_MARGIN_X) tx = TXT_MARGIN_X;
    int ty = y + (h - 8 * scale) / 2;
    display_text(tx, ty, s, fg, bg, scale);
}

static void draw_build_banner(void)
{
    const esp_app_desc_t *d = esp_app_get_description();
    char buf[80];
    snprintf(buf, sizeof buf, "%s  %s", d->version, d->date);
    draw_banner(LCD_V_RES - BOT_BANNER_H, BOT_BANNER_H, buf, VER_SCALE, COL_BOT_FG, COL_BOT_BG);
}

esp_err_t display_init(void)
{
    if (s_ready) return ESP_OK;
    ESP_RETURN_ON_ERROR(backlight_init(), TAG, "backlight");

    spi_bus_config_t buscfg = {
        .sclk_io_num = PIN_SCLK, .mosi_io_num = PIN_MOSI, .miso_io_num = -1,
        .quadwp_io_num = -1, .quadhd_io_num = -1,
        .max_transfer_sz = LCD_H_RES * LCD_V_RES * (int)sizeof(uint16_t), // full-frame single push
    };
    esp_err_t err = spi_bus_initialize(LCD_HOST, &buscfg, SPI_DMA_CH_AUTO);
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
        ESP_LOGE(TAG, "spi_bus_initialize: %s", esp_err_to_name(err));
        return err;
    }

    esp_lcd_panel_io_spi_config_t io_config = {
        .dc_gpio_num = PIN_DC, .cs_gpio_num = PIN_CS, .pclk_hz = LCD_PCLK_HZ,
        .lcd_cmd_bits = LCD_CMD_BITS, .lcd_param_bits = LCD_PARAM_BITS,
        .spi_mode = 0, .trans_queue_depth = 10,
    };
    ESP_RETURN_ON_ERROR(
        esp_lcd_new_panel_io_spi((esp_lcd_spi_bus_handle_t)LCD_HOST, &io_config, &s_io),
        TAG, "panel_io");

    // needed by the fallback (direct-draw) path in fill_rect/display_text to know
    // when it's safe to free a DMA transfer buffer.
    s_trans_sem = xSemaphoreCreateBinary();
    if (!s_trans_sem) { ESP_LOGE(TAG, "trans_sem alloc failed"); return ESP_ERR_NO_MEM; }
    esp_lcd_panel_io_callbacks_t io_cbs = { .on_color_trans_done = trans_done_cb };
    ESP_RETURN_ON_ERROR(esp_lcd_panel_io_register_event_callbacks(s_io, &io_cbs, NULL),
        TAG, "trans_done_cb");

    esp_lcd_panel_dev_config_t panel_config = {
        .reset_gpio_num = PIN_RST, .rgb_ele_order = LCD_RGB_ELEMENT_ORDER_RGB, .bits_per_pixel = 16,
    };
    ESP_RETURN_ON_ERROR(esp_lcd_new_panel_st7789(s_io, &panel_config, &s_panel), TAG, "st7789");

    ESP_RETURN_ON_ERROR(esp_lcd_panel_reset(s_panel), TAG, "reset");
    ESP_RETURN_ON_ERROR(esp_lcd_panel_init(s_panel), TAG, "init");
    esp_lcd_panel_invert_color(s_panel, LCD_INVERT_COLOR);
    esp_lcd_panel_swap_xy(s_panel, LCD_SWAP_XY);
    esp_lcd_panel_mirror(s_panel, LCD_MIRROR_X, LCD_MIRROR_Y);
    esp_lcd_panel_set_gap(s_panel, LCD_X_GAP, LCD_Y_GAP);
    ESP_RETURN_ON_ERROR(esp_lcd_panel_disp_on_off(s_panel, true), TAG, "disp_on");

    s_ready = true;

    // Off-screen framebuffer (~110 KB), allocated once, early — heap is plentiful before WiFi
    // and TLS come up. On failure we log and fall back to direct banded draws (functional, but
    // per-glyph DMA-buffer reuse can corrupt text, which is exactly what this avoids).
    s_fb = heap_caps_malloc((size_t)LCD_H_RES * LCD_V_RES * sizeof(uint16_t), MALLOC_CAP_DMA);
    if (!s_fb) ESP_LOGW(TAG, "framebuffer alloc failed; direct draw (text may corrupt)");

    display_clear(DISPLAY_BLACK);
    draw_banner(0, TOP_BANNER_H, "NAVFEEDER-ESP", TITLE_SCALE, COL_TOP_FG, COL_TOP_BG);
    draw_build_banner();
    fill_rect(0, STATUS_Y0, LCD_H_RES, STATUS_Y1 - STATUS_Y0, COL_STATUS_BG);
    display_flush();
    display_backlight_set_percent(BL_DEFAULT_PERCENT);
    ESP_LOGI(TAG, "ST7789 %dx%d ready (BL %d%%, %s)", LCD_H_RES, LCD_V_RES, BL_DEFAULT_PERCENT,
             s_fb ? "double-buffered" : "direct");
    return ESP_OK;
}

bool display_is_ready(void) { return s_ready; }

static void fill_rect(int x, int y, int w, int h, uint16_t color)
{
    if (!s_ready || w <= 0 || h <= 0) return;
    if (x < 0) { w += x; x = 0; }
    if (y < 0) { h += y; y = 0; }
    if (x >= LCD_H_RES || y >= LCD_V_RES) return;
    if (x + w > LCD_H_RES) w = LCD_H_RES - x;
    if (y + h > LCD_V_RES) h = LCD_V_RES - y;
    if (w <= 0 || h <= 0) return;

    uint16_t be = swab16(color);

    // Framebuffer path: write into RAM; the caller flushes the whole frame once.
    if (s_fb) {
        for (int row = y; row < y + h; row++) {
            uint16_t *dst = &s_fb[(size_t)row * LCD_H_RES + x];
            for (int col = 0; col < w; col++) dst[col] = be;
        }
        return;
    }

    // Fallback: banded direct-to-panel draw.
    int band = LCD_BAND_ROWS;
    uint16_t *buf = heap_caps_malloc((size_t)w * band * sizeof(uint16_t), MALLOC_CAP_DMA);
    if (!buf) return;
    for (int i = 0; i < w * band; i++) buf[i] = be;
    for (int yy = y; yy < y + h; yy += band) {
        int rows = (yy + band <= y + h) ? band : (y + h - yy);
        drain_stale_trans_signal(); // regression fix
        esp_lcd_panel_draw_bitmap(s_panel, x, yy, x + w, yy + rows, buf);
        wait_trans_done(); // block until THIS band's DMA transfer is done before reusing buf
    }
    heap_caps_free(buf); // safe: the last band's transfer is confirmed complete above
}

void display_clear(uint16_t color) { fill_rect(0, 0, LCD_H_RES, LCD_V_RES, color); }

// display_flush pushes the whole framebuffer to the panel in one transaction (no-op in the
// direct-draw fallback). Call once per complete screen update, never per glyph.
static void display_flush(void)
{
    if (s_ready && s_fb) {
        esp_lcd_panel_draw_bitmap(s_panel, 0, 0, LCD_H_RES, LCD_V_RES, s_fb);
    }
}

void display_text(int x, int y, const char *s, uint16_t fg, uint16_t bg, int scale)
{
    if (!s_ready || !s || scale < 1) return;
    int glyph = 8 * scale;
    uint16_t fg_be = swab16(fg), bg_be = swab16(bg);

    // Framebuffer path: blit scaled glyph pixels straight into RAM with per-pixel clipping;
    // the caller flushes the whole frame once. No per-glyph DMA buffer to race on.
    if (s_fb) {
        int cx = x;
        for (const char *p = s; *p; ++p) {
            if (cx >= LCD_H_RES) break;
            unsigned char ch = (unsigned char)*p;
            if (ch >= 128) ch = '?';
            const uint8_t *rows = font8x8_basic[ch];
            for (int gy = 0; gy < 8; gy++) {
                uint8_t bits = rows[gy];
                for (int sy = 0; sy < scale; sy++) {
                    int py = y + gy * scale + sy;
                    if (py < 0 || py >= LCD_V_RES) continue;
                    uint16_t *line = &s_fb[(size_t)py * LCD_H_RES];
                    for (int gx = 0; gx < 8; gx++) {
                        uint16_t px = (bits & (1u << gx)) ? fg_be : bg_be;
                        for (int sx = 0; sx < scale; sx++) {
                            int pxx = cx + gx * scale + sx;
                            if (pxx >= 0 && pxx < LCD_H_RES) line[pxx] = px;
                        }
                    }
                }
            }
            cx += GLYPH_ADV(scale);
        }
        return;
    }

    // Fallback: per-glyph draw straight to the panel via a reused DMA cell.
    uint16_t *cell = heap_caps_malloc((size_t)glyph * glyph * sizeof(uint16_t), MALLOC_CAP_DMA);
    if (!cell) return;

    int cx = x;
    for (const char *p = s; *p; ++p) {
        if (cx >= LCD_H_RES) break;
        unsigned char ch = (unsigned char)*p;
        if (ch >= 128) ch = '?';
        const uint8_t *rows = font8x8_basic[ch];
        for (int gy = 0; gy < 8; gy++) {
            uint8_t bits = rows[gy];
            for (int gx = 0; gx < 8; gx++) {
                uint16_t px = (bits & (1u << gx)) ? fg_be : bg_be;
                for (int sy = 0; sy < scale; sy++) {
                    uint16_t *line = &cell[(gy * scale + sy) * glyph + gx * scale];
                    for (int sx = 0; sx < scale; sx++) line[sx] = px;
                }
            }
        }
        int w = glyph;
        if (cx + w > LCD_H_RES) w = LCD_H_RES - cx;
        if (w == glyph) {
            drain_stale_trans_signal(); // regression fix
            esp_lcd_panel_draw_bitmap(s_panel, cx, y, cx + glyph, y + glyph, cell);
            wait_trans_done(); // must complete before the next glyph overwrites cell
        } else {
            for (int gy = 0; gy < glyph; gy++) {
                drain_stale_trans_signal(); // regression fix
                esp_lcd_panel_draw_bitmap(s_panel, cx, y + gy, cx + w, y + gy + 1, &cell[gy * glyph]);
                wait_trans_done(); // same reasoning, per clipped row
            }
        }
        cx += GLYPH_ADV(scale);
    }
    heap_caps_free(cell); // safe: the last glyph's transfer is confirmed complete above
}

// ---- navfeeder dashboard ----------------------------------------------------------------

static void status_line(int *y, const char *s, uint16_t fg)
{
    if (*y + STATUS_LINE_H > STATUS_Y1) return;
    fill_rect(0, *y, LCD_H_RES, STATUS_LINE_H, COL_STATUS_BG);
    display_text(TXT_MARGIN_X, *y, s, fg, COL_STATUS_BG, STATUS_SCALE);
    *y += STATUS_LINE_H;
}

void display_render_status(const nvf_status_t *st)
{
    if (!s_ready || !st) return;
    int y = STATUS_Y0;
    char buf[28];

    snprintf(buf, sizeof buf, "%.20s", st->station ? st->station : "?");
    status_line(&y, buf, COL_STATUS_FG);

    const char *ls = st->link_up ? "LINK UP" : (st->wifi_up ? "LINK DOWN" : "NO WIFI");
    status_line(&y, ls, st->link_up ? COL_UP : COL_DOWN);

    snprintf(buf, sizeof buf, "nav %u +%u", (unsigned)st->nav, (unsigned)st->nav_rate);
    status_line(&y, buf, COL_STATUS_FG);

    snprintf(buf, sizeof buf, "tlm %u ck %u", (unsigned)st->telem, (unsigned)st->bad_ck);
    status_line(&y, buf, COL_STATUS_FG);

    snprintf(buf, sizeof buf, "spool %u dr %llu", st->spool_depth, (unsigned long long)st->dropped);
    status_line(&y, buf, st->dropped ? COL_DOWN : COL_STATUS_FG);

    if (st->collector) snprintf(buf, sizeof buf, "%.20s", st->collector);
    else               snprintf(buf, sizeof buf, "unprovisioned");
    status_line(&y, buf, COL_STATUS_FG);

    // Fill any remaining rows so a shrinking dashboard leaves no stale text.
    if (y < STATUS_Y1) fill_rect(0, y, LCD_H_RES, STATUS_Y1 - y, COL_STATUS_BG);
    display_flush();
}

void display_show_portal(const char *ssid, const char *pass, const char *reason)
{
    if (!s_ready) return;
    int y = STATUS_Y0;
    fill_rect(0, STATUS_Y0, LCD_H_RES, STATUS_Y1 - STATUS_Y0, COL_STATUS_BG);
    status_line(&y, "SETUP - join AP:", COL_UP);
    status_line(&y, ssid ? ssid : "?", COL_STATUS_FG);
    status_line(&y, "pass:", COL_STATUS_FG);
    status_line(&y, pass ? pass : "?", COL_STATUS_FG);
    status_line(&y, "http://192.168.4.1", COL_STATUS_FG);
    // why the portal came up. Six lines still fit — STATUS_Y0..Y1 is 136 px at
    // STATUS_LINE_H 18, so seven lines are available; status_line drops anything that would
    // overflow rather than scribbling into the banners.
    if (reason && reason[0]) status_line(&y, reason, COL_DOWN);
    if (y < STATUS_Y1) fill_rect(0, y, LCD_H_RES, STATUS_Y1 - y, COL_STATUS_BG);
    display_flush();
}

void display_show_config_reset_armed(void)
{
    if (!s_ready) return;
    int y = STATUS_Y0;
    fill_rect(0, STATUS_Y0, LCD_H_RES, STATUS_Y1 - STATUS_Y0, COL_STATUS_BG);
    status_line(&y, "CONFIG RESET ARMED", COL_DOWN);
    status_line(&y, "RELEASE BUTTON", COL_STATUS_FG);
    status_line(&y, "TO ERASE SETTINGS", COL_STATUS_FG);
    status_line(&y, "POWER CYCLE TO CANCEL", COL_UP);
    if (y < STATUS_Y1) fill_rect(0, y, LCD_H_RES, STATUS_Y1 - y, COL_STATUS_BG);
    display_flush();
}
