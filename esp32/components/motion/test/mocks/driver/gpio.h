#pragma once
#include <stdint.h>
#include "esp_err.h"
enum { GPIO_MODE_INPUT, GPIO_PULLUP_DISABLE, GPIO_PULLDOWN_ENABLE,
       GPIO_INTR_DISABLE, GPIO_INTR_POSEDGE };
typedef struct {
    uint64_t pin_bit_mask;
    int mode, pull_up_en, pull_down_en, intr_type;
} gpio_config_t;
esp_err_t gpio_config(const gpio_config_t *config);
esp_err_t gpio_install_isr_service(int flags);
esp_err_t gpio_set_intr_type(int gpio, int type);
esp_err_t gpio_isr_handler_add(int gpio, void (*handler)(void *), void *arg);
