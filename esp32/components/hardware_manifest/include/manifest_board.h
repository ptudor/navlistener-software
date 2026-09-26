#ifndef MANIFEST_BOARD_H
#define MANIFEST_BOARD_H
// Which observer board a verified manifest describes. Each manifest names its board with
// one installed CAT_INTSAT descriptor whose address byte is the revision (1 = A); the
// firmware knows a board's pins and parts from that ID and revision. Pure: the ESP side
// reads the descriptors with the discovery library.
#include <stdbool.h>
#include <stdint.h>
#include "esp_hardware_discovery.h"

typedef enum {
    BOARD_MODEL_NONE = 0,     // no usable manifest, or it names no board: not configured
    BOARD_MODEL_NEO_A,
    BOARD_MODEL_X20_A,
    BOARD_MODEL_MAX_A,
    BOARD_MODEL_UNSUPPORTED,  // a board or revision this firmware does not know
    BOARD_MODEL_CONFLICT,     // more than one board named
} board_model_t;

// From eeprom_find_board(caps, CAT_INTSAT, &id, &revision).
board_model_t manifest_board_decide(eeprom_board_result_t found, uint8_t id, uint8_t revision);
const char *manifest_board_name(board_model_t model);
#endif
