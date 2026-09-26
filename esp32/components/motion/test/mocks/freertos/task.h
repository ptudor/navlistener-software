#pragma once
#include "FreeRTOS.h"
typedef void *TaskHandle_t;
BaseType_t xTaskCreate(void (*entry)(void *), const char *name, unsigned stack,
                       void *arg, unsigned priority, TaskHandle_t *task);
void vTaskDelay(TickType_t ticks);
unsigned ulTaskNotifyTake(BaseType_t clear, TickType_t ticks);
void vTaskNotifyGiveFromISR(TaskHandle_t task, BaseType_t *woken);
