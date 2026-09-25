// tunnel — WireGuard uplink to the collector. See include/tunnel.h.
//
// Routing: the WireGuard netif is never the default route. lwIP routes a destination
// through a point-to-point netif (one without the broadcast flag, which the port's netif
// is) when the destination equals that netif's gateway, so the collector's tunnel address
// is installed as the gateway and only that one host reaches the tunnel. The peer's
// allowed addresses are this observer's own tunnel address and the collector, so the
// port also refuses to send or accept anything else through it.
//
// Time: a WireGuard handshake carries a TAI64N timestamp and the peer drops any that is
// not newer than the last it accepted from this key. A reboot into 1970 would therefore
// be ignored until the clock is right, so the first attempt waits for a plausible wall
// clock (SNTP today; the RTC path is P-hw work).
//
// Threading: the port drives lwIP's raw netif, udp and dns APIs directly. This firmware
// runs lwIP without core locking (the ESP-IDF default), so those calls are legal only on
// the tcpip thread, and every one below is executed there through esp_netif_tcpip_exec.
// Handshake crypto also runs on that thread when a peer packet arrives, which is why
// sdkconfig.defaults.s3 raises its stack.

#include "tunnel.h"

#include <stdatomic.h>

#include "esp_log.h"
#include "sdkconfig.h"

// A Kconfig bool left at 'n' emits no #define; give the preprocessor a value.
#ifndef CONFIG_NVF_WIREGUARD
#define CONFIG_NVF_WIREGUARD 0
#endif

static const char *TAG = "tunnel";

#if CONFIG_NVF_WIREGUARD

#include <stdio.h>
#include <string.h>
#include <time.h>

#include "esp_netif.h"
#include "esp_netif_sntp.h"
#include "esp_wireguard.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lwip/ip4_addr.h"
#include "lwip/netif.h"
#include "netcfg_tunnel.h"

#define WALL_CLOCK_FLOOR  1600000000LL // ~2020-09: the same bar app_now_ns() applies
#define POLL_MS           1000
// The port stops re-sending handshake initiations 90 s after the last trigger. A peer
// still down after that gets a fresh connect, which also re-resolves the endpoint name.
#define HANDSHAKE_GRACE_S 90
#define BACKOFF_MIN_S     5
#define BACKOFF_MAX_S     60
#define TIME_LOG_EVERY_S  30

static char s_private[NETCFG_TUNNEL_KEY_B64 + 1];
static char s_public[NETCFG_TUNNEL_KEY_B64 + 1];
static char s_psk[NETCFG_TUNNEL_KEY_B64 + 1];
static char s_address[16], s_collector[16];
static char s_endpoint[sizeof ((netcfg_tunnel_t *)0)->endpoint_host];
static int s_prefix;
static wireguard_config_t s_wg = ESP_WIREGUARD_CONFIG_DEFAULT();
static wireguard_ctx_t s_ctx = ESP_WIREGUARD_CONTEXT_DEFAULT();
static atomic_bool s_up;
static bool s_started, s_initialized;
static bool (*s_uplink_up)(void);

bool tunnel_up(void) { return atomic_load_explicit(&s_up, memory_order_relaxed); }

const char *tunnel_collector_address(void) { return s_started ? s_collector : NULL; }

static bool wall_clock_plausible(void) { return time(NULL) > (time_t)WALL_CLOCK_FLOOR; }

// --- bodies executed on the tcpip thread --------------------------------------------------

static esp_err_t in_tcpip_init(void *ctx)
{
    (void)ctx;
    return esp_wireguard_init(&s_wg, &s_ctx);
}

static esp_err_t in_tcpip_connect(void *ctx)
{
    (void)ctx;
    return esp_wireguard_connect(&s_ctx);
}

// in_tcpip_route admits the collector through the peer and installs it as the netif's
// gateway, the one host route lwIP grants a point-to-point interface.
static esp_err_t in_tcpip_route(void *ctx)
{
    (void)ctx;
    esp_err_t err = esp_wireguard_add_allowed_ip(&s_ctx, s_collector, "255.255.255.255");
    if (err != ESP_OK) return err;
    ip4_addr_t gw;
    if (!s_ctx.netif || !ip4addr_aton(s_collector, &gw)) return ESP_ERR_INVALID_STATE;
    netif_set_gw(s_ctx.netif, &gw);
    return ESP_OK;
}

