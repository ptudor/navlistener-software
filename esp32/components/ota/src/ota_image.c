#include <stdint.h>
#include "sdkconfig.h"
#ifndef CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE
#define CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE 0
#endif
#include "nvf_board.h"
#ifndef CONFIG_NVF_MANIFEST_FACTORY_INIT
#define CONFIG_NVF_MANIFEST_FACTORY_INIT 0
#endif
#ifndef CONFIG_NVF_BOARD_GNSS_COLOR
#define CONFIG_NVF_BOARD_GNSS_COLOR 0
#endif
// The board this image is built for (CONFIG_NVF_BOARD_ASSEMBLY). The C6 development image
// carries layout 0, which every consumer refuses, so its board byte is never read.
#if CONFIG_NVF_BOARD_GNSS_COLOR_ZED_X20
#define IMAGE_BOARD NVF_BOARD_ID_ZED_X20
#elif CONFIG_NVF_BOARD_GNSS_COLOR_MAX
#define IMAGE_BOARD NVF_BOARD_ID_MAX
#elif CONFIG_NVF_BOARD_GNSS_COLOR_NEO
#define IMAGE_BOARD NVF_BOARD_ID_NEO
#else
#define IMAGE_BOARD NVF_BOARD_ID_UNIVERSAL
#endif
// ESP-IDF places this directly after esp_app_desc_t in the first image segment.
const uint8_t nvf_ota_metadata[16] __attribute__((used, section(".rodata_custom_desc"))) = {
    'N','V','F','O','T','A','1',0, 2, IMAGE_BOARD,
    CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE, CONFIG_NVF_MANIFEST_FACTORY_INIT,
    CONFIG_NVF_BOARD_GNSS_COLOR ? 3 : 0,0,0,0
};
