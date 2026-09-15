#include "hardware_manifest_policy.h"

#include <stdio.h>

static int failures;

#define CHECK(expected, observation, known) do {                              \
        hardware_manifest_action_t got = hardware_manifest_decide(           \
            (observation), (known));                                          \
        if (got != (expected)) {                                              \
            fprintf(stderr, "%s:%d: expected %d, got %d\n",                 \
                    __FILE__, __LINE__, (expected), got);                     \
            failures++;                                                      \
        }                                                                     \
    } while (0)

int main(void)
{
    CHECK(HARDWARE_MANIFEST_ACTION_ABSENT,
          HARDWARE_MANIFEST_OBS_ABSENT, HARDWARE_MANIFEST_KNOWN_NONE);
    CHECK(HARDWARE_MANIFEST_ACTION_IO_ERROR,
          HARDWARE_MANIFEST_OBS_ABSENT, HARDWARE_MANIFEST_KNOWN_SAME);
    CHECK(HARDWARE_MANIFEST_ACTION_IO_ERROR,
          HARDWARE_MANIFEST_OBS_ABSENT, HARDWARE_MANIFEST_KNOWN_DIFFERENT);

    CHECK(HARDWARE_MANIFEST_ACTION_IO_ERROR,
          HARDWARE_MANIFEST_OBS_IO_ERROR, HARDWARE_MANIFEST_KNOWN_NONE);
    CHECK(HARDWARE_MANIFEST_ACTION_INITIALIZE,
          HARDWARE_MANIFEST_OBS_BLANK, HARDWARE_MANIFEST_KNOWN_NONE);
    CHECK(HARDWARE_MANIFEST_ACTION_RECOVER,
          HARDWARE_MANIFEST_OBS_BLANK, HARDWARE_MANIFEST_KNOWN_SAME);
    CHECK(HARDWARE_MANIFEST_ACTION_CONFIRM_REPLACEMENT,
          HARDWARE_MANIFEST_OBS_BLANK, HARDWARE_MANIFEST_KNOWN_DIFFERENT);
    CHECK(HARDWARE_MANIFEST_ACTION_REJECT_INVALID,
          HARDWARE_MANIFEST_OBS_PROGRAMMED_INVALID,
          HARDWARE_MANIFEST_KNOWN_NONE);
    CHECK(HARDWARE_MANIFEST_ACTION_REJECT_INVALID,
          HARDWARE_MANIFEST_OBS_PROGRAMMED_INVALID,
          HARDWARE_MANIFEST_KNOWN_SAME);
    CHECK(HARDWARE_MANIFEST_ACTION_USE,
          HARDWARE_MANIFEST_OBS_PROGRAMMED_VALID,
          HARDWARE_MANIFEST_KNOWN_NONE);
    CHECK(HARDWARE_MANIFEST_ACTION_USE,
          HARDWARE_MANIFEST_OBS_PROGRAMMED_VALID,
          HARDWARE_MANIFEST_KNOWN_SAME);
    CHECK(HARDWARE_MANIFEST_ACTION_CONFIRM_REPLACEMENT,
          HARDWARE_MANIFEST_OBS_PROGRAMMED_VALID,
          HARDWARE_MANIFEST_KNOWN_DIFFERENT);

    printf("hardware_manifest policy: %d failure(s)\n", failures);
    return failures ? 1 : 0;
}
