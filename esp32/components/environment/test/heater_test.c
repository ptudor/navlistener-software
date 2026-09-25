#include "env_heater.h"
#include <assert.h>
#include <math.h>
#include <stdio.h>
#include <string.h>
// The shipped Kconfig defaults.
static const env_heater_config_t config = {
    .trigger_rh = 98.0, .min_c = -20.0, .max_c = 60.0, .dry_rh = 5.0, .overtemp_c = 80.0, .stable_c = 0.2,
    .trigger_ms = 4 * 3600000LL, .gap_ms = 30 * 60000LL, .interval_s = 14 * 86400LL, .step_ms = 10000,
    .max_on_ms = 300000, .settle_ms = 10 * 60000LL, .stable_ms = 5 * 60000LL, .recovery_max_ms = 60 * 60000LL,
};
static const int64_t T0 = 1000000, UTC = 1800000000, DAY = 86400;
static env_sample_t reading(bool valid, double rh, double c)
{ return (env_sample_t){.hdc_valid = valid, .rh_percent = rh, .hdc_c = c, .mcp_valid = true, .mcp_c = c - 2}; }
// Condensing samples every 30 s from *now; the START must come exactly four hours after the first.
static void condense(env_heater_t *h, int64_t *now, int64_t utc)
{
    env_sample_t s = reading(true, 99.5, 12.0);
    int64_t first = *now;
    for (; *now - first < config.trigger_ms; *now += 30000)
        assert(env_heater_sample(h, *now, true, utc, &s) == ENV_HEATER_HOLD);
    assert(*now - first == config.trigger_ms);
    assert(env_heater_sample(h, *now, true, utc, &s) == ENV_HEATER_START);
    assert(h->next.start_utc == utc && h->next.rh_before == 99.5 && h->next.hdc_before == 12.0 &&
           h->next.mcp_before == 10.0 && h->next.valid == (ENV_RUN_RH_BEFORE | ENV_RUN_HDC_BEFORE | ENV_RUN_MCP_BEFORE));
}
static void test_trigger(void)
{
    env_heater_t h; int64_t now = T0;
    env_heater_init(&h, &config, true, 0);
    condense(&h, &now, UTC);
    // Any valid sample below the trigger restarts the count, even one just before four hours.
    env_heater_init(&h, &config, true, 0); now = T0;
    env_sample_t wet = reading(true, 98.0, 12), dry = reading(true, 97.99, 12), bad = reading(false, 0, 0);
    for (int64_t end = now + config.trigger_ms - 30000; now < end; now += 30000)
        assert(env_heater_sample(&h, now, true, UTC, &wet) == ENV_HEATER_HOLD);
    assert(env_heater_sample(&h, now, true, UTC, &dry) == ENV_HEATER_HOLD);
    now += 30000; condense(&h, &now, UTC);
    // Invalid samples neither count nor break the run while valid ones are at most 30 min apart.
    env_heater_init(&h, &config, true, 0); now = T0;
    int64_t first = now;
    assert(env_heater_sample(&h, now, true, UTC, &wet) == ENV_HEATER_HOLD);
    for (unsigned i = 0; i < 8; i++) {
        for (unsigned j = 1; j < 60; j++) assert(env_heater_sample(&h, now + j * 30000, true, UTC, &bad) == ENV_HEATER_HOLD);
        now += config.gap_ms;
        assert(env_heater_sample(&h, now, true, UTC, &wet) == (now - first >= config.trigger_ms ? ENV_HEATER_START : ENV_HEATER_HOLD));
    }
    assert(now - first == config.trigger_ms);
    // A gap one millisecond longer restarts the count at the next valid sample.
    env_heater_init(&h, &config, true, 0); now = T0;
    assert(env_heater_sample(&h, now, true, UTC, &wet) == ENV_HEATER_HOLD);
    now += config.gap_ms + 1;
    condense(&h, &now, UTC);
    // The start needs the HDC between -20 and +60 C; the count continues while it is outside.
    env_heater_init(&h, &config, true, 0); now = T0;
    env_sample_t hot = reading(true, 99, 60.01), cold = reading(true, 99, -20.01);
    env_sample_t top = reading(true, 99, 60.0), bottom = reading(true, 99, -20.0);
    for (int64_t end = now + config.trigger_ms + 3600000; now <= end; now += 30000)
        assert(env_heater_sample(&h, now, true, UTC, (now / 30000) % 2 ? &hot : &cold) == ENV_HEATER_HOLD);
    assert(env_heater_sample(&h, now, true, UTC, &top) == ENV_HEATER_START);
    assert(env_heater_sample(&h, now + 30000, true, UTC, &bottom) == ENV_HEATER_START);
}
static void test_time_gates(void)
{
    env_heater_t h; int64_t now = T0; env_sample_t wet = reading(true, 99, 12);
    // No trusted UTC, or an unreadable last-run record: never automatic.
    env_heater_init(&h, &config, true, 0);
    for (int64_t end = now + 2 * config.trigger_ms; now <= end; now += 30000)
        assert(env_heater_sample(&h, now, false, 0, &wet) == ENV_HEATER_HOLD);
    assert(env_heater_sample(&h, now, true, UTC, &wet) == ENV_HEATER_START); // UTC arrives: start now
    env_heater_init(&h, &config, false, 0); now = T0;
    for (int64_t end = now + 2 * config.trigger_ms; now <= end; now += 30000)
        assert(env_heater_sample(&h, now, true, UTC, &wet) == ENV_HEATER_HOLD);
    // Fourteen days after the previous run, to the second; a stored run in the future holds.
    env_heater_init(&h, &config, true, UTC - 14 * DAY + 1); now = T0;
    for (int64_t end = now + config.trigger_ms; now <= end; now += 30000)
        assert(env_heater_sample(&h, now, true, UTC, &wet) == ENV_HEATER_HOLD);
    assert(env_heater_sample(&h, now, true, UTC + 1, &wet) == ENV_HEATER_START);
    env_heater_init(&h, &config, true, UTC + DAY); now = T0;
    for (int64_t end = now + config.trigger_ms; now <= end; now += 30000)
        assert(env_heater_sample(&h, now, true, UTC, &wet) == ENV_HEATER_HOLD);
    // A run time that cannot be saved does not start, and the four-hour count restarts.
    env_heater_init(&h, &config, true, 0); now = T0;
    condense(&h, &now, UTC);
    env_heater_declined(&h);
    assert(h.state == ENV_HEATER_NORMAL && h.runs == 0 && h.last_utc == 0);
    now += 30000; condense(&h, &now, UTC);
}
// Steps every 10 s from a confirmed start; returns the stop reason and leaves *now at the stop.
static env_heater_stop_t heat(env_heater_t *h, int64_t *now, double rh0, double rh_step, double c0, double c_step,
                              bool heat_en)
{
    for (unsigned i = 1;; i++) {
        assert(!env_heater_step_due(h, *now + config.step_ms - 1));
        *now += config.step_ms;
        assert(env_heater_step_due(h, *now));
        env_sample_t s = reading(true, fmax(rh0 - rh_step * i, 0), c0 + c_step * i);
        if (env_heater_step(h, *now, &s, heat_en) == ENV_HEATER_STOP) {
            assert(h->state == ENV_HEATER_STOPPING && h->run.stop != ENV_HEATER_RUNNING);
            return h->run.stop;
        }
        assert(h->state == ENV_HEATER_HEATING && i < 40);
    }
}
static void start(env_heater_t *h, int64_t *now)
{
    env_heater_init(h, &config, true, 0); *now = T0;
    condense(h, now, UTC);
    assert(env_heater_started(h, *now, ENV_HEATER_RUNNING) == ENV_HEATER_HOLD);
    assert(h->state == ENV_HEATER_HEATING && h->runs == 1 && h->last_utc == UTC && h->run.start_ms == *now);
}
static void test_stops(void)
{
    env_heater_t h; int64_t now, begin;
    // Dry: 99.5 %RH falling 10 points per step reaches 5 %RH or less on step 10.
    start(&h, &now); begin = now;
    assert(heat(&h, &now, 99.5, 10, 20, 3, true) == ENV_HEATER_DRY && now - begin == 100000);
    assert(h.run.rh_stop == 0 && h.run.hdc_peak == 50 && h.run.mcp_peak == 48 && (h.run.valid & ENV_RUN_RH_STOP));
    assert(!env_heater_step_due(&h, now) && env_heater_step_due(&h, now + config.step_ms));
    env_heater_cleared(&h, now + 50, true);
    assert(h.state == ENV_HEATER_RECOVERING && h.run.on_ms == 100050 && !env_heater_step_due(&h, now + 3600000));
    // Over-temperature at 80.0 C, before dry when both happen in one step.
    start(&h, &now); begin = now;
    assert(heat(&h, &now, 99, 4, 20, 5, true) == ENV_HEATER_OVERTEMP && now - begin == 120000);
    start(&h, &now);
    assert(heat(&h, &now, 99, 99, 20, 60, true) == ENV_HEATER_OVERTEMP);
    // Timeout after exactly 300 s; the final step is scheduled at the limit.
    start(&h, &now); begin = now;
    assert(heat(&h, &now, 60, 0, 30, 0, true) == ENV_HEATER_TIMEOUT && now - begin == 300000);
    env_heater_config_t odd = config; odd.step_ms = 7000;
    env_heater_init(&h, &odd, true, 0); now = T0; condense(&h, &now, UTC);
    begin = now; env_heater_started(&h, now, ENV_HEATER_RUNNING);
    env_sample_t mid = reading(true, 60, 30);
    for (unsigned i = 1; i <= 42; i++) {
        now += 7000; assert(env_heater_step_due(&h, now));
        assert(env_heater_step(&h, now, &mid, true) == ENV_HEATER_HOLD);
    }
    assert(!env_heater_step_due(&h, begin + 299999) && env_heater_step_due(&h, begin + 300000));
    assert(env_heater_step(&h, begin + 300000, &mid, true) == ENV_HEATER_STOP && h.run.stop == ENV_HEATER_TIMEOUT);
    // Any I2C error, even on the MCP9808 alone, stops before the other conditions.
    start(&h, &now);
    env_sample_t s = reading(true, 3, 90); s.bus_error = true;
    now += config.step_ms; assert(env_heater_step(&h, now, &s, true) == ENV_HEATER_STOP && h.run.stop == ENV_HEATER_BUS_ERROR);
    assert(h.run.rh_stop == 3 && h.run.hdc_peak == 90); // the valid conversion is still recorded
    // No conversion, or HEAT_EN found clear (the part reset): the sensor is lost.
    start(&h, &now);
    s = reading(false, 0, 0);
    now += config.step_ms; assert(env_heater_step(&h, now, &s, true) == ENV_HEATER_STOP && h.run.stop == ENV_HEATER_SENSOR_LOST);
    assert(!(h.run.valid & (ENV_RUN_RH_STOP | ENV_RUN_HDC_PEAK)));
    start(&h, &now);
    assert(heat(&h, &now, 99, 1, 20, 1, false) == ENV_HEATER_SENSOR_LOST && h.run.rh_stop == 98);
    // A start that did not read back set is a run: its time is saved, then the heater is cleared.
    env_heater_init(&h, &config, true, 0); now = T0; condense(&h, &now, UTC);
    assert(env_heater_started(&h, now, ENV_HEATER_BUS_ERROR) == ENV_HEATER_STOP);
    assert(h.state == ENV_HEATER_STOPPING && h.runs == 1 && h.last_utc == UTC && h.run.stop == ENV_HEATER_BUS_ERROR);
    const char *names[] = {"running", "dry", "timeout", "overtemp", "bus_error", "sensor_lost", "unknown"};
    for (unsigned i = 0; i < sizeof names / sizeof names[0]; i++) assert(!strcmp(env_heater_stop_name(i), names[i]));
}
static void test_clear_retry(void)
{
    env_heater_t h; int64_t now, begin;
    start(&h, &now); begin = now;
    assert(heat(&h, &now, 30, 10, 20, 1, true) == ENV_HEATER_DRY);
    // Until HEAT_EN reads back clear, the heater may be on: retry each step, keep the HDC out.
    for (unsigned i = 0; i < 3; i++) {
        env_heater_cleared(&h, now, false);
        assert(h.state == ENV_HEATER_STOPPING && !env_heater_step_due(&h, now + 9999));
        env_sample_t s = reading(true, 99, 20); s.mcp_c = 70;
        assert(env_heater_sample(&h, now + 5000, true, UTC, &s) == ENV_HEATER_HOLD);
        assert(h.run.mcp_peak == 70 && h.run.hdc_peak == 23 && !h.wet && h.rh95_ms == h.rh98_ms);
        now += config.step_ms; assert(env_heater_step_due(&h, now));
    }
    env_heater_status_t status;
    env_heater_status(&h, now, &status);
    assert(status.state == ENV_HEATER_STOPPING && status.run.on_ms == now - begin && status.runs == 1);
    env_heater_cleared(&h, now, true);
    assert(h.state == ENV_HEATER_RECOVERING && h.run.on_ms == now - begin);
}
// Recovery samples every 30 s from the heater-off time; returns minutes until normal service.
static double recover(double (*temperature)(double minutes), bool (*valid)(double minutes))
{
    env_heater_t h; int64_t now;
    start(&h, &now);
    env_heater_cleared(&h, now, true);
    int64_t off = now;
    for (unsigned i = 1; i <= 200; i++) {
        now = off + i * 30000LL;
        double minutes = i / 2.0;
        env_sample_t s = reading(valid ? valid(minutes) : true, 40, temperature(minutes));
        assert(env_heater_sample(&h, now, true, UTC, &s) == ENV_HEATER_HOLD);
        if (h.state == ENV_HEATER_NORMAL) {
            assert(h.run.recovery_ms == now - off && h.prev_valid == s.hdc_valid && !h.wet);
            assert(!!(h.run.valid & ENV_RUN_HDC_END) == s.hdc_valid && (h.run.valid & ENV_RUN_MCP_END));
            if (s.hdc_valid) assert(h.run.hdc_end == s.hdc_c && h.run.mcp_end == s.mcp_c);
            return minutes;
        }
        env_heater_status_t status;
        env_heater_status(&h, now, &status);
        assert(status.state == ENV_HEATER_RECOVERING && status.run.recovery_ms == now - off);
    }
    assert(!"recovery did not end");
    return 0;
}
static double flat(double m) { (void)m; return 25; }
static double ramp(double m) { return m < 20 ? 45 - m : 25; }        // 0.5 C per sample, then flat
static double noisy(double m) { return 25 + ((int)(m * 2) % 2 ? 0.09 : -0.09); }
static double wobble(double m) { return 25 + ((int)(m * 2) % 4 < 2 ? 0.1 : -0.1); } // spans exactly 0.2 C
static double slow(double m) { return 30 - 0.03 * m; }                 // 0.15 C per 5 minutes
static bool gap(double m) { return m != 8; }
static bool dead(double m) { return m < 3; }
static void test_recovery(void)
{
    assert(recover(flat, NULL) == 10);    // at least ten minutes
    assert(recover(ramp, NULL) == 25);    // five flat minutes, including the reading at the window start
    assert(recover(noisy, NULL) == 10);   // noise below 0.2 C
    assert(recover(slow, NULL) == 10);    // a slow drift under the limit is settled
    assert(recover(wobble, NULL) == 60);  // 0.2 C is not "less than 0.2 C": the hour limit ends it
    assert(recover(flat, gap) == 13.5);   // a missed conversion restarts the five-minute window
    assert(recover(flat, dead) == 60);    // no conversions: the hour limit, with no HDC end value
    // The history keeps a full window at any sample rate: 5 s samples, one per 10 s retained.
    env_heater_t h; int64_t now;
    start(&h, &now); env_heater_cleared(&h, now, true);
    int64_t off = now;
    env_sample_t s = reading(true, 40, 25);
    while (h.state == ENV_HEATER_RECOVERING) { now += 5000; env_heater_sample(&h, now, true, UTC, &s); }
    assert(now - off == config.settle_ms && h.count == ENV_HEATER_HISTORY);
    // Peaks cover heating and recovery; the MCP9808 lags the HDC.
    start(&h, &now); env_heater_cleared(&h, now + 1000, true);
    s = reading(true, 40, 30); s.mcp_c = 55;
    env_heater_sample(&h, now + 31000, true, UTC, &s);
    assert(h.run.mcp_peak == 55 && h.run.hdc_peak == 30 && (h.run.valid & (ENV_RUN_HDC_PEAK | ENV_RUN_MCP_PEAK)) == (ENV_RUN_HDC_PEAK | ENV_RUN_MCP_PEAK));
}
static void test_dwell_and_status(void)
{
    env_heater_t h; int64_t now = T0; env_heater_status_t status;
    env_heater_init(&h, &config, true, 0);
    env_sample_t s96 = reading(true, 96, 20), s99 = reading(true, 99, 20), s90 = reading(true, 90, 20);
    for (int64_t end = now + 3600000; now <= end; now += 30000) env_heater_sample(&h, now, false, 0, &s96);
    assert(h.rh95_ms == 3600000 && h.rh98_ms == 0);
    // 96 -> 99 counts only toward 95; then two hours at 99 count toward both.
    for (int64_t end = now + 7200000; now <= end; now += 30000) env_heater_sample(&h, now, false, 0, &s99);
    assert(h.rh95_ms == 3600000 + 30000 + 7200000 && h.rh98_ms == 7200000);
    now -= 30000;
    env_heater_status(&h, now + 1000, &status);
    assert(status.active && !status.utc_valid && status.last_known && status.state == ENV_HEATER_NORMAL &&
           status.streak_ms == 7200000 && status.rh95_ms == h.rh95_ms && status.runs == 0 && status.last_utc == 0);
    // A longer gap earns no credit and reports no streak; a low reading also ends it.
    env_heater_status(&h, now + config.gap_ms + 1, &status); assert(status.streak_ms == 0);
    now += config.gap_ms + 1; env_heater_sample(&h, now, false, 0, &s99);
    assert(h.rh98_ms == 7200000);
    now += 30000; env_heater_sample(&h, now, false, 0, &s90);
    env_heater_status(&h, now, &status); assert(status.streak_ms == 0 && h.rh95_ms == 3600000 + 30000 + 7200000);
    // A full cycle: the second run needs fourteen more days and four new hours.
    start(&h, &now);
    env_heater_status(&h, now + 20000, &status);
    assert(status.state == ENV_HEATER_HEATING && status.run.on_ms == 20000 && status.run.start_ms == now);
    heat(&h, &now, 50, 10, 20, 2, true); env_heater_cleared(&h, now, true);
    for (int i = 1; h.state != ENV_HEATER_NORMAL; i++) { env_sample_t f = reading(true, 99, 25); env_heater_sample(&h, now + i * 30000LL, true, UTC, &f); }
    now += 20 * 30000;
    int64_t first = now;
    env_sample_t wet = reading(true, 99, 12);
    for (; now - first < config.trigger_ms + 3600000; now += 30000)
        assert(env_heater_sample(&h, now, true, UTC + 14 * DAY - 1, &wet) == ENV_HEATER_HOLD);
    assert(env_heater_sample(&h, now, true, UTC + 14 * DAY, &wet) == ENV_HEATER_START);
    env_heater_started(&h, now, ENV_HEATER_RUNNING);
    assert(h.runs == 2 && h.last_utc == UTC + 14 * DAY && h.run.stop == ENV_HEATER_RUNNING && !(h.run.valid & ENV_RUN_HDC_END));
}
int main(void)
{
    test_trigger();
    test_time_gates();
    test_stops();
    test_clear_retry();
    test_recovery();
    test_dwell_and_status();
    puts("Humidity heater: condensing trigger, UTC/interval gates, stop conditions, heater-off retry, "
         "cool-down stability and dwell counters passed");
    return 0;
}
