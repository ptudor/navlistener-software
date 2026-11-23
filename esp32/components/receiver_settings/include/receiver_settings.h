#ifndef NVF_RECEIVER_SETTINGS_H
#define NVF_RECEIVER_SETTINGS_H
// Per-unit receiver settings. They describe the antenna a unit is installed with, not its
// firmware image or its assembly, so one image serves every unit of a board. They are kept
// in the `nvf_rx` NVS namespace, which the network configuration reset leaves alone.
#include <stdbool.h>
#include <stdint.h>
#include "esp_err.h"

// The mode of the ZED-X20P's internal LNAs, one to an RF block, all three set alike
// (integration manual UBXDOC-963802114-12901, 4.3.1). Low gain takes 20 to 50 dB of gain
// ahead of RF_IN: an antenna whose amplifier, less its cable, brings the receiver's gain
// control to its floor in normal gain. Normal gain, the receiver's own default, takes 17 to
// 40 dB: an antenna that brings less. A receiver without the setting ignores it.
typedef enum {
    RECEIVER_LNA_LOW = 0,
    RECEIVER_LNA_NORMAL = 1,
} receiver_lna_t;

typedef struct {
    receiver_lna_t lna;
} receiver_settings_t;

#define RECEIVER_SETTINGS_DEFAULT ((receiver_settings_t){.lna = RECEIVER_LNA_LOW})

bool receiver_settings_valid(const receiver_settings_t *settings);
// Console values: "low" or "normal".
bool receiver_settings_parse_lna(const char *text, receiver_lna_t *lna);
const char *receiver_lna_name(receiver_lna_t lna);
// Missing settings are the defaults and loading never writes flash. A stored value out of
// range returns ESP_ERR_INVALID_ARG; any error leaves the defaults in out.
esp_err_t receiver_settings_load(receiver_settings_t *out);
esp_err_t receiver_settings_save(const receiver_settings_t *settings);
#endif
