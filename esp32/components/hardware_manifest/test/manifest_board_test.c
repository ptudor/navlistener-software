#include "manifest_board.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// The library maps a profile to the memory part in its bus-facing source; the lists need only this.
uint8_t eeprom_profile_memory_id(eeprom_profile_t profile)
{
    switch (profile) {
    case EEPROM_PROFILE_24CS128: return MEMORY_24CS128;
    case EEPROM_PROFILE_24CS256: return MEMORY_24CS256;
    case EEPROM_PROFILE_24CS512: return MEMORY_24CS512;
    case EEPROM_PROFILE_M24128_U: return MEMORY_M24128_U;
    default: return 0;
    }
}

static eeprom_capabilities_t list(uint8_t board, uint8_t revision, uint8_t humidity, uint8_t pressure)
{
    eeprom_capabilities_t caps;
    const eeprom_intsat_options_t batch = {.humidity_id = humidity, .pressure_id = pressure};
    assert(eeprom_intsat_template(board, revision, &batch, EEPROM_PROFILE_24CS128, &caps));
    return caps;
}

// A stored manifest against one board revision's lists: the selected batch, another batch, or neither.
static void batch_parts(void)
{
    const eeprom_profile_t profile = EEPROM_PROFILE_24CS128;
    const eeprom_intsat_options_t neo_384 = {SENSOR_HDC2080, PRESSURE_BMP384}, neo_388 = {SENSOR_HDC2080, PRESSURE_BMP388};
    const eeprom_intsat_options_t x20_2080 = {SENSOR_HDC2080, 0}, x20_2022 = {SENSOR_HDC2022, 0};
    eeprom_intsat_options_t other = {0xee, 0xee};

    // A NEO written as a BMP388 where the batch carries the BMP384, and the reverse.
    eeprom_capabilities_t stored = list(INTSAT_NEO, 1, SENSOR_HDC2080, PRESSURE_BMP388);
    assert(manifest_batch_classify(&stored, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_OTHER);
    assert(other.humidity_id == SENSOR_HDC2080 && other.pressure_id == PRESSURE_BMP388);
    assert(manifest_batch_classify(&stored, INTSAT_NEO, 1, &neo_388, profile, &other) == MANIFEST_BATCH_SELECTED);
    stored = list(INTSAT_NEO, 1, SENSOR_HDC2080, PRESSURE_BMP384);
    assert(manifest_batch_classify(&stored, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_SELECTED);
    assert(manifest_batch_classify(&stored, INTSAT_NEO, 1, &neo_388, profile, &other) == MANIFEST_BATCH_OTHER);
    assert(other.humidity_id == SENSOR_HDC2080 && other.pressure_id == PRESSURE_BMP384);
    // Both batch parts at once.
    stored = list(INTSAT_NEO, 1, SENSOR_HDC2022, PRESSURE_BMP388);
    assert(manifest_batch_classify(&stored, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_OTHER);
    assert(other.humidity_id == SENSOR_HDC2022 && other.pressure_id == PRESSURE_BMP388);
    // A board whose list has its own barometer varies in its humidity sensor only.
    stored = list(INTSAT_X20, 1, SENSOR_HDC2022, 0);
    assert(manifest_batch_classify(&stored, INTSAT_X20, 1, &x20_2080, profile, &other) == MANIFEST_BATCH_OTHER);
    assert(other.humidity_id == SENSOR_HDC2022 && other.pressure_id == 0);
    assert(manifest_batch_classify(&stored, INTSAT_X20, 1, &x20_2022, profile, &other) == MANIFEST_BATCH_SELECTED);

    // Another board's list, or another revision's, is never one of this board revision's.
    assert(manifest_batch_classify(&stored, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    assert(manifest_batch_classify(&stored, INTSAT_X20, 2, &x20_2080, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    stored = list(INTSAT_NEO, 1, SENSOR_HDC2080, PRESSURE_BMP388);
    assert(manifest_batch_classify(&stored, INTSAT_MAX, 1, &x20_2080, profile, &other) == MANIFEST_BATCH_UNKNOWN);

    // Anything that is not a list as built is left alone: a field status, another part, one
    // entry more or fewer, another memory part, another header, an invalid manifest.
    eeprom_capabilities_t edited = stored;
    for (int i = 0; i < edited.component_count; i++)
        if (edited.components[i].category == CAT_TEMP) edited.components[i].status = IC_STATUS_FAILED;
    assert(manifest_batch_classify(&edited, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    edited = stored;
    for (int i = 0; i < edited.component_count; i++)
        if (edited.components[i].category == CAT_PRESSURE) edited.components[i].id = PRESSURE_BMP390;
    assert(manifest_batch_classify(&edited, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    edited = stored; edited.component_count--;
    assert(manifest_batch_classify(&edited, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    edited = stored; edited.components[edited.component_count++] = IC_INSTALLED(CAT_COMM, COMM_W5500);
    assert(manifest_batch_classify(&edited, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    assert(manifest_batch_classify(&stored, INTSAT_NEO, 1, &neo_384, EEPROM_PROFILE_24CS256, &other) == MANIFEST_BATCH_UNKNOWN);
    edited = stored; edited.revision = 2;
    assert(manifest_batch_classify(&edited, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    edited = stored; edited.pcb_id++;
    assert(manifest_batch_classify(&edited, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    edited = stored; edited.is_valid = false;
    assert(manifest_batch_classify(&edited, INTSAT_NEO, 1, &neo_384, profile, &other) == MANIFEST_BATCH_UNKNOWN);

    // A selection the board's list cannot carry builds nothing, so nothing is classified as its.
    const eeprom_intsat_options_t no_barometer = {SENSOR_HDC2080, 0}, x20_barometer = {SENSOR_HDC2080, PRESSURE_BMP384};
    assert(manifest_batch_classify(&stored, INTSAT_NEO, 1, &no_barometer, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    stored = list(INTSAT_X20, 1, SENSOR_HDC2080, 0);
    assert(manifest_batch_classify(&stored, INTSAT_X20, 1, &x20_barometer, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    assert(manifest_batch_classify(NULL, INTSAT_X20, 1, &x20_2080, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    assert(manifest_batch_classify(&stored, INTSAT_X20, 1, NULL, profile, &other) == MANIFEST_BATCH_UNKNOWN);
    assert(manifest_batch_classify(&stored, INTSAT_X20, 1, &x20_2080, profile, NULL) == MANIFEST_BATCH_UNKNOWN);
    // The programming time is not part of a list.
    stored.timestamp = 1790000000;
    assert(manifest_batch_classify(&stored, INTSAT_X20, 1, &x20_2080, profile, &other) == MANIFEST_BATCH_SELECTED);
}

int main(void)
{
    batch_parts();
    // Revision A of each observer board.
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_NEO, 1) == BOARD_MODEL_NEO_A);
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_X20, 1) == BOARD_MODEL_X20_A);
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_X20, 2) == BOARD_MODEL_X20_B);
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_MAX, 1) == BOARD_MODEL_MAX_A);
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_MOSAIC_P3, 1) == BOARD_MODEL_P3_A);
    // No board named: not configured. Two: the manifest is wrong. Either way nothing
    // board-specific runs.
    assert(manifest_board_decide(EEPROM_BOARD_NONE, 0, 0) == BOARD_MODEL_NONE);
    assert(manifest_board_decide(EEPROM_BOARD_CONFLICT, 0, 0) == BOARD_MODEL_CONFLICT);
    // A board or revision this firmware does not know is not guessed at.
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_NEO, 2) == BOARD_MODEL_UNSUPPORTED);
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_X20, 0) == BOARD_MODEL_UNSUPPORTED);
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_X20, 3) == BOARD_MODEL_UNSUPPORTED);
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_MOSAIC_P3, 2) == BOARD_MODEL_UNSUPPORTED);
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, INTSAT_CARRIER_ARDUSIMPLE, 1) == BOARD_MODEL_UNSUPPORTED);
    assert(manifest_board_decide(EEPROM_BOARD_FOUND, 200, 1) == BOARD_MODEL_UNSUPPORTED);
    assert(!strcmp(manifest_board_name(BOARD_MODEL_X20_A), "X20 revision A"));
    assert(!strcmp(manifest_board_name(BOARD_MODEL_X20_B), "X20 revision B"));
    assert(!strcmp(manifest_board_name(BOARD_MODEL_P3_A), "P3 revision A"));
    assert(!strcmp(manifest_board_name(BOARD_MODEL_NONE), "no board"));
    puts("manifest board: the named board and revision, unconfigured, unsupported and conflicting manifests, "
         "and a stored manifest's batch parts passed");
    return 0;
}
