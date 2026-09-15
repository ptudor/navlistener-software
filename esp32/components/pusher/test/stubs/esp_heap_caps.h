#pragma once
#include <stdlib.h>
#include <stdint.h>
#define MALLOC_CAP_INTERNAL 1
#define MALLOC_CAP_8BIT 2
static inline void *heap_caps_malloc(size_t n, uint32_t caps) { (void)caps; return malloc(n); }
