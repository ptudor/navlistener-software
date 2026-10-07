#include "io_expander.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated MCP23008 (DS20001919) with INTCON = 0 semantics: an enabled input that changes
// state flags INTF and, unless an interrupt is already pending, captures the port in INTCAP
// and drives INT low. Reading INTCAP or GPIO clears the interrupt and INTF. Each changed
// line's reference becomes its new level. Changes queued in during[] arrive one per clearing
// read, as if they happened while the interrupt was being handled.
typedef struct {
    uint8_t reg[0x0a];
    uint8_t port;      // the levels on GP0-GP7
    uint8_t reference; // each line's level as last flagged or read (INTCON = 0)
    bool pending;      // INT low
    uint8_t ignored;   // register bits that do not take a write
    unsigned operations, fail_at;
    struct { uint8_t reg, value; } writes[64];
    unsigned write_count, intf_reads, intcap_reads, gpio_reads;
    uint8_t during[8];
    unsigned during_count;
} chip_t;
static void chip_drive(chip_t *c, uint8_t port)
{
    uint8_t changed = (uint8_t)(c->reference ^ port) & c->reg[MCP23008_GPINTEN] & c->reg[MCP23008_IODIR];
    c->port = port;
    if (!changed) return;
    c->reg[MCP23008_INTF] |= changed;
    c->reference = (uint8_t)((c->reference & ~changed) | (port & changed));
    if (!c->pending) { c->reg[MCP23008_INTCAP] = port; c->pending = true; }
}
static void chip_clear(chip_t *c)
{
    c->pending = false;
    c->reg[MCP23008_INTF] = 0;
    c->reference = c->port;
    if (c->during_count) {
        uint8_t next = c->during[0];
        memmove(c->during, c->during + 1, --c->during_count);
        chip_drive(c, next);
    }
}
static bool chip_read(void *ctx, uint8_t reg, uint8_t *value)
{
    chip_t *c = ctx;
    assert(reg < sizeof c->reg);
    if (++c->operations == c->fail_at) return false;
    *value = reg == MCP23008_GPIO ? c->port : c->reg[reg];
    if (reg == MCP23008_INTF) c->intf_reads++;
    if (reg == MCP23008_INTCAP) c->intcap_reads++;
    if (reg == MCP23008_GPIO) c->gpio_reads++;
    if (reg == MCP23008_INTCAP || reg == MCP23008_GPIO) chip_clear(c);
    return true;
}
static bool chip_write(void *ctx, uint8_t reg, uint8_t value)
{
    chip_t *c = ctx;
    assert(reg <= MCP23008_GPPU);
    if (++c->operations == c->fail_at) return false;
    assert(c->write_count < sizeof c->writes / sizeof c->writes[0]);
    c->writes[c->write_count].reg = reg;
    c->writes[c->write_count++].value = value;
    if (reg == MCP23008_IOCON) value &= 0x3e; // bits 7, 6 and 0 are unimplemented
    c->reg[reg] = (uint8_t)((value & ~c->ignored) | (c->reg[reg] & c->ignored));
    return true;
}
static bool chip_int_low(void *ctx) { return ((chip_t *)ctx)->pending; }
// Power-on: every pin an input and nothing enabled, with every line idle high (GP0 as the
// programme's pull-up leaves it with no chain attached).
static io_expander_io_t chip_io(chip_t *c)
{
    memset(c, 0, sizeof *c);
    c->reg[MCP23008_IODIR] = 0xff;
    c->port = c->reference = 0xff;
    return (io_expander_io_t){.ctx = c, .read = chip_read, .write = chip_write, .int_low = chip_int_low};
}
static void chip_reset_counts(chip_t *c) { c->operations = c->intf_reads = c->intcap_reads = c->gpio_reads = 0; }

