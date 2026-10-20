#include "mcp9808.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated MCP9808 at one address: 16-bit registers read and written MSB first, the
// power-on CONFIG of 0x0000 (or a part left in shutdown), the ambient register's alert-flag
// and sign bits (DS25095 Register 5-4), and transfers that fail on demand.
typedef struct {
    uint8_t address;
    uint16_t config, ambient, manufacturer, device;
    bool absent, fail_ambient;
    unsigned writes, slept_ms;
} chip_t;
static bool chip_read(void *ctx, uint8_t address, uint8_t reg, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    assert(length == 2);
    if (c->absent || address != c->address) return false;
    uint16_t value;
    switch (reg) {
    case 0x01: value = c->config; break;
    case 0x05: if (c->fail_ambient) return false; value = c->ambient; break;
    case 0x06: value = c->manufacturer; break;
    case 0x07: value = c->device; break;
    default: assert(!"unexpected register"); return false;
    }
    out[0] = value >> 8; out[1] = (uint8_t)value;
    return true;
}
static bool chip_write(void *ctx, uint8_t address, uint8_t reg, const uint8_t *data, size_t length)
{
    chip_t *c = ctx;
    // Only CONFIG is written, and only to clear the shutdown bit.
    assert(reg == 0x01 && length == 2);
    if (c->absent || address != c->address) return false;
    c->config = (uint16_t)(data[0] << 8 | data[1]);
    c->writes++;
    return true;
}
static void chip_delay(void *ctx, unsigned ms) { ((chip_t *)ctx)->slept_ms += ms; }
static env_io_t chip_io(chip_t *c, uint8_t address)
{
    *c = (chip_t){.address = address, .manufacturer = 0x0054, .device = 0x0400};
    return (env_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay_ms = chip_delay};
}

int main(void)
{
    chip_t c;
    int16_t t;
    // The P3's U47: identified at 0x1F, already awake, so nothing is written and no wait.
    env_io_t io = chip_io(&c, 0x1F);
    assert(mcp9808_start(&io, 0x1F) && c.writes == 0 && c.slept_ms == 0);
    c.ambient = 0x0194;                              // +25.25 C
    assert(mcp9808_read(&io, 0x1F, &t) && t == 404);
    c.ambient = 0xE000 | 0x05A4;                     // +90.25 C with all three alert flags set
    assert(mcp9808_read(&io, 0x1F, &t) && t == 1444);
    c.ambient = 0x1FF0;                              // -1 C: sign bit with two's complement
    assert(mcp9808_read(&io, 0x1F, &t) && t == -16);
    c.ambient = 0x07D0;                              // +125 C, the top of the range
    assert(mcp9808_read(&io, 0x1F, &t) && t == 2000);
    c.ambient = 0x07D1;                              // just past it
    assert(!mcp9808_read(&io, 0x1F, &t) && t == 2000);
    c.ambient = 0x1D80;                              // -40 C, the bottom
    assert(mcp9808_read(&io, 0x1F, &t) && t == -640);
    c.ambient = 0x1D7F;
    assert(!mcp9808_read(&io, 0x1F, &t));
    c.fail_ambient = true;
    assert(!mcp9808_read(&io, 0x1F, &t));
    // Only the part at the requested address answers.
    assert(!mcp9808_start(&io, 0x18));

    // A part left in shutdown is woken, keeping CONFIG's other bits, and given a conversion.
    io = chip_io(&c, 0x1F);
    c.config = 0x0108;                               // shutdown, plus an alert-control bit
    assert(mcp9808_start(&io, 0x1F) && c.writes == 1 && c.config == 0x0008 && c.slept_ms == 300);

    // Identity: another manufacturer or device is not used; an absent part is not ready.
    io = chip_io(&c, 0x1F); c.manufacturer = 0x0055;
    assert(!mcp9808_start(&io, 0x1F));
    io = chip_io(&c, 0x1F); c.device = 0x0500;
    assert(!mcp9808_start(&io, 0x1F));
    io = chip_io(&c, 0x1F); c.absent = true;
    assert(!mcp9808_start(&io, 0x1F) && !mcp9808_read(&io, 0x1F, &t));
    puts("MCP9808 identification, wake, alert-bit masking, sign and range passed");
    return 0;
}
