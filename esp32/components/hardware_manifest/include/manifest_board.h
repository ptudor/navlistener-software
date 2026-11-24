#ifndef MANIFEST_BOARD_H
#define MANIFEST_BOARD_H
// Which observer board a verified manifest describes. Each manifest names its board with
// one installed CAT_INTSAT descriptor whose address byte is the revision (1 = A, 2 = B); the
// firmware knows a board's pins and parts from that ID and revision. Pure: the ESP side
// reads the descriptors with the discovery library.
#include <stdbool.h>
#include <stdint.h>
#include "esp_hardware_discovery.h"

typedef enum {
    BOARD_MODEL_NONE = 0,     // no usable manifest, or it names no board: not configured
    BOARD_MODEL_NEO_A,
    BOARD_MODEL_X20_A,
    BOARD_MODEL_X20_B,
    BOARD_MODEL_MAX_A,
    BOARD_MODEL_P3_A,
    BOARD_MODEL_UNSUPPORTED,  // a board or revision this firmware does not know
    BOARD_MODEL_CONFLICT,     // more than one board named
} board_model_t;

// From eeprom_find_board(caps, CAT_INTSAT, &id, &revision).
board_model_t manifest_board_decide(eeprom_board_result_t found, uint8_t id, uint8_t revision);
const char *manifest_board_name(board_model_t model);

// A stored manifest against the lists the discovery library holds for one board revision.
// A list varies only in its batch parts: the humidity sensor and, on the NEO, the barometer.
typedef enum {
    MANIFEST_BATCH_SELECTED = 0, // the stored manifest is the list for the selected batch parts
    MANIFEST_BATCH_OTHER,        // it is, byte for byte, the same board revision's list for other batch parts
    MANIFEST_BATCH_UNKNOWN,      // it is none of that board revision's lists, or the selection builds none
} manifest_batch_t;
// *other is set for MANIFEST_BATCH_OTHER to the batch parts the stored manifest lists.
manifest_batch_t manifest_batch_classify(const eeprom_capabilities_t *stored, uint8_t board_id, uint8_t revision,
                                         const eeprom_intsat_options_t *selected, eeprom_profile_t profile,
                                         eeprom_intsat_options_t *other);
#endif
