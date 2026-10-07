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
uint64_t nvf_update_weekly(uint64_t now,const uint8_t board_uid[NVF_BOARD_UID_SIZE],unsigned channel,uint32_t jitter,const nvf_tuf_io_t *);
uint64_t nvf_update_retry(uint64_t now,unsigned attempt,uint32_t random);
// next_check after a check whose verdict is not retried: a stable eligibility result or a
// release that already rolled back is an answer, so a slot still in the future stays and an
// overdue one moves to the weekly slot (the first retry step when the clock cannot place
// one). Always after now, so the worker never re-checks at its loop rate.
uint64_t nvf_update_settle(uint64_t next_check,uint64_t now,const uint8_t board_uid[NVF_BOARD_UID_SIZE],unsigned channel,uint32_t jitter,const nvf_tuf_io_t *);
// When automatic installation retries after a safety gate held it: 5 minutes after the first
// attempt, then the retry ladder (1 h, 6 h, 24 h) for every later one.
uint64_t nvf_update_install_retry(uint64_t now,unsigned attempts,uint32_t random);
// An automatic install may trust the last persisted check instead of refreshing: no check is
// due yet, the last one succeeded, and the staged release is still the channel's verified
// choice (sequence, generation and hash). Re-verifying the partition signature is local.
bool nvf_update_staged_current(const nvf_update_status_t *,uint64_t now);
void nvf_update_encode_status(const nvf_update_status_t *,unsigned profile,uint8_t out[140]);
const char *nvf_update_error_name(unsigned error);