static esp_err_t in_tcpip_status(void *ctx)
{
    *(bool *)ctx = esp_wireguard_peer_is_up(&s_ctx) == ESP_OK;
    return ESP_OK;
}

static esp_err_t in_tcpip_disconnect(void *ctx)
{
    (void)ctx;
    // esp_wireguard_disconnect dereferences the netif unconditionally; before the first
    // successful connect there is none.
    return s_ctx.netif ? esp_wireguard_disconnect(&s_ctx) : ESP_OK;
}

// --- the tunnel task ----------------------------------------------------------------------

static int grow(int backoff) { return backoff * 2 > BACKOFF_MAX_S ? BACKOFF_MAX_S : backoff * 2; }

static void tunnel_task(void *arg)
{
    (void)arg;
    int backoff = BACKOFF_MIN_S;
    bool sntp_requested = false;
    unsigned waited_s = 0;
    for (;;) {
        if (!s_uplink_up()) {
            vTaskDelay(pdMS_TO_TICKS(POLL_MS));
            continue;
        }
        if (!wall_clock_plausible()) {
            if (!sntp_requested) {
                // Station-mode OTA normally starts SNTP first; this is the fallback when that
                // option is off. Already-initialised is not an error.
                esp_sntp_config_t sntp = ESP_NETIF_SNTP_DEFAULT_CONFIG("pool.ntp.org");
                esp_err_t err = esp_netif_sntp_init(&sntp);
                if (err != ESP_OK && err != ESP_ERR_INVALID_STATE)
                    ESP_LOGW(TAG, "SNTP start failed (%s); waiting for another time source",
                             esp_err_to_name(err));
                sntp_requested = true;
            }
            if (waited_s++ % TIME_LOG_EVERY_S == 0)
                ESP_LOGI(TAG, "waiting for time sync before the first WireGuard handshake");
            vTaskDelay(pdMS_TO_TICKS(POLL_MS));
            continue;
        }

        if (!s_initialized) {
            esp_err_t err = esp_netif_tcpip_exec(in_tcpip_init, NULL);
            if (err != ESP_OK) {
                ESP_LOGE(TAG, "WireGuard init failed (%s); retrying in %d s", esp_err_to_name(err), backoff);
                vTaskDelay(pdMS_TO_TICKS(backoff * 1000));
                backoff = grow(backoff);
                continue;
            }
            s_initialized = true;
        }

        esp_err_t err = esp_netif_tcpip_exec(in_tcpip_connect, NULL);
        if (err == ESP_ERR_RETRY) { // endpoint name still resolving
            vTaskDelay(pdMS_TO_TICKS(POLL_MS));
            continue;
        }
        if (err == ESP_OK) err = esp_netif_tcpip_exec(in_tcpip_route, NULL);
        if (err != ESP_OK) {
            ESP_LOGW(TAG, "WireGuard connect to %s:%d failed (%s); retrying in %d s",
                     s_endpoint, (int)s_wg.port, esp_err_to_name(err), backoff);
            (void)esp_netif_tcpip_exec(in_tcpip_disconnect, NULL);
            vTaskDelay(pdMS_TO_TICKS(backoff * 1000));
            backoff = grow(backoff);
            continue;
        }
        ESP_LOGI(TAG, "WireGuard peer %s:%d; this observer %s/%d, collector %s inside the tunnel",
                 s_endpoint, (int)s_wg.port, s_address, s_prefix, s_collector);

        TickType_t down_since = xTaskGetTickCount();
        bool was_up = false;
        for (;;) {
            vTaskDelay(pdMS_TO_TICKS(POLL_MS));
            if (!s_uplink_up()) {
                ESP_LOGW(TAG, "uplink down; tearing the tunnel down until it returns");
                break;
            }
            bool up = false;
            if (esp_netif_tcpip_exec(in_tcpip_status, &up) != ESP_OK) up = false;
            atomic_store_explicit(&s_up, up, memory_order_relaxed);
            TickType_t now = xTaskGetTickCount();
            if (up != was_up) {
                ESP_LOGI(TAG, "%s", up ? "tunnel up: peer session established"
                                      : "tunnel down: no valid peer session");
                was_up = up;
            }
            if (up) {
                down_since = now;
                backoff = BACKOFF_MIN_S;
            } else if (now - down_since >= pdMS_TO_TICKS(HANDSHAKE_GRACE_S * 1000)) {
                ESP_LOGW(TAG, "no handshake for %d s; re-resolving %s and reconnecting",
                         HANDSHAKE_GRACE_S, s_endpoint);
                break;
            }
        }
        atomic_store_explicit(&s_up, false, memory_order_relaxed);
        (void)esp_netif_tcpip_exec(in_tcpip_disconnect, NULL);
        vTaskDelay(pdMS_TO_TICKS(backoff * 1000));
        backoff = grow(backoff);
    }
}