static void programme_test(void)
{
    // The exact register programme, in register order, each read back, then GPIO.
    chip_t c; io_expander_io_t io = chip_io(&c);
    io_expander_t x = {0};
    assert(io_expander_configure(&x, &io, 100) && x.configured);
    static const uint8_t want[7][2] = {{0x00, 0xff}, {0x01, 0x00}, {0x02, 0x3e}, {0x03, 0x00}, {0x04, 0x00},
                                       {0x05, 0x00}, {0x06, 0xc1}};
    assert(c.write_count == 7);
    for (unsigned i = 0; i < 7; i++) assert(c.writes[i].reg == want[i][0] && c.writes[i].value == want[i][1]);
    assert(c.operations == 7 + 7 + 1 && c.gpio_reads == 1);
    assert(x.image == 0xff && x.image_valid);
    for (unsigned s = 0; s < IO_SOURCE_COUNT; s++) assert(x.lines[s].count == 0);

    // A readback that differs in any register leaves it unconfigured, as does any failed transfer.
    for (unsigned reg = 0; reg < 7; reg++) {
        // One bit of the register holds the opposite of the programme's value.
        io = chip_io(&c);
        const uint8_t value = io_expander_programme[reg], bit = value ? (uint8_t)(value & -value) : 0x02;
        c.ignored = bit;
        c.reg[reg] = value ^ bit;
        io_expander_t y = {0};
        assert(!io_expander_configure(&y, &io, 0) && !y.configured);
    }
    for (unsigned fail = 1; fail <= 15; fail++) {
        io = chip_io(&c); c.fail_at = fail;
        io_expander_t y = {0};
        assert(!io_expander_configure(&y, &io, 0) && !y.configured);
    }

    // A line already low at the first configuration counts as asserting then.
    io = chip_io(&c);
    c.port = c.reference = (uint8_t)(0xff & ~IO_SOURCE_PIN(IO_SOURCE_PWR));
    io_expander_t y = {0};
    assert(io_expander_configure(&y, &io, 250));
    assert(y.lines[IO_SOURCE_PWR].count == 1 && y.lines[IO_SOURCE_PWR].level == 0 && y.lines[IO_SOURCE_PWR].last_ms == 250);
    assert(y.lines[IO_SOURCE_ETH].count == 0 && io_expander_take_fell(&y) == IO_SOURCE_PIN(IO_SOURCE_PWR));
    // A later configuration records only what changed since the image.
    c.port = c.reference = 0xff;
    assert(io_expander_configure(&y, &io, 300));
    assert(y.lines[IO_SOURCE_PWR].count == 2 && y.lines[IO_SOURCE_PWR].level == 1 && y.lines[IO_SOURCE_ETH].count == 0);
    assert(io_expander_take_fell(&y) == 0);

    // Disabling clears GPINTEN and leaves it unconfigured, even when the write fails.
    chip_reset_counts(&c);
    assert(io_expander_disable(&y, &io) && !y.configured && c.reg[MCP23008_GPINTEN] == 0);
    assert(io_expander_configure(&y, &io, 400) && c.reg[MCP23008_GPINTEN] == 0x3e);
    c.fail_at = c.operations + 1;
    assert(!io_expander_disable(&y, &io) && !y.configured);
}

// A configured expander with all lines idle high.
static io_expander_io_t configured(chip_t *c, io_expander_t *x)
{
    io_expander_io_t io = chip_io(c);
    memset(x, 0, sizeof *x);
    assert(io_expander_configure(x, &io, 0) && x->image == 0xff);
    chip_reset_counts(c);
    return io;
}

