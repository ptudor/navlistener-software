#pragma once
#include <stdlib.h>
#ifdef ESP_PLATFORM
#include "esp_heap_caps.h"
// Metadata must not consume the internal RAM needed by UART, TLS and the spool.
// The S3 has external RAM; absent/failed PSRAM leaves a bounded internal reserve.
static inline void *nvf_update_alloc(size_t n) {
    void *p=heap_caps_malloc(n,MALLOC_CAP_SPIRAM|MALLOC_CAP_8BIT);
    if(!p && heap_caps_get_free_size(MALLOC_CAP_INTERNAL|MALLOC_CAP_8BIT)>n+65536)
        p=heap_caps_malloc(n,MALLOC_CAP_INTERNAL|MALLOC_CAP_8BIT);
    return p;
}
#else
static inline void *nvf_update_alloc(size_t n){return malloc(n);}
#endif
