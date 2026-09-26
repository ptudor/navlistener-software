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
// The ID registers are identical; the manifest says which part is fitted.
typedef enum {
    ENV_HDC_NONE = 0,
    ENV_HDC2080,
    ENV_HDC2022,
} env_hdc_variant_t;
typedef struct {
    env_io_t io;
    env_hdc_variant_t hdc_variant;
    unsigned parts;
    bool mcp_ready, hdc_ready, bmp_ready;
    bool io_error; // any failed I2C transfer; each read or heater operation clears it first
    struct bmp3_dev bmp;
    struct bmp3_settings settings;
} env_sensors_t;
typedef struct {
    bool mcp_valid, hdc_valid, bmp_valid;
    bool bus_error; // an I2C transfer failed during this sample
    double mcp_c, hdc_c, rh_percent, bmp_c, pressure_pa;
} env_sample_t;
// The MCP9808 and BMP388 are probed only when parts lists them (the manifest does);
// an unknown/disabled HDC variant leaves the HDC unavailable. Others still initialize.
enum { ENV_PART_MCP9808 = 1, ENV_PART_BMP388 = 4 };
void env_sensors_init(env_sensors_t *s, const env_io_t *io, env_hdc_variant_t hdc_variant, unsigned parts);
// Repeats the HDC identification and heater-off configuration if it failed at init. A reboot
// during a heater run leaves HEAT_EN set until the part is configured again. True once ready.
bool env_sensors_retry_hdc(env_sensors_t *s);
// Always clears validity first: a failed conversion cannot expose an old value.
void env_sensors_read(env_sensors_t *s, env_sample_t *sample);
// The same for sensors in mask (bit 0 MCP9808, 1 HDC, 2 BMP); the others stay invalid.
// Listed MCP9808/BMP388 parts retry identification after a failed init/read. HDC retries
// remain explicit so they cannot turn off the heater during a managed heater run.
void env_sensors_read_some(env_sensors_t *s, env_sample_t *sample, uint8_t mask);
// HDC CONFIG HEAT_EN (0x0E bit 3), read-modify-write keeping the interrupt and measurement
// bits. True only when the register reads back exactly as written. Requires hdc_ready.
bool env_hdc_heater_set(env_sensors_t *s, bool on);
bool env_hdc_heater_get(env_sensors_t *s, bool *on);
// For a board whose manifest lists no HDC: measures nothing, but if an HDC2080-family
// part answers at 0x40 with HEAT_EN set (a reboot mid-run leaves it on), clears it.
// *present reports whether the family ID answered. False only when a present part's
// heater could not be confirmed off.
bool env_hdc_heater_off_unlisted(const env_io_t *io, bool *present);
#endif
