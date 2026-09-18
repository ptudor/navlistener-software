// The GNF1 evidence exchange in the real handshake (docs/COMMISSIONING.md §6): a feeder that
// holds evidence announces it in the HELLO and sends one EVIDENCE frame straight after it,
// before it reads anything; the collector's verdict comes back in the WELCOME. A feeder with
// nothing to present must send exactly the bytes it always did, because the collector reads
// an extra frame only when the flag is set — a flag without a frame, or a frame without a
// flag, desynchronizes the handshake.
#define _POSIX_C_SOURCE 200809L
#include <assert.h>
#include <stdio.h>
#include <string.h>
#include "idf_stubs.h"
static int test_export(mbedtls_ssl_context *, uint8_t *, size_t, const char *, size_t, const unsigned char *, size_t, int);
#define mbedtls_ssl_export_keying_material test_export
#define select test_select
#include "../src/pusher.c"
#undef select
#undef mbedtls_ssl_export_keying_material

static esp_tls_t transport;
static uint8_t written[4096], inbound[512];
static size_t written_len, head, tail, written_at_first_read;
static bool export_fails, first_read_seen;
static int exports;
static char export_label[64];

static const uint8_t PAYLOAD[] = { 0x01, 0x00, 0x03, 'r', 'e', 'c', 0x00, 0x00, 0x00, 0x00 };
static size_t payload_len;       // what the evidence callback returns
static bool oversize;            // the callback claims more than the frame may carry
static const uint8_t *seen_exported;
static uint8_t exported_copy[GNF1_EVIDENCE_EXPORTED_SIZE];
static int evidence_calls, verdict_calls;
static char verdict_trust[32], verdict_error[32];
static bool verdict_trust_null, verdict_error_null;

static int test_export(mbedtls_ssl_context *ssl, uint8_t *out, size_t n, const char *label, size_t label_len,
                       const unsigned char *context, size_t context_len, int use_context)
{
    assert(ssl == (void *)&transport && n == GNF1_EVIDENCE_EXPORTED_SIZE && !context && !context_len && !use_context);
    assert(label_len < sizeof export_label);
    memcpy(export_label, label, label_len); export_label[label_len] = 0;
    exports++;
    if (export_fails) return -1;
    for (size_t i = 0; i < n; i++) out[i] = (uint8_t)(0x40 + i);
    return 0;
}

static size_t evidence_stub(const uint8_t *exported, uint8_t *out, size_t cap)
{
    evidence_calls++;
    assert(cap == GNF1_EVIDENCE_MAX);
    seen_exported = exported;
    if (exported) memcpy(exported_copy, exported, sizeof exported_copy);
    if (payload_len) memcpy(out, PAYLOAD, payload_len);
    return oversize ? GNF1_EVIDENCE_MAX + 1 : payload_len;
}
static void verdict_stub(const char *trust, const char *error)
{
    verdict_calls++;
    verdict_trust_null = !trust; verdict_error_null = !error;
    snprintf(verdict_trust, sizeof verdict_trust, "%s", trust ? trust : "");
    snprintf(verdict_error, sizeof verdict_error, "%s", error ? error : "");
}

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
    return (ssize_t)n;
}
ssize_t esp_tls_conn_read(esp_tls_t *t, void *p, size_t n)
{
    (void)t;
    if (!first_read_seen) { first_read_seen = true; written_at_first_read = written_len; }
    if (head == tail) return -1;
    if (n > tail - head) n = tail - head;
    memcpy(p, inbound + head, n);
    head += n;
    return (ssize_t)n;
}

static int run(const char *welcome)
{
    written_len = head = tail = written_at_first_read = 0;
    first_read_seen = false;
    exports = evidence_calls = verdict_calls = 0;
    seen_exported = NULL;
    gnf1_frame_header(inbound, GNF1_F_WELCOME, (uint32_t)strlen(welcome));
    memcpy(inbound + GNF1_FRAME_HDR, welcome, strlen(welcome));
    tail = GNF1_FRAME_HDR + strlen(welcome);
    return handshake(&transport);
}

static bool contains(const uint8_t *body, size_t len, const char *text)
{
    size_t n = strlen(text);
    for (size_t i = 0; i + n <= len; i++) if (!memcmp(body + i, text, n)) return true;
    return false;
}