static void decode_test(void)
{
    chip_t c; io_expander_t x; bool low;
    // INTF 0x20 with INTCAP 0x1E: PWR asserted (low).
    io_expander_io_t io = configured(&c, &x);
    c.reg[MCP23008_INTF] = 0x20; c.reg[MCP23008_INTCAP] = 0x1e; c.pending = true;
    c.port = (uint8_t)(c.port & ~0x20);
    assert(io_expander_service(&x, &io, 1000, &low) && !low);
    assert(x.lines[IO_SOURCE_PWR].count == 1 && x.lines[IO_SOURCE_PWR].level == 0 && x.lines[IO_SOURCE_PWR].last_ms == 1000);
    for (unsigned s = 0; s < IO_SOURCE_PWR; s++) assert(x.lines[s].count == 0);
    assert(c.intf_reads == 1 && c.intcap_reads == 1 && c.gpio_reads == 1);
    assert(io_expander_take_fell(&x) == 0x20 && io_expander_take_fell(&x) == 0);

    // INTF 0x06 with both lines low: two sources in one event.
    io = configured(&c, &x);
    chip_drive(&c, (uint8_t)(c.port & ~0x06));
    assert(c.reg[MCP23008_INTF] == 0x06 && c.pending);
    assert(io_expander_service(&x, &io, 2000, &low) && !low);
    assert(x.lines[IO_SOURCE_ETH].count == 1 && x.lines[IO_SOURCE_ETH].level == 0);
    assert(x.lines[IO_SOURCE_BARO].count == 1 && x.lines[IO_SOURCE_BARO].level == 0);
    assert(x.lines[IO_SOURCE_HUM].count == 0 && c.gpio_reads == 1);
    assert(io_expander_take_fell(&x) == 0x06);

    // INT still low after the read: changes arrived during handling, and further reads take
    // them. TEMP falls as INTCAP is read and BARO as the confirming GPIO read clears it.
    io = configured(&c, &x);
    c.during[0] = 0xcf; c.during[1] = 0xcb; c.during_count = 2;
    chip_drive(&c, 0xdf);
    assert(io_expander_service(&x, &io, 3000, &low) && !low);
    assert(c.intcap_reads == 1 && c.gpio_reads == 2);
    assert(x.lines[IO_SOURCE_PWR].count == 1 && x.lines[IO_SOURCE_TEMP].count == 1 && x.lines[IO_SOURCE_TEMP].level == 0);
    assert(x.lines[IO_SOURCE_BARO].count == 1 && x.lines[IO_SOURCE_BARO].level == 0 && x.image == 0xcb);

    // A second line flagged after the capture shows its old level in INTCAP: it changed all the same.
    io = configured(&c, &x);
    chip_drive(&c, (uint8_t)(c.port & ~0x20));
    chip_drive(&c, (uint8_t)(c.port & ~0x10));
    assert(c.reg[MCP23008_INTF] == 0x30 && (c.reg[MCP23008_INTCAP] & 0x10));
    assert(io_expander_service(&x, &io, 4000, &low) && !low);
    assert(x.lines[IO_SOURCE_PWR].count == 1 && x.lines[IO_SOURCE_PWR].level == 0);
    assert(x.lines[IO_SOURCE_TEMP].count == 1 && x.lines[IO_SOURCE_TEMP].level == 0 && c.gpio_reads == 1);

    // The INA3221's alert released before the service (a rail sample read its flags first):
    // the assertion is recorded, the confirming read records the release, and the next
    // alert is still an assertion.
    io = configured(&c, &x);
    chip_drive(&c, (uint8_t)(c.port & ~0x20));
    chip_drive(&c, (uint8_t)(c.port | 0x20));
    assert(io_expander_service(&x, &io, 5000, &low) && !low);
    assert(x.lines[IO_SOURCE_PWR].count == 2 && x.lines[IO_SOURCE_PWR].level == 1 && (x.image & 0x20));
    assert(io_expander_take_fell(&x) == 0x20);
    chip_drive(&c, (uint8_t)(c.port & ~0x20));
    assert(io_expander_service(&x, &io, 5100, &low));
    assert(x.lines[IO_SOURCE_PWR].count == 3 && x.lines[IO_SOURCE_PWR].level == 0 && io_expander_take_fell(&x) == 0x20);

    // A line that keeps changing holds INT low: the reads stop at the limit and report it.
    io = configured(&c, &x);
    for (unsigned i = 0; i < 5; i++) c.during[i] = i & 1 ? 0xfb : 0xff;
    c.during_count = 5;
    chip_drive(&c, 0xfb);
    assert(io_expander_service(&x, &io, 6000, &low) && low && x.stuck == 1);
    assert(c.gpio_reads == IO_EXPANDER_REREADS);

    // Nothing flagged (a panel readback's GPIO read already took the change): no INTCAP read.
    io = configured(&c, &x);
    assert(io_expander_service(&x, &io, 7000, &low) && !low);
    assert(c.intf_reads == 1 && c.intcap_reads == 0 && c.gpio_reads == 0);

    // GP0, GP6 and GP7 never record a change; the image follows them.
    io = configured(&c, &x);
    io_expander_gpio(&x, (uint8_t)(x.image ^ 0xc1), 7500);
    for (unsigned s = 0; s < IO_SOURCE_COUNT; s++) assert(x.lines[s].count == 0);
    assert(x.image == (uint8_t)(c.port ^ 0xc1) && io_expander_take_fell(&x) == 0);

    // A failed transfer leaves the expander unconfigured, for the board task to configure again.
    io = configured(&c, &x);
    chip_drive(&c, (uint8_t)(c.port & ~0x20));
    c.fail_at = 2; // INTCAP
    assert(!io_expander_service(&x, &io, 8000, &low) && !x.configured && low);
}

