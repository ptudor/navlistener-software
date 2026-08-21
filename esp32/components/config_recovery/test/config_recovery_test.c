#include <stdio.h>

#include "config_recovery.h"

static int failures;

#define CHECK(cond, ...)                                  \
    do {                                                  \
        if (!(cond)) {                                    \
            fprintf(stderr, "FAIL: " __VA_ARGS__);        \
            fputc('\n', stderr);                          \
            failures++;                                   \
        }                                                 \
    } while (0)

static config_recovery_event_t run_ms(config_recovery_t *state, bool pressed, uint32_t ms)
{
    config_recovery_event_t last = CONFIG_RECOVERY_EVENT_NONE;
    while (ms) {
        uint32_t step = ms > 20 ? 20 : ms;
        config_recovery_event_t event = config_recovery_update(state, pressed, step);
        if (event != CONFIG_RECOVERY_EVENT_NONE) last = event;
        ms -= step;
    }
    return last;
}

int main(void)
{
    config_recovery_t state;
    config_recovery_init(&state);

    CHECK(run_ms(&state, true, 7999) == CONFIG_RECOVERY_EVENT_NONE,
          "armed before the eight-second threshold");
    CHECK(config_recovery_update(&state, true, 1) == CONFIG_RECOVERY_EVENT_ARMED,
          "did not arm at exactly eight seconds");
    CHECK(state.armed, "armed event did not leave the gesture armed");
    CHECK(run_ms(&state, true, 1000) == CONFIG_RECOVERY_EVENT_NONE,
          "confirmed while the button was still held");

    CHECK(run_ms(&state, false, 80) == CONFIG_RECOVERY_EVENT_NONE,
          "confirmed before release debounce elapsed");
    CHECK(run_ms(&state, true, 20) == CONFIG_RECOVERY_EVENT_NONE,
          "re-press after release bounce emitted an event");
    CHECK(run_ms(&state, false, 100) == CONFIG_RECOVERY_EVENT_CONFIRMED,
          "debounced release did not confirm");
    CHECK(!state.armed, "confirmed gesture did not consume its armed state");
    CHECK(run_ms(&state, false, 1000) == CONFIG_RECOVERY_EVENT_NONE,
          "one release emitted confirmation more than once");

    config_recovery_init(&state);
    CHECK(run_ms(&state, true, 4000) == CONFIG_RECOVERY_EVENT_NONE,
          "short press unexpectedly armed");
    CHECK(run_ms(&state, false, 20) == CONFIG_RECOVERY_EVENT_NONE,
          "short-press release emitted an event");
    CHECK(run_ms(&state, true, 4000) == CONFIG_RECOVERY_EVENT_NONE,
          "separate short presses accumulated toward the threshold");
    CHECK(!state.armed && state.held_ms == 4000,
          "short-press cancellation left unexpected state");

    CHECK(config_recovery_update(NULL, true, UINT32_MAX) == CONFIG_RECOVERY_EVENT_NONE,
          "NULL state emitted an event");

    config_recovery_init(&state);
    CHECK(config_recovery_update(&state, true, UINT32_MAX) == CONFIG_RECOVERY_EVENT_ARMED,
          "large elapsed interval did not saturate and arm safely");

    if (failures) {
        fprintf(stderr, "config_recovery_test: %d failure(s)\n", failures);
        return 1;
    }
    printf("config_recovery_test: OK\n");
    return 0;
}
