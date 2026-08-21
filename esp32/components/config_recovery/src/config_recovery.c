#include "config_recovery.h"

#include <limits.h>
#include <stddef.h>

static uint32_t add_saturated(uint32_t a, uint32_t b)
{
    return b > UINT32_MAX - a ? UINT32_MAX : a + b;
}

void config_recovery_init(config_recovery_t *state)
{
    if (state) *state = (config_recovery_t){0};
}

config_recovery_event_t config_recovery_update(config_recovery_t *state, bool pressed,
                                                uint32_t elapsed_ms)
{
    if (!state) return CONFIG_RECOVERY_EVENT_NONE;

    if (!state->armed) {
        state->released_ms = 0;
        if (!pressed) {
            // A short press, or contact bounce before the hold threshold, cancels instead
            // of accumulating unrelated presses into one destructive gesture.
            state->held_ms = 0;
            return CONFIG_RECOVERY_EVENT_NONE;
        }
        state->held_ms = add_saturated(state->held_ms, elapsed_ms);
        if (state->held_ms >= CONFIG_RECOVERY_HOLD_MS) {
            state->armed = true;
            return CONFIG_RECOVERY_EVENT_ARMED;
        }
        return CONFIG_RECOVERY_EVENT_NONE;
    }

    if (pressed) {
        state->released_ms = 0;
        return CONFIG_RECOVERY_EVENT_NONE;
    }

    state->released_ms = add_saturated(state->released_ms, elapsed_ms);
    if (state->released_ms < CONFIG_RECOVERY_RELEASE_DEBOUNCE_MS)
        return CONFIG_RECOVERY_EVENT_NONE;

    // Consume the confirmation before returning it. A button that remains released cannot
    // emit CONFIG_RECOVERY_EVENT_CONFIRMED again on the next poll.
    config_recovery_init(state);
    return CONFIG_RECOVERY_EVENT_CONFIRMED;
}
