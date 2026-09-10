/* both TLS I/O loops used to `continue` immediately on
 * ESP_TLS_ERR_SSL_WANT_READ/WANT_WRITE, so a flow-controlled or half-open
 * connection spun at full speed for as long as OP_DEADLINE_S. On the single-core
 * C6 the priority-6 pusher then owned the core between higher-priority
 * interrupts, starving provisioning, display and housekeeping work and burning
 * power on a link making no progress. Every WANT must now yield — through socket
 * readiness where possible, otherwise a bounded tick delay — while the
 * no-progress deadline, its reset on positive progress, and partial-I/O
 * semantics all stay exactly as they were. */
#define _POSIX_C_SOURCE 200809L
#include <assert.h>
#include <stdio.h>
#include <string.h>
#include "idf_stubs.h"
#define select test_select
#include "../src/pusher.c"
#undef select

static int64_t clock_us;
static int want_writes_remaining, want_reads_remaining;
static int io_calls, yields, readiness_waits;
static int sockfd_available = 1;
static int progress_bytes;          /* bytes each successful op transfers */
static uint8_t written[256];
static size_t written_len;
static uint8_t inbound_bytes[256];
static size_t inbound_len, inbound_off;
static esp_tls_t transport;

int64_t esp_timer_get_time(void) { return clock_us; }

ssize_t esp_tls_conn_write(esp_tls_t *tls, const void *buf, size_t n)
{
    (void)tls;
    io_calls++;
    if (want_writes_remaining > 0) { want_writes_remaining--; return ESP_TLS_ERR_SSL_WANT_WRITE; }
    if (want_reads_remaining > 0) { want_reads_remaining--; return ESP_TLS_ERR_SSL_WANT_READ; }
    size_t take = n;
    if (progress_bytes > 0 && (size_t)progress_bytes < take) take = (size_t)progress_bytes;
    assert(written_len + take <= sizeof written);
    memcpy(written + written_len, buf, take);
    written_len += take;
    return (ssize_t)take;
}

ssize_t esp_tls_conn_read(esp_tls_t *tls, void *buf, size_t n)
{
    (void)tls;
    io_calls++;
    if (want_reads_remaining > 0) { want_reads_remaining--; return ESP_TLS_ERR_SSL_WANT_READ; }
    if (want_writes_remaining > 0) { want_writes_remaining--; return ESP_TLS_ERR_SSL_WANT_WRITE; }
    size_t avail = inbound_len - inbound_off;
    size_t take = n < avail ? n : avail;
    if (progress_bytes > 0 && (size_t)progress_bytes < take) take = (size_t)progress_bytes;
    if (take == 0) return -1;
    memcpy(buf, inbound_bytes + inbound_off, take);
    inbound_off += take;
    return (ssize_t)take;
}

int esp_tls_get_bytes_avail(esp_tls_t *tls) { (void)tls; return 0; }
esp_tls_t *esp_tls_init(void) { return &transport; }
int esp_tls_conn_new_sync(const char *h, int l, int p, const esp_tls_cfg_t *c, esp_tls_t *t)
{ (void)h; (void)l; (void)p; (void)c; (void)t; return 1; }
void esp_tls_conn_destroy(esp_tls_t *t) { (void)t; }
esp_err_t esp_tls_get_conn_sockfd(esp_tls_t *t, int *fd)
{ (void)t; if (!sockfd_available) return -1; *fd = 1; return ESP_OK; }
int esp_crt_bundle_attach(void *p) { (void)p; return 0; }
uint32_t esp_random(void) { return 0; }
int xTaskCreate(void (*f)(void*), const char *n, int sz, void *a, int pri, void *h)
{ (void)f; (void)n; (void)sz; (void)a; (void)pri; (void)h; return pdPASS; }

