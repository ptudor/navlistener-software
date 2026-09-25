// Exercise the firmware adapter, including startup failure cleanup and the
// interface/peer routes passed to the lwIP WireGuard port.
#define _POSIX_C_SOURCE 200809L
#include <assert.h>
#include <stdio.h>
#include <string.h>
#include "idf.h"
#include "../src/tunnel.c"

static bool fail_task, tcpip, online;
static unsigned tasks;
static wireguard_config_t configured;
static struct netif interface;
static unsigned disconnects;

static bool uplink(void) { return online; }
esp_err_t esp_netif_sntp_init(const esp_sntp_config_t *c) { (void)c; return ESP_OK; }
esp_err_t esp_netif_tcpip_exec(esp_err_t (*fn)(void *), void *arg)
{ assert(!tcpip); tcpip=true; int err=fn(arg); tcpip=false; return err; }
esp_err_t esp_wireguard_init(wireguard_config_t *cfg, wireguard_ctx_t *ctx)
{ assert(tcpip); (void)ctx; configured=*cfg; return ESP_OK; }
esp_err_t esp_wireguard_connect(wireguard_ctx_t *ctx)
{ assert(tcpip); ctx->netif=&interface; return ESP_OK; }
esp_err_t esp_wireguard_add_allowed_ip(wireguard_ctx_t *ctx, const char *ip, const char *mask)
{ assert(tcpip && ctx->netif); assert(!strcmp(ip,"10.77.0.1")); assert(!strcmp(mask,"255.255.255.255")); return ESP_OK; }
esp_err_t esp_wireguard_peer_is_up(wireguard_ctx_t *ctx)
{ assert(tcpip && ctx->netif); return ESP_OK; }
esp_err_t esp_wireguard_disconnect(wireguard_ctx_t *ctx)
{ assert(tcpip && ctx->netif); ctx->netif=NULL; disconnects++; return ESP_OK; }
int ip4addr_aton(const char *s, ip4_addr_t *out)
{
    uint8_t bytes[4]; if (!netcfg_tunnel_ip4_parse(s,strlen(s),bytes)) return 0;
    out->addr=(uint32_t)bytes[0]<<24 | (uint32_t)bytes[1]<<16 | (uint32_t)bytes[2]<<8 | bytes[3];
    return 1;
}
void netif_set_gw(struct netif *n, const ip4_addr_t *gw) { assert(tcpip); n->gateway=*gw; }
TickType_t xTaskGetTickCount(void) { return 0; }
void vTaskDelay(TickType_t t) { (void)t; }
int xTaskCreate(void (*f)(void *), const char *n, int stack, void *a, int priority, void *h)
{ (void)f; (void)n; (void)stack; (void)a; (void)priority; (void)h; if (fail_task) return 0; tasks++; return pdPASS; }

int main(void)
{
    netcfg_tunnel_t cfg={.enabled=true,.private_key={1},.peer_public_key={2},
        .endpoint_host="wg.collector.invalid",.endpoint_port=51820,
        .address={10,77,0,12},.prefix=24,.collector={10,77,0,1},.keepalive=25};
    assert(tunnel_start(NULL,uplink)==ESP_ERR_INVALID_ARG);
    assert(tunnel_start(&cfg,NULL)==ESP_ERR_INVALID_ARG && tasks==0);
    fail_task=true;
    assert(tunnel_start(&cfg,uplink)==ESP_ERR_NO_MEM && !tunnel_collector_address());
    fail_task=false;
    assert(tunnel_start(&cfg,uplink)==ESP_OK && tasks==1 && !tunnel_up());
    assert(!strcmp(tunnel_collector_address(),"10.77.0.1"));
    // The task follows whichever link the caller reports, Wi-Fi or Ethernet.
    assert(!s_uplink_up()); online=true; assert(s_uplink_up());
    assert(tunnel_start(&cfg,uplink)==ESP_ERR_INVALID_STATE && tasks==1);
    assert(esp_netif_tcpip_exec(in_tcpip_disconnect,NULL)==ESP_OK && disconnects==0);
    assert(esp_netif_tcpip_exec(in_tcpip_init,NULL)==ESP_OK);
    // The profile's /24 must never become a connected subnet route. With /32,
    // lwIP's subnet check matches only self and its point-to-point gateway
    // check matches only the collector; 10.77.0.53 stays on the uplink's route.
    assert(!strcmp(configured.netmask,"255.255.255.255"));
    assert(!strcmp(configured.address,"10.77.0.12"));
    assert(configured.listen_port==0 && configured.persistent_keepalive==25);
    assert(esp_netif_tcpip_exec(in_tcpip_connect,NULL)==ESP_OK);
    assert(esp_netif_tcpip_exec(in_tcpip_route,NULL)==ESP_OK);
    assert(interface.gateway.addr==0x0a4d0001);
    assert(esp_netif_tcpip_exec(in_tcpip_disconnect,NULL)==ESP_OK && disconnects==1);
    puts("Tunnel host route, TCP/IP thread calls and startup cleanup passed");
    return 0;
}
