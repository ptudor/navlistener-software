#include "nvf_board.h"

static uint8_t device_id = NVF_BOARD_ID_NONE;
static const char *device_family;

void nvf_board_set_device(uint8_t id, const char *family) { device_id = id; device_family = family; }
uint8_t nvf_board_device_id(void) { return device_id; }
const char *nvf_board_device_family(void) { return device_family; }
