#include "regulator_thermal.h"
#include <assert.h>
#include <stdio.h>

// The P3's limits: throttle at 95 C, everything at 105 C, release at 85 C.
static const regulator_thermal_limits_t p3 = {.throttle_centi_c = 9500, .full_centi_c = 10500, .release_centi_c = 8500};

// Samples every 5 s from *now until the level changes or `ms` pass; returns the time taken.
static int64_t hold(regulator_thermal_t *t, int64_t *now, int16_t centi_c, int64_t ms)
{
    const int64_t start = *now;
    while (*now - start < ms) {
        *now += 5000;
        if (regulator_thermal_sample(t, *now, true, centi_c)) break;
    }
    return *now - start;
}

static void limits_test(void)
{
    regulator_thermal_t t;
    assert(regulator_thermal_init(&t, &p3, 0) && t.level == 0 && !t.seen && !regulator_thermal_responding(&t, 0));
    // Release must sit below the throttle point, which may equal the full point but not exceed it.
    assert(!regulator_thermal_init(&t, &(regulator_thermal_limits_t){9500, 10500, 9500}, 0));
    assert(!regulator_thermal_init(&t, &(regulator_thermal_limits_t){9500, 9000, 8500}, 0));
    assert(regulator_thermal_init(&t, &(regulator_thermal_limits_t){9500, 9500, 8500}, 0));
    // Each level lowers the cap, every value one of ESP-IDF's mapping-table steps.
    assert(regulator_thermal_tx_cap(0) == 0 && regulator_thermal_tx_cap(1) == 66 && regulator_thermal_tx_cap(2) == 52 &&
           regulator_thermal_tx_cap(3) == 44 && regulator_thermal_tx_cap(4) == 34 && regulator_thermal_tx_cap(9) == 34);
}

static void throttle_test(void)
{
    regulator_thermal_t t;
    int64_t now = 1000;
    assert(regulator_thermal_init(&t, &p3, now));
    // A warm but healthy spreader leaves the radio alone.
    assert(hold(&t, &now, 9499, 600000) == 600000 && t.level == 0 && t.changes == 0 && t.throttled_ms == 0);
    // The first hot reading throttles at once; each further step waits 30 s.
    now += 5000;
    assert(regulator_thermal_sample(&t, now, true, 9500) && t.level == 1);
    assert(hold(&t, &now, 9800, 60000) == 30000 && t.level == 2);
    assert(hold(&t, &now, 9800, 60000) == 30000 && t.level == 3);
    assert(hold(&t, &now, 9800, 60000) == 30000 && t.level == 4);
    assert(hold(&t, &now, 10400, 600000) == 600000 && t.level == 4 && t.changes == 4); // nowhere further to go
    // In the hysteresis band the level holds indefinitely.
    assert(hold(&t, &now, 9000, 3600000) == 3600000 && t.level == 4);
    // Cooling releases one step per two minutes, counting from the last change.
    assert(hold(&t, &now, 8500, 600000) == 5000 && t.level == 3); // the band already served the dwell
    assert(hold(&t, &now, 8500, 600000) == 120000 && t.level == 2);
    assert(hold(&t, &now, 8000, 600000) == 120000 && t.level == 1);
    assert(hold(&t, &now, 8000, 600000) == 120000 && t.level == 0);
    assert(hold(&t, &now, 2500, 600000) == 600000 && t.level == 0 && t.changes == 8);
    // Heating again within 30 s of the last change waits out the step-up dwell.
    regulator_thermal_init(&t, &p3, now);
    now += 5000; assert(regulator_thermal_sample(&t, now, true, 9600) && t.level == 1);
    now += 5000; assert(!regulator_thermal_sample(&t, now, true, 8400) && t.level == 1);
    assert(hold(&t, &now, 9600, 60000) == 25000 && t.level == 2);
}

static void full_test(void)
{
    regulator_thermal_t t;
    int64_t now = 0;
    regulator_thermal_init(&t, &p3, now);
    // At the full point the last level applies at once, from anywhere below it.
    now += 5000; assert(regulator_thermal_sample(&t, now, true, 10500) && t.level == 4 && t.changes == 1);
    regulator_thermal_init(&t, &p3, now);
    now += 5000; assert(regulator_thermal_sample(&t, now, true, 9700) && t.level == 1);
    now += 5000; assert(regulator_thermal_sample(&t, now, true, 10600) && t.level == 4 && t.changes == 2);
}

static void sensor_loss_test(void)
{
    regulator_thermal_t t;
    int64_t now = 0;
    regulator_thermal_init(&t, &p3, now);
    now += 5000; regulator_thermal_sample(&t, now, true, 10000);
    assert(t.level == 1 && regulator_thermal_responding(&t, now));
    // Failed reads neither relax nor tighten the throttle; time still counts as throttled.
    for (int i = 0; i < 100; i++) { now += 5000; assert(!regulator_thermal_sample(&t, now, false, 0)); }
    assert(t.level == 1 && t.centi_c == 10000 && t.throttled_ms == 500000);
    assert(!regulator_thermal_responding(&t, now) && regulator_thermal_responding(&t, t.sample_ms + 59999));
    // The sensor returns cool: release resumes on the dwell, counted from the last change.
    now += 5000; assert(regulator_thermal_sample(&t, now, true, 8000) && t.level == 0);
    assert(regulator_thermal_responding(&t, now) && t.throttled_ms == 505000);
    // Never seen: not responding, unthrottled.
    regulator_thermal_init(&t, &p3, now);
    for (int i = 0; i < 20; i++) { now += 5000; regulator_thermal_sample(&t, now, false, 0); }
    assert(t.level == 0 && !t.seen && !regulator_thermal_responding(&t, now) && t.throttled_ms == 0);
}

static void peak_test(void)
{
    regulator_thermal_t t;
    int64_t now = 0;
    regulator_thermal_init(&t, &p3, now);
    assert(!t.peak_valid);
    regulator_thermal_report_sent(&t);
    assert(!t.peak_valid); // nothing read yet
    now += 5000; regulator_thermal_sample(&t, now, true, 7000);
    now += 5000; regulator_thermal_sample(&t, now, true, 7400);
    now += 5000; regulator_thermal_sample(&t, now, true, 7200);
    assert(t.peak_valid && t.peak_centi_c == 7400 && t.centi_c == 7200);
    // After a report the peak restarts from the latest reading, never below it.
    regulator_thermal_report_sent(&t);
    assert(t.peak_valid && t.peak_centi_c == 7200);
    now += 5000; regulator_thermal_sample(&t, now, true, 7100);
    assert(t.peak_centi_c == 7200);
    now += 5000; regulator_thermal_sample(&t, now, true, -500);
    assert(t.peak_centi_c == 7200 && t.centi_c == -500);
}

int main(void)
{
    limits_test();
    throttle_test();
    full_test();
    sensor_loss_test();
    peak_test();
    puts("regulator thermal: limits, stepped throttle and release, full point, sensor loss and peaks passed");
    return 0;
}
