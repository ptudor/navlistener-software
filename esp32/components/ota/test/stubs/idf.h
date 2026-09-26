#pragma once
#include <stdbool.h>
#include <stdint.h>
#include <stddef.h>
#define CONFIG_NVF_OTA 1
#define CONFIG_IDF_FIRMWARE_CHIP_ID 9
#define ESP_OK 0
#define ESP_FAIL -1
#define ESP_ERR_NO_MEM 1
#define ESP_ERR_INVALID_STATE 2
#define ESP_ERR_TIMEOUT 3
#define ESP_ERR_INVALID_SIZE 4
#define ESP_ERR_INVALID_RESPONSE 5
#define ESP_ERR_INVALID_VERSION 6
#define ESP_ERR_INVALID_CRC 7
#define ESP_PARTITION_TYPE_APP 0
#define ESP_PARTITION_SUBTYPE_APP_OTA_0 16
#define ESP_PARTITION_SUBTYPE_APP_OTA_1 17
#define OTA_WITH_SEQUENTIAL_WRITES 0xffffffff
#define HTTP_TRANSPORT_OVER_SSL 2
#define pdMS_TO_TICKS(n) (n)
typedef int esp_err_t;
typedef struct { int type, subtype; size_t size; } esp_partition_t;
typedef unsigned esp_ota_handle_t;
typedef struct { char project_name[32]; } esp_app_desc_t;
typedef void *esp_http_client_handle_t;
#define HTTP_EVENT_ON_HEADER 1
typedef struct {int event_id;const char *header_key,*header_value;void *user_data;} esp_http_client_event_t;
typedef struct {
    esp_err_t (*event_handler)(esp_http_client_event_t *);void *user_data;
    const char *url; int (*crt_bundle_attach)(void *);
    int transport_type; bool disable_auto_redirect; int timeout_ms, buffer_size;
} esp_http_client_config_t;
typedef struct { uint32_t value; } mbedtls_sha256_context;
const esp_partition_t *esp_ota_get_running_partition(void);
const esp_partition_t *esp_ota_get_next_update_partition(const esp_partition_t *);
const esp_partition_t *esp_ota_get_boot_partition(void);
esp_err_t esp_ota_begin(const esp_partition_t *,size_t,esp_ota_handle_t *);
esp_err_t esp_ota_write(esp_ota_handle_t,const void *,size_t);
esp_err_t esp_ota_end(esp_ota_handle_t);
esp_err_t esp_ota_abort(esp_ota_handle_t);
esp_err_t esp_ota_set_boot_partition(const esp_partition_t *);
const esp_app_desc_t *esp_app_get_description(void);
int esp_crt_bundle_attach(void *);
esp_http_client_handle_t esp_http_client_init(const esp_http_client_config_t *);
esp_err_t esp_http_client_set_header(esp_http_client_handle_t,const char *,const char *);
esp_err_t esp_http_client_open(esp_http_client_handle_t,int);
int64_t esp_http_client_fetch_headers(esp_http_client_handle_t);
int esp_http_client_get_status_code(esp_http_client_handle_t);
bool esp_http_client_is_chunked_response(esp_http_client_handle_t);
int esp_http_client_read(esp_http_client_handle_t,char *,int);
bool esp_http_client_is_complete_data_received(esp_http_client_handle_t);
void esp_http_client_cleanup(esp_http_client_handle_t);
int64_t esp_timer_get_time(void);
void vTaskDelay(int);
void mbedtls_sha256_init(mbedtls_sha256_context *);
int mbedtls_sha256_starts(mbedtls_sha256_context *,int);
int mbedtls_sha256_update(mbedtls_sha256_context *,const uint8_t *,size_t);
int mbedtls_sha256_finish(mbedtls_sha256_context *,uint8_t *);
void mbedtls_sha256_free(mbedtls_sha256_context *);
void spool_stats(uint64_t *,uint64_t *,size_t *);
uint64_t spool_acked(void);
