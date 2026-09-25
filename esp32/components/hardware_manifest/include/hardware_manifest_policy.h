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

// One board-identity read as the ESP-IDF 5.5 I2C master driver reports it: an
// address probe, the write-then-read transfer, and, only after a failed
// transfer, a second probe of the same address. The driver returns the same
// error for a byte the device refused (NACK) and for a bus timeout, so a
// device that still acknowledges its address has refused the transfer, and
// one that does not has met a bus failure.
typedef enum {
    HARDWARE_MANIFEST_I2C_OK = 0,
    HARDWARE_MANIFEST_I2C_NOT_FOUND,       // the address was not acknowledged
    HARDWARE_MANIFEST_I2C_TRANSFER_FAILED, // a NACK after the address, or a bus timeout
    HARDWARE_MANIFEST_I2C_FAULT,           // any other driver error
} hardware_manifest_i2c_t;

typedef enum {
    HARDWARE_MANIFEST_READ_OK = 0,
    HARDWARE_MANIFEST_READ_ABSENT,
    HARDWARE_MANIFEST_READ_REFUSED,
    HARDWARE_MANIFEST_READ_IO_ERROR,
} hardware_manifest_read_t;

hardware_manifest_read_t hardware_manifest_classify_read(
    hardware_manifest_i2c_t probe,
    hardware_manifest_i2c_t transfer,
    hardware_manifest_i2c_t reprobe);

#ifdef __cplusplus
}
#endif

#endif
