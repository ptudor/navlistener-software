// An sbf session in the real handshake and send path (SBF record format 1, docs/DESIGN.md §2):
// the HELLO names the feed and the format, a WELCOME that accepts the session without
// confirming that format is a rejection, and a spooled SBF record longer than an inline DATA
// frame still goes out whole, as the DATA header followed by the record.
#define _POSIX_C_SOURCE 200809L
#include <assert.h>
#include <stdio.h>
#include <string.h>
#include "idf_stubs.h"
#define select test_select
#include "../src/pusher.c"
#undef select

static esp_tls_t transport;
static uint8_t written[16384], inbound[1024];
static size_t written_len, head, tail;
static unsigned writes;

int64_t esp_timer_get_time(void) { return 0; }
esp_tls_t *esp_tls_init(void) { return &transport; }
int esp_crt_bundle_attach(void *p) { (void)p; return 0; }
uint32_t esp_random(void) { return 0; }
int xTaskCreate(void (*f)(void *), const char *n, int s, void *a, int p, void *h)
{ (void)f; (void)n; (void)s; (void)a; (void)p; (void)h; return pdPASS; }
void esp_tls_conn_destroy(esp_tls_t *t) { (void)t; }
esp_err_t esp_tls_get_conn_sockfd(esp_tls_t *t, int *fd) { (void)t; *fd = 3; return ESP_OK; }
int esp_tls_get_bytes_avail(esp_tls_t *t) { (void)t; return (int)(tail - head); }
int esp_tls_conn_new_sync(const char *h, int l, int p, const esp_tls_cfg_t *c, esp_tls_t *t)
{ (void)h; (void)l; (void)p; (void)c; (void)t; return 1; }
void vTaskDelay(int ticks) { (void)ticks; }
int test_select(int n, fd_set *r, fd_set *w, fd_set *e, struct timeval *tv)
{ (void)n; (void)r; (void)w; (void)e; (void)tv; return 0; }

ssize_t esp_tls_conn_write(esp_tls_t *t, const void *p, size_t n)
{
    (void)t;
    assert(written_len + n <= sizeof written);
    memcpy(written + written_len, p, n);
    written_len += n;
    writes++;
    return (ssize_t)n;
}
ssize_t esp_tls_conn_read(esp_tls_t *t, void *p, size_t n)
{
    (void)t;
    if (head == tail) return -1;
    if (n > tail - head) n = tail - head;
    memcpy(p, inbound + head, n);
    head += n;
    return (ssize_t)n;
}

static void reception_stub(uint8_t type, const uint8_t *b, size_t n) { (void)type; (void)b; (void)n; }

static int run(const char *welcome)
{
    written_len = head = tail = 0;
    gnf1_frame_header(inbound, GNF1_F_WELCOME, (uint32_t)strlen(welcome));
    memcpy(inbound + GNF1_FRAME_HDR, welcome, strlen(welcome));
    tail = GNF1_FRAME_HDR + strlen(welcome);
    return handshake(&transport);
}

static bool hello_contains(const char *text)
{
    size_t len = gnf1_rd_be32(written + 4 + 1), n = strlen(text);
    const uint8_t *body = written + 4 + GNF1_FRAME_HDR;
    assert(written[4] == GNF1_F_HELLO);
    for (size_t i = 0; i + n <= len; i++) if (!memcmp(body + i, text, n)) return true;
    return false;
}

int main(void)
{
    s_cfg = (pusher_cfg_t){ .host = "collector.invalid", .port = 5580, .token = "t", .station = "p3",
                            .feed = "sbf", .sbf_format = GNF1_SBF_FORMAT, .session = "boot-1" };

    // 1. The HELLO names the feed and the format; a WELCOME confirming it opens the session.
    assert(run("{\"ok\":true,\"durable_ack\":true,\"sbf\":1}") == 0);
    assert(hello_contains("\"feed\":\"sbf\"") && hello_contains(",\"sbf\":1}"));
    assert(atomic_load(&s_durable));

    // 2. A collector that accepts the session without confirming the format, or confirms
    //    another, is a rejection: nothing is sent to a collector that would misread it.
    assert(run("{\"ok\":true}") == -2);
    assert(run("{\"ok\":true,\"sbf\":2}") == -2);
    assert(run("{\"ok\":true,\"sbf\":true}") == -2);
    assert(run("{\"ok\":false,\"error\":\"unsupported sbf format\"}") == -2);

    // 3. The reception capability rides along after the format.
    s_cfg.reception_control = reception_stub;
    assert(run("{\"ok\":true,\"sbf\":1}") == 0 && hello_contains(",\"sbf\":1,\"reception\":2}"));
    s_cfg.reception_control = NULL;

    // 4. A u-blox feeder asks for no format and is unaffected by one in the WELCOME.
    s_cfg.feed = "ubx"; s_cfg.sbf_format = 0;
    assert(run("{\"ok\":true}") == 0 && !hello_contains("sbf"));

    // 5. A record that fits goes out in one write; an SBF record longer than GNF1_RECORD_MAX
    //    goes out as the DATA header and then the record, whole.
    static uint8_t frame[GNF1_DATA_MAX], large[GNF1_SBF_RECORD_MAX];
    uint8_t small[40];
    memset(small, 0x11, sizeof small);
    for (size_t i = 0; i < sizeof large; i++) large[i] = (uint8_t)(i * 13);
    written_len = 0; writes = 0;
    assert(send_record(&transport, frame, &(spool_frame_t){ .seq = 5, .len = sizeof small, .data = small }) == 0);
    assert(writes == 1 && written_len == GNF1_DATA_HDR + sizeof small);
    written_len = 0; writes = 0;
    assert(send_record(&transport, frame, &(spool_frame_t){ .seq = 6, .len = sizeof large, .data = large }) == 0);
    assert(writes == 2 && written_len == GNF1_DATA_HDR + sizeof large);
    assert(written[0] == GNF1_F_DATA && gnf1_rd_be32(written + 1) == 8 + sizeof large);
    assert(gnf1_rd_be64(written + GNF1_FRAME_HDR) == 6);
    assert(!memcmp(written + GNF1_DATA_HDR, large, sizeof large));

    // 6. A backlog of large records is collected a few at a time, so the transport copies stay
    //    within SPOOL_COLLECT_BYTES of internal RAM; small records still come 64 to a batch.
    assert(spool_init(1024));
    for (int i = 0; i < 8; i++) assert(spool_append(large, sizeof large));
    spool_frame_t batch[DRAIN_BATCH];
    size_t got = spool_collect(0, batch, DRAIN_BATCH);
    assert(got == SPOOL_COLLECT_BYTES / sizeof large && got >= 1);
    for (size_t i = 0; i < got; i++) free(batch[i].data);
    spool_ack(8);
    for (int i = 0; i < 100; i++) assert(spool_append(small, sizeof small));
    got = spool_collect(8, batch, DRAIN_BATCH);
    assert(got == DRAIN_BATCH);
    for (size_t i = 0; i < got; i++) free(batch[i].data);

    printf("sbf_session_test: OK\n");
    return 0;
}
