#pragma once
#include "update_tuf.h"

enum { UP_MANUAL=1, UP_DOWNLOAD=2, UP_INSTALL=3 };
enum { UP_IDLE, UP_CHECKING, UP_AVAILABLE, UP_DOWNLOADING, UP_STAGED, UP_WAITING_SAFE,
       UP_QUIESCING, UP_REBOOT_PENDING, UP_TRIAL_BOOT, UP_CONFIRMED, UP_ROLLED_BACK, UP_FAILED };
enum { UP_CHECK=1, UP_STAGE=2, UP_APPLY=3, UP_CANCEL=4 };
typedef struct {
    uint8_t action;
    bool hint;
    uint64_t id, generation, release, expires;
} nvf_update_command_t;
typedef struct {
    uint8_t mode, channel, state, security;
    uint16_t layout, error;
    uint64_t running, failed, last_check, next_check, error_time, last_command;
    uint32_t received, retry, discarded;
    nvf_update_command_t command;
    nvf_update_release_t available, staged;
} nvf_update_status_t;

bool nvf_update_decode_command(const uint8_t *,size_t,nvf_update_command_t *);
// 1 new, 0 exact duplicate, -1 invalid/stale/expired. Callers persist the accepted
// command before effects, and report telemetry for duplicates without replay.
int nvf_update_accept_command(nvf_update_status_t *,const nvf_update_command_t *,uint64_t now);
// Attended network-failure fallback only. A newer accepted channel invalidates
// the old staging decision even if fetching its manifest failed or power stopped.
bool nvf_update_offline_install_allowed(const nvf_update_status_t *,const nvf_tuf_trust_t *);
uint64_t nvf_update_weekly(uint64_t now,const uint8_t eui[8],unsigned channel,uint32_t jitter,const nvf_tuf_io_t *);
uint64_t nvf_update_retry(uint64_t now,unsigned attempt,uint32_t random);
void nvf_update_encode_status(const nvf_update_status_t *,uint8_t out[140]);
const char *nvf_update_error_name(unsigned error);
