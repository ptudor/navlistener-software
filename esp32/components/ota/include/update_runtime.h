#pragma once
#include "update_state.h"
#include "esp_err.h"
typedef struct {
    nvf_update_device_t device;
    bool (*online)(void);
    bool (*durable_link)(void);
    // Pause all producers at a complete record boundary and return the final
    // spool watermark. Resume must work after every timeout/error.
    bool (*pause)(uint64_t *watermark);
    void (*resume)(void);
} nvf_update_hooks_t;
esp_err_t nvf_update_start(const nvf_update_hooks_t *);
bool nvf_update_boot_ready(void);
void nvf_update_confirmed(void);
void nvf_update_status(nvf_update_status_t *);
bool nvf_update_request(unsigned action,uint64_t release,bool discard);
bool nvf_update_policy(unsigned mode,unsigned channel);
void nvf_update_control(const uint8_t *bytes,size_t length);
bool nvf_update_wire(uint8_t out[140]);
// The build's fixed trust profile (UP_PROFILE_*). It is a claim in telemetry,
// never evidence: only locked hardware can prove which firmware is running.
unsigned nvf_update_profile(void);
bool nvf_update_busy(void);
// One exclusion gate for the repository worker and attended service recovery.
bool nvf_update_claim(void);
void nvf_update_release(void);
bool nvf_update_discard_result(uint32_t *records,unsigned timeout_ms);
void nvf_update_discard_response(bool sent);
