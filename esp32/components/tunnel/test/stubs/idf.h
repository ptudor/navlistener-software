#pragma once
#include <stdbool.h>
#include <stdint.h>
#include <stddef.h>
#define CONFIG_NVF_WIREGUARD 1
#define ESP_OK 0
#define ESP_FAIL -1
#define ESP_ERR_INVALID_ARG 1
#define ESP_ERR_INVALID_STATE 2
#define ESP_ERR_NO_MEM 3
#define ESP_ERR_RETRY 4
#define pdPASS 1
#define pdMS_TO_TICKS(n) (n)
#define ESP_LOGI(tag, ...) ((void)(tag))
#define ESP_LOGW(tag, ...) ((void)(tag))
#define ESP_LOGE(tag, ...) ((void)(tag))
typedef int esp_err_t;
typedef uint32_t TickType_t;
typedef struct { uint32_t addr; } ip4_addr_t;
struct netif { ip4_addr_t gateway; };
typedef struct { const char *server; } esp_sntp_config_t;
#define ESP_NETIF_SNTP_DEFAULT_CONFIG(host) {host}
typedef struct {
    const char *private_key, *public_key, *preshared_key, *address, *netmask, *endpoint;
    uint16_t port, persistent_keepalive, listen_port;
} wireguard_config_t;
typedef struct { struct netif *netif; } wireguard_ctx_t;
#define ESP_WIREGUARD_CONFIG_DEFAULT() {0}
#define ESP_WIREGUARD_CONTEXT_DEFAULT() {0}
esp_err_t esp_netif_sntp_init(const esp_sntp_config_t *);
esp_err_t esp_netif_tcpip_exec(esp_err_t (*)(void *), void *);
esp_err_t esp_wireguard_init(wireguard_config_t *, wireguard_ctx_t *);
esp_err_t esp_wireguard_connect(wireguard_ctx_t *);
esp_err_t esp_wireguard_add_allowed_ip(wireguard_ctx_t *, const char *, const char *);
esp_err_t esp_wireguard_peer_is_up(wireguard_ctx_t *);
esp_err_t esp_wireguard_disconnect(wireguard_ctx_t *);
int ip4addr_aton(const char *, ip4_addr_t *);
void netif_set_gw(struct netif *, const ip4_addr_t *);
TickType_t xTaskGetTickCount(void);
void vTaskDelay(TickType_t);
int xTaskCreate(void (*)(void *), const char *, int, void *, int, void *);
