#ifndef NVF_BOARD_H
#define NVF_BOARD_H
#include "esp_err.h"
#include "hardware_manifest.h"
void observer_board_manifest(const hardware_manifest_result_t *manifest, uint64_t (*now_ns)(void));
// Thread-safe request, applied and saved by the board task. First-boot default
// 20%; subsequent boots restore the saved setting before enabling PWM.
void observer_board_set_brightness(unsigned percent);
void observer_board_cycle_brightness(void);
// Peripheral identification, GNSS-only RTC startup and the two-colour panel.
esp_err_t observer_board_start(void);
// The identifiers a commissioning record binds (docs/COMMISSIONING.md §2), read from the
// parts themselves. An identifier that cannot be read is left invalid, never guessed; the
// slot-14 record is unreadable until the ATECC data zone is locked. Bench use: it holds the
// secure element for several I2C transactions.
typedef struct {
    bool atecc_valid, rtc_present, rtc_valid, board_valid, attestation_valid, revision_valid;
    uint8_t atecc_serial[9], rtc_eui64[8], board_eui64[8], attestation[72];
    uint16_t revision;
} observer_board_identity_t;
void observer_board_identity(observer_board_identity_t *out);
#endif
