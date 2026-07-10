// pusher — GNF1/TLS push consumer. See include/pusher.h.
// Mirrors navfeeder.c serve_collector(), single-task (select-gated ACK reads).

#include "pusher.h"

#include <string.h>
#include <sys/socket.h>
#include <sys/select.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_log.h"
#include "esp_tls.h"
#include "esp_crt_bundle.h"
#include "esp_timer.h"

#include "gnf1.h"
#include "spool.h"

static const char *TAG = "pusher";

#define DRAIN_BATCH   64
#define KEEPALIVE_S   30   // PING when idle this long (under the collector's idle timeout)
#define BACKOFF_MAX_S 30

// esp-tls maps a socket timeout to WANT_READ/WANT_WRITE; on a half-open link (no
// RST ever arrives) tls_write_all/tls_read_full retried that forever with no bound. Fail
// the operation -- and so the whole connection, via the normal error-return path -- after
// this long without any actual progress (a partial read/write resets the clock).
#define OP_DEADLINE_S 30

// TCP-level keepalive (distinct from the GNF1 PING above, which is application-level and
// requires the peer to be GNF1-aware): detects a link the OS itself can determine is dead
// (no ACK to repeated probes) even when nothing at the TLS layer is trying to talk, so a
// silently vanished collector is found without waiting for OP_DEADLINE_S to elapse on some
// future write.
#define TCP_KEEPIDLE_S  10
#define TCP_KEEPINTVL_S 5
#define TCP_KEEPCNT     3

static pusher_cfg_t s_cfg;   // owned copy (strings duplicated)
static volatile bool s_connected;

bool pusher_connected(void) { return s_connected; }

// --- TLS frame I/O -----------------------------------------------------------------------

static int tls_write_all(esp_tls_t *tls, const uint8_t *buf, size_t n)
{
    size_t off = 0;
    int64_t deadline_start_us = esp_timer_get_time();
    while (off < n) {
        ssize_t w = esp_tls_conn_write(tls, buf + off, n - off);
        if (w == ESP_TLS_ERR_SSL_WANT_WRITE || w == ESP_TLS_ERR_SSL_WANT_READ) {
            if (esp_timer_get_time() - deadline_start_us > (int64_t)OP_DEADLINE_S * 1000000) {
                return -1; // no progress for OP_DEADLINE_S -- declare the link dead
            }
            continue;
        }
        if (w <= 0) return -1;
        off += (size_t)w;
        deadline_start_us = esp_timer_get_time(); // progress made; restart the deadline
    }
    return 0;
}

static int tls_read_full(esp_tls_t *tls, uint8_t *buf, size_t n)
{
    size_t off = 0;
    int64_t deadline_start_us = esp_timer_get_time();
    while (off < n) {
        ssize_t r = esp_tls_conn_read(tls, buf + off, n - off);
        if (r == ESP_TLS_ERR_SSL_WANT_READ || r == ESP_TLS_ERR_SSL_WANT_WRITE) {
            if (esp_timer_get_time() - deadline_start_us > (int64_t)OP_DEADLINE_S * 1000000) {
                return -1; // no progress for OP_DEADLINE_S -- declare the link dead
            }
            continue;
        }
        if (r <= 0) return -1;
        off += (size_t)r;
        deadline_start_us = esp_timer_get_time(); // progress made; restart the deadline
    }
    return 0;
}

// read_frame reads one GNF1 frame [type][4B len][payload]. Inbound frames (ACK/PONG/WELCOME)
// are small; a payload over cap is a protocol error.
static int read_frame(esp_tls_t *tls, uint8_t *type, uint8_t *buf, size_t cap, size_t *len)
{
    uint8_t hdr[GNF1_FRAME_HDR];
    if (tls_read_full(tls, hdr, sizeof hdr) != 0) return -1;
    uint32_t n = gnf1_rd_be32(hdr + 1);
    if (n > GNF1_MAX_FRAME || n > cap) return -1;
    if (n && tls_read_full(tls, buf, n) != 0) return -1;
    *type = hdr[0];
    *len = n;
    return 0;
}

