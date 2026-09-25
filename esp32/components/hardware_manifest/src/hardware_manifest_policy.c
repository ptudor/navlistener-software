#include "hardware_manifest_policy.h"

hardware_manifest_action_t hardware_manifest_decide(
    hardware_manifest_observation_t observation,
    hardware_manifest_known_eui_t known_eui)
{
    switch (observation) {
    case HARDWARE_MANIFEST_OBS_ABSENT:
        return known_eui == HARDWARE_MANIFEST_KNOWN_NONE
                   ? HARDWARE_MANIFEST_ACTION_ABSENT : HARDWARE_MANIFEST_ACTION_IO_ERROR;

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

hardware_manifest_read_t hardware_manifest_classify_read(
    hardware_manifest_i2c_t probe,
    hardware_manifest_i2c_t transfer,
    hardware_manifest_i2c_t reprobe)
{
    if (probe == HARDWARE_MANIFEST_I2C_NOT_FOUND)
        return HARDWARE_MANIFEST_READ_ABSENT;
    if (probe != HARDWARE_MANIFEST_I2C_OK)
        return HARDWARE_MANIFEST_READ_IO_ERROR;
    if (transfer == HARDWARE_MANIFEST_I2C_OK)
        return HARDWARE_MANIFEST_READ_OK;
    // A Microchip 24CS acknowledges the F8h Manufacturer ID code whatever its
    // strap address, then refuses another device's address byte. That is how
    // the empty strap address reads beside a single fitted 24CS.
    if (transfer == HARDWARE_MANIFEST_I2C_TRANSFER_FAILED && reprobe == HARDWARE_MANIFEST_I2C_OK)
        return HARDWARE_MANIFEST_READ_REFUSED;
    return HARDWARE_MANIFEST_READ_IO_ERROR;
}