/* Both yield mechanisms advance the clock, exactly as a real wait would. */
void vTaskDelay(int ticks)
{
    yields++;
    clock_us += (int64_t)ticks * 1000;
}
int test_select(int n, fd_set *r, fd_set *w, fd_set *e, struct timeval *timeout)
{
    (void)n; (void)e;
    /* Exactly one direction may be waited on, and it must be the one the WANT
     * code asked for — a write that needs readability must wait for readability. */
    assert((r != NULL) != (w != NULL));
    readiness_waits++;
    yields++;
    clock_us += timeout->tv_sec * 1000000LL + timeout->tv_usec;
    return 0; /* timed out: nothing became ready */
}

static void reset(void)
{
    clock_us = 0;
    want_writes_remaining = want_reads_remaining = 0;
    io_calls = yields = readiness_waits = 0;
    sockfd_available = 1;
    progress_bytes = 0;
    written_len = 0;
    inbound_len = inbound_off = 0;
}

static int failures;
static void expect(int cond, const char *what)
{
    if (!cond) { printf("FAIL: %s\n", what); failures++; }
}

int main(void)
{
    const uint8_t payload[8] = { 1, 2, 3, 4, 5, 6, 7, 8 };

    /* A peer that never becomes writable: the deadline must still bound the loop,
     * and every single retry must have yielded rather than spun. */
    reset();
    want_writes_remaining = 1000000;
    expect(tls_write_all(&transport, payload, sizeof payload) == -1,
           "an endless WANT must trip the no-progress deadline");
    expect(yields == io_calls - 1 || yields == io_calls,
           "every WANT retry must yield");
    expect(readiness_waits > 0, "readiness must be waited on when the socket fd is available");
    /* OP_DEADLINE_S at TLS_WANT_WAIT_MS per wait bounds the call count. Before the
     * fix this loop ran millions of iterations in the same window. */
    int bound = (OP_DEADLINE_S * 1000) / TLS_WANT_WAIT_MS + 8;
    expect(io_calls <= bound, "WANT retries must be paced, not spun");
    expect(clock_us >= (int64_t)OP_DEADLINE_S * 1000000,
           "the 30-second dead-link bound must be preserved");

    /* WANT_READ during a write must wait for READABILITY (the select assert above
     * proves the direction), and WANT_WRITE during a read for writability. */
    reset();
    want_reads_remaining = 3;
    expect(tls_write_all(&transport, payload, sizeof payload) == 0,
           "a write that needs readability must still complete");
    expect(readiness_waits == 3, "each WANT_READ during a write waits once");
    reset();
    inbound_len = sizeof payload;
    memcpy(inbound_bytes, payload, sizeof payload);
    want_writes_remaining = 2;
    uint8_t got[8] = { 0 };
    expect(tls_read_full(&transport, got, sizeof got) == 0,
           "a read that needs writability must still complete");
    expect(!memcmp(got, payload, sizeof payload), "bytes must arrive in order");
    expect(readiness_waits == 2, "each WANT_WRITE during a read waits once");

    /* Partial progress: the deadline restarts on every byte transferred, so a slow
     * but advancing connection is never declared dead. */
    reset();
    progress_bytes = 1;             /* one byte per successful op */
    want_writes_remaining = 0;
    written_len = 0;
    for (size_t i = 0; i < sizeof payload; i++) {
        want_writes_remaining += 2; /* two WANTs before each byte */
    }
    expect(tls_write_all(&transport, payload, sizeof payload) == 0,
           "partial progress must keep resetting the deadline");
    expect(written_len == sizeof payload, "every byte must be written");
    expect(!memcmp(written, payload, sizeof payload), "bytes must be written in order");

    /* Without a socket fd, the loop must fall back to a tick delay and still yield
     * — never spin. */
    reset();
    sockfd_available = 0;
    want_writes_remaining = 1000000;
    expect(tls_write_all(&transport, payload, sizeof payload) == -1, "deadline still bounds it");
    expect(readiness_waits == 0, "no readiness wait is possible without an fd");
    expect(yields > 0, "the fallback must still yield the core");
    expect(io_calls <= bound, "the fallback must also be paced");

    printf("pusher TLS WANT yielding: %d failure(s)\n", failures);
    return failures != 0;
}
