// Exercise the real reconnect loop through DNS/TLS failure and recovery.
#define _POSIX_C_SOURCE 200809L
#include <assert.h>
#include <setjmp.h>
#include <stdio.h>
#include "idf_stubs.h"
#include "../src/pusher.c"
static jmp_buf stop;
static int attempts, waits, destroyed;
static esp_tls_t transport;
int64_t esp_timer_get_time(void) { return 0; }
ssize_t esp_tls_conn_write(esp_tls_t *t,const void *p,size_t n) { (void)t;(void)p;(void)n; return -1; }
ssize_t esp_tls_conn_read(esp_tls_t *t,void *p,size_t n) { (void)t;(void)p;(void)n; return -1; }
int esp_tls_get_bytes_avail(esp_tls_t *t) { (void)t; return 0; }
esp_tls_t *esp_tls_init(void) { return &transport; }
int esp_tls_conn_new_sync(const char *host,int len,int port,const esp_tls_cfg_t *cfg,esp_tls_t *tls) {
    (void)tls;
    assert(len == (int)strlen(host) && port == 5580 && cfg->crt_bundle_attach == esp_crt_bundle_attach);
    assert(!strcmp(host, attempts % 2 ? "in.intsat.space" : "in.intsat.net"));
    assert(!strcmp(s_cfg.session,"same-boot") && spool_acked() == 0);
    if (++attempts == 4) longjmp(stop, 1);
    return 0;
}
void esp_tls_conn_destroy(esp_tls_t *t) { (void)t; destroyed++; }
esp_err_t esp_tls_get_conn_sockfd(esp_tls_t *t,int *fd) { (void)t; *fd=-1; return -1; }
int esp_crt_bundle_attach(void *p) { (void)p; return 0; }
void vTaskDelay(int ticks) { assert(ticks == (1000 << waits)); waits++; }
uint32_t esp_random(void) { return 0; }
int xTaskCreate(void (*f)(void*),const char *n,int s,void *a,int p,void *h)
{ (void)f;(void)n;(void)s;(void)a;(void)p;(void)h; return pdPASS; }
int main(void) {
    char other[128];
    assert(nav_endpoint_secondary("IN.INTSAT.NET.",other,sizeof other) && !strcmp(other,"in.intsat.space"));
    assert(nav_endpoint_secondary("in.intsat.space",other,sizeof other) && !strcmp(other,"in.intsat.net"));
    const char *invalid[] = {"intsat.net", "intsat.space", "in.intsat.net.evil.invalid", "other.intsat.net", "127.0.0.1"};
    for (unsigned i=0;i<sizeof invalid/sizeof invalid[0];i++) assert(!nav_endpoint_secondary(invalid[i],other,sizeof other));
    assert(!nav_endpoint_secondary("in.intsat.net",other,5) && !other[0]);
    assert(nav_endpoint_secondary_url("https://firmware.intsat.net:443/a.bin?x=1",other,sizeof other));
    assert(!strcmp(other,"https://firmware.intsat.space:443/a.bin?x=1"));
    assert(!nav_endpoint_secondary_url("https://firmware.intsat.net:443@evil.invalid/a",other,sizeof other));
    assert(!nav_endpoint_secondary_url("http://firmware.intsat.net/a",other,sizeof other));
    assert(spool_init(1024));
    s_cfg=(pusher_cfg_t){.host="in.intsat.net",.port=5580,.session="same-boot"};
    if (!setjmp(stop)) pusher_task(NULL);
    assert(attempts == 4 && destroyed == 3 && waits == 3);
    puts("endpoint aliases, TLS verification and bounded reconnect fallback PASS");
}
