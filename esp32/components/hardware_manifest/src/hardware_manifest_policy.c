#include "hardware_manifest_policy.h"

hardware_manifest_action_t hardware_manifest_decide(
    hardware_manifest_observation_t observation,
    hardware_manifest_known_eui_t known_eui)
{
    switch (observation) {
    case HARDWARE_MANIFEST_OBS_IO_ERROR:
        return HARDWARE_MANIFEST_ACTION_IO_ERROR;

    case HARDWARE_MANIFEST_OBS_BLANK:
        if (known_eui == HARDWARE_MANIFEST_KNOWN_NONE)
            return HARDWARE_MANIFEST_ACTION_INITIALIZE;
        if (known_eui == HARDWARE_MANIFEST_KNOWN_SAME)
            return HARDWARE_MANIFEST_ACTION_RECOVER;
        return HARDWARE_MANIFEST_ACTION_CONFIRM_REPLACEMENT;

    case HARDWARE_MANIFEST_OBS_PROGRAMMED_INVALID:
        return HARDWARE_MANIFEST_ACTION_REJECT_INVALID;

    case HARDWARE_MANIFEST_OBS_PROGRAMMED_VALID:
        if (known_eui == HARDWARE_MANIFEST_KNOWN_DIFFERENT)
            return HARDWARE_MANIFEST_ACTION_CONFIRM_REPLACEMENT;
        return HARDWARE_MANIFEST_ACTION_USE;
    }

    return HARDWARE_MANIFEST_ACTION_IO_ERROR;
}
