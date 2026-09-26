#include "manifest_board.h"

board_model_t manifest_board_decide(eeprom_board_result_t found, uint8_t id, uint8_t revision)
{
    if (found == EEPROM_BOARD_NONE) return BOARD_MODEL_NONE;
    if (found != EEPROM_BOARD_FOUND) return BOARD_MODEL_CONFLICT;
    if (revision != 1) return BOARD_MODEL_UNSUPPORTED;
    switch (id) {
    case INTSAT_NEO: return BOARD_MODEL_NEO_A;
    case INTSAT_X20: return BOARD_MODEL_X20_A;
    case INTSAT_MAX: return BOARD_MODEL_MAX_A;
    default: return BOARD_MODEL_UNSUPPORTED;
    }
}

const char *manifest_board_name(board_model_t model)
{
    switch (model) {
    case BOARD_MODEL_NEO_A: return "NEO revision A";
    case BOARD_MODEL_X20_A: return "X20 revision A";
    case BOARD_MODEL_MAX_A: return "MAX revision A";
    case BOARD_MODEL_UNSUPPORTED: return "an unsupported board or revision";
    case BOARD_MODEL_CONFLICT: return "more than one board";
    default: return "no board";
    }
}
