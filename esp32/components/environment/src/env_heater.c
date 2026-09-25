#include "env_heater.h"
void env_heater_init(env_heater_t *h, const env_heater_config_t *config, bool last_known, int64_t last_utc)
{ *h = (env_heater_t){.c = *config, .last_known = last_known, .last_utc = last_utc}; }
const char *env_heater_stop_name(unsigned stop)
{
    static const char *names[] = {"running", "dry", "timeout", "overtemp", "bus_error", "sensor_lost"};
    return stop < sizeof names / sizeof names[0] ? names[stop] : "unknown";
}
static void peak(double *value, uint8_t *valid, uint8_t bit, double sample)
{
    if (!(*valid & bit) || sample > *value) { *value = sample; *valid |= bit; }
}
// Normal service: dwell credit needs both ends of an interval at or above the bucket, and a
// gap over gap_ms (or any valid reading below the trigger) restarts the condensing count.
static void normal(env_heater_t *h, int64_t now, const env_sample_t *s)
{
    if (!s->hdc_valid) return;
    bool continuous = h->prev_valid && now - h->prev_ms <= h->c.gap_ms;
    if (continuous && h->prev_rh >= 95 && s->rh_percent >= 95) h->rh95_ms += now - h->prev_ms;
    if (continuous && h->prev_rh >= 98 && s->rh_percent >= 98) h->rh98_ms += now - h->prev_ms;
    if (s->rh_percent < h->c.trigger_rh) h->wet = false;
    else if (!h->wet || !continuous) { h->wet = true; h->wet_ms = now; }
    h->prev_valid = true; h->prev_ms = now; h->prev_rh = s->rh_percent;
}
static int64_t next_step(const env_heater_t *h, int64_t now)
{
    int64_t step = now + h->c.step_ms, limit = h->run.start_ms + h->c.max_on_ms;
    return step < limit ? step : limit;
}
static env_heater_action_t stop(env_heater_t *h, env_heater_stop_t why)
{ h->run.stop = why; h->state = ENV_HEATER_STOPPING; return ENV_HEATER_STOP; }
// The newest reading plus history back to the first entry at least stable_ms old must span less
// than stable_c. Entries are at least stable_ms / (HISTORY - 2) apart, so a window always fits.
static bool stable(const env_heater_t *h, int64_t now, double current)
{
    double low = current, high = current;
    for (unsigned i = 0; i < h->count; i++) {
        unsigned n = (h->head + ENV_HEATER_HISTORY - 1 - i) % ENV_HEATER_HISTORY;
        if (h->history[n].c < low) low = h->history[n].c;
        if (h->history[n].c > high) high = h->history[n].c;
        if (now - h->history[n].ms >= h->c.stable_ms) return high - low < h->c.stable_c;
    }
    return false;
}
static void recover(env_heater_t *h, int64_t now, const env_sample_t *s)
{
    if (s->hdc_valid) peak(&h->run.hdc_peak, &h->run.valid, ENV_RUN_HDC_PEAK, s->hdc_c);
    if (s->mcp_valid) peak(&h->run.mcp_peak, &h->run.valid, ENV_RUN_MCP_PEAK, s->mcp_c);
    bool settled = false;
    if (!s->hdc_valid) h->count = 0; // stability needs an unbroken run of conversions
    else {
        settled = stable(h, now, s->hdc_c);
        unsigned newest = (h->head + ENV_HEATER_HISTORY - 1) % ENV_HEATER_HISTORY;
        if (!h->count || now - h->history[newest].ms >= h->c.stable_ms / (ENV_HEATER_HISTORY - 2)) {
            h->history[h->head].ms = now; h->history[h->head].c = s->hdc_c;
            h->head = (h->head + 1) % ENV_HEATER_HISTORY;
            if (h->count < ENV_HEATER_HISTORY) h->count++;
        }
    }
    int64_t elapsed = now - h->off_ms;
    if (elapsed < h->c.recovery_max_ms && (elapsed < h->c.settle_ms || !settled)) return;
    h->run.recovery_ms = elapsed;
    if (s->hdc_valid) { h->run.hdc_end = s->hdc_c; h->run.valid |= ENV_RUN_HDC_END; }
    if (s->mcp_valid) { h->run.mcp_end = s->mcp_c; h->run.valid |= ENV_RUN_MCP_END; }
    h->state = ENV_HEATER_NORMAL;
    normal(h, now, s); // this reading is the first of normal service
}
env_heater_action_t env_heater_sample(env_heater_t *h, int64_t now, bool utc_valid, int64_t utc,
                                      const env_sample_t *s)
{
    h->utc_valid = utc_valid;
    if (h->state == ENV_HEATER_RECOVERING) { recover(h, now, s); return ENV_HEATER_HOLD; }
    if (h->state != ENV_HEATER_NORMAL) {
        if (s->mcp_valid) peak(&h->run.mcp_peak, &h->run.valid, ENV_RUN_MCP_PEAK, s->mcp_c);
        return ENV_HEATER_HOLD;
    }
    normal(h, now, s);
    if (!s->hdc_valid || !h->wet || now - h->wet_ms < h->c.trigger_ms ||
        s->hdc_c < h->c.min_c || s->hdc_c > h->c.max_c || !utc_valid || !h->last_known ||
        (h->last_utc && utc - h->last_utc < h->c.interval_s)) return ENV_HEATER_HOLD;
    h->next = (env_heater_run_t){.start_utc = utc, .rh_before = s->rh_percent, .hdc_before = s->hdc_c,
        .mcp_before = s->mcp_valid ? s->mcp_c : 0,
        .valid = ENV_RUN_RH_BEFORE | ENV_RUN_HDC_BEFORE | (s->mcp_valid ? ENV_RUN_MCP_BEFORE : 0)};
    return ENV_HEATER_START;
}
void env_heater_declined(env_heater_t *h) { h->wet = false; }
env_heater_action_t env_heater_started(env_heater_t *h, int64_t now, env_heater_stop_t failure)
{
    h->run = h->next; h->run.start_ms = now; h->last_utc = h->run.start_utc; h->runs++;
    h->state = ENV_HEATER_HEATING; h->wet = h->prev_valid = false;
    h->next_step_ms = next_step(h, now);
    return failure == ENV_HEATER_RUNNING ? ENV_HEATER_HOLD : stop(h, failure);
}
bool env_heater_step_due(const env_heater_t *h, int64_t now)
{ return (h->state == ENV_HEATER_HEATING || h->state == ENV_HEATER_STOPPING) && now >= h->next_step_ms; }
env_heater_action_t env_heater_step(env_heater_t *h, int64_t now, const env_sample_t *s, bool heat_en)
{
    h->next_step_ms = next_step(h, now);
    if (s->mcp_valid) peak(&h->run.mcp_peak, &h->run.valid, ENV_RUN_MCP_PEAK, s->mcp_c);
    if (s->hdc_valid) {
        peak(&h->run.hdc_peak, &h->run.valid, ENV_RUN_HDC_PEAK, s->hdc_c);
        h->run.rh_stop = s->rh_percent; h->run.valid |= ENV_RUN_RH_STOP;
    }
    // Safety first when several conditions coincide in one step.
    if (s->bus_error) return stop(h, ENV_HEATER_BUS_ERROR);
    if (!s->hdc_valid || !heat_en) return stop(h, ENV_HEATER_SENSOR_LOST); // no data, or reset
    if (s->hdc_c >= h->c.overtemp_c) return stop(h, ENV_HEATER_OVERTEMP);
    if (s->rh_percent <= h->c.dry_rh) return stop(h, ENV_HEATER_DRY);
    if (now - h->run.start_ms >= h->c.max_on_ms) return stop(h, ENV_HEATER_TIMEOUT);
    return ENV_HEATER_HOLD;
}
void env_heater_cleared(env_heater_t *h, int64_t now, bool confirmed)
{
    if (!confirmed) { h->next_step_ms = now + h->c.step_ms; return; }
    h->run.on_ms = now - h->run.start_ms; h->off_ms = now; h->count = 0;
    h->state = ENV_HEATER_RECOVERING;
}
void env_heater_status(const env_heater_t *h, int64_t now, env_heater_status_t *out)
{
    *out = (env_heater_status_t){.active = true, .utc_valid = h->utc_valid, .last_known = h->last_known,
        .state = h->state, .runs = h->runs, .last_utc = h->last_utc,
        .rh95_ms = h->rh95_ms, .rh98_ms = h->rh98_ms, .run = h->run};
    if (h->wet && now - h->prev_ms <= h->c.gap_ms) out->streak_ms = h->prev_ms - h->wet_ms;
    if (h->state == ENV_HEATER_HEATING || h->state == ENV_HEATER_STOPPING) out->run.on_ms = now - h->run.start_ms;
    else if (h->state == ENV_HEATER_RECOVERING) out->run.recovery_ms = now - h->off_ms;
}
