#include "manifest_board.h"
#include <string.h>

board_model_t manifest_board_decide(eeprom_board_result_t found, uint8_t id, uint8_t revision)
{
    if (found == EEPROM_BOARD_NONE) return BOARD_MODEL_NONE;
    if (found != EEPROM_BOARD_FOUND) return BOARD_MODEL_CONFLICT;
    if (id == INTSAT_X20) {
        if (revision == 1) return BOARD_MODEL_X20_A;
        if (revision == 2) return BOARD_MODEL_X20_B;
        return BOARD_MODEL_UNSUPPORTED;
    }
    if (revision != 1) return BOARD_MODEL_UNSUPPORTED;
    switch (id) {
    case INTSAT_NEO: return BOARD_MODEL_NEO_A;
    case INTSAT_MAX: return BOARD_MODEL_MAX_A;
    case INTSAT_MOSAIC_P3: return BOARD_MODEL_P3_A;
    default: return BOARD_MODEL_UNSUPPORTED;
    }
}

const char *manifest_board_name(board_model_t model)
{
    switch (model) {
    case BOARD_MODEL_NEO_A: return "NEO revision A";
    case BOARD_MODEL_X20_A: return "X20 revision A";
    case BOARD_MODEL_X20_B: return "X20 revision B";
    case BOARD_MODEL_MAX_A: return "MAX revision A";
    case BOARD_MODEL_P3_A: return "P3 revision A";
    case BOARD_MODEL_UNSUPPORTED: return "an unsupported board or revision";
    case BOARD_MODEL_CONFLICT: return "more than one board";
    default: return "no board";
    }
}

static bool same_list(const eeprom_capabilities_t *a, const eeprom_capabilities_t *b)
{
    return a->is_valid && b->is_valid && a->project_id == b->project_id && a->pcb_id == b->pcb_id &&
           a->revision == b->revision && a->i2c_address == b->i2c_address &&
           a->component_count == b->component_count &&
           !memcmp(a->components, b->components, a->component_count * sizeof a->components[0]);
}

manifest_batch_t manifest_batch_classify(const eeprom_capabilities_t *stored, uint8_t board_id, uint8_t revision,
                                         const eeprom_intsat_options_t *selected, eeprom_profile_t profile,
                                         eeprom_intsat_options_t *other)
{
    eeprom_capabilities_t list;
    if (!stored || !selected || !other || !eeprom_intsat_template(board_id, revision, selected, profile, &list))
        return MANIFEST_BATCH_UNKNOWN;
    if (same_list(stored, &list)) return MANIFEST_BATCH_SELECTED;
    // Every batch the library builds a list for; it refuses a part a board's list cannot carry.
    static const uint8_t humidity[] = {SENSOR_HDC2080, SENSOR_HDC2022};
    static const uint8_t pressure[] = {0, PRESSURE_BMP388, PRESSURE_BMP384};
    for (size_t h = 0; h < sizeof humidity; h++) {
        for (size_t p = 0; p < sizeof pressure; p++) {
            const eeprom_intsat_options_t batch = {.humidity_id = humidity[h], .pressure_id = pressure[p]};
            if (eeprom_intsat_template(board_id, revision, &batch, profile, &list) && same_list(stored, &list)) {
                *other = batch;
                return MANIFEST_BATCH_OTHER;
            }
        }
    }
    return MANIFEST_BATCH_UNKNOWN;
}