static void log_test(void)
{
    chip_t c; io_expander_t x;
    io_expander_io_t io = configured(&c, &x);
    assert(io_expander_take_log(&x, 0) == 0 && io_expander_log_wait_ms(&x, 0) == -1);
    chip_drive(&c, (uint8_t)(c.port & ~0x20));
    assert(io_expander_service(&x, &io, 1000, NULL));
    assert(io_expander_log_wait_ms(&x, 1000) == 0 && io_expander_take_log(&x, 1000) == 0x20);
    // The release 2 s later is counted at once and logged when the 10 s have passed.
    chip_drive(&c, (uint8_t)(c.port | 0x20));
    assert(io_expander_service(&x, &io, 3000, NULL) && x.lines[IO_SOURCE_PWR].count == 2);
    assert(io_expander_take_log(&x, 3000) == 0 && io_expander_log_wait_ms(&x, 3000) == 8000);
    // Another line is not held back by it.
    chip_drive(&c, (uint8_t)(c.port & ~0x10));
    assert(io_expander_service(&x, &io, 4000, NULL));
    assert(io_expander_log_wait_ms(&x, 4000) == 0 && io_expander_take_log(&x, 4000) == 0x10);
    assert(io_expander_take_log(&x, 10999) == 0 && io_expander_take_log(&x, 11000) == 0x20);
    assert(io_expander_log_wait_ms(&x, 11000) == -1);
}

// A TLC5916 chain of length bits ending in GP0: it holds the frame from the previous write.
// stuck: -1 for none, or the level GP0 is held at.
static uint32_t chain_samples(uint32_t frame, unsigned length, int stuck)
{
    unsigned chain[24] = {0}; // chain[length - 1] is the last stage, on GP0
    for (unsigned pass = 0; pass < 2; pass++) {
        uint32_t samples = 0;
        for (unsigned k = 0; k < 24; k++) {
            unsigned sdo = stuck < 0 ? chain[length - 1] : (unsigned)stuck;
            samples |= (uint32_t)sdo << k;
            for (unsigned i = length - 1; i > 0; i--) chain[i] = chain[i - 1];
            chain[0] = (frame >> (23 - k)) & 1;
        }
        if (pass) return samples;
    }
    return 0;
}

static uint32_t next_random(uint32_t *state)
{
    *state ^= *state << 13; *state ^= *state >> 17; *state ^= *state << 5;
    return *state;
}

