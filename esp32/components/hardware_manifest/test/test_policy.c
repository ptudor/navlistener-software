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

#define READ(expected, probe, transfer, reprobe) do {                        \
        hardware_manifest_read_t got = hardware_manifest_classify_read(      \
            HARDWARE_MANIFEST_I2C_##probe, HARDWARE_MANIFEST_I2C_##transfer, \
            HARDWARE_MANIFEST_I2C_##reprobe);                                \
        if (got != HARDWARE_MANIFEST_READ_##expected) {                      \
            fprintf(stderr, "%s:%d: expected %s, got %d\n",                  \
                    __FILE__, __LINE__, #expected, got);                     \
            failures++;                                                      \
        }                                                                    \
    } while (0)

    READ(ABSENT, NOT_FOUND, FAULT, FAULT);
    READ(IO_ERROR, FAULT, FAULT, FAULT);
    READ(OK, OK, OK, FAULT);
    // A refused byte: the device still acknowledges its address.
    READ(REFUSED, OK, TRANSFER_FAILED, OK);
    // A bus timeout, or a device gone mid-read, is never absence.
    READ(IO_ERROR, OK, TRANSFER_FAILED, FAULT);
    READ(IO_ERROR, OK, TRANSFER_FAILED, NOT_FOUND);
    READ(IO_ERROR, OK, FAULT, OK);

    printf("hardware_manifest policy: %d failure(s)\n", failures);
    return failures ? 1 : 0;
}
