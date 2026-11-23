#pragma once
#include "esp_err.h"
#include "ubx.h"
#include "gnss_status.h"
// The receiver model the manifest lists, as MON-VER names it ("NEO-M9N", "ZED-X20P",
// "MAX-M10S"), or NULL, which configures no receiver. Call before receiver_start.
void receiver_set_module(const char *name);
// A Septentrio receiver on its COM2 (the P3's mosaic-G5), spoken to in SBF rather than UBX:
// the ESP32 pins of its RTS and CTS lines and of its reset driver, high to hold the receiver
// in reset (the P3's GPIO13 into Q1), or -1 for none. Call before receiver_start.
typedef struct { int rts, cts, reset; } receiver_sbf_pins_t;
void receiver_set_sbf(const receiver_sbf_pins_t *pins);
// Whether the Septentrio path was selected: the receiver's blocks then go to the parser's emit
// callback as GNF1 SBF records, and the pusher announces the sbf feed.
bool receiver_sbf(void);
esp_err_t receiver_start(ubx_parser_t *parser);
bool receiver_alive(void);
void receiver_status(gnss_status_t *out);
void receiver_request_snapshot(void);
bool receiver_request_rf_survey(void);
// Whether the listed receiver has an internal LNA mode that the unit sets (the ZED-X20P),
// and, when it does, whether the unit is set to low gain rather than normal gain.
bool receiver_lna_setting(bool *low_gain);
// The console's "rf lna" command, given what follows those words: nothing to report the
// unit's LNA mode setting (receiver_settings.h), or " low" or " normal" to save it and apply
// it to the receiver. Each answers with one NVF-RF-LNA... line. Call from one task.
void receiver_lna_console(const char *argument);
// The console's "rf signals" command: the healthy signals the receiver has code lock on,
// with each one's C/N0 and its satellite's elevation, as one NVF-RF-SIGNALS {json} line, or
// NVF-RF-SIGNALS-ERROR when the receiver has reported none lately. Call from that same task.
void receiver_signals_console(void);
