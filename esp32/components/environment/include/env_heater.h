#ifndef NVF_ENV_HEATER_H
#define NVF_ENV_HEATER_H
#include <stdbool.h>
#include <stdint.h>
#include "env_sensors.h"
// HDC2080/HDC2022 condensation recovery, after TI SNAS678C section 8.3.3: heat only after a
// sustained condensing reading, keep converting while heating, stop near 0 %RH, then wait for
// the die to cool before its readings count again. Pure policy: the caller injects monotonic
// milliseconds, trusted UTC and conversions, and performs every register and NVS operation.
typedef enum { ENV_HEATER_NORMAL, ENV_HEATER_HEATING, ENV_HEATER_STOPPING, ENV_HEATER_RECOVERING } env_heater_state_t;
// Stop reasons in wire order; RUNNING until the first stop condition.
typedef enum { ENV_HEATER_RUNNING, ENV_HEATER_DRY, ENV_HEATER_TIMEOUT, ENV_HEATER_OVERTEMP,
    ENV_HEATER_BUS_ERROR, ENV_HEATER_SENSOR_LOST } env_heater_stop_t;
typedef enum { ENV_HEATER_HOLD, ENV_HEATER_START, ENV_HEATER_STOP } env_heater_action_t;
typedef struct {
    double trigger_rh, min_c, max_c, dry_rh, overtemp_c, stable_c;
    int64_t trigger_ms, gap_ms, interval_s, step_ms, max_on_ms, settle_ms, stable_ms, recovery_max_ms;
} env_heater_config_t;
// Validity bits for the run measurements below.
enum { ENV_RUN_RH_BEFORE = 1, ENV_RUN_RH_STOP = 2, ENV_RUN_HDC_BEFORE = 4, ENV_RUN_HDC_PEAK = 8,
    ENV_RUN_HDC_END = 16, ENV_RUN_MCP_BEFORE = 32, ENV_RUN_MCP_PEAK = 64, ENV_RUN_MCP_END = 128 };
typedef struct {
    int64_t start_utc, start_ms, on_ms, recovery_ms;
    uint8_t stop, valid;
    double rh_before, rh_stop, hdc_before, hdc_peak, hdc_end, mcp_before, mcp_peak, mcp_end;
} env_heater_run_t;
// Recovery temperatures kept for the stability window; see env_heater.c.
#define ENV_HEATER_HISTORY 32
typedef struct {
    env_heater_config_t c;
    env_heater_state_t state;
    bool last_known, utc_valid, prev_valid, wet;
    int64_t last_utc, prev_ms, wet_ms, rh95_ms, rh98_ms, next_step_ms, off_ms;
    double prev_rh;
    unsigned runs, count, head;
    env_heater_run_t run, next; // latest run this boot; the run a START proposes
    struct { int64_t ms; double c; } history[ENV_HEATER_HISTORY];
} env_heater_t;
typedef struct {
    bool active, utc_valid, last_known;
    env_heater_state_t state;
    unsigned runs;
    // Humidity dwell uses fixed 95/98 %RH buckets, independent of the configured trigger.
    int64_t last_utc, rh95_ms, rh98_ms, streak_ms;
    env_heater_run_t run; // on/recovery times run on while those phases last
} env_heater_status_t;
// last_known: the previous automatic run was read back (last_utc, zero if none). Without it
// no automatic run starts this boot, rather than risk repeating one inside the interval.
void env_heater_init(env_heater_t *h, const env_heater_config_t *config, bool last_known, int64_t last_utc);
// Every normal-cadence sample. HDC readings are ignored while HEAT_EN may be set; heater steps
// supply them. START: persist next.start_utc, set HEAT_EN, then report env_heater_started();
// if persisting fails, env_heater_declined() restarts the condensing count instead.
env_heater_action_t env_heater_sample(env_heater_t *h, int64_t now_ms, bool utc_valid, int64_t utc,
                                      const env_sample_t *s);
void env_heater_declined(env_heater_t *h);
// failure is RUNNING when HEAT_EN read back set. STOP: clear HEAT_EN, then env_heater_cleared().
env_heater_action_t env_heater_started(env_heater_t *h, int64_t now_ms, env_heater_stop_t failure);
// Heating: a conversion step is due. Stopping: another attempt to clear HEAT_EN is due.
bool env_heater_step_due(const env_heater_t *h, int64_t now_ms);
// A heating step: fresh MCP9808/HDC conversions and the HEAT_EN read-back taken after them.
env_heater_action_t env_heater_step(env_heater_t *h, int64_t now_ms, const env_sample_t *s, bool heat_en);
// Until HEAT_EN reads back clear the heater may be on: stay stopping and retry at the next step.
void env_heater_cleared(env_heater_t *h, int64_t now_ms, bool confirmed);
void env_heater_status(const env_heater_t *h, int64_t now_ms, env_heater_status_t *out);
const char *env_heater_stop_name(unsigned stop);
#endif