// readable reports whether the connection has an inbound frame waiting, without blocking
// longer than timeout_ms. mbedTLS may hold decrypted bytes past what select() sees, so check
// the TLS buffer first.
static bool readable(esp_tls_t *tls, int fd, int timeout_ms)
{
    if (esp_tls_get_bytes_avail(tls) > 0) return true;
    if (fd < 0) return false;
    fd_set rd;
    FD_ZERO(&rd);
    FD_SET(fd, &rd);
    struct timeval tv = { timeout_ms / 1000, (timeout_ms % 1000) * 1000 };
    return select(fd + 1, &rd, NULL, NULL, &tv) > 0;
}

// drain_acks applies every pending ACK (pruning the spool). Returns false on disconnect.
static bool drain_acks(esp_tls_t *tls, int fd)
{
    uint8_t buf[64];
    while (readable(tls, fd, 0)) {
        uint8_t type;
        size_t len;
        if (read_frame(tls, &type, buf, sizeof buf, &len) != 0) return false;
        if (type == GNF1_F_ACK) {
            uint64_t seq;
            if (gnf1_decode_ack(buf, len, &seq)) spool_ack(seq);
        }
        // PONG and anything else: ignore.
    }
    return true;
}

// --- connect + handshake -----------------------------------------------------------------

// static storage -- esp_tls_cfg_t.keep_alive_cfg is a pointer, and while
// esp_tls_conn_new_sync is synchronous (so even a stack lifetime would technically survive
// the call), a static avoids any doubt about esp-tls retaining the pointer past setup.
static const tls_keep_alive_cfg_t s_keep_alive_cfg = {
    .keep_alive_enable = true,
    .keep_alive_idle = TCP_KEEPIDLE_S,
    .keep_alive_interval = TCP_KEEPINTVL_S,
    .keep_alive_count = TCP_KEEPCNT,
};

static esp_tls_t *connect_collector(void)
{
    esp_tls_cfg_t tls_cfg = {
.tls_version = ESP_TLS_VER_TLS_1_2, // pin (regression fix / collector floor)
        .timeout_ms = 10000,
.keep_alive_cfg = (tls_keep_alive_cfg_t *)&s_keep_alive_cfg, // regression fix
    };
    if (s_cfg.ca_pem) {
        tls_cfg.cacert_buf = (const unsigned char *)s_cfg.ca_pem;
        tls_cfg.cacert_bytes = strlen(s_cfg.ca_pem) + 1;
    } else if (!s_cfg.insecure) {
        tls_cfg.crt_bundle_attach = esp_crt_bundle_attach;
    }
    // insecure: neither CA nor bundle -> the collector is not authenticated (dev only).

    esp_tls_t *tls = esp_tls_init();
    if (!tls) return NULL;
    int r = esp_tls_conn_new_sync(s_cfg.host, (int)strlen(s_cfg.host), s_cfg.port, &tls_cfg, tls);
    if (r != 1) {
        esp_tls_conn_destroy(tls);
        return NULL;
    }
    return tls;
}

// handshake sends MAGIC + HELLO and checks the WELCOME. Returns 0 on accept, -1 on error,
// -2 on an explicit rejection (back off hard, like navfeeder).
static int handshake(esp_tls_t *tls)
{
    if (tls_write_all(tls, (const uint8_t *)GNF1_MAGIC, 4) != 0) return -1;

    char hello[512];
    int hn = gnf1_build_hello(hello, sizeof hello, s_cfg.token, s_cfg.station, s_cfg.feed, false);
    if (hn < 0) return -1;
    uint8_t hdr[GNF1_FRAME_HDR];
    gnf1_frame_header(hdr, GNF1_F_HELLO, (uint32_t)hn);
    if (tls_write_all(tls, hdr, sizeof hdr) != 0) return -1;
    if (tls_write_all(tls, (const uint8_t *)hello, (size_t)hn) != 0) return -1;

    uint8_t buf[512];
    uint8_t type;
    size_t len;
    if (read_frame(tls, &type, buf, sizeof buf - 1, &len) != 0) return -1;
    if (type != GNF1_F_WELCOME) return -1;
    buf[len] = 0;
    if (!gnf1_welcome_ok((const char *)buf, len)) {
        ESP_LOGW(TAG, "collector rejected handshake: %.*s", (int)len, (const char *)buf);
        return -2;
    }
    return 0;
}

