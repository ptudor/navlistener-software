#include "rtc_discipline.h"
#include "rtc_io.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// A simulated board: an MCP7941X whose crystal runs crystal_ppb fast and whose digital trim
// shortens (positive steps) or lengthens one second in sixty by two 32.768 kHz cycles a step
// (DS20002266 section 5.6), timed against a GNSS pulse on an ESP32 counter that itself runs
// 3.3 ppm fast, with a few ticks of capture noise.
#define GNSS_PERIOD_TICKS 80000264u
#define CYCLE_PS 30517578 // one 32.768 kHz cycle, in picoseconds
typedef struct {
    int32_t crystal_ppb;
    int steps;
    unsigned noise_ticks;
    int64_t phase_ps; // RTC edge minus GNSS edge, unwrapped
    uint64_t rtc_seconds, gnss_seconds;
    uint32_t random;
    rtc_discipline_sample_t sample;
} sim_t;
static uint32_t next_random(sim_t *s) { s->random = s->random * 1664525u + 1013904223u; return s->random >> 8; }
// One more RTC second: its length against the true second moves the phase.
static void rtc_second(sim_t *s)
{
    s->rtc_seconds++;
    s->phase_ps -= (int64_t)s->crystal_ppb * 1000;
    if (s->rtc_seconds % 60 == 0) s->phase_ps -= (int64_t)s->steps * 2 * CYCLE_PS;
}
// A reading after `rtc` more RTC seconds and `gnss` more GNSS seconds.
static const rtc_discipline_sample_t *reading(sim_t *s, unsigned rtc, unsigned gnss)
{
    for (unsigned i = 0; i < rtc; i++) rtc_second(s);
    s->gnss_seconds += gnss;
    // Within half a second either way, as the board reports it.
    int64_t wrapped = s->phase_ps % INT64_C(1000000000000);
    if (wrapped >= INT64_C(500000000000)) wrapped -= INT64_C(1000000000000);
    if (wrapped < -INT64_C(500000000000)) wrapped += INT64_C(1000000000000);
    int64_t ticks = wrapped * GNSS_PERIOD_TICKS / INT64_C(1000000000000);
    if (s->noise_ticks) ticks += (int64_t)(next_random(s) % (2 * s->noise_ticks + 1)) - s->noise_ticks;
    s->sample.valid = true;
    s->sample.phase_ticks = (int32_t)ticks;
    s->sample.gnss_period_ticks = GNSS_PERIOD_TICKS;
    s->sample.gnss_captured = s->gnss_seconds;
    s->sample.rtc_captured = s->rtc_seconds;
    return &s->sample;
}
// Feeds readings until a window fills; returns how many it took.
static unsigned window(rtc_discipline_t *d, sim_t *s, rtc_discipline_measure_t *m)
{
    for (unsigned n = 1; n < 5000; n++) {
        // The board task does not poll in step with the pulses: now and then a reading sees no
        // new RTC edge, and the next sees two.
        const unsigned pattern = next_random(s) % 50;
        if (pattern == 0) {
            assert(!rtc_discipline_feed(d, reading(s, 0, 1), m));
            n++;
            if (rtc_discipline_feed(d, reading(s, 2, 1), m)) return n;
        } else if (rtc_discipline_feed(d, reading(s, 1, 1), m)) return n;
    }
    assert(!"a window never filled");
    return 0;
}
static int32_t magnitude(int32_t v) { return v < 0 ? -v : v; }

static void trim_test(void)
{
    // SIGN set adds cycles: faster. Zero disables trimming.
    assert(rtc_trim_steps(0x00) == 0 && rtc_trim_steps(0xff) == 127 && rtc_trim_steps(0x7f) == -127);
    assert(rtc_trim_steps(0x83) == 3 && rtc_trim_steps(0x03) == -3 && rtc_trim_steps(0x80) == 0);
    assert(rtc_trim_register(0) == 0x00 && rtc_trim_register(127) == 0xff && rtc_trim_register(-127) == 0x7f);
    assert(rtc_trim_register(3) == 0x83 && rtc_trim_register(-3) == 0x03);
    assert(rtc_trim_register(500) == 0xff && rtc_trim_register(-500) == 0x7f);
    for (int steps = -127; steps <= 127; steps++) assert(rtc_trim_steps(rtc_trim_register(steps)) == steps);
    // 1.017 ppm a step; the full range is about 129 ppm either way.
    assert(rtc_trim_ppb(1) == 1017 && rtc_trim_ppb(-3) == -3052 && rtc_trim_ppb(127) == 129191 && rtc_trim_ppb(0) == 0);

    // The first unit: a crystal 3.09 ppm fast under a register that woke up at +127.
    assert(rtc_discipline_trim(3090, 127) == -3);
    // Held while it leaves three quarters of a step or less (763 ppb), moved beyond that.
    assert(rtc_discipline_trim(3090, -3) == -3 && rtc_discipline_trim(3800, -3) == -3 && rtc_discipline_trim(2300, -3) == -3);
    assert(rtc_discipline_trim(3820, -3) == -4 && rtc_discipline_trim(2280, -3) == -2);
    assert(rtc_discipline_trim(-700, 0) == 0 && rtc_discipline_trim(-800, 0) == 1 && rtc_discipline_trim(800, 0) == -1);
    assert(rtc_discipline_trim(0, 0) == 0 && rtc_discipline_trim(0, 5) == 0 && rtc_discipline_trim(-20500, 0) == 20);
    // A crystal beyond the register's reach gets the end of the range, and stays there.
    assert(rtc_discipline_trim(200000, 0) == -127 && rtc_discipline_trim(200000, -127) == -127);
    assert(rtc_discipline_trim(-200000, 0) == 127 && rtc_discipline_trim(-129000, 127) == 127);
}

