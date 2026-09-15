#ifndef HARDWARE_MANIFEST_POLICY_H
#define HARDWARE_MANIFEST_POLICY_H

#ifdef __cplusplus
extern "C" {
#endif

// Pure boot policy. Keep this header free of ESP-IDF dependencies so every
// transition can be tested on the host.
typedef enum {
    HARDWARE_MANIFEST_OBS_IO_ERROR = 0,
    HARDWARE_MANIFEST_OBS_ABSENT,
    HARDWARE_MANIFEST_OBS_BLANK,
    HARDWARE_MANIFEST_OBS_PROGRAMMED_INVALID,
    HARDWARE_MANIFEST_OBS_PROGRAMMED_VALID,
} hardware_manifest_observation_t;

typedef enum {
    HARDWARE_MANIFEST_KNOWN_NONE = 0,
    HARDWARE_MANIFEST_KNOWN_SAME,
    HARDWARE_MANIFEST_KNOWN_DIFFERENT,
} hardware_manifest_known_eui_t;

typedef enum {
    HARDWARE_MANIFEST_ACTION_IO_ERROR = 0,
    HARDWARE_MANIFEST_ACTION_INITIALIZE,
    HARDWARE_MANIFEST_ACTION_RECOVER,
    HARDWARE_MANIFEST_ACTION_CONFIRM_REPLACEMENT,
    HARDWARE_MANIFEST_ACTION_REJECT_INVALID,
    HARDWARE_MANIFEST_ACTION_USE,
    HARDWARE_MANIFEST_ACTION_ABSENT,
} hardware_manifest_action_t;

hardware_manifest_action_t hardware_manifest_decide(
    hardware_manifest_observation_t observation,
    hardware_manifest_known_eui_t known_eui);

#ifdef __cplusplus
}
#endif

#endif
