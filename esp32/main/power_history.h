#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "gnss_status.h"
#include "../../common/reception_power.h"

#define POWER_HISTORY_CAPACITY 4096u
#define POWER_HISTORY_DAYS NRP_MAX_MIN_SUPPORT
#define POWER_HISTORY_PHASE_SECONDS 600u
#define POWER_HISTORY_WIRE_MAX (32u + POWER_HISTORY_CAPACITY*16u + 4u)

// Bind persisted local history to the stable server-provided site/configuration
// identity. A changed identity clears the old antenna/receiver baseline.
bool power_history_bind(uint64_t site_id,uint8_t min_deviation,uint8_t mad_multiplier,uint8_t min_support);
uint64_t power_history_model_id(void);

// Fill the sample's observed values and local assessment. learn must be true
// only once per fresh receiver measurement.
void power_history_assess(const nr_expectation_t *base,const gnss_status_t *status,
                          uint64_t utc,uint64_t now_ms,bool learn,nrp_sample_t *sample);

// Best-effort two-slot NVS checkpoint, rate-limited internally.
void power_history_checkpoint(uint64_t now_ms);

// Versioned core serialization is public for host verification.
size_t power_history_export(uint8_t *out,size_t capacity,uint32_t checkpoint_generation);
bool power_history_import(const uint8_t *bytes,size_t length,uint64_t expected_site_id);
void power_history_test_reset(void);
