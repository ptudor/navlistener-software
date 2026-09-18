// pusher — GNF1/TLS push consumer. See include/pusher.h.
// Mirrors navfeeder.c serve_collector(), single-task (select-gated ACK reads).

#include "pusher.h"
#include "ack_progress.h"
#include "update_json.h"
#include "../../../../common/endpoint_fallback.h"

#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/select.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_log.h"
#include "esp_random.h"
#include "esp_tls.h"
#include "esp_crt_bundle.h"
#include "esp_timer.h"
#include "mbedtls/ssl.h"

#include "gnf1.h"
#include "spool.h"

static const char *TAG = "pusher";

#define DRAIN_BATCH   64
#define KEEPALIVE_S   30   // PING when idle this long (under the collector's idle timeout)
#define BACKOFF_MAX_S 30
// regression fix (ESP32 half): a post-handshake session must run this long to count as
// "useful" and reset the reconnect backoff ladder. Mirrors navfeeder.c's USEFUL_CONN_S
// and the collector's usefulConnectionDuration : without the reset,
// backoff climbs monotonically to BACKOFF_MAX_S over the process lifetime, so a routine
// collector redeploy weeks into uptime waits the full 30 s on every future reconnect.
#define USEFUL_CONN_S 3

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
#define NVF_TCP_KEEP_COUNT     3

// The Kconfig bool is undefined (not 0) when off; give the preprocessor a value
// so #if works under -Wundef, mirroring netcfg.c.
#ifndef CONFIG_NVF_INSECURE
#define CONFIG_NVF_INSECURE 0
#endif

static pusher_cfg_t s_cfg;   // owned copy (strings duplicated)
static atomic_bool s_connected;
static atomic_bool s_via_tunnel;
// A WireGuard session can be healthy while the collector's private TCP/TLS
// listener is unavailable. Give the public endpoint a useful interval before
// trying that path again. Accessed only by the pusher task.
static int64_t s_tunnel_retry_us;
#define TUNNEL_RETRY_US (60LL * 1000000)
static atomic_bool s_durable;

bool pusher_connected(void) { return atomic_load_explicit(&s_connected, memory_order_relaxed); }
bool pusher_via_tunnel(void) { return atomic_load_explicit(&s_via_tunnel, memory_order_relaxed); }

// tunnel_preferred reports whether the next connection should run inside the WireGuard
// tunnel: one is configured and its peer session is valid right now. Cheap; consulted per
// connection attempt and once per idle turn of a direct session.
static bool tunnel_preferred(void)
{
    return s_cfg.tunnel_host && s_cfg.tunnel_up && s_cfg.tunnel_up() &&
           esp_timer_get_time() >= s_tunnel_retry_us;
}
bool pusher_durable_connected(void) { return pusher_connected() && atomic_load(&s_durable); }

// pusher_cfg_free releases the owned config copies and zeroes s_cfg. The struct
// fields are const char * (the caller's view is borrowed/immutable), so the owned
// duplicates are cast back to the mutable pointers dup_or_null returned;
// free(NULL) is safe for the never-populated ones.
static void pusher_cfg_free(void)
{
    free((char *)s_cfg.host);
    free((char *)s_cfg.token);
    free((char *)s_cfg.station);
    free((char *)s_cfg.feed);
    free((char *)s_cfg.session);
    free((char *)s_cfg.ca_pem);
    free((char *)s_cfg.tunnel_host);
    memset(&s_cfg, 0, sizeof s_cfg);
}

// --- TLS frame I/O -----------------------------------------------------------------------

// TLS_WANT_WAIT_MS bounds one readiness wait. It is short enough that the
// no-progress deadline below still trips at OP_DEADLINE_S, and long enough that a
// flow-controlled or half-open connection costs a few hundred wakeups over that
// window instead of millions of spins.
#define TLS_WANT_WAIT_MS 10