static void measure_test(void)
{
    static rtc_discipline_t d;
    rtc_discipline_measure_t m;
    // The crystal's rate comes out the same whatever the trim in force, because the median
    // does not see the one second in sixty that carries the trim.
    static const int trims[] = {127, -3, 0, -127, 60};
    for (unsigned i = 0; i < sizeof trims / sizeof trims[0]; i++) {
        sim_t s = {.crystal_ppb = 3090, .steps = trims[i], .noise_ticks = 3, .phase_ps = -INT64_C(120000000000), .random = 7 + i};
        rtc_discipline_reset(&d);
        const unsigned readings = window(&d, &s, &m);
        assert(readings >= RTC_DISCIPLINE_SAMPLES + 1 && readings <= RTC_DISCIPLINE_SAMPLES + 40);
        assert(magnitude(m.crystal_ppb - 3090) <= 15 && m.spread_ppb <= 150 && rtc_discipline_usable(&m));
        assert(rtc_discipline_trim(m.crystal_ppb, trims[i]) == -3);
        // The next window follows straight on, and agrees.
        (void)window(&d, &s, &m);
        assert(magnitude(m.crystal_ppb - 3090) <= 15);
    }
    // A slow crystal, and a phase that wraps past half a second inside the window: +127 steps
    // on a crystal 40 ppm fast moves it 0.1 s in ten minutes.
    sim_t slow = {.crystal_ppb = -20500, .steps = 0, .noise_ticks = 3, .phase_ps = INT64_C(499999000000), .random = 3};
    rtc_discipline_reset(&d);
    (void)window(&d, &slow, &m);
    assert(magnitude(m.crystal_ppb + 20500) <= 15 && rtc_discipline_usable(&m) && rtc_discipline_trim(m.crystal_ppb, 0) == 20);
    sim_t wrapping = {.crystal_ppb = 40000, .steps = 127, .noise_ticks = 3, .phase_ps = -INT64_C(449000000000), .random = 5};
    rtc_discipline_reset(&d);
    (void)window(&d, &wrapping, &m);
    assert(wrapping.phase_ps < -INT64_C(500000000000) && magnitude(m.crystal_ppb - 40000) <= 15 && rtc_discipline_usable(&m));
    // Readings too rough to agree are measured but not used.
    sim_t rough = {.crystal_ppb = 3090, .steps = 0, .noise_ticks = 400, .random = 11};
    rtc_discipline_reset(&d);
    (void)window(&d, &rough, &m);
    assert(m.spread_ppb > RTC_DISCIPLINE_SPREAD_MAX_PPB && !rtc_discipline_usable(&m));
    assert(!rtc_discipline_usable(&(rtc_discipline_measure_t){.crystal_ppb = 160000, .spread_ppb = 10}));
    assert(!rtc_discipline_usable(&(rtc_discipline_measure_t){.crystal_ppb = -160000, .spread_ppb = 10}));
    assert(rtc_discipline_usable(&(rtc_discipline_measure_t){.crystal_ppb = -150000, .spread_ppb = 1000}));
}

