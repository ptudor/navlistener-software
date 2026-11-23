#include "jam_pulse.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A receiver that reports once a second: one report at each whole second from the tracker's
// last to `until`, flagged in the seconds listed in `on`.
static jam_pulse_t p;
static int64_t clock_s;
static unsigned known_events, forgotten_events;
static bool explained_any;

static void report(bool flagged, uint32_t signature)
{
    const jam_pulse_change_t change = jam_pulse_feed(&p, flagged, signature, clock_s * 1000);
    if (change == JAM_PULSE_KNOWN) known_events++;
    if (change == JAM_PULSE_FORGOTTEN) forgotten_events++;
}
// Quiet reports up to, not including, second `to`.
static void quiet(int64_t to) { for (; clock_s < to; clock_s++) report(false, 0); }
// A crossing at second `at`, flagged for `reports` reports; returns whether every one of its
// reports was explained.
static bool crossing(int64_t at, unsigned reports, uint32_t signature)
{
    bool all = true;
    quiet(at);
    for (unsigned i = 0; i < reports; i++, clock_s++) {
        report(true, signature);
        const bool explained = jam_pulse_explains(&p, clock_s * 1000 + 300);
        all = all && explained;
        explained_any = explained_any || explained;
    }
    report(false, 0); // the report that ends it
    clock_s++;
    return all;
}
static void restart(void)
{
    memset(&p, 0, sizeof p);
    clock_s = 0;
    known_events = forgotten_events = 0;
    explained_any = false;
}
// A crossing whose one flagged report arrives at a time in milliseconds, cleared by the
// report a second later; returns whether it was explained.
static bool crossing_at(int64_t at_ms, uint32_t signature)
{
    jam_pulse_change_t change = jam_pulse_feed(&p, true, signature, at_ms);
    if (change == JAM_PULSE_KNOWN) known_events++;
    if (change == JAM_PULSE_FORGOTTEN) forgotten_events++;
    const bool explained = jam_pulse_explains(&p, at_ms + 100);
    change = jam_pulse_feed(&p, false, 0, at_ms + 1000);
    if (change == JAM_PULSE_KNOWN) known_events++;
    if (change == JAM_PULSE_FORGOTTEN) forgotten_events++;
    return explained;
}
// The few milliseconds by which a report's arrival differs from its second, as the bench
// showed them: -12 to +12 ms, from a fixed sequence.
static int64_t jitter(unsigned *state)
{
    *state = *state * 1103515245u + 12345u;
    return (int64_t)(*state >> 16) % 25 - 12;
}

