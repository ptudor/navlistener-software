#ifndef JAM_PULSE_H
#define JAM_PULSE_H
// Jamming flags that come one report at a time at a steady period: the beam of a rotating
// radar crossing the antenna. A long-range L-band radar turns once in 10 to 12 seconds and
// transmits inside the band of GPS L2, GLONASS L2, BeiDou B3 and Galileo E6, and a receiver
// within its sight reports each crossing as jamming, for one report, on the frequencies of
// that RF path (esp32/README.md, The ATTENTION lamp). The tracker recognises such a source
// from the reports themselves and says of the flags now up whether they are its next
// crossing, so that an observer beside an airway is not held in alarm by its radar. It
// judges only when flags arrive and how long they last: flags that outlast a crossing,
// arrive when none is due or name other frequencies are never a crossing.
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

// Flags that are still up in a report this long after their first are no crossing: one
// report holds a crossing, or two when it falls across their boundary.
#define JAM_PULSE_UP_MS 1500
// The flags now up are explained for no longer than this, reports or none.
#define JAM_PULSE_EXPLAIN_MS 3500
// The periods looked for. Below four seconds a report every second cannot tell a period
// from chance.
#define JAM_PULSE_PERIOD_MIN_MS 4000
#define JAM_PULSE_PERIOD_MAX_MS 20000
// A crossing is due within a quarter of the period of its time, and within this.
#define JAM_PULSE_TOLERANCE_MAX_MS 1500
// A source may go unflagged for this many turns and still be due; after them it is forgotten.
#define JAM_PULSE_TURNS_MAX 10
// Crossings that must agree before a source is known, two of their intervals single turns.
#define JAM_PULSE_LOCK 4
// Crossings on the source's frequencies that fit no turn, in a row, before it is forgotten.
#define JAM_PULSE_MISSES 2
#define JAM_PULSE_HISTORY 6

typedef struct {
    // The flags now up: when their episode began, when a report last carried them, what
    // frequencies they name, whether they began when the known source was due, and whether
    // the frequencies changed while they were up.
    bool up, expected, mixed;
    int64_t began_ms, seen_ms;
    uint32_t signature;
    // The known source: its frequencies, its period, its latest crossing, the crossings
    // that fitted since it became known (it stops at 65535) and the misfits since the last.
    bool known;
    uint32_t source;
    int32_t period_ms;
    int64_t last_ms;
    uint16_t crossings;
    uint8_t misses;
    // Crossings that are not yet a source: the starts of the latest with one signature.
    uint32_t candidate;
    uint8_t count;
    int64_t start_ms[JAM_PULSE_HISTORY];
} jam_pulse_t;

typedef enum { JAM_PULSE_SAME, JAM_PULSE_KNOWN, JAM_PULSE_FORGOTTEN } jam_pulse_change_t;

// jam_pulse_feed takes one jamming report: whether it carries flags, a signature of the
// frequencies flagged (jam_pulse_signature; any constant where the receiver names none) and
// when it came. It returns whether a source became known or was forgotten with this report.
// jam_pulse_explains says whether the flags now up are a crossing of the known source: they
// began when it was due, on its frequencies, and have not outlasted a crossing.
// jam_pulse_signature is a signature of flagged frequencies in kHz, in the order the receiver
// gives them; never 0. The functions are inline so that every build of the receiver status
// has them without another source file.

static inline uint32_t jam_pulse_signature(const uint32_t *khz, size_t count)
{
    uint32_t hash = 2166136261u; // FNV-1a
    for (size_t i = 0; i < count; i++)
        for (unsigned b = 0; b < 4; b++) { hash ^= khz[i] >> (8 * b) & 0xff; hash *= 16777619u; }
    return hash ? hash : 1;
}

static inline int32_t jam_pulse_tolerance(int32_t period_ms)
{
    const int32_t quarter = period_ms / 4;
    return quarter < JAM_PULSE_TOLERANCE_MAX_MS ? quarter : JAM_PULSE_TOLERANCE_MAX_MS;
}

// The whole turns of the period nearest to an interval, and how far the interval is from them.
static inline int32_t jam_pulse_turns(int64_t interval_ms, int32_t period_ms, int64_t *off_ms)
{
    const int64_t turns = (interval_ms + period_ms / 2) / period_ms;
    *off_ms = interval_ms - turns * period_ms;
    if (*off_ms < 0) *off_ms = -*off_ms;
    return turns > INT32_MAX ? INT32_MAX : (int32_t)turns;
}

// Whether a crossing of the known source is due at a time: a whole number of turns after its
// latest, no more than it may go unflagged.
static inline bool jam_pulse_due(const jam_pulse_t *p, int64_t at_ms, int32_t *turns)
{
    int64_t off;
    *turns = jam_pulse_turns(at_ms - p->last_ms, p->period_ms, &off);
    return *turns >= 1 && *turns <= JAM_PULSE_TURNS_MAX && off <= jam_pulse_tolerance(p->period_ms);
}