// --- one connection lifetime -------------------------------------------------------------

static int send_ping(esp_tls_t *tls)
{
    uint8_t f[GNF1_FRAME_HDR];
    gnf1_frame_header(f, GNF1_F_PING, 0);
    return tls_write_all(tls, f, sizeof f);
}

// serve drains the spool over one connection until it drops. Returns -2 on auth rejection,
// -1 otherwise.
static int serve(void)
{
    esp_tls_t *tls = connect_collector();
    if (!tls) {
        ESP_LOGW(TAG, "TLS connect to %s:%d failed", s_cfg.host, s_cfg.port);
        return -1;
    }
    int hs = handshake(tls);
    if (hs != 0) {
        esp_tls_conn_destroy(tls);
        return hs;
    }

    int fd = -1;
    esp_tls_get_conn_sockfd(tls, &fd);
    s_connected = true;
    ESP_LOGI(TAG, "connected: station=%s feed=%s -> %s:%d", s_cfg.station, s_cfg.feed,
             s_cfg.host, s_cfg.port);

    uint64_t sent_upto = spool_acked(); // replay-on-reconnect: resume from the last ack
    spool_frame_t batch[DRAIN_BATCH];
    uint8_t frame[GNF1_DATA_MAX];
    int64_t last_tx_us = esp_timer_get_time();
    int rc = -1;

    for (;;) {
        if (!drain_acks(tls, fd)) break;

        size_t n = spool_collect(sent_upto, batch, DRAIN_BATCH);
        if (n == 0) {
            if (esp_timer_get_time() - last_tx_us >= (int64_t)KEEPALIVE_S * 1000000) {
                if (send_ping(tls) != 0) break;
                last_tx_us = esp_timer_get_time();
            }
            readable(tls, fd, 100); // wait briefly for ACKs / new work
            continue;
        }
        bool ok = true;
        for (size_t i = 0; i < n; i++) {
            if (ok) {
                size_t flen = gnf1_encode_data(frame, batch[i].seq, batch[i].data, batch[i].len);
                if (tls_write_all(tls, frame, flen) == 0) sent_upto = batch[i].seq;
                else ok = false;
            }
            free(batch[i].data);
        }
        last_tx_us = esp_timer_get_time();
        if (!ok) break;
    }

    s_connected = false;
    esp_tls_conn_destroy(tls);
    uint64_t dropped = 0;
    size_t count = 0;
    spool_stats(NULL, &dropped, &count);
    ESP_LOGI(TAG, "disconnected (spooled=%u dropped=%llu)", (unsigned)count,
             (unsigned long long)dropped);
    return rc;
}

static void pusher_task(void *arg)
{
    (void)arg;
    int backoff = 1;
    for (;;) {
        int rc = serve();
        int wait = (rc == -2) ? BACKOFF_MAX_S : backoff; // hard back-off on auth rejection
        ESP_LOGI(TAG, "reconnecting in %ds", wait);
        vTaskDelay(pdMS_TO_TICKS(wait * 1000));
        if (rc == -2) backoff = 1;
        else if ((backoff *= 2) > BACKOFF_MAX_S) backoff = BACKOFF_MAX_S;
    }
}

static char *dup_or_null(const char *s) { return s ? strdup(s) : NULL; }

bool pusher_start(const pusher_cfg_t *cfg)
{
    s_cfg = *cfg;
    // Duplicate the strings so the caller's buffers need not outlive us.
    s_cfg.host = dup_or_null(cfg->host);
    s_cfg.token = dup_or_null(cfg->token);
    s_cfg.station = dup_or_null(cfg->station);
    s_cfg.feed = dup_or_null(cfg->feed ? cfg->feed : "ubx");
    s_cfg.ca_pem = dup_or_null(cfg->ca_pem);
    return xTaskCreate(pusher_task, "pusher", 8192, NULL, 6, NULL) == pdPASS;
}