static void continuity_test(void)
{
    static rtc_discipline_t d;
    rtc_discipline_measure_t m;
    sim_t s = {.crystal_ppb = 3090, .steps = 127, .noise_ticks = 3, .random = 1};
    rtc_discipline_reset(&d);
    for (unsigned i = 0; i < 400; i++) assert(!rtc_discipline_feed(&d, reading(&s, 1, 1), &m));
    assert(d.count == 399);
    // A reading that is not valid (no position, a stale pulse, another trim mode) starts again.
    rtc_discipline_sample_t invalid = *reading(&s, 1, 1);
    invalid.valid = false;
    assert(!rtc_discipline_feed(&d, &invalid, &m) && d.count == 0 && !d.have_last);
    for (unsigned i = 0; i < 300; i++) assert(!rtc_discipline_feed(&d, reading(&s, 1, 1), &m));
    assert(d.count == 299);
    // So does a missed or irregular pulse on either input, and a gap between readings.
    s.sample.gnss_missing = 1;
    assert(!rtc_discipline_feed(&d, reading(&s, 1, 1), &m) && d.count == 0 && d.have_last);
    assert(!rtc_discipline_feed(&d, reading(&s, 1, 1), &m) && d.count == 1);
    s.sample.rtc_discontinuities = 1;
    assert(!rtc_discipline_feed(&d, reading(&s, 1, 1), &m) && d.count == 0);
    assert(!rtc_discipline_feed(&d, reading(&s, 1, 1), &m) && d.count == 1);
    assert(!rtc_discipline_feed(&d, reading(&s, 4, 4), &m) && d.count == 0);
    assert(!rtc_discipline_feed(&d, reading(&s, 3, 3), &m) && d.count == 1);
    // A counter that went back (the ESP32's pulse statistics restarted) is a gap too.
    s.rtc_seconds = 5; s.gnss_seconds = 5;
    assert(!rtc_discipline_feed(&d, reading(&s, 1, 1), &m) && d.count == 0);
    // A zero GNSS period cannot scale the phase.
    rtc_discipline_sample_t zero = *reading(&s, 1, 1);
    zero.gnss_period_ticks = 0;
    assert(!rtc_discipline_feed(&d, &zero, &m) && !d.have_last);
    // After all that, a clean run still measures the crystal.
    (void)window(&d, &s, &m);
    assert(magnitude(m.crystal_ppb - 3090) <= 15 && rtc_discipline_usable(&m));
}

// CONTROL (0x07) and OSCTRIM (0x08) of a simulated part.
typedef struct { uint8_t control, osctrim; unsigned writes; bool read_fails, write_fails, write_ignored; } chip_t;
static bool chip_read(void *ctx, uint8_t reg, uint8_t *out, size_t length)
{
    chip_t *c = ctx;
    if (c->read_fails || reg != 7 || length != 2) return false;
    out[0] = c->control; out[1] = c->osctrim;
    return true;
}
static bool chip_write_trim(void *ctx, uint8_t osctrim)
{
    chip_t *c = ctx;
    c->writes++;
    if (c->write_fails) return false;
    if (!c->write_ignored) c->osctrim = osctrim;
    return true;
}
static void register_test(void)
{
    chip_t c = {.control = 0x40, .osctrim = 0xff};
    const rtc_io_t io = {.ctx = &c, .read = chip_read, .write_trim = chip_write_trim};
    uint8_t control, osctrim, previous = 0;
    assert(rtc_trim_read(&io, &control, &osctrim) && control == 0x40 && osctrim == 0xff);
    // Written, read back, and the old value reported.
    assert(rtc_trim_write(&io, 0x03, &previous) && c.osctrim == 0x03 && previous == 0xff && c.writes == 1);
    // Already there: no write.
    assert(rtc_trim_write(&io, 0x03, &previous) && previous == 0x03 && c.writes == 1 && rtc_trim_write(&io, 0x03, NULL));
    // Coarse trim, or an external clock: not this discipline's register to change.
    c.control = 0x44;
    assert(!rtc_trim_write(&io, 0x00, &previous) && c.osctrim == 0x03 && previous == 0x03 && c.writes == 1);
    c.control = 0x48;
    assert(!rtc_trim_write(&io, 0x00, NULL) && c.osctrim == 0x03 && c.writes == 1);
    // A failed write, a write the part did not take, and a failed read are all failures.
    c.control = 0x40; c.write_fails = true;
    assert(!rtc_trim_write(&io, 0x00, NULL) && c.osctrim == 0x03 && c.writes == 2);
    c.write_fails = false; c.write_ignored = true;
    assert(!rtc_trim_write(&io, 0x00, NULL) && c.osctrim == 0x03 && c.writes == 3);
    c.write_ignored = false; c.read_fails = true;
    assert(!rtc_trim_write(&io, 0x00, NULL) && !rtc_trim_read(&io, &control, &osctrim) && c.writes == 3);
    // A part with no trim writer (the temperature-compensated RTCs) is never written.
    c.read_fails = false;
    const rtc_io_t no_trim = {.ctx = &c, .read = chip_read};
    assert(!rtc_trim_write(&no_trim, 0x00, NULL) && c.osctrim == 0x03);
}

int main(void)
{
    trim_test();
    measure_test();
    continuity_test();
    register_test();
    puts("RTC rate discipline: trim register and step arithmetic, the crystal's rate through any trim, phase "
         "wrap, rough and broken windows, and the guarded OSCTRIM write passed");
    return 0;
}