int main(void)
{
    const uint32_t l2[] = {1227600, 1246000, 1268520, 1278750}, l1[] = {1575420, 1602000};
    const uint32_t path2 = jam_pulse_signature(l2, 4), path0 = jam_pulse_signature(l1, 2);
    const uint32_t reordered[] = {1246000, 1227600, 1268520, 1278750};
    assert(path2 && path0 && path2 != path0 && jam_pulse_signature(l2, 3) != path2);
    assert(jam_pulse_signature(reordered, 4) != path2 && jam_pulse_signature(NULL, 0) != 0);

    // A radar that turns in 12 s and is flagged at every turn. The first four crossings are
    // jamming like any other; with the fourth the source is known, and from the fifth its
    // crossings are explained. Nothing between them is.
    restart();
    for (unsigned turn = 0; turn < 4; turn++) {
        assert(!crossing(30 + 12 * turn, 1, path2) && !p.known == (turn < 3));
        assert(!jam_pulse_explains(&p, clock_s * 1000));
    }
    assert(p.known && known_events == 1 && !explained_any && p.period_ms == 12000 && p.source == path2);
    for (unsigned turn = 4; turn < 40; turn++) assert(crossing(30 + 12 * turn, 1, path2));
    assert(p.known && p.crossings == 36 && known_events == 1 && !forgotten_events && p.period_ms == 12000);

    // A crossing that falls across two reports is one crossing. Flags in a third report are
    // not: explained while they could be a crossing, then not, and the source stays known.
    assert(crossing(30 + 12 * 40, 2, path2) && p.crossings == 37);
    quiet(30 + 12 * 41);
    report(true, path2); assert(jam_pulse_explains(&p, clock_s * 1000 + 300)); clock_s++;
    report(true, path2); assert(jam_pulse_explains(&p, clock_s * 1000 + 300)); clock_s++;
    report(true, path2); assert(!jam_pulse_explains(&p, clock_s * 1000 + 300)); clock_s++;
    for (unsigned i = 0; i < 20; i++, clock_s++) { report(true, path2); assert(!jam_pulse_explains(&p, clock_s * 1000)); }
    report(false, 0); clock_s++;
    assert(p.known && p.crossings == 37 && !p.misses);
    // The turns it hid are counted from the last crossing: the next one due is still explained.
    assert(crossing(30 + 12 * 44, 1, path2) && p.crossings == 38);

    // Flags when no crossing is due, on other frequencies, or on frequencies that change
    // while they are up, are not explained, whatever their length.
    assert(!crossing(30 + 12 * 44 + 5, 1, path2) && p.known && p.misses == 1);
    assert(crossing(30 + 12 * 45, 1, path2) && !p.misses);       // one stray does not unseat it
    assert(!crossing(30 + 12 * 46, 1, path0) && p.known);        // due, but not its frequencies
    quiet(30 + 12 * 47);
    report(true, path2); assert(jam_pulse_explains(&p, clock_s * 1000)); clock_s++;
    report(true, path0); assert(!jam_pulse_explains(&p, clock_s * 1000)); clock_s++;
    report(false, 0); clock_s++;
    assert(p.known && p.crossings == 39);                        // and nothing was learnt from it
    // Explained flags are explained for seconds, not for as long as no report contradicts them.
    quiet(30 + 12 * 48);
    report(true, path2);
    assert(jam_pulse_explains(&p, clock_s * 1000 + JAM_PULSE_EXPLAIN_MS));
    assert(!jam_pulse_explains(&p, clock_s * 1000 + JAM_PULSE_EXPLAIN_MS + 1) && !jam_pulse_explains(&p, clock_s * 1000 - 1));
    clock_s++; report(false, 0); clock_s++;

    // Two crossings in a row that fit no turn: the source is forgotten, and its frequencies
    // are jamming again until four crossings agree anew.
    const int64_t base = 30 + 12 * 48;
    assert(!crossing(base + 5, 1, path2) && p.known && p.misses == 1);
    assert(!crossing(base + 16, 1, path2) && !p.known && forgotten_events == 1);
    assert(!crossing(base + 27, 1, path2) && !crossing(base + 38, 1, path2) && p.known && known_events == 2);
    assert(p.period_ms == 11000 && crossing(base + 49, 1, path2));

    // A source that is flagged at some turns only: 59, 71, 143 and 155 s, as one board saw it.
    // Whole turns apart count, when two of the intervals are single turns.
    restart();
    assert(!crossing(59, 1, path2) && !crossing(71, 1, path2) && !crossing(143, 1, path2) && !p.known);
    assert(!crossing(155, 1, path2) && p.known && p.period_ms == 12000);
    assert(crossing(155 + 24, 1, path2) && crossing(155 + 24 + 120, 1, path2));  // two turns on, then ten
    // Unflagged for more turns than it may be: forgotten at the next report, flagged or not.
    assert(!crossing(155 + 24 + 120 + 132, 1, path2) && !p.known && forgotten_events == 1);
    restart();
    assert(!crossing(59, 1, path2) && !crossing(83, 1, path2) && !crossing(107, 1, path2) && !crossing(143, 1, path2));
    assert(!p.known); // 24, 24 and 36 s apart: no interval beyond doubt a single turn within reach

    // Reports a second apart see a period that is no whole number of seconds as intervals of
    // two lengths. 12.5 s: 12 and 13 s by turns.
    restart();
    int64_t at = 40;
    for (unsigned turn = 0; turn < 60; turn++) {
        const bool explained = crossing(at, 1, path2);
        assert(explained == (turn >= 4));
        at += turn % 2 ? 13 : 12;
    }
    assert(p.known && p.period_ms > 12200 && p.period_ms < 12800 && known_events == 1 && !forgotten_events);
    // 4.8 s, a terminal radar's turn: 5, 5, 5, 5, 4 s.
    restart();
    at = 20;
    for (unsigned turn = 0; turn < 50; turn++) {
        const bool explained = crossing(at, 1, path0);
        assert(explained == (turn >= 4));
        at += turn % 5 == 4 ? 4 : 5;
    }
    assert(p.known && p.period_ms > 4500 && p.period_ms < 5100 && !forgotten_events);

    // No period outside 4 to 20 s is learnt, and none from flags that last.
    restart();
    for (unsigned turn = 0; turn < 12; turn++) assert(!crossing(20 + 3 * turn, 1, path2));
    assert(!p.known);
    restart();
    for (unsigned turn = 0; turn < 12; turn++) assert(!crossing(20 + 25 * turn, 1, path2));
    assert(!p.known);
    // Three individually admissible intervals must not average into a period above the
    // range: 20, 21 and 21 seconds would otherwise produce 20.666 seconds.
    restart();
    assert(!crossing(20, 1, path2) && !crossing(40, 1, path2));
    assert(!crossing(61, 1, path2) && !crossing(82, 1, path2) && !p.known);
    // Nor may tolerance pull a source learnt at the lower boundary down below it. Two
    // three-second misfits forget it instead of adapting its four-second period.
    restart();
    assert(!crossing(20, 1, path2) && !crossing(24, 1, path2));
    assert(!crossing(28, 1, path2) && !crossing(32, 1, path2));
    assert(p.known && p.period_ms == JAM_PULSE_PERIOD_MIN_MS);
    assert(!crossing(35, 1, path2) && p.known && p.misses == 1);
    assert(!crossing(38, 1, path2) && !p.known && forgotten_events == 1);
    // A source whose period is the very edge of the range keeps its place when its reports
    // come some milliseconds early or late: it is learnt once, never forgotten, explained at
    // every crossing after the fourth, and its period is held to the range throughout.
    static const int32_t edges[] = {JAM_PULSE_PERIOD_MIN_MS, JAM_PULSE_PERIOD_MAX_MS};
    for (unsigned e = 0; e < 2; e++) {
        restart();
        unsigned state = 1 + e;
        for (unsigned turn = 0; turn < 400; turn++) {
            const bool explained = crossing_at(20000 + (int64_t)turn * edges[e] + jitter(&state), path2);
            assert(explained == (turn >= 4));
            if (p.known) assert(p.period_ms >= JAM_PULSE_PERIOD_MIN_MS && p.period_ms <= JAM_PULSE_PERIOD_MAX_MS);
        }
        assert(p.known && known_events == 1 && !forgotten_events && p.crossings == 396);
        assert(p.period_ms >= edges[e] - 2 && p.period_ms <= edges[e] + 2);
    }
    // The allowance is for the timing of reports and no more. A turn that is the allowance
    // short of the range is taken; a millisecond more, and it is not, nor is one that is a
    // whole report short, however the reports' timing falls.
    restart();
    for (unsigned turn = 0; turn < 4; turn++) (void)crossing_at(20000 + 4000 * turn, path2);
    assert(p.known && p.period_ms == JAM_PULSE_PERIOD_MIN_MS);
    assert(crossing_at(32000 + 4000 - JAM_PULSE_JITTER_MS, path2) && p.period_ms == JAM_PULSE_PERIOD_MIN_MS);
    int64_t last = 32000 + 4000 - JAM_PULSE_JITTER_MS;
    assert(!crossing_at(last + 4000 - JAM_PULSE_JITTER_MS - 1, path2) && p.known && p.misses == 1);
    assert(crossing_at(last + 8000, path2) && !p.misses);          // two turns after the last that fitted
    last += 8000;
    assert(!crossing_at(last + 3000 + JAM_PULSE_JITTER_MS - 1, path2) && p.misses == 1); // 3 s, as late as it can come
    restart();
    for (unsigned turn = 0; turn < 4; turn++) (void)crossing_at(20000 + 20000 * turn, path2);
    assert(p.known && p.period_ms == JAM_PULSE_PERIOD_MAX_MS);
    assert(crossing_at(80000 + 20000 + JAM_PULSE_JITTER_MS, path2) && p.period_ms == JAM_PULSE_PERIOD_MAX_MS);
    last = 80000 + 20000 + JAM_PULSE_JITTER_MS;
    assert(!crossing_at(last + 20000 + JAM_PULSE_JITTER_MS + 1, path2) && p.misses == 1);
    // Learning takes the same allowance and no more: four crossings a hair under four
    // seconds apart are a source at the edge, held to it; a quarter second more under are not.
    restart();
    for (unsigned turn = 0; turn < 4; turn++) (void)crossing_at(20000 + 3990 * turn, path2);
    assert(p.known && p.period_ms == JAM_PULSE_PERIOD_MIN_MS);
    restart();
    for (unsigned turn = 0; turn < 8; turn++) assert(!crossing_at(20000 + (4000 - JAM_PULSE_JITTER_MS - 1) * turn, path2));
    assert(!p.known);
    restart();
    for (unsigned turn = 0; turn < 4; turn++) (void)crossing_at(20000 + 20010 * turn, path2);
    assert(p.known && p.period_ms == JAM_PULSE_PERIOD_MAX_MS);
    restart();
    for (unsigned turn = 0; turn < 8; turn++) assert(!crossing_at(20000 + (20000 + JAM_PULSE_JITTER_MS + 1) * turn, path2));
    assert(!p.known);

    restart();
    for (unsigned turn = 0; turn < 12; turn++) assert(!crossing(20 + 12 * turn, 3, path2));
    assert(!p.known && !explained_any);
    // Jamming that stays is never a crossing, with a source known or without one.
    restart();
    quiet(10);
    for (unsigned i = 0; i < 120; i++, clock_s++) { report(true, path2); assert(!jam_pulse_explains(&p, clock_s * 1000)); }
    report(false, 0);
    assert(!p.known && !p.count);
    // Irregular short bursts: no four of them agree.
    restart();
    static const int64_t bursts[] = {12, 21, 44, 51, 79, 92, 99, 137, 150, 171};
    for (unsigned i = 0; i < sizeof bursts / sizeof bursts[0]; i++) assert(!crossing(bursts[i], 1, path2));
    assert(!p.known && !known_events);

    // A receiver that names no frequencies gives one signature, and is tracked all the same.
    restart();
    for (unsigned turn = 0; turn < 8; turn++) assert(crossing(25 + 10 * turn, 1, 1) == (turn >= 4));
    assert(p.known && p.period_ms == 10000 && p.source == 1);
    puts("jam pulse: a periodic source learnt from four crossings, explained when due, held to its range at the edges, and forgotten on misfits or silence passed");
    return 0;
}
