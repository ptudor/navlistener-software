// Tunnel preference in the real reconnect loop: the WireGuard path is used only while the
// peer session is valid, the certificate is still checked against the configured collector
// name, a down tunnel falls straight through to the public endpoint, and a direct session
// moves onto the tunnel once it comes up with nothing outstanding.
#define _POSIX_C_SOURCE 200809L
#include <assert.h>
#include <setjmp.h>
#include <stdio.h>
#include "idf_stubs.h"
#define select test_select
#include "../src/pusher.c"
#undef select

static jmp_buf stop;
static bool tunnel_state;
static int attempts, destroyed, last_wait, phase;
static int64_t clock_us;
static esp_tls_t transport;
static struct { const char *host; const char *verify; bool ok; } script[] = {
    { "collector.invalid", NULL, false },              // 1: tunnel down, public endpoint refused
    { "10.77.0.1", "collector.invalid", false },       // 2: tunnel up, tunnel address refused
    { "collector.invalid", NULL, true },               // 3: tunnel still up, private listener refused
    { "10.77.0.1", "collector.invalid", true },        // 4: cooldown elapsed: retry private listener
};
static uint8_t inbound[1024];
static size_t head, tail;

static bool tunnel_up_stub(void) { return tunnel_state; }

static void queue(uint8_t type, const uint8_t *body, size_t len)
{
    assert(tail + GNF1_FRAME_HDR + len <= sizeof inbound);
    gnf1_frame_header(inbound + tail, type, (uint32_t)len);
    tail += GNF1_FRAME_HDR;
    if (len) memcpy(inbound + tail, body, len);
    tail += len;
}

int64_t esp_timer_get_time(void) { return clock_us; }
esp_tls_t *esp_tls_init(void) { return &transport; }
int esp_crt_bundle_attach(void *p) { (void)p; return 0; }
uint32_t esp_random(void) { return 0; }
int xTaskCreate(void (*f)(void *), const char *n, int s, void *a, int p, void *h)
{ (void)f; (void)n; (void)s; (void)a; (void)p; (void)h; return pdPASS; }
void esp_tls_conn_destroy(esp_tls_t *t) { (void)t; destroyed++; }
esp_err_t esp_tls_get_conn_sockfd(esp_tls_t *t, int *fd) { (void)t; *fd = 3; return ESP_OK; }
int esp_tls_get_bytes_avail(esp_tls_t *t) { (void)t; return (int)(tail - head); }
void vTaskDelay(int ticks) { last_wait = ticks; clock_us += (int64_t)ticks * 1000; }

int esp_tls_conn_new_sync(const char *host, int len, int port, const esp_tls_cfg_t *cfg, esp_tls_t *tls)
{
    (void)tls;
    assert(attempts < (int)(sizeof script / sizeof script[0]));
    assert(len == (int)strlen(host) && port == 5580 && cfg->crt_bundle_attach == esp_crt_bundle_attach);
    assert(!strcmp(host, script[attempts].host));
    assert((cfg->common_name == NULL) == (script[attempts].verify == NULL));
    if (cfg->common_name) assert(!strcmp(cfg->common_name, script[attempts].verify));
    assert(!pusher_connected() && !pusher_via_tunnel());
    phase = 0;
    head = tail = 0;
    bool ok = script[attempts++].ok;
    // The peer stays up after the private listener refuses attempt 2. A handshake
    // alone must not prevent fallback to the working public listener.
    if (attempts == 1) tunnel_state = true;
    return ok ? 1 : 0;
}

ssize_t esp_tls_conn_write(esp_tls_t *t, const void *p, size_t n)
{
    (void)t;
    const uint8_t *b = p;
    if (phase == 0) { assert(n == 4 && !memcmp(b, "GNF1", 4)); phase++; return (ssize_t)n; }
    if (phase == 1) { assert(n == 5 && b[0] == GNF1_F_HELLO); phase++; return (ssize_t)n; }
    if (phase == 2) {
        assert(strstr((const char *)b, "\"session\":\"same-boot\""));
        phase++;
        static const uint8_t welcome[] = "{\"ok\":true}";
        queue(GNF1_F_WELCOME, welcome, sizeof welcome - 1);
        return (ssize_t)n;
    }
    assert(b[0] == GNF1_F_PING); // idle keepalive only; nothing is spooled in this test
    queue(GNF1_F_PONG, NULL, 0);
    return (ssize_t)n;
}

ssize_t esp_tls_conn_read(esp_tls_t *t, void *p, size_t n)
{
    (void)t;
    if (head == tail) return ESP_TLS_ERR_SSL_WANT_READ;
    if (n > tail - head) n = tail - head;
    memcpy(p, inbound + head, n);
    head += n;
    return (ssize_t)n;
}

// The idle loop parks in readable(); each turn advances the clock by the wait. The tunnel
// remains up during the direct session (attempt 3). Retry only after the cooldown,
// with no outstanding records; once attempt 4 is serving inside it the test is done.
int test_select(int n, fd_set *r, fd_set *w, fd_set *e, struct timeval *tv)
{
    (void)n; (void)r; (void)w; (void)e;
    clock_us += (int64_t)tv->tv_sec * 1000000 + tv->tv_usec;
    static int64_t direct_started;
    if (attempts == 3 && pusher_connected()) {
        assert(!pusher_via_tunnel());
        if (!direct_started) direct_started = clock_us;
        if (clock_us < s_tunnel_retry_us) assert(!tunnel_preferred());
    }
    if (attempts == 4 && pusher_connected()) {
        assert(pusher_via_tunnel());
        assert(clock_us - direct_started >= TUNNEL_RETRY_US - 4000000);
        longjmp(stop, 1);
    }
    return 0;
}

int main(void)
{
    assert(spool_init(64));
    s_cfg = (pusher_cfg_t){ .host = "collector.invalid", .port = 5580, .session = "same-boot",
                            .tunnel_host = "10.77.0.1", .tunnel_up = tunnel_up_stub };
    // Attempt 1 runs with the tunnel down. pusher_task never returns; the stubs drive the
    // script and test_select leaves through longjmp once attempt 4 is serving.
    tunnel_state = false;
    if (!setjmp(stop)) pusher_task(NULL);
    assert(attempts == 4);
    // Attempts 1-3 each destroyed their transport (two refused connects, one session that
    // was deliberately ended to move over); the moved-onto tunnel session is still live.
    assert(destroyed == 3);
    // The direct session ran past USEFUL_CONN_S, so the ladder reset: the reconnect that
    // moved onto the tunnel waited the 1 s floor, not a grown backoff.
    assert(last_wait == 1000);
    // Without a configured tunnel or an up-callback the choice is never taken.
    s_cfg.tunnel_host = NULL;
    assert(!tunnel_preferred());
    s_cfg.tunnel_host = "10.77.0.1"; s_cfg.tunnel_up = NULL;
    assert(!tunnel_preferred());
    puts("tunnel preference, certificate name pinning, fallback and migration PASS");
    return 0;
}
