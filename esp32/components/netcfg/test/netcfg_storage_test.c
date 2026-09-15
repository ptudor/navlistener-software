// Actual storage code against the documented NVS atomic-blob contract. Model
// cuts/failures during chunk preparation, index publication, and commit; reboot
// reads only the durable index. This is not an electrical flash-fault simulator.
#include "netcfg.h"
#include "nvs.h"
#include <assert.h>
#include <setjmp.h>
#include <stdio.h>
#include <string.h>

static unsigned char durable[1024];
static size_t durable_size;
static netcfg_t legacy_cfg;
static bool has_legacy, legacy_reset;
static int failure_step, step, writes;
static bool power_cut, open_failure, read_failure, erase_failure;
static unsigned erased_mask; // bit per legacy key erased since fresh()
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
static const char *const legacy_keys[] = {"ssid", "pass", "host", "token", "station", "port", "insecure", "reset"};
esp_err_t nvs_erase_key(nvs_handle_t h, const char *key)
{
    assert(h == 1);
    if (erase_failure) return ESP_FAIL;
    for (unsigned i = 0; i < 8; i++) if (!strcmp(key, legacy_keys[i])) { erased_mask |= 1u << i; return ESP_OK; }
    assert(!"unexpected key erased");
    return ESP_FAIL;
}
esp_err_t nvs_get_u8(nvs_handle_t h, const char *key, uint8_t *v)
{
    assert(h == 1);
    if (!strcmp(key, "reset")) { *v = legacy_reset; return ESP_OK; }
    assert(!strcmp(key, "insecure"));
    if (!has_legacy) return ESP_ERR_NVS_NOT_FOUND;
    *v = legacy_cfg.insecure; return ESP_OK;
}
static bool same_tunnel(const netcfg_tunnel_t *a, const netcfg_tunnel_t *b)
{
    return a->enabled == b->enabled && !memcmp(a->private_key, b->private_key, 32) &&
        !memcmp(a->peer_public_key, b->peer_public_key, 32) &&
        !memcmp(a->preshared_key, b->preshared_key, 32) &&
        !strcmp(a->endpoint_host, b->endpoint_host) && a->endpoint_port == b->endpoint_port &&
        !memcmp(a->address, b->address, 4) && a->prefix == b->prefix &&
        !memcmp(a->collector, b->collector, 4) && a->keepalive == b->keepalive;
}
static bool same(const netcfg_t *a, const netcfg_t *b)
{
    return !strcmp(a->wifi_ssid,b->wifi_ssid) && !strcmp(a->wifi_pass,b->wifi_pass) &&
        !strcmp(a->host,b->host) && !strcmp(a->token,b->token) && !strcmp(a->station,b->station) &&
        a->port == b->port && a->insecure == b->insecure && same_tunnel(&a->tunnel, &b->tunnel);
}
static uint32_t crc32_le(const unsigned char *p, size_t n)
{
    uint32_t crc = UINT32_MAX;
    for (size_t i = 0; i < n; i++) { crc ^= p[i]; for (int j = 0; j < 8; j++) crc = (crc >> 1) ^ (0xedb88320u & (0u - (crc & 1u))); }
    return ~crc;
}
static void seal(size_t size)
{
    uint32_t crc = crc32_le(durable, size - 4);
    for (int i = 0; i < 4; i++) durable[size - 4 + i] = (unsigned char)(crc >> (8 * i));
    durable_size = size;
}
// A format-1 record exactly as firmware before the WireGuard profile wrote it.
static void store_v1(const netcfg_t *c)
{
    memset(durable, 0, sizeof durable);
    memcpy(durable, "NFC1", 4);
    durable[4] = 1; durable[6] = 348 & 0xff; durable[7] = 348 >> 8; durable[8] = 7; // generation 7
    durable[17] = c->insecure; durable[18] = (unsigned char)c->port; durable[19] = (unsigned char)(c->port >> 8);
    size_t off = 20;
    memcpy(durable + off, c->wifi_ssid, strlen(c->wifi_ssid)); off += 33;
    memcpy(durable + off, c->wifi_pass, strlen(c->wifi_pass)); off += 65;
    memcpy(durable + off, c->host, strlen(c->host)); off += 64;
    memcpy(durable + off, c->token, strlen(c->token)); off += 129;
    memcpy(durable + off, c->station, strlen(c->station)); off += 33;
    assert(off == 344);
    seal(348);
}
static void fresh(void)
{
    durable_size = 0; has_legacy = legacy_reset = power_cut = open_failure = read_failure = erase_failure = false;
    failure_step = step = writes = 0; erased_mask = 0;
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
    // A physical reset retires the legacy per-field keys too (best effort): no
    // bearer token stays readable in flash after "erase settings".
    fresh(); has_legacy = true; legacy_cfg = old;
    assert(netcfg_reset_provisioning() == ESP_OK && erased_mask == 0xffu);
    assert(!netcfg_load(&out, NULL, 0) && !out.token[0]);
    // ...and an erase failure never changes the reset's outcome or its record.
    fresh(); has_legacy = true; legacy_cfg = old; erase_failure = true;
    assert(netcfg_reset_provisioning() == ESP_OK && erased_mask == 0);
    assert(!netcfg_load(&out, NULL, 0) && !out.token[0] && !out.host[0]);
    // A malformed record (good CRC, unterminated field) fails closed and never
    // uncovers legacy keys — the astra-6 verification's N6/N7 gaps.
    fresh(); has_legacy = true; legacy_cfg = old; legacy_reset = false;
    assert(netcfg_save(&next) == ESP_OK);
    memset(durable + 118, 'x', 64); // host field, no terminator
    seal(522);
    assert(!netcfg_load(&out, NULL, 0) && !out.host[0] && !out.token[0]);

    // --- format 2: the optional WireGuard profile ---
    // Every write is format 2 (522 bytes); a profile round-trips whole, a disabled one
    // is stored as zeros, and the reset marker retires the profile with everything else.
    netcfg_t tunneled = next;
    tunneled.tunnel.enabled = true;
    for (int i = 0; i < 32; i++) { tunneled.tunnel.private_key[i] = (uint8_t)(i + 1); tunneled.tunnel.peer_public_key[i] = (uint8_t)(200 - i); tunneled.tunnel.preshared_key[i] = (uint8_t)(i * 3); }
    strcpy(tunneled.tunnel.endpoint_host, "wg.collector.invalid");
    tunneled.tunnel.endpoint_port = 51820;
    memcpy(tunneled.tunnel.address, (uint8_t[]){10, 77, 0, 12}, 4); tunneled.tunnel.prefix = 24;
    memcpy(tunneled.tunnel.collector, (uint8_t[]){10, 77, 0, 1}, 4); tunneled.tunnel.keepalive = 25;
    fresh(); assert(netcfg_save(&tunneled) == ESP_OK && durable_size == 522 && durable[4] == 2 && durable[344] == 1);
    assert(netcfg_load(&out, NULL, 0) && same(&out, &tunneled));
    assert(netcfg_save(&next) == ESP_OK && durable_size == 522);
    for (size_t i = 344; i < 518; i++) assert(durable[i] == 0); // no key material left behind
    assert(netcfg_load(&out, NULL, 0) && same(&out, &next) && !out.tunnel.enabled);
    assert(netcfg_save(&tunneled) == ESP_OK && netcfg_reset_provisioning() == ESP_OK);
    for (size_t i = 344; i < 518; i++) assert(durable[i] == 0);
    assert(!netcfg_load(&out, NULL, 0) && !out.tunnel.enabled && !out.host[0]);
    // An enabled but incomplete profile is refused before any write, like any other field.
    netcfg_t half = tunneled; memset(half.tunnel.collector, 0, 4);
    fresh(); assert(netcfg_save(&half) == ESP_ERR_INVALID_ARG && writes == 0);
    half = tunneled; memset(half.tunnel.endpoint_host, 'h', sizeof half.tunnel.endpoint_host);
    assert(netcfg_save(&half) == ESP_ERR_INVALID_ARG && writes == 0);
    // Power cuts while replacing a profile leave either the whole old or the whole new record.
    for (int fail = 1; fail <= 24; fail++) {
        fresh(); assert(netcfg_save(&next) == ESP_OK);
        step = 0; failure_step = fail; power_cut = true;
        if (!setjmp(reboot)) (void)netcfg_save(&tunneled);
        failure_step = 0; power_cut = false;
        assert(netcfg_load(&out, NULL, 0) && (same(&out, &next) || same(&out, &tunneled)));
    }
    // A format-1 record written by earlier firmware loads with the tunnel disabled and every
    // other field intact, and the next save upgrades it in place under the same key.
    fresh(); store_v1(&old);
    assert(netcfg_load(&out, NULL, 0) && same(&out, &old) && !out.tunnel.enabled);
    assert(netcfg_save(&tunneled) == ESP_OK && durable_size == 522 && durable[8] == 8); // generation continues
    assert(netcfg_load(&out, NULL, 0) && same(&out, &tunneled));
    // Corrupt format-2 tails fail closed: a flag byte other than 0/1, a short blob claiming
    // format 2, and a format-1 length carrying a format-2 version.
    fresh(); assert(netcfg_save(&tunneled) == ESP_OK);
    durable[344] = 2; seal(522);
    assert(!netcfg_load(&out, NULL, 0) && !out.host[0]);
    fresh(); store_v1(&old); durable[4] = 2; seal(348);
    assert(!netcfg_load(&out, NULL, 0) && !out.host[0]);
    fresh(); assert(netcfg_save(&tunneled) == ESP_OK); durable[4] = 1; seal(522);
    assert(!netcfg_load(&out, NULL, 0) && !out.host[0]);
    // A format-2 record whose profile fails validation (zero peer key, good CRC) is refused,
    // never loaded as a half-configured tunnel.
    fresh(); assert(netcfg_save(&tunneled) == ESP_OK); memset(durable + 344 + 33, 0, 32); seal(522);
    assert(!netcfg_load(&out, NULL, 0) && !out.host[0] && !out.tunnel.enabled);
    puts("netcfg atomic record: write/commit errors, power-cut boundaries, migration, reset, legacy retirement, corruption and the format-2 tunnel profile PASS");
}
