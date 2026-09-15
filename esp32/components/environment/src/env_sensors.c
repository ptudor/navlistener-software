#include "env_sensors.h"
#include <math.h>
#include <string.h>
static uint16_t le16(const uint8_t *p) { return p[0] | ((uint16_t)p[1] << 8); }
static uint16_t be16(const uint8_t *p) { return ((uint16_t)p[0] << 8) | p[1]; }
static bool read_reg(env_sensors_t *s, uint8_t address, uint8_t reg, uint8_t *p, size_t n)
{ return s->io.read(s->io.ctx, address, reg, p, n); }
static bool write_reg(env_sensors_t *s, uint8_t address, uint8_t reg, const uint8_t *p, size_t n)
{ return s->io.write(s->io.ctx, address, reg, p, n); }
static int8_t bmp_read(uint8_t reg, uint8_t *p, uint32_t n, void *ctx)
{ return read_reg(ctx, 0x76, reg, p, n) ? 0 : -1; }
static int8_t bmp_write(uint8_t reg, const uint8_t *p, uint32_t n, void *ctx)
{ return write_reg(ctx, 0x76, reg, p, n) ? 0 : -1; }
static void bmp_delay(uint32_t us, void *ctx)
{
    env_sensors_t *s = ctx;
    s->io.delay_ms(s->io.ctx, (us + 999) / 1000);
}
void env_sensors_init(env_sensors_t *s, const env_io_t *io)
{
    memset(s, 0, sizeof *s); s->io = *io;
    uint8_t p[21], value;
    if (read_reg(s, 0x18, 6, p, 2) && be16(p) == 0x0054 &&
        read_reg(s, 0x18, 7, p, 2) && p[0] == 4 && read_reg(s, 0x18, 1, p, 2)) {
        bool asleep = p[0] & 1; // CONFIG shutdown bit 8
        p[0] &= ~1;
        if (!asleep || write_reg(s, 0x18, 1, p, 2)) {
            if (asleep) s->io.delay_ms(s->io.ctx, 300);
            s->mcp_ready = true;
        }
    }
    if (read_reg(s, 0x40, 0xfc, p, 4) && le16(p) == 0x5449 && le16(p + 2) == 0x07d0 &&
        read_reg(s, 0x40, 0x0e, p, 1)) {
        value = p[0] & 7; // manual conversions, heater off, preserve interrupt configuration
        s->hdc_ready = write_reg(s, 0x40, 0x0e, &value, 1);
    }
    if (!read_reg(s, 0x76, 0, p, 1) || p[0] != 0x50 ||
        !read_reg(s, 0x76, 0x31, p, sizeof p)) return;
    bool all_zero = true, all_ff = true;
    for (unsigned i = 0; i < sizeof p; i++) { all_zero &= p[i] == 0; all_ff &= p[i] == 255; }
    if (all_zero || all_ff) return;
    s->bmp = (struct bmp3_dev){.intf = BMP3_I2C_INTF, .intf_ptr = s,
        .read = bmp_read, .write = bmp_write, .delay_us = bmp_delay};
    if (bmp3_init(&s->bmp) != BMP3_OK) return;
    s->settings.press_en = BMP3_ENABLE; s->settings.temp_en = BMP3_ENABLE;
    s->settings.odr_filter.press_os = BMP3_OVERSAMPLING_8X;
    s->settings.odr_filter.temp_os = BMP3_OVERSAMPLING_2X;
    s->settings.odr_filter.iir_filter = BMP3_IIR_FILTER_DISABLE;
    s->settings.op_mode = BMP3_MODE_FORCED;
    // Keep each vendor API call to one configuration register.
    const uint32_t groups[] = {BMP3_SEL_PRESS_EN | BMP3_SEL_TEMP_EN,
        BMP3_SEL_PRESS_OS | BMP3_SEL_TEMP_OS, BMP3_SEL_IIR_FILTER};
    for (unsigned i = 0; i < sizeof groups / sizeof groups[0]; i++)
        if (bmp3_set_sensor_settings(groups[i], &s->settings, &s->bmp) != BMP3_OK) return;
    s->bmp_ready = true;
}
static bool read_hdc(env_sensors_t *s, env_sample_t *sample)
{
    uint8_t p[4], trigger = 1; // 14-bit temperature + humidity, single measurement
    if (!read_reg(s, 0x40, 4, p, 1) || // clear any old data-ready indication
        !write_reg(s, 0x40, 0x0f, &trigger, 1)) return false;
    for (unsigned i = 0; i < 20; i++) {
        s->io.delay_ms(s->io.ctx, 5);
        if (!read_reg(s, 0x40, 4, p, 1)) return false;
        if (!(p[0] & 0x80)) continue;
        if (!read_reg(s, 0x40, 0, p, sizeof p)) return false; // LSB first for both channels
        // TI SNAS678C equations 2/3; PSRR uses the board's nominal 3.3 V supply.
        sample->hdc_c = le16(p) * (165.0 / 65536.0) - 40.5 + 0.08 * (3.3 - 1.8);
        sample->rh_percent = le16(p + 2) * (100.0 / 65536.0);
        return sample->hdc_c >= -40 && sample->hdc_c <= 125 && sample->rh_percent <= 100;
    }
    return false;
}
static bool read_bmp(env_sensors_t *s, env_sample_t *sample)
{
    struct bmp3_data data;
    // Consume any old ready data before triggering a new forced conversion.
    uint8_t old[6];
    if (!read_reg(s, 0x76, 4, old, sizeof old) ||
        bmp3_set_op_mode(&s->settings, &s->bmp) != BMP3_OK) return false;
    for (unsigned i = 0; i < 25; i++) {
        s->io.delay_ms(s->io.ctx, 10);
        struct bmp3_status status = {0};
        if (bmp3_get_status(&status, &s->bmp) != BMP3_OK ||
            status.err.fatal || status.err.cmd || status.err.conf) return false;
        if (!status.sensor.drdy_press || !status.sensor.drdy_temp) continue;
        if (bmp3_get_sensor_data(BMP3_PRESS | BMP3_TEMP, &data, &s->bmp) != BMP3_OK) return false;
        sample->bmp_c = data.temperature; sample->pressure_pa = data.pressure;
        return isfinite(data.temperature) && isfinite(data.pressure) &&
            data.temperature >= -40 && data.temperature <= 85 && data.pressure >= 30000 && data.pressure <= 125000;
    }
    return false;
}
void env_sensors_read(env_sensors_t *s, env_sample_t *sample)
{
    *sample = (env_sample_t){0};
    uint8_t p[2];
    if (s->mcp_ready && read_reg(s, 0x18, 5, p, 2)) {
        uint16_t raw = be16(p);
        sample->mcp_c = (raw & 0xfff) / 16.0 - ((raw & 0x1000) ? 256.0 : 0.0);
        sample->mcp_valid = sample->mcp_c >= -40 && sample->mcp_c <= 125;
    }
    if (s->hdc_ready) sample->hdc_valid = read_hdc(s, sample);
    if (s->bmp_ready) sample->bmp_valid = read_bmp(s, sample);
}
