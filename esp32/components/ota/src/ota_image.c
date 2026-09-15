#include <stdint.h>
#include "sdkconfig.h"
#ifndef CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE
#define CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE 0
#endif
#ifndef CONFIG_NVF_BOARD_GNSS_COLOR_NEO
#define CONFIG_NVF_BOARD_GNSS_COLOR_NEO 0
#endif
#ifndef CONFIG_NVF_MANIFEST_FACTORY_INIT
#define CONFIG_NVF_MANIFEST_FACTORY_INIT 0
#endif
// ESP-IDF places this directly after esp_app_desc_t in the first image segment.
const uint8_t nvf_ota_metadata[16] __attribute__((used, section(".rodata_custom_desc"))) = {
    'N','V','F','O','T','A','1',0, 2, CONFIG_NVF_BOARD_GNSS_COLOR_NEO,
    CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE, CONFIG_NVF_MANIFEST_FACTORY_INIT,
    CONFIG_NVF_BOARD_GNSS_COLOR_NEO ? 3 : 0,0,0,0
};