// frame returns the payload of the index-th frame after the 4-byte magic, or NULL.
static const uint8_t *frame(unsigned index, uint8_t *type, size_t *len)
{
    size_t at = 4;
    for (unsigned i = 0; at + GNF1_FRAME_HDR <= written_len; i++) {
        size_t n = gnf1_rd_be32(written + at + 1);
        if (i == index) { *type = written[at]; *len = n; return written + at + GNF1_FRAME_HDR; }
        at += GNF1_FRAME_HDR + n;
    }
    return NULL;
}

int main(void)
{
    uint8_t type; size_t len; const uint8_t *body;
    s_cfg = (pusher_cfg_t){ .host = "collector.invalid", .port = 5580, .token = "t", .station = "s",
                            .feed = "ubx", .session = "boot-1" };

    // 1. No evidence source: the historical handshake, byte for byte.
    assert(run("{\"ok\":true}") == 0 && !exports && !evidence_calls);
    body = frame(0, &type, &len);
    assert(body && type == GNF1_F_HELLO && !contains(body, len, "evidence") && !frame(1, &type, &len));

    // 2. Evidence is announced, sent straight after the HELLO and before the first read, and
    //    built from this session's exported keying material under the GNF1 label.
    s_cfg.evidence = evidence_stub; s_cfg.hardware_trust = verdict_stub;
    payload_len = sizeof PAYLOAD;
    assert(run("{\"ok\":true,\"durable_ack\":true,\"hardware_trust\":\"trusted\"}") == 0);
    assert(exports == 1 && evidence_calls == 1 && !strcmp(export_label, GNF1_EVIDENCE_EXPORTER_LABEL));
    assert(seen_exported && exported_copy[0] == 0x40 && exported_copy[31] == 0x40 + 31);
    body = frame(0, &type, &len);
    assert(body && type == GNF1_F_HELLO && contains(body, len, ",\"evidence\":true}"));
    body = frame(1, &type, &len);
    assert(body && type == GNF1_F_EVIDENCE && len == sizeof PAYLOAD && !memcmp(body, PAYLOAD, len));
    assert(!frame(2, &type, &len) && written_at_first_read == written_len);
    assert(verdict_calls == 1 && !strcmp(verdict_trust, "trusted") && verdict_error_null);
    assert(atomic_load(&s_durable));

    // 3. A rejection is reported with its reason and does not fail the handshake.
    assert(run("{\"ok\":true,\"hardware_trust\":\"none\",\"evidence_error\":\"proof\"}") == 0);
    assert(verdict_calls == 1 && !strcmp(verdict_trust, "none") && !strcmp(verdict_error, "proof"));

    // 4. A collector that predates the exchange answers without a verdict.
    assert(run("{\"ok\":true}") == 0 && verdict_calls == 1 && verdict_trust_null && verdict_error_null);

    // 5. No exported keying material: the source is told, and when it then presents nothing
    //    the HELLO carries no flag, no frame follows and no verdict is expected.
    export_fails = true; payload_len = 0;
    assert(run("{\"ok\":true,\"hardware_trust\":\"none\"}") == 0);
    assert(exports == 1 && evidence_calls == 1 && !seen_exported && !verdict_calls);
    body = frame(0, &type, &len);
    assert(body && !contains(body, len, "evidence") && !frame(1, &type, &len));

    // 6. A source may still present a record alone (an open or test board) without it.
    payload_len = 7;
    assert(run("{\"ok\":true,\"hardware_trust\":\"open\"}") == 0 && !seen_exported);
    body = frame(1, &type, &len);
    assert(body && type == GNF1_F_EVIDENCE && len == 7 && verdict_calls == 1 && !strcmp(verdict_trust, "open"));

    // 7. A rejected handshake reports no verdict.
    assert(run("{\"ok\":false,\"error\":\"unauthorized\"}") == -2 && !verdict_calls);

    // 8. A length the frame may not carry is treated as no evidence: no flag, no frame.
    payload_len = sizeof PAYLOAD; export_fails = false; oversize = true;
    assert(run("{\"ok\":true,\"hardware_trust\":\"trusted\"}") == 0 && evidence_calls == 1 && !verdict_calls);
    body = frame(0, &type, &len);
    assert(body && !contains(body, len, "evidence") && !frame(1, &type, &len));
    puts("GNF1 evidence is announced, sent after HELLO before any read, and its verdict reported");
    return 0;
}