static void chain_test(void)
{
    // The formula against a simulated chain of each length, for random frames.
    uint32_t seed = 0x2468ace1;
    for (unsigned i = 0; i < 2000; i++) {
        uint32_t frame = next_random(&seed) & 0xffffff;
        assert(io_expander_chain_expected(frame, 24) == chain_samples(frame, 24, -1));
        assert(io_expander_chain_expected(frame, 16) == chain_samples(frame, 16, -1));
        // 24 bits read the frame back in shift order; 16 bits lag it by a byte.
        for (unsigned k = 0; k < 24; k++) {
            assert(((io_expander_chain_expected(frame, 24) >> k) & 1) == ((frame >> (23 - k)) & 1));
            assert(((io_expander_chain_expected(frame, 16) >> k) & 1) == ((frame >> (23 - (k + 8) % 24)) & 1));
        }
    }
    // A worked frame: status 0x00, amber 0xbf, green 0x00 (the boot frame).
    assert(io_expander_chain_expected(0x00bf00, 24) == 0x00fd00);
    assert(io_expander_chain_expected(0x00bf00, 16) == 0x0000fd);

    // Detection adopts the length of a simulated chain of each length.
    for (unsigned length = 16; length <= 24; length += 8) {
        io_expander_chain_t c = {0};
        uint32_t frame = 0x0263c1, diff;
        assert(io_expander_chain_check(&c, frame, chain_samples(frame, length, -1), &diff) == IO_CHAIN_DETECTED);
        assert(c.length == length && diff == 0 && c.runs == 1);
        assert(io_expander_chain_check(&c, frame, chain_samples(frame, length, -1), &diff) == IO_CHAIN_MATCH);
        // GP0 pulled to ground with the panel fitted: a mismatch naming the clocks that read 1.
        uint32_t expected = io_expander_chain_expected(frame, length);
        assert(io_expander_chain_check(&c, frame, chain_samples(frame, length, 0), &diff) == IO_CHAIN_MISMATCH);
        assert(diff == expected && c.mismatches == 1 && c.runs == 3);
    }

    // A chain stuck at 1 (no panel: the GP0 pull-up) matches neither length.
    io_expander_chain_t c = {0};
    uint32_t diff;
    for (unsigned run = 1; run <= 4; run++) {
        uint32_t frame = next_random(&seed) & 0xffffff;
        if (frame == 0xffffff) frame = 0x00bf00;
        io_chain_result_t r = io_expander_chain_check(&c, frame, chain_samples(frame, 24, 1), &diff);
        assert(r == (run == 3 ? IO_CHAIN_UNDETECTED : IO_CHAIN_FAULT) && c.length == 0 && c.faults == run);
    }
    // Samples that match both lengths decide nothing and count no fault (all ones through
    // a line stuck at 1 look like either); samples that match neither still count one.
    io_expander_chain_t d = {0};
    assert(io_expander_chain_check(&d, 0xffffff, 0xffffff, &diff) == IO_CHAIN_INCONCLUSIVE);
    assert(io_expander_chain_check(&d, 0x5a5a5a, 0, &diff) == IO_CHAIN_FAULT);
    assert(io_expander_chain_check(&d, 0x5a5a5a, io_expander_chain_expected(0x5a5a5a, 24), &diff) == IO_CHAIN_INCONCLUSIVE);
    assert(d.length == 0 && d.faults == 1 && d.runs == 3);
    // Once detected, a later frame is checked against the adopted length.
    assert(io_expander_chain_check(&d, 0x0263c1, chain_samples(0x0263c1, 16, -1), &diff) == IO_CHAIN_DETECTED && d.length == 16);
    assert(io_expander_chain_check(&d, 0x00bf00, chain_samples(0x00bf00, 24, -1), &diff) == IO_CHAIN_MISMATCH);
    assert(diff == (io_expander_chain_expected(0x00bf00, 24) ^ io_expander_chain_expected(0x00bf00, 16)));
}

int main(void)
{
    assert(!strcmp(io_source_name(IO_SOURCE_ETH), "ETH") && !strcmp(io_source_name(IO_SOURCE_PWR), "PWR"));
    assert(IO_SOURCE_PIN(IO_SOURCE_ETH) == 0x02 && IO_SOURCE_PIN(IO_SOURCE_PWR) == 0x20);
    programme_test(); decode_test(); log_test(); chain_test();
    puts("MCP23008 programme, interrupt decode, rate-limited logs and panel chain readback passed");
    return 0;
}