esp_err_t tunnel_start(const netcfg_tunnel_t *cfg, bool (*uplink_up)(void))
{
    if (s_started) return ESP_ERR_INVALID_STATE;
    if (!cfg || !cfg->enabled || !uplink_up || !netcfg_tunnel_validate(cfg, NULL, 0))
        return ESP_ERR_INVALID_ARG;

    // The port takes base64 keys and dotted addresses; format them once, for the boot.
    netcfg_tunnel_key_encode(cfg->private_key, s_private);
    netcfg_tunnel_key_encode(cfg->peer_public_key, s_public);
    static const uint8_t no_psk[NETCFG_TUNNEL_KEY_LEN];
    bool psk = memcmp(cfg->preshared_key, no_psk, sizeof no_psk) != 0;
    if (psk) netcfg_tunnel_key_encode(cfg->preshared_key, s_psk);
    netcfg_tunnel_ip4_format(cfg->address, s_address);
    netcfg_tunnel_ip4_format(cfg->collector, s_collector);
    snprintf(s_endpoint, sizeof s_endpoint, "%s", cfg->endpoint_host);
    s_prefix = cfg->prefix;

    s_wg.private_key = s_private;
    s_wg.public_key = s_public;
    s_wg.preshared_key = psk ? s_psk : NULL;
    s_wg.address = s_address;
    // lwIP checks connected subnets before point-to-point gateways. Using the
    // pasted prefix here would capture every host in that subnet, including
    // the uplink's DNS server or the outer WireGuard endpoint. Only the collector's
    // gateway route belongs to this interface; retain the profile prefix for logs.
    s_wg.netmask = "255.255.255.255";
    s_wg.endpoint = s_endpoint;
    s_wg.port = (uint16_t)cfg->endpoint_port;
    s_wg.persistent_keepalive = (uint16_t)cfg->keepalive;
    s_wg.listen_port = 0; // ephemeral: the observer only ever initiates

    s_uplink_up = uplink_up;
    s_started = true; // tunnel_collector_address() is valid from here
    if (xTaskCreate(tunnel_task, "tunnel", 4096, NULL, 5, NULL) != pdPASS) {
        s_started = false;
        return ESP_ERR_NO_MEM;
    }
    ESP_LOGI(TAG, "WireGuard tunnel configured: peer %s:%d, keepalive %d s%s", s_endpoint,
             cfg->endpoint_port, cfg->keepalive, psk ? ", preshared key" : "");
    return ESP_OK;
}

#else // !CONFIG_NVF_WIREGUARD

esp_err_t tunnel_start(const netcfg_tunnel_t *cfg, bool (*uplink_up)(void))
{
    (void)cfg;
    (void)uplink_up;
    ESP_LOGW(TAG, "WireGuard tunnel not supported on this build; using the public collector endpoint");
    return ESP_ERR_NOT_SUPPORTED;
}

bool tunnel_up(void) { return false; }

const char *tunnel_collector_address(void) { return NULL; }

#endif // CONFIG_NVF_WIREGUARD