static inline void jam_pulse_forget(jam_pulse_t *p)
{
    p->known = false;
    p->expected = false;
    p->crossings = 0;
    p->misses = 0;
}

static inline void jam_pulse_remember(jam_pulse_t *p, uint32_t signature, int64_t start_ms)
{
    if (signature != p->candidate) { p->candidate = signature; p->count = 0; }
    if (p->count == JAM_PULSE_HISTORY) {
        memmove(p->start_ms, p->start_ms + 1, sizeof p->start_ms - sizeof p->start_ms[0]);
        p->count--;
    }
    p->start_ms[p->count++] = start_ms;
}

// A source is known when the latest crossings of one signature are whole turns of one period
// apart, two of the intervals single turns: the shortest interval is the period looked for.
static inline bool jam_pulse_learn(jam_pulse_t *p)
{
    if (p->count < JAM_PULSE_LOCK) return false;
    const int64_t *start = p->start_ms + p->count - JAM_PULSE_LOCK;
    int64_t shortest = start[1] - start[0];
    for (unsigned i = 1; i + 1 < JAM_PULSE_LOCK; i++)
        if (start[i + 1] - start[i] < shortest) shortest = start[i + 1] - start[i];
    if (shortest < JAM_PULSE_PERIOD_MIN_MS || shortest > JAM_PULSE_PERIOD_MAX_MS) return false;
    const int32_t period = (int32_t)shortest;
    int32_t all_turns = 0;
    unsigned single = 0;
    for (unsigned i = 0; i + 1 < JAM_PULSE_LOCK; i++) {
        int64_t off;
        const int32_t turns = jam_pulse_turns(start[i + 1] - start[i], period, &off);
        if (turns < 1 || turns > JAM_PULSE_TURNS_MAX || off > jam_pulse_tolerance(period)) return false;
        if (turns == 1) single++;
        all_turns += turns;
    }
    if (single < 2) return false;
    p->known = true;
    p->source = p->candidate;
    p->period_ms = (int32_t)((start[JAM_PULSE_LOCK - 1] - start[0]) / all_turns);
    p->last_ms = start[JAM_PULSE_LOCK - 1];
    p->crossings = 0;
    p->misses = 0;
    p->count = 0;
    return true;
}

static inline jam_pulse_change_t jam_pulse_feed(jam_pulse_t *p, bool flagged, uint32_t signature, int64_t now)
{
    jam_pulse_change_t change = JAM_PULSE_SAME;
    // A known source that has been due and unflagged for all the turns it may be is gone.
    if (p->known && now - p->last_ms > (int64_t)JAM_PULSE_TURNS_MAX * p->period_ms + jam_pulse_tolerance(p->period_ms)) {
        jam_pulse_forget(p);
        p->count = 0;
        change = JAM_PULSE_FORGOTTEN;
    }
    if (flagged) {
        if (!p->up) {
            int32_t turns;
            p->up = true;
            p->mixed = false;
            p->began_ms = now;
            p->signature = signature;
            p->expected = p->known && signature == p->source && jam_pulse_due(p, now, &turns);
        } else {
            if (signature != p->signature) p->mixed = true;
            if (p->mixed || now - p->began_ms > JAM_PULSE_UP_MS) p->expected = false;
        }
        p->seen_ms = now;
        return change;
    }
    if (!p->up) return change;
    p->up = false;
    p->expected = false;
    // Flags that outlasted a crossing, or whose frequencies changed, are no crossing: nothing
    // is learnt from them and a known source stays as it is.
    if (p->mixed || p->seen_ms - p->began_ms > JAM_PULSE_UP_MS) return change;
    if (p->known && p->signature == p->source) {
        int32_t turns;
        if (jam_pulse_due(p, p->began_ms, &turns)) {
            // Each fitting crossing moves the period an eighth of the way to what it measured.
            const int32_t measured = (int32_t)((p->began_ms - p->last_ms) / turns);
            p->period_ms += (measured - p->period_ms) / 8;
            p->last_ms = p->began_ms;
            p->misses = 0;
            p->count = 0; // a misfit before it was a stray
            if (p->crossings < UINT16_MAX) p->crossings++;
            return change;
        }
        jam_pulse_remember(p, p->signature, p->began_ms);
        if (++p->misses < JAM_PULSE_MISSES) return change;
        jam_pulse_forget(p);
        return jam_pulse_learn(p) ? JAM_PULSE_KNOWN : JAM_PULSE_FORGOTTEN;
    }
    if (p->known) return change; // a crossing on other frequencies while a source is known
    jam_pulse_remember(p, p->signature, p->began_ms);
    return jam_pulse_learn(p) ? JAM_PULSE_KNOWN : change;
}

static inline bool jam_pulse_explains(const jam_pulse_t *p, int64_t now)
{
    return p->up && p->expected && now >= p->began_ms && now - p->began_ms <= JAM_PULSE_EXPLAIN_MS;
}
#endif
