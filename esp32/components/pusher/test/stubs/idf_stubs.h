#ifndef IDF_STUBS_H
#define IDF_STUBS_H
#include <stdbool.h>
#include <stdint.h>
#include <stddef.h>
#include <sys/types.h>
#include <sys/select.h>
#define ESP_OK 0
#define pdPASS 1
#define pdTRUE 1
#define pdMS_TO_TICKS(n) (n)
#define portMAX_DELAY 0
#define ESP_TLS_ERR_SSL_WANT_WRITE -2
#define ESP_TLS_ERR_SSL_WANT_READ -3
#define ESP_TLS_VER_TLS_1_2 1
#define ESP_LOGI(tag,...) ((void)(tag))
#define ESP_LOGW(tag,...) ((void)(tag))
#define ESP_LOGE(tag,...) ((void)(tag))
typedef int esp_err_t;
typedef int TickType_t;
typedef int SemaphoreHandle_t;
typedef struct {int dummy;} esp_tls_t;
typedef struct {bool keep_alive_enable;int keep_alive_idle,keep_alive_interval,keep_alive_count;} tls_keep_alive_cfg_t;
typedef struct {int tls_version,timeout_ms;tls_keep_alive_cfg_t *keep_alive_cfg;const unsigned char *cacert_buf;size_t cacert_bytes;int (*crt_bundle_attach)(void*);const char *common_name;} esp_tls_cfg_t;
int64_t esp_timer_get_time(void);
ssize_t esp_tls_conn_write(esp_tls_t*,const void*,size_t);
ssize_t esp_tls_conn_read(esp_tls_t*,void*,size_t);
int esp_tls_get_bytes_avail(esp_tls_t*);
esp_tls_t *esp_tls_init(void);
int esp_tls_conn_new_sync(const char*,int,int,const esp_tls_cfg_t*,esp_tls_t*);
void esp_tls_conn_destroy(esp_tls_t*);
esp_err_t esp_tls_get_conn_sockfd(esp_tls_t*,int*);
int esp_crt_bundle_attach(void*);
// TLS keying-material export (mbedtls/ssl.h). The default stand-ins succeed with a fixed
// pattern so every suite links; evidence_handshake_test substitutes its own.
#define MBEDTLS_SSL_KEYING_MATERIAL_EXPORT
typedef struct {int dummy;} mbedtls_ssl_context;
static inline void *esp_tls_get_ssl_context(esp_tls_t *t){return t;}
static inline int mbedtls_ssl_export_keying_material(mbedtls_ssl_context *s,uint8_t *out,size_t n,const char *label,size_t label_len,const unsigned char *c,size_t cn,int use)
{(void)s;(void)label;(void)label_len;(void)c;(void)cn;(void)use;for(size_t i=0;i<n;i++)out[i]=(uint8_t)(0xe0+i);return 0;}
void vTaskDelay(int);
uint32_t esp_random(void);
int xTaskCreate(void (*)(void*),const char*,int,void*,int,void*);
static inline SemaphoreHandle_t xSemaphoreCreateMutex(void){return 1;}
static inline int xSemaphoreTake(SemaphoreHandle_t m,int t){(void)m;(void)t;return pdTRUE;}
static inline void vSemaphoreDelete(SemaphoreHandle_t m){(void)m;}
static inline void xSemaphoreGive(SemaphoreHandle_t m){(void)m;}
int test_select(int,fd_set*,fd_set*,fd_set*,struct timeval*);
#endif
