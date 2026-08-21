// config_recovery — pure long-press/release gesture state machine.
//
// The GPIO, NVS erase, feedback, and reboot remain in main.c. Keeping elapsed-time
// recognition here makes the destructive gesture host-testable without ESP-IDF.

#ifndef CONFIG_RECOVERY_H
#define CONFIG_RECOVERY_H

#include <stdbool.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define CONFIG_RECOVERY_HOLD_MS             8000u
#define CONFIG_RECOVERY_RELEASE_DEBOUNCE_MS  100u

typedef enum {
    CONFIG_RECOVERY_EVENT_NONE = 0,
    CONFIG_RECOVERY_EVENT_ARMED,
    CONFIG_RECOVERY_EVENT_CONFIRMED,
} config_recovery_event_t;

typedef struct {
    uint32_t held_ms;
    uint32_t released_ms;
    bool armed;
} config_recovery_t;

void config_recovery_init(config_recovery_t *state);

// config_recovery_update consumes one sampled button interval. pressed is true for the
// active-low button's logical pressed state; elapsed_ms is the time represented by the
// sample. The gesture arms only after one continuous eight-second hold and confirms only
// after a debounced release, so merely reaching the threshold never erases anything.
config_recovery_event_t config_recovery_update(config_recovery_t *state, bool pressed,
                                                uint32_t elapsed_ms);

#ifdef __cplusplus
}
#endif

#endif // CONFIG_RECOVERY_H
