// Actual storage code against the documented NVS atomic-blob contract. Model
// cuts/failures during chunk preparation, index publication, and commit; reboot
// reads only the durable index. This is not an electrical flash-fault simulator.
#include "netcfg.h"
#include "nvs.h"
#include <assert.h>
#include <setjmp.h>
#include <stdio.h>
#include <string.h>

static unsigned char durable[512];
static size_t durable_size;
static netcfg_t legacy_cfg;
static bool has_legacy, legacy_reset;
static int failure_step, step, writes;
static bool power_cut, open_failure, read_failure;
static jmp_buf reboot;
static const char factory_identity[] = "separate factory namespace";

static bool boundary(void)
{
    if (++step != failure_step) return false;
    if (power_cut) longjmp(reboot, 1);
    return true;
}
esp_err_t nvs_open(const char *ns, int mode, nvs_handle_t *h)
{
    assert(strcmp(ns, "navfeeder") == 0);
    (void)mode; *h = 1;
    return open_failure ? ESP_FAIL : ESP_OK;
}
void nvs_close(nvs_handle_t h) { assert(h == 1); }
esp_err_t nvs_get_blob(nvs_handle_t h, const char *key, void *out, size_t *size)
{
    assert(h == 1 && strcmp(key, "config_v1") == 0);
    if (read_failure) return ESP_FAIL;
    if (!durable_size) return ESP_ERR_NVS_NOT_FOUND;
    if (*size < durable_size) return ESP_ERR_NVS_INVALID_LENGTH;
    memcpy(out, durable, durable_size); *size = durable_size;
    return ESP_OK;
}
esp_err_t nvs_set_blob(nvs_handle_t h, const char *key, const void *data, size_t size)
{
    assert(h == 1 && strcmp(key, "config_v1") == 0 && size <= sizeof durable);
    writes++;
    // Full flash before allocation and cuts at every 32-byte preparation unit
    // keep the old blob indexed, regardless of how many new chunks were written.
    for (size_t n = 0; n < size; n += 32) if (boundary()) return ESP_ERR_NVS_NOT_ENOUGH_SPACE;
    if (boundary()) return ESP_FAIL; // before publishing the complete index
    memcpy(durable, data, size); durable_size = size;
    if (boundary()) return ESP_FAIL; // publication succeeded, caller sees failure
    return ESP_OK;
}
esp_err_t nvs_commit(nvs_handle_t h)
{
    assert(h == 1);
    if (boundary()) return ESP_FAIL;
    if (boundary()) return ESP_FAIL;
    return ESP_OK;
}
esp_err_t nvs_get_str(nvs_handle_t h, const char *key, char *out, size_t *cap)
{
    assert(h == 1);
    if (!has_legacy) return ESP_ERR_NVS_NOT_FOUND;
    const char *s = NULL;
    if (!strcmp(key, "ssid")) s = legacy_cfg.wifi_ssid;
    if (!strcmp(key, "pass")) s = legacy_cfg.wifi_pass;
    if (!strcmp(key, "host")) s = legacy_cfg.host;
    if (!strcmp(key, "token")) s = legacy_cfg.token;
    if (!strcmp(key, "station")) s = legacy_cfg.station;
    assert(s && strlen(s) + 1 <= *cap);
    strcpy(out, s); *cap = strlen(s) + 1;
    return ESP_OK;
}
esp_err_t nvs_get_i32(nvs_handle_t h, const char *key, int32_t *v)
{
    assert(h == 1 && !strcmp(key, "port"));
    if (!has_legacy) return ESP_ERR_NVS_NOT_FOUND;
    *v = legacy_cfg.port; return ESP_OK;
}
esp_err_t nvs_get_u8(nvs_handle_t h, const char *key, uint8_t *v)
{
    assert(h == 1);
    if (!strcmp(key, "reset")) { *v = legacy_reset; return ESP_OK; }
    assert(!strcmp(key, "insecure"));
    if (!has_legacy) return ESP_ERR_NVS_NOT_FOUND;
    *v = legacy_cfg.insecure; return ESP_OK;
}
static bool same(const netcfg_t *a, const netcfg_t *b)
{
    return !strcmp(a->wifi_ssid,b->wifi_ssid) && !strcmp(a->wifi_pass,b->wifi_pass) &&
        !strcmp(a->host,b->host) && !strcmp(a->token,b->token) && !strcmp(a->station,b->station) &&
        a->port == b->port && a->insecure == b->insecure;
}
static void fresh(void)
{
    durable_size = 0; has_legacy = legacy_reset = power_cut = open_failure = read_failure = false;
    failure_step = step = writes = 0;
}
int main(void)
{
    const netcfg_t old = {.wifi_ssid="old-ssid",.wifi_pass="old-pass",.host="old.invalid",.port=5580,
        .token="old-token",.station="old-station",.insecure=false};
    const netcfg_t next = {.wifi_ssid="new-ssid",.wifi_pass="new-pass",.host="new.invalid",.port=5581,
        .token="new-token",.station="new-station",.insecure=true};
    netcfg_t out;
    // Migration from old keys, ordinary blob replacement, and provisioning after
    // both legacy and versioned physical reset must each stay whole after reboot.
    for (int scenario = 0; scenario < 4; scenario++) {
        for (int cut = 0; cut < 2; cut++) {
            for (int fail = 1; fail <= 16; fail++) {
                fresh();
                if (scenario == 0) { has_legacy = true; legacy_cfg = old; }
                if (scenario == 1) assert(netcfg_save(&old) == ESP_OK);
                if (scenario == 2) { legacy_reset = true; has_legacy = true; legacy_cfg = old; }
                if (scenario == 3) { assert(netcfg_save(&old) == ESP_OK); assert(netcfg_reset_provisioning() == ESP_OK); }
                step = 0; failure_step = fail; power_cut = cut;
                if (!setjmp(reboot)) (void)netcfg_save(&next);
                failure_step = 0; power_cut = false; // reboot: old/new durable index only
                bool valid = netcfg_load(&out, NULL, 0);
                if (scenario < 2) assert(valid && (same(&out,&old) || same(&out,&next)));
                else assert((valid && same(&out,&next)) || (!valid && out.host[0] == 0 && out.token[0] == 0));
                assert(strcmp(factory_identity, "separate factory namespace") == 0);
            }
        }
    }
    // Reset itself is one transaction: prior whole config or the persistent
    // empty marker; compiled bench defaults cannot reappear after publication.
    for (int fail = 1; fail <= 16; fail++) {
        fresh(); assert(netcfg_save(&old) == ESP_OK);
        step = 0; failure_step = fail; power_cut = true;
        if (!setjmp(reboot)) (void)netcfg_reset_provisioning();
        failure_step = 0; power_cut = false;
        bool valid = netcfg_load(&out,NULL,0);
        assert((valid && same(&out,&old)) || (!valid && !out.host[0] && !out.token[0]));
    }
    fresh(); assert(netcfg_save(&next) == ESP_OK);
    assert(writes == 1 && netcfg_load(&out,NULL,0) && same(&out,&next));
    has_legacy = true; legacy_cfg = old; legacy_reset = true;
    assert(netcfg_load(&out,NULL,0) && same(&out,&next)); // no stale per-field fallback
    durable[30] ^= 1;
    assert(!netcfg_load(&out,NULL,0) && !out.host[0] && !out.token[0]);
    assert(netcfg_reset_provisioning() == ESP_OK); // physical repair of malformed unit
    assert(!netcfg_load(&out,NULL,0) && !out.host[0]);
    assert(netcfg_save(&next) == ESP_OK && netcfg_load(&out,NULL,0));
    int before = writes;
    read_failure = true;
    assert(netcfg_save(&old) != ESP_OK && writes == before);
    assert(!netcfg_load(&out,NULL,0) && !out.token[0]);
    read_failure = false; open_failure = true;
    assert(netcfg_save(&old) != ESP_OK && writes == before);
    open_failure = false;
    netcfg_t bad = next; memset(bad.host,'x',sizeof bad.host);
    assert(netcfg_save(&bad) == ESP_ERR_INVALID_ARG && writes == before);
    assert(netcfg_load(&out,NULL,0) && same(&out,&next));
    puts("netcfg atomic record: write/commit errors, power-cut boundaries, migration, reset and corruption PASS");
}
