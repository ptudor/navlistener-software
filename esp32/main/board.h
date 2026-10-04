#ifndef NVF_BOARD_H
#define NVF_BOARD_H
#include "esp_err.h"
#include "hardware_manifest.h"
#include "board_reservations.h"
#include "../../common/board_uid.h"
// Adopts the manifest at boot, before observer_board_start and netcfg_load: its CAT_INTSAT
// entry names the board, and every driver runs only for a part it lists installed.
void observer_board_manifest(const hardware_manifest_result_t *manifest, uint64_t (*now_ns)(void));
// The board the manifest names, when this image drives it; NULL without a usable manifest,
// for an unknown, unsupported or conflicting board, and on the C6.
const observer_board_t *observer_board_current(void);
// Whether that board's manifest lists the part installed; false whenever the board is NULL.
bool observer_board_lists(uint8_t category, uint8_t id);
// The board's Ethernet port is listed and driven by this image: the wired uplink.
bool observer_board_wired_uplink(void);
// The manifest lists a part with per-unit settings (the thermocouple converter or the IMU).
bool observer_board_sensor_settings(void);
// Thread-safe request, applied and saved by the board task. First-boot default
// 20%; subsequent boots restore the saved setting before enabling PWM.
void observer_board_set_brightness(unsigned percent);
void observer_board_cycle_brightness(void);
// Peripheral identification, GNSS-only RTC startup and the two-colour panel.
esp_err_t observer_board_start(void);
// The identifiers a commissioning record binds (docs/COMMISSIONING.md §2), read from the
// parts themselves, and the RTC it records. An identifier that cannot be read is left
// invalid, never guessed; the slot-14 record is unreadable until the ATECC data zone is
// locked. rtc_present means the board's RTC answered as its model; rtc_valid means its
// factory EUI-64 was read, which only an MCP79412 has. Bench use: it holds the secure
// element for several I2C transactions.
typedef struct {
    bool atecc_valid, rtc_present, rtc_valid, board_valid, attestation_valid, revision_valid;
    uint16_t rtc_model_id; // NVF_RTC_* for the board's fitted RTC
    uint8_t atecc_serial[9], rtc_eui64[8], board_uid[NVF_BOARD_UID_SIZE], attestation[72];
    bool eeprom_valid;
    uint8_t board_uid_address, eeprom_address, eeprom_uid[NVF_BOARD_UID_SIZE];
    uint16_t revision;
} observer_board_identity_t;
void observer_board_identity(observer_board_identity_t *out);
#endif
