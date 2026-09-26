#pragma once
#include <stdint.h>
typedef unsigned TickType_t;
typedef int BaseType_t;
#define pdFALSE 0
#define pdTRUE 1
#define pdPASS 1
#define portMAX_DELAY UINT32_MAX
#define portTICK_PERIOD_MS 1
#define pdMS_TO_TICKS(ms) (ms)
#define portYIELD_FROM_ISR(woken) ((void)(woken))
#define IRAM_ATTR
