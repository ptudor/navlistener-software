#pragma once
// The custom observer board a firmware image is built for (CONFIG_NVF_BOARD_ASSEMBLY).
// Byte 9 of the image's NVFOTA1 metadata carries the ID, and a signed release names the
// family in its board_family field, so an image never installs on another board.
#include "sdkconfig.h"

enum { NVF_BOARD_ID_NONE = 0, NVF_BOARD_ID_NEO = 1, NVF_BOARD_ID_ZED_X20 = 2, NVF_BOARD_ID_MAX = 3 };

#if CONFIG_NVF_BOARD_GNSS_COLOR_ZED_X20
#define NVF_BOARD_ID     NVF_BOARD_ID_ZED_X20
#define NVF_BOARD_FAMILY "gnss-color-zed-x20"
#elif CONFIG_NVF_BOARD_GNSS_COLOR_MAX
#define NVF_BOARD_ID     NVF_BOARD_ID_MAX
#define NVF_BOARD_FAMILY "gnss-color-max"
#elif CONFIG_NVF_BOARD_GNSS_COLOR_NEO
#define NVF_BOARD_ID     NVF_BOARD_ID_NEO
#define NVF_BOARD_FAMILY "gnss-color-neo"
#else
#define NVF_BOARD_ID     NVF_BOARD_ID_NONE
#define NVF_BOARD_FAMILY ""
#endif