// tls_wait_ready blocks until the connection's socket is ready in the direction
// esp-tls asked for, or until TLS_WANT_WAIT_MS elapses.
//
// both I/O loops used to `continue` immediately on
// WANT_READ/WANT_WRITE, spinning at full speed for as long as OP_DEADLINE_S. On
// the single-core C6 the priority-6 pusher then owned the core between
// higher-priority interrupts, starving provisioning, display and housekeeping
// work and burning power on a connection making no progress. The producer's
// higher priority protected the producer, not the rest of the firmware.
//
// The direction comes from the WANT code, not from which loop we are in: TLS
// renegotiation makes a write need readability and a read need writability.
// A select() error is not fatal here — the caller re-issues the operation and its
// own deadline decides when the link is dead — but it must still yield, so the
// tick delay is the fallback whenever readiness cannot be waited on.
static void tls_wait_ready(esp_tls_t *tls, int want_write)
{
    int fd = -1;
    if (esp_tls_get_conn_sockfd(tls, &fd) == ESP_OK && fd >= 0 && fd < FD_SETSIZE) {
        fd_set ready;
        FD_ZERO(&ready);
        FD_SET(fd, &ready);
        struct timeval tv = { .tv_sec = 0, .tv_usec = TLS_WANT_WAIT_MS * 1000 };
        if (select(fd + 1, want_write ? NULL : &ready, want_write ? &ready : NULL, NULL, &tv) >= 0) {
            return;
        }
    }
    TickType_t ticks = pdMS_TO_TICKS(TLS_WANT_WAIT_MS);
    vTaskDelay(ticks ? ticks : 1); // never 0: a 0-tick delay does not yield
}

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
            tls_wait_ready(tls, w == ESP_TLS_ERR_SSL_WANT_WRITE); // always yield
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
            tls_wait_ready(tls, r == ESP_TLS_ERR_SSL_WANT_WRITE); // always yield
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
    if (fd < 0) {
        // regression fix defense-in-depth: serve() rejects a connection whose fd it cannot get,
        // so this branch is unreachable today. If it ever regresses, burn the caller's
        // timeout as a real delay instead of returning instantly — an immediate false
        // here turns serve()'s idle loop into a hot spin on an unwatched task.
        if (timeout_ms > 0) vTaskDelay(pdMS_TO_TICKS(timeout_ms));
        return false;
    }
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
    // Bound each drain so a stream of unchanged ACKs/PONGs cannot starve the
    // durability watchdog in the outer loop.
    for (unsigned n = 0; n < DRAIN_BATCH && readable(tls, fd, 0); n++) {
        uint8_t type;
        size_t len;
        if (read_frame(tls, &type, buf, sizeof buf, &len) != 0) return false;
        if (type == GNF1_F_ACK) {
            uint64_t seq;
            if (gnf1_decode_ack(buf, len, &seq)) spool_ack(seq);
        } else if (type == 0x09 && s_cfg.update_control) {
            s_cfg.update_control(buf,len);
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
    .keep_alive_count = NVF_TCP_KEEP_COUNT,
};

// connect_collector opens TLS to host. verify, when non-NULL, is the name the certificate
// must carry (and the SNI sent) instead of host: through the tunnel the TCP peer is an
// address inside it, while the collector's certificate still names its public hostname.
static esp_tls_t *connect_collector(const char *host, const char *verify)
{
    esp_tls_cfg_t tls_cfg = {
.tls_version = ESP_TLS_VER_TLS_1_2, // pin (regression fix / collector floor)
        .timeout_ms = 10000,
.keep_alive_cfg = (tls_keep_alive_cfg_t *)&s_keep_alive_cfg, // regression fix
        .common_name = verify,
    };
    if (s_cfg.ca_pem) {
        tls_cfg.cacert_buf = (const unsigned char *)s_cfg.ca_pem;
        tls_cfg.cacert_bytes = strlen(s_cfg.ca_pem) + 1;
    } else {
#if CONFIG_NVF_INSECURE
        // insecure: neither CA nor bundle -> the collector is not authenticated (dev
        // only). this only actually skips verification (instead of esp-tls
        // hard-failing the connection with neither option set) because NVF_INSECURE's
        // Kconfig selects CONFIG_ESP_TLS_INSECURE +
        // CONFIG_ESP_TLS_SKIP_SERVER_CERT_VERIFY (Kconfig.projbuild).
        if (!s_cfg.insecure) {
            tls_cfg.crt_bundle_attach = esp_crt_bundle_attach;
        }
#else
        // regression fix follow-up: on a production build the skip-verify support is not
        // compiled in, so honoring a stored insecure=1 (e.g. written by an old
        // portal build's checkbox) would make esp-tls hard-fail every connect —
        // an unrecoverable reconnect-loop brick until an NVS erase. Ignore the
        // flag, verify with the bundle, and say so.
        if (s_cfg.insecure) {
            ESP_LOGW(TAG, "ignoring stored insecure=1: built without NVF_INSECURE; verifying the collector against the certificate bundle");
        }
        tls_cfg.crt_bundle_attach = esp_crt_bundle_attach;
#endif
    }

    esp_tls_t *tls = esp_tls_init();
    if (!tls) return NULL;
    int r = esp_tls_conn_new_sync(host, (int)strlen(host), s_cfg.port, &tls_cfg, tls);
    if (r != 1) {
        esp_tls_conn_destroy(tls);
        return NULL;
    }
    return tls;
}

// session_keying_material exports the value a session proof signs. It exists only inside
// this TLS session and is never sent; the collector derives the same bytes on its side. A
// TLS 1.2 session exports it only with the extended master secret, which mbedTLS offers and
// the collector requires.
static bool session_keying_material(esp_tls_t *tls, uint8_t out[GNF1_EVIDENCE_EXPORTED_SIZE])
{
#if defined(MBEDTLS_SSL_KEYING_MATERIAL_EXPORT)
    mbedtls_ssl_context *ssl = esp_tls_get_ssl_context(tls);
    return ssl && mbedtls_ssl_export_keying_material(ssl, out, GNF1_EVIDENCE_EXPORTED_SIZE,
               GNF1_EVIDENCE_EXPORTER_LABEL, sizeof GNF1_EVIDENCE_EXPORTER_LABEL - 1, NULL, 0, 0) == 0;
#else
    (void)tls; (void)out;
    return false; // built without CONFIG_MBEDTLS_SSL_KEYING_MATERIAL_EXPORT
#endif
}

// handshake sends MAGIC + HELLO (+ EVIDENCE) and checks the WELCOME. Returns 0 on accept,
// -1 on error, -2 on an explicit rejection (back off hard, like navfeeder).
static int handshake(esp_tls_t *tls)
{
    if (tls_write_all(tls, (const uint8_t *)GNF1_MAGIC, 4) != 0) return -1;

    // The evidence is built first because the HELLO has to announce it: the collector reads
    // an EVIDENCE frame exactly when the flag says one follows. Heap, not this task's stack.
    uint8_t *evidence = NULL;
    size_t evidence_len = 0;
    if (s_cfg.evidence && (evidence = malloc(GNF1_EVIDENCE_MAX)) != NULL) {
        uint8_t exported[GNF1_EVIDENCE_EXPORTED_SIZE];
        bool have = session_keying_material(tls, exported);
        evidence_len = s_cfg.evidence(have ? exported : NULL, evidence, GNF1_EVIDENCE_MAX);
        memset(exported, 0, sizeof exported);
        if (evidence_len > GNF1_EVIDENCE_MAX) evidence_len = 0;
    }

    char hello[512];
    // s_cfg.session is validated once in pusher_start, so the only way this build can fail is
    // truncation from an over-long token/station (regression fix adds ~45 bytes to the HELLO).
    int hn = gnf1_build_hello(hello, sizeof hello, s_cfg.token, s_cfg.station, s_cfg.feed,
                              s_cfg.session, false, evidence_len > 0);
    if (hn < 0) {
        ESP_LOGE(TAG, "could not build HELLO (token/station too long?); dropping connection");
        free(evidence);
        return -1;
    }
    uint8_t hdr[GNF1_FRAME_HDR];
    gnf1_frame_header(hdr, GNF1_F_HELLO, (uint32_t)hn);
    int sent = tls_write_all(tls, hdr, sizeof hdr);
    if (sent == 0) sent = tls_write_all(tls, (const uint8_t *)hello, (size_t)hn);
    if (sent == 0 && evidence_len) {
        // Sent straight after the HELLO, without waiting: the collector authenticates the
        // HELLO and only then reads this frame, so an unauthenticated peer costs it nothing.
        gnf1_frame_header(hdr, GNF1_F_EVIDENCE, (uint32_t)evidence_len);
        sent = tls_write_all(tls, hdr, sizeof hdr);
        if (sent == 0) sent = tls_write_all(tls, evidence, evidence_len);
    }
    free(evidence);
    if (sent != 0) return -1;

    uint8_t buf[512];
    uint8_t type;
    size_t len;
    if (read_frame(tls, &type, buf, sizeof buf - 1, &len) != 0) return -1;
    if (type != GNF1_F_WELCOME) return -1;
    buf[len] = 0;
    uj_doc welcome;
    if (!uj_parse(&welcome,(const char *)buf,len)) return -1;
    const uj_node *accepted=uj_get(&welcome,uj_root(&welcome),"ok");
    const uj_node *durable=uj_get(&welcome,uj_root(&welcome),"durable_ack");
    bool ok=accepted && accepted->kind==UJ_BOOL && accepted->number==1;
    atomic_store(&s_durable,ok && durable && durable->kind==UJ_BOOL && durable->number==1);
    // The collector's verdict on the evidence is informational: it labels the data this
    // session sends and never decides whether the session continues.
    if (ok && evidence_len && s_cfg.hardware_trust)
        s_cfg.hardware_trust(uj_string(uj_get(&welcome,uj_root(&welcome),"hardware_trust")),
                             uj_string(uj_get(&welcome,uj_root(&welcome),"evidence_error")));
    uj_free(&welcome);
    if (!ok) {
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

// serve drains the spool over one connection until it drops. Returns 0 when the session
// authenticated and then ran for at least USEFUL_CONN_S (a "useful" session — pusher_task
// resets the backoff ladder, regression fix), -2 on an auth rejection (back off hard), -1
// otherwise. The usefulness clock deliberately starts AFTER the handshake: a slow-FAILING
// connect (connect_collector's timeout_ms is 10 s, over USEFUL_CONN_S) must never count as
// useful, or an unreachable collector would reset backoff on every attempt and the ladder
// would never grow.
static int serve(const char *host, const char *verify, bool via_tunnel)
{
    esp_tls_t *tls = connect_collector(host, verify);
    if (!tls) {
        ESP_LOGW(TAG, "TLS connect to %s:%d%s failed", host, s_cfg.port,
                 via_tunnel ? " (tunnel)" : "");
        return -1;
    }
    int hs = handshake(tls);
    if (hs != 0) {
        esp_tls_conn_destroy(tls);
        return hs;
    }
    int64_t session_start_us = esp_timer_get_time(); // monotonic since boot

    int fd = -1;
    esp_err_t fdrc = esp_tls_get_conn_sockfd(tls, &fd);
    if (fdrc != ESP_OK || fd < 0) {
        // without a valid fd, readable() cannot actually wait (select needs the
        // fd), so the idle loop would degenerate into a 100% busy-spin on a task that is
        // deliberately NOT task-WDT-subscribed — a single-core C6 burning hot
        // indefinitely with no watchdog backstop and no self-heal. A connection we cannot
        // poll is a failed connection: tear it down and let the backoff loop reconnect.
        ESP_LOGW(TAG, "esp_tls_get_conn_sockfd failed (err=%d fd=%d); dropping connection",
                 (int)fdrc, fd);
        esp_tls_conn_destroy(tls);
        return -1;
    }
    atomic_store_explicit(&s_via_tunnel, via_tunnel, memory_order_relaxed);
    atomic_store_explicit(&s_connected, true, memory_order_relaxed);
    ESP_LOGI(TAG, "connected: station=%s feed=%s -> %s:%d%s", s_cfg.station, s_cfg.feed,
             host, s_cfg.port, via_tunnel ? " via tunnel" : "");

    uint64_t sent_upto = spool_acked(); // replay-on-reconnect: resume from the last ack
    spool_frame_t batch[DRAIN_BATCH];
    uint8_t frame[GNF1_DATA_MAX];
    int64_t last_tx_us = esp_timer_get_time();
    ack_progress_t progress = { .acked = sent_upto, .since_us = last_tx_us };

    for (;;) {
        if (!drain_acks(tls, fd)) break;
        // healthy writes/PONGs are not historian progress. The C6's
        // default RAM ring holds about 100 s of traffic, so recover stalled
        // durability after 30 s rather than the larger C feeder's ten minutes.
        // Reconnect preserves s_cfg.session and replays from spool_acked().
        if (ack_progress_stalled(&progress, spool_acked(), sent_upto, esp_timer_get_time())) {
            ESP_LOGW(TAG, "durable ACK stalled with records outstanding; reconnecting for replay");
            break;
        }

        size_t n = spool_collect(sent_upto, batch, DRAIN_BATCH);
        if (n == 0) {
            // Prefer the tunnel: a direct session that began while the peer was down moves
            // over once the tunnel is up and nothing sent still awaits its ACK. The
            // reconnect replays from spool_acked() as always, so nothing is lost, and the
            // session counts as useful only if it ran long enough (return contract above).
            if (!via_tunnel && tunnel_preferred() && sent_upto == spool_acked() &&
                esp_timer_get_time() - session_start_us >= (int64_t)USEFUL_CONN_S * 1000000) {
                ESP_LOGI(TAG, "tunnel is up; moving the session onto it");
                break;
            }
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

    atomic_store_explicit(&s_connected, false, memory_order_relaxed);
    atomic_store_explicit(&s_via_tunnel, false, memory_order_relaxed);
    atomic_store(&s_durable,false);
    esp_tls_conn_destroy(tls);
    uint64_t dropped = 0;
    size_t count = 0;
    spool_stats(NULL, &dropped, &count);
    ESP_LOGI(TAG, "disconnected (spooled=%u dropped=%llu)", (unsigned)count,
             (unsigned long long)dropped);
    return (esp_timer_get_time() - session_start_us >= (int64_t)USEFUL_CONN_S * 1000000) ? 0 : -1;
}

static void pusher_task(void *arg)
{
    (void)arg;
    int backoff = 1;
    char secondary[64];
    nav_endpoint_secondary(s_cfg.host, secondary, sizeof secondary);
    const char *host = s_cfg.host;
    for (;;) {
        // The WireGuard path is taken whenever its peer session is valid; TLS then verifies
        // the configured collector name against the certificate exactly as it does
        // directly. A down tunnel costs nothing: the public endpoint carries on.
        bool via_tunnel = tunnel_preferred();
        int rc = via_tunnel ? serve(s_cfg.tunnel_host, s_cfg.host, true)
                            : serve(host, NULL, false);
        if (via_tunnel && rc == -1)
            s_tunnel_retry_us = esp_timer_get_time() + TUNNEL_RETRY_US;
        // A new TLS connection verifies the selected hostname. Keep the same
        // session, credentials and durable ACK watermark when changing aliases.
        // Explicit authorization rejection retains the normal hard backoff. A tunnel
        // outcome says nothing about the public aliases, so it leaves that choice alone.
        if (rc != -2 && !via_tunnel)
            host = rc != 0 && host == s_cfg.host && secondary[0] ? secondary : s_cfg.host;
        // a useful session (authenticated + streamed >= USEFUL_CONN_S, see
        // serve()'s return contract) resets the ladder so a routine collector redeploy
        // weeks into uptime reconnects in ~1 s instead of the 30 s cap. Mirrors
        // navfeeder.c main's regression fix reset; failed/slow connects never reset (rc == -1).
        if (rc == 0) backoff = 1;
        int wait = (rc == -2) ? BACKOFF_MAX_S : backoff; // hard back-off on auth rejection
        // 0..25% jitter — a collector redeploy fails the whole fleet at the
        // same instant, and identical deterministic ladders would then re-attempt TLS
        // handshakes (the most expensive per-connection event on both ends) in
        // synchronized bursts. Mirrors navfeeder.c's sleep_with_jitter.
        int wait_ms = wait * 1000;
        wait_ms += (int)(esp_random() % ((unsigned)wait_ms / 4u + 1u));
        ESP_LOGI(TAG, "reconnecting in %d ms", wait_ms);
        vTaskDelay(pdMS_TO_TICKS(wait_ms));
        if (rc == -2) backoff = 1;
        else if ((backoff *= 2) > BACKOFF_MAX_S) backoff = BACKOFF_MAX_S;
    }
}

static char *dup_or_null(const char *s) { return s ? strdup(s) : NULL; }

// pusher_str_ok reports whether duplicating one config string succeeded :
// dup_or_null legitimately returns NULL for a NULL input (no value configured at all), but
// a non-NULL input turning into a NULL output means strdup failed under memory pressure --
// that must not be silently treated the same as "no value configured," which previously let
// connect_collector() go on to do strlen(s_cfg.host) on a NULL pointer.
static bool pusher_str_ok(const char *in, const char *out) { return !in || out; }

bool pusher_start(const pusher_cfg_t *cfg)
{
    // refuse to start without a usable session identity. Unreachable by
    // construction (app_main mints one before it builds this config), but the failure it
    // guards is deliberately loud rather than silent: an absent or malformed session makes
    // every single HELLO unbuildable/rejected, which would otherwise present as an endless
    // "reconnecting in N ms" loop with no stated cause.
    if (!gnf1_session_valid(cfg->session)) {
        ESP_LOGE(TAG, "pusher_start: missing or invalid GNF1 session identity; pusher not started");
        return false;
    }
    s_cfg = *cfg;
    // Duplicate the strings so the caller's buffers need not outlive us.
    s_cfg.host = dup_or_null(cfg->host);
    s_cfg.token = dup_or_null(cfg->token);
    s_cfg.station = dup_or_null(cfg->station);
    s_cfg.feed = dup_or_null(cfg->feed ? cfg->feed : "ubx");
    s_cfg.session = dup_or_null(cfg->session);
    s_cfg.ca_pem = dup_or_null(cfg->ca_pem);
    s_cfg.tunnel_host = dup_or_null(cfg->tunnel_host);
    if (!pusher_str_ok(cfg->host, s_cfg.host) ||
        !pusher_str_ok(cfg->tunnel_host, s_cfg.tunnel_host) ||
        !pusher_str_ok(cfg->token, s_cfg.token) ||
        !pusher_str_ok(cfg->station, s_cfg.station) ||
        !s_cfg.feed || // feed's input is never NULL (falls back to "ubx"), so its dup must succeed
        !s_cfg.session || // validated non-NULL above, so its dup must succeed too
        !pusher_str_ok(cfg->ca_pem, s_cfg.ca_pem)) {
        ESP_LOGE(TAG, "pusher_start: out of memory duplicating config strings; pusher not started");
        pusher_cfg_free();
        return false;
    }
    if (xTaskCreate(pusher_task, "pusher", 8192, NULL, 6, NULL) != pdPASS) {
        // regression fix follow-up: a failed task create must not leak the owned copies or
        // leave s_cfg half-populated for a later retry to double-free.
        ESP_LOGE(TAG, "pusher_start: task create failed; pusher not started");
        pusher_cfg_free();
        return false;
    }
    return true;
}
