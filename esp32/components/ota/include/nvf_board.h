#pragma once
// The custom observer boards an image and a device are matched by. Byte 9 of an image's
// NVFOTA1 metadata carries the board it was built for (CONFIG_NVF_BOARD_ASSEMBLY), and a
// signed release names the same in its board_family field. The universal image carries
// NVF_BOARD_ID_UNIVERSAL and installs on every board, because it drives only what the
// manifest lists; a single-board test image installs only on a device whose manifest
// names that board.
#include <stdint.h>

enum {
    NVF_BOARD_ID_NONE = 0, NVF_BOARD_ID_NEO = 1, NVF_BOARD_ID_ZED_X20 = 2, NVF_BOARD_ID_MAX = 3,
    NVF_BOARD_ID_UNIVERSAL = 4,
};
#define NVF_BOARD_FAMILY_UNIVERSAL "gnss-color"

// The board this device is, from the manifest's CAT_INTSAT entry: set once at boot, before
// the update service starts or an operator push is accepted. Until then, and on a device
// whose manifest names no known board, the ID is NVF_BOARD_ID_NONE and the family NULL,
// so only universal images and releases are accepted.
void nvf_board_set_device(uint8_t id, const char *family);
uint8_t nvf_board_device_id(void);
const char *nvf_board_device_family(void);
