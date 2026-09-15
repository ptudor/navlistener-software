#pragma once
#include "update_state.h"
typedef struct {
    unsigned action,mode,channel;
    uint64_t release;
    bool discard;
} nvf_update_api_request_t;
bool nvf_update_decimal(const char *,uint64_t *);
bool nvf_update_api_parse(const char *method,const char *path,const char *body,size_t length,nvf_update_api_request_t *);
