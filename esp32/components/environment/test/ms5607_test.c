#include "ms5607.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated MS5607: PROM words, conversion results and command sequencing.
typedef struct {
    uint16_t prom[8];
    uint32_t d1, d2, pending;
    bool reset, absent, fail_read, skip_wait;
    unsigned waited, conversions;
} chip_t;
static bool chip_write(void *ctx, uint8_t address, uint8_t command, const uint8_t *data, size_t length)
{
    chip_t *c = ctx;
    assert(address == MS5607_ADDRESS && length == 0 && !data);
    if (c->absent) return false;
    if (command == 0x1e) { c->reset = true; c->pending = 0; return true; }
    assert(command == 0x48 || command == 0x58); // OSR 4096 only
    c->pending = command == 0x48 ? c->d1 : c->d2;
    c->waited = 0;
    c->conversions++;
    return true;
}
static bool chip_read(void *ctx, uint8_t address, uint8_t command, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    assert(address == MS5607_ADDRESS);
    if (c->absent || c->fail_read) return false;
    if (command >= 0xa0 && command <= 0xae) {
        assert(!(command & 1) && length == 2 && c->reset);
        uint16_t word = c->prom[(command - 0xa0) / 2];
        out[0] = word >> 8; out[1] = word;
        return true;
    }
    assert(command == 0 && length == 3);
    // Reading before the 9.04 ms conversion finishes returns zero.
    uint32_t value = c->waited >= 10 || c->skip_wait ? c->pending : 0;
    out[0] = value >> 16; out[1] = value >> 8; out[2] = value;
    c->pending = 0;
    return true;
}
static void chip_delay(void *ctx, unsigned ms) { ((chip_t *)ctx)->waited += ms; }
static env_io_t chip_io(chip_t *c)
{
    // The datasheet's typical coefficients; word 7's CRC nibble matches them.
    static const uint16_t prom[8] = {0x0012, 46372, 43981, 29059, 27842, 31553, 28165, 0x5a3f};
    memset(c, 0, sizeof *c);
    memcpy(c->prom, prom, sizeof prom);
    c->d1 = 6465444; c->d2 = 8077636;
    return (env_io_t){.ctx = c, .read = chip_read, .write = chip_write, .delay_ms = chip_delay};
}

static void compensation_test(void)
{
    chip_t c; chip_io(&c);
    int32_t t, p;
    // TE MS5607-02BA03 page 8's worked example: 20.00 C and 1100.02 mbar.
    assert(ms5607_compensate(c.prom, 6465444, 8077636, &t, &p) && t == 2000 && p == 110002);
    // Second order below 20 C, the extra term below -15 C, and none above 20 C. Expected
    // values come from an independent implementation of the datasheet's pages 8 and 9.
    assert(ms5607_compensate(c.prom, 6465444, 7600000, &t, &p) && t == 291 && p == 105957);
    assert(ms5607_compensate(c.prom, 6465444, 7000000, &t, &p) && t == -2157 && p == 100348);
    assert(ms5607_compensate(c.prom, 6465444, 8400000, &t, &p) && t == 3082 && p == 112608);
    // Out of range: -44.54 C, and 9.51 mbar below the extended range's 10 mbar.
    assert(!ms5607_compensate(c.prom, 6465444, 6500000, &t, &p));
    assert(ms5607_compensate(c.prom, 4000000, 7600000, &t, &p) && p == 1000);
    assert(!ms5607_compensate(c.prom, 4000000, 8400000, &t, &p));
    // A conversion read too early is zero; 24 bits is the most a conversion holds.
    assert(!ms5607_compensate(c.prom, 0, 8077636, &t, &p) && !ms5607_compensate(c.prom, 6465444, 0, &t, &p));
    assert(!ms5607_compensate(c.prom, 1u << 24, 8077636, &t, &p));
}

static void crc_test(void)
{
    chip_t c; chip_io(&c);
    assert(ms5607_crc4(c.prom) == 0xf);
    // The serial bits of word 7 are covered and its low byte is not.
    uint16_t prom[8]; memcpy(prom, c.prom, sizeof prom);
    prom[7] = (prom[7] & 0xff00) | 0x00;
    assert(ms5607_crc4(prom) == 0xf);
    // CRC-4 detects every single-bit error in the covered words.
    for (unsigned word = 0; word < 8; word++)
        for (unsigned bit = word == 7 ? 8 : 0; bit < 16; bit++) {
            memcpy(prom, c.prom, sizeof prom);
            prom[word] ^= 1u << bit;
            assert(ms5607_crc4(prom) != 0xf);
        }
}

static void driver_test(void)
{
    chip_t c; env_io_t io = chip_io(&c);
    ms5607_t m;
    int32_t t, p;
    assert(ms5607_init(&m, &io) == MS5607_READY && c.waited >= 3);
    assert(ms5607_read(&m, &io, &t, &p) && t == 2000 && p == 110002 && c.conversions == 2);
    // Each conversion is given its maximum time before the ADC is read.
    c.d1 = 6465444; c.d2 = 7600000;
    assert(ms5607_read(&m, &io, &t, &p) && t == 291 && p == 105957);
    c.fail_read = true;
    assert(!ms5607_read(&m, &io, &t, &p));

    io = chip_io(&c); c.prom[3] ^= 0x0100;
    assert(ms5607_init(&m, &io) == MS5607_PROM_REJECTED && !ms5607_read(&m, &io, &t, &p));
    // Constant bus data with a matching CRC is still not a calibration.
    io = chip_io(&c); memset(c.prom, 0, sizeof c.prom);
    assert(ms5607_crc4(c.prom) == 0 && ms5607_init(&m, &io) == MS5607_PROM_REJECTED);
    io = chip_io(&c); c.absent = true;
    assert(ms5607_init(&m, &io) == MS5607_ABSENT && !ms5607_read(&m, &io, &t, &p));
    io = chip_io(&c); c.fail_read = true;
    assert(ms5607_init(&m, &io) == MS5607_ABSENT);
}

int main(void)
{
    compensation_test(); crc_test(); driver_test();
    puts("MS5607 compensation, CRC-4 and conversion sequencing passed");
    return 0;
}
