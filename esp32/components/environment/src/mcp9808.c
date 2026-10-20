#include "mcp9808.h"

enum { REG_CONFIG = 0x01, REG_AMBIENT = 0x05, REG_MANUFACTURER = 0x06, REG_DEVICE = 0x07 };
enum { CONFIG_SHUTDOWN = 0x01 }; // CONFIG bit 8, in the register's first byte

bool mcp9808_start(const env_io_t *io, uint8_t address)
{
    uint8_t p[2];
    if (!io->read(io->ctx, address, REG_MANUFACTURER, p, 2) || (p[0] << 8 | p[1]) != MCP9808_MANUFACTURER_ID ||
        !io->read(io->ctx, address, REG_DEVICE, p, 2) || p[0] != MCP9808_DEVICE_ID ||
        !io->read(io->ctx, address, REG_CONFIG, p, 2)) return false;
    if (!(p[0] & CONFIG_SHUTDOWN)) return true;
    p[0] &= (uint8_t)~CONFIG_SHUTDOWN;
    if (!io->write(io->ctx, address, REG_CONFIG, p, 2)) return false;
    io->delay_ms(io->ctx, 300);
    return true;
}

bool mcp9808_read(const env_io_t *io, uint8_t address, int16_t *sixteenths)
{
    uint8_t p[2];
    if (!io->read(io->ctx, address, REG_AMBIENT, p, 2)) return false;
    const uint16_t raw = (uint16_t)(p[0] << 8 | p[1]);
    const int16_t value = (int16_t)((raw & 0x0fff) - ((raw & 0x1000) ? 4096 : 0));
    if (value < MCP9808_MIN_SIXTEENTHS || value > MCP9808_MAX_SIXTEENTHS) return false;
    *sixteenths = value;
    return true;
}
