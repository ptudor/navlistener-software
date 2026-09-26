#include "manifest_board.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

int main(void)
{
    // Revision A of each observer board.
    assert(manifest_board_decide(1, INTSAT_NEO, 1) == BOARD_MODEL_NEO_A);
    assert(manifest_board_decide(1, INTSAT_X20, 1) == BOARD_MODEL_X20_A);
    assert(manifest_board_decide(1, INTSAT_MAX, 1) == BOARD_MODEL_MAX_A);
    // No board named: not configured. Two: the manifest is wrong. Either way nothing
    // board-specific runs.
    assert(manifest_board_decide(0, 0, 0) == BOARD_MODEL_NONE);
    assert(manifest_board_decide(2, INTSAT_NEO, 1) == BOARD_MODEL_CONFLICT);
    // A board or revision this firmware does not know is not guessed at.
    assert(manifest_board_decide(1, INTSAT_NEO, 2) == BOARD_MODEL_UNSUPPORTED);
    assert(manifest_board_decide(1, INTSAT_X20, 0) == BOARD_MODEL_UNSUPPORTED);
    assert(manifest_board_decide(1, INTSAT_CARRIER_ARDUSIMPLE, 1) == BOARD_MODEL_UNSUPPORTED);
    assert(manifest_board_decide(1, 200, 1) == BOARD_MODEL_UNSUPPORTED);
    assert(!strcmp(manifest_board_name(BOARD_MODEL_X20_A), "X20 revision A"));
    assert(!strcmp(manifest_board_name(BOARD_MODEL_NONE), "no board"));
    puts("manifest board: the named board and revision, unconfigured, unsupported and conflicting manifests passed");
    return 0;
}
