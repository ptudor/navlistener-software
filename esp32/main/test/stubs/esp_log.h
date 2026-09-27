#pragma once
#include <stdio.h>
#define ESP_LOGI(tag,format,...) do { (void)(tag); if(0)printf(format,##__VA_ARGS__); } while(0)
#define ESP_LOGW ESP_LOGI
