// Receiver discovery and volatile configuration packets. MON-VER, SEC-UNIQID, CFG-VALSET and
// every key the firmware sets on more than one receiver are identical in the M9 SPG 4.04
// (UBX-21022436), M10 SPG 5.10 (UBX-21035062) and X20 HPG 2.11 (UBXDOC-304424225-21617)
// interface descriptions; the ZED-X20P's own keys are from the last.
#ifndef UBX_PROBE_H
#define UBX_PROBE_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
size_t ubx_poll_version(uint8_t out[8]);
size_t ubx_get_ram(uint8_t out[16], uint32_t key);
// UBX-SEC-UNIQID: the poll, and the answer's chip ID as lower-case hex (10 or 12 digits).
size_t ubx_poll_unique_id(uint8_t out[8]);
bool ubx_unique_id(const uint8_t *body, size_t len, char out[13]);
size_t ubx_set_ram(uint8_t out[20], uint32_t key, uint32_t value, unsigned width);
// One RAM-only CFG-VALSET (version 0) of up to UBX_SETTING_ITEMS one-byte (L or U1) items.
// The receiver checks a message whole and applies all of it or none (M10 SPG 5.10, 3.10.5),
// so a combination that is valid only whole, such as the M10's four GNSS, is one setting.
#define UBX_SETTING_ITEMS 5u
#define UBX_SETTING_MAX (12u + 5u * UBX_SETTING_ITEMS)
typedef struct { uint32_t key; uint8_t value; } ubx_item_t;
typedef struct { uint8_t count; ubx_item_t item[UBX_SETTING_ITEMS]; } ubx_setting_t;
// The settings applied, in order, to a listed receiver; returns the count. lna_low_gain
// chooses the mode of the ZED-X20P's internal LNAs, low gain or the receiver's default
// normal gain, and no other receiver's list depends on it. Both of its lists have the
// same settings in the same order.
size_t ubx_settings(const char *module, bool lna_low_gain, const ubx_setting_t **settings);
// The setting's packet and its length; 0 for an empty or oversized setting.
size_t ubx_encode_setting(uint8_t out[UBX_SETTING_MAX], const ubx_setting_t *setting);
// Whether setting the key resets the receiver's GNSS subsystem: wait for its ACK and then
// 500 ms before the next command. A setting does when any of its items does.
bool ubx_key_resets_gnss(uint32_t key);
bool ubx_setting_resets_gnss(const ubx_setting_t *setting);
// Whether the receiver applies the setting only when its GNSS subsystem next restarts: the
// ZED-X20P's internal LNA modes do (integration manual UBXDOC-963802114-12901, 2.2.3). Such a
// setting is read before it is written, and the restart is sent only when the write changed
// it: a receiver that kept its RAM while the observer restarted holds it already, and keeps
// its fix.
bool ubx_setting_needs_restart(const ubx_setting_t *setting);
// The CFG-VALGET (version 0, RAM layer) that reads the setting's keys, and its length; 0 for
// an empty or oversized setting.
#define UBX_SETTING_POLL_MAX (12u + 4u * UBX_SETTING_ITEMS)
size_t ubx_encode_setting_poll(uint8_t out[UBX_SETTING_POLL_MAX], const ubx_setting_t *setting);
// What a CFG-VALGET answer says of the setting: every item at its value in the RAM layer
// (held), every item there and at least one at another value (differs), or an answer that is
// not about all of this setting's keys in that layer (other).
typedef enum { UBX_SETTING_OTHER, UBX_SETTING_DIFFERS, UBX_SETTING_HELD } ubx_setting_read_t;
ubx_setting_read_t ubx_setting_read(const ubx_setting_t *setting, const uint8_t *body, size_t len);
// UBX-CFG-RST for a controlled software reset of the GNSS subsystem alone (resetMode 0x02)
// that clears nothing in the backup memory, so the receiver starts hot. The RAM configuration,
// the UART rate with it, stays as it is; the receiver does not acknowledge the message (X20
// HPG 2.02 interface description, 3.10.3; integration manual, 3.6.1).
size_t ubx_restart_gnss(uint8_t out[12]);
// CFG-UART1-BAUDRATE, the U4 key set before the others when the receiver is found at
// another rate.
#define UBX_KEY_UART1_BAUDRATE 0x40520001u
// CFG-HW-RF1_LNA_MODE_LOWGAIN and its two siblings: the ZED-X20P's internal LNA of each RF
// block in low gain rather than normal gain (X20 HPG 2.02 interface description, 6.9.4).
#define UBX_KEY_RF1_LNA_LOWGAIN 0x10a3006au
#define UBX_KEY_RF2_LNA_LOWGAIN 0x10a3006bu
#define UBX_KEY_RF3_LNA_LOWGAIN 0x10a3006cu
// The interface descriptions' name for a key the firmware sets, for the log; NULL for any other.
const char *ubx_key_name(uint32_t key);
// A MON-VER body whose extensions include exactly "MOD=<module>", such as "NEO-M9N".
bool ubx_version_is_module(const uint8_t *body, size_t len, const char *module);
typedef struct { unsigned state, length; uint8_t checksum, expected; } nmea_probe_t;
bool nmea_probe_feed(nmea_probe_t *p, uint8_t byte);
#endif
