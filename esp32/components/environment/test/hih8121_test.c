#include "hih8121.h"
#include <assert.h>
#include <stdio.h>

// A simulated HIH8121 at 0x27 (technical note 009061-2-EN): an address-only write starts a
// conversion; a four-byte fetch returns the status bits and the results, fresh once the
// conversion has finished and stale until then or after they have been fetched.
typedef struct {
    bool absent, command_mode, converting;
    unsigned requests, slept_ms, requested_at, cycle_ms;
    uint16_t humidity, temperature; // 14-bit counts
    bool fetched;
} chip_t;
static bool chip_request(void *ctx, uint8_t address)
{
    chip_t *c = ctx;
    if (c->absent || address != 0x27) return false;
    c->requests++; c->requested_at = c->slept_ms; c->converting = true; c->fetched = false;
    return true;
}
static bool chip_receive(void *ctx, uint8_t address, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    assert(length == 4);
    if (c->absent || address != 0x27) return false;
    if (c->converting && c->slept_ms - c->requested_at >= c->cycle_ms) c->converting = false;
    unsigned status = c->command_mode ? 2 : (c->converting || c->fetched) ? 1 : 0;
    out[0] = (uint8_t)(status << 6 | c->humidity >> 8); out[1] = (uint8_t)c->humidity;
    out[2] = (uint8_t)(c->temperature >> 6); out[3] = (uint8_t)(c->temperature << 2 | 3); // bits 1:0 don't care
    if (!c->converting) c->fetched = true;
    return true;
}
static bool chip_read(void *ctx, uint8_t address, uint8_t reg, uint8_t *out, size_t length)
{
    (void)ctx; (void)address; (void)reg; (void)out; (void)length;
    assert(!"the part has no registers");
    return false;
}
static bool chip_write(void *ctx, uint8_t address, uint8_t reg, const uint8_t *data, size_t length)
{
    (void)ctx; (void)address; (void)reg; (void)data; (void)length;
    assert(!"a data byte after the address is not a measurement request");
    return false;
}
static void chip_delay(void *ctx, unsigned ms) { ((chip_t *)ctx)->slept_ms += ms; }
static env_io_t chip_io(chip_t *c)
{
    // The typical measurement cycle is 36.65 ms; model it as 37.
    *c = (chip_t){.humidity = 8191, .temperature = 8191, .cycle_ms = 37};
    return (env_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay_ms = chip_delay,
                      .receive = chip_receive, .request = chip_request};
}

int main(void)
{
    chip_t c;
    int16_t t = 0;
    uint16_t rh = 0;
    hih8121_result_t result;
    env_io_t io = chip_io(&c);
    // Half scale: 50 %RH and 42.5 C, after one request and the full wait.
    assert(hih8121_read(&io, 0x27, &t, &rh, &result) && result.answered && result.status == HIH8121_FRESH);
    assert(t == 4250 && rh == 5000 && c.requests == 1 && c.slept_ms == HIH8121_MEASURE_MS);
    // Full scale is 2^14 - 2 counts; the one count past it is still 100 %RH, but not 125.01 C.
    c.humidity = 16382; c.temperature = 16382;
    assert(hih8121_read(&io, 0x27, &t, &rh, &result) && rh == 10000 && t == 12500);
    c.humidity = 16383;
    assert(hih8121_read(&io, 0x27, &t, &rh, &result) && rh == 10000);
    c.temperature = 16383;
    assert(!hih8121_read(&io, 0x27, &t, &rh, &result) && result.answered && result.status == HIH8121_FRESH);
    c.humidity = 0; c.temperature = 0;
    assert(hih8121_read(&io, 0x27, &t, &rh, &result) && rh == 0 && t == -4000);
    // A conversion still running at the fetch reads stale and is not used.
    io = chip_io(&c); c.cycle_ms = HIH8121_MEASURE_MS + 1;
    assert(!hih8121_read(&io, 0x27, &t, &rh, &result) && result.answered && result.status == HIH8121_STALE);
    // Command mode is reported, not measured.
    io = chip_io(&c); c.command_mode = true;
    assert(!hih8121_read(&io, 0x27, &t, &rh, &result) && result.answered && result.status == HIH8121_COMMAND_MODE);
    // An absent part, another address, or a bus without address-only writes: nothing answered.
    io = chip_io(&c); c.absent = true;
    assert(!hih8121_read(&io, 0x27, &t, &rh, &result) && !result.answered && c.slept_ms == 0);
    io = chip_io(&c);
    assert(!hih8121_read(&io, 0x28, &t, &rh, &result) && !result.answered);
    io = chip_io(&c); io.request = NULL;
    assert(!hih8121_read(&io, 0x27, &t, &rh, &result) && !result.answered && !c.requests);
    puts("HIH8121 address-only request, wait, fetch, status bits, full scale and range passed");
    return 0;
}
