#pragma once
#include <stdint.h>
void network_monitor_start(uint64_t (*utc_ns)(void));
uint8_t network_monitor_socket(int fd,uint8_t peer[4]);
