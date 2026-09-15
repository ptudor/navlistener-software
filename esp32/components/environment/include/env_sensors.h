#ifndef NVF_ENV_SENSORS_H
#define NVF_ENV_SENSORS_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "bmp3.h"
typedef struct {
    void *ctx;
    bool (*read)(void *ctx, uint8_t address, uint8_t reg, uint8_t *data, size_t len);
    bool (*write)(void *ctx, uint8_t address, uint8_t reg, const uint8_t *data, size_t len);
    void (*delay_ms)(void *ctx, unsigned ms);
} env_io_t;
typedef struct {
    env_io_t io;
    bool mcp_ready, hdc_ready, bmp_ready;
    struct bmp3_dev bmp;
    struct bmp3_settings settings;
} env_sensors_t;
typedef struct {
    bool mcp_valid, hdc_valid, bmp_valid;
    double mcp_c, hdc_c, rh_percent, bmp_c, pressure_pa;
} env_sample_t;
void env_sensors_init(env_sensors_t *s, const env_io_t *io);
// Always clears validity first: a failed conversion cannot expose an old value.
void env_sensors_read(env_sensors_t *s, env_sample_t *sample);
#endif
