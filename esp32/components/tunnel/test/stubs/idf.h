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
#define ESP_RETURN_ON_ERROR(call, tag, ...) do { int e = (call); if (e) return e; } while (0)
typedef int esp_err_t;
typedef uint32_t TickType_t;
typedef const char *esp_event_base_t;
typedef void *esp_event_handler_instance_t;
static const char IP_EVENT[] = "IP";
static const char WIFI_EVENT[] = "WIFI";
#define IP_EVENT_STA_GOT_IP 1
#define WIFI_EVENT_STA_DISCONNECTED 2
typedef struct { uint32_t addr; } ip4_addr_t;
struct netif { ip4_addr_t gateway; };
typedef struct { int unused; } esp_netif_t;
typedef struct { ip4_addr_t ip; } esp_netif_ip_info_t;
typedef struct { const char *server; } esp_sntp_config_t;
#define ESP_NETIF_SNTP_DEFAULT_CONFIG(host) {host}
typedef struct {
    const char *private_key, *public_key, *preshared_key, *address, *netmask, *endpoint;
    uint16_t port, persistent_keepalive, listen_port;
} wireguard_config_t;
typedef struct { struct netif *netif; } wireguard_ctx_t;
#define ESP_WIREGUARD_CONFIG_DEFAULT() {0}
#define ESP_WIREGUARD_CONTEXT_DEFAULT() {0}
esp_err_t esp_event_handler_instance_register(esp_event_base_t, int32_t,
    void (*)(void *, esp_event_base_t, int32_t, void *), void *, esp_event_handler_instance_t *);
esp_err_t esp_event_handler_instance_unregister(esp_event_base_t, int32_t, esp_event_handler_instance_t);
esp_netif_t *esp_netif_get_handle_from_ifkey(const char *);
esp_err_t esp_netif_get_ip_info(esp_netif_t *, esp_netif_ip_info_t *);
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
