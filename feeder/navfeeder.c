/*
 * navfeeder — the navlistener edge feeder (C).
 *
 * A tiny forwarder for the GNSS observer fleet: read raw broadcast nav frames off a
 * local u-blox receiver (UBX-RXM-SFRBX, from a serial device or a TCP bridge) and push
 * each frame to the navlistener collector's authenticated TLS ingest endpoint, framed
 * per ../go/internal/wire (GNF1). It also forwards the RF-front-end and per-SV telemetry
 * the collector's PNT-defense layer needs (UBX-MON-RF/MON-HW/NAV-SAT → GNF1 telemetry
 * records, docs/DEFENSE-PNT.md §1). It does NOT decode — the edge is dumb, all decoding and
 * orbit math (and all threat detection) is central in the collector (docs/DESIGN.md §1,
 * docs/CONSTELLATIONS.md §2).
 * C because the observer fleet is OpenWrt/mips routers and SBCs where a tiny static binary
 * fits and a Go runtime does not — the same shape as radiolistener/feeder/feeder.c, which
 * this is a near-verbatim port of (the resilient spool/ack/replay core is reused verbatim;
 * only the source parser and the on-wire record are GNSS-specific).
 *
 * Store-and-forward: a producer thread reads the receiver and appends each SFRBX frame to a
 * bounded in-memory ring (assigning a monotonic sequence); a consumer drains it to the
 * collector. Reading and sending are decoupled, so a collector restart or network blip does
 * NOT lose data — on every reconnect the consumer REPLAYS all unacked frames (galmon's rule:
 * "the receiver must never go down"). The collector ACKs the highest sequence it has RECEIVED
 * this connection  -- not "last contiguous," and not a durability guarantee that it has
 * actually reached the historian -- pruning the ring up to that seq. On overflow the OLDEST
 * frame spills to a disk spool (--spool-file) rather
 * than being dropped; the spool is recovered and replayed on restart, so an outage longer
 * than RAM, or an ORDERLY router reboot, still loses nothing. Disk-spooled frames are
 * fflush()'d but deliberately not fsync()'d  — a bounded flash-wear trade for the
 * fleet's mips/SBC hardware, not an oversight — so an UNCLEAN power loss can still drop the
 * page-cache tail that hadn't reached disk yet; spool_recover's torn-tail scan handles that
 * cleanly (no corruption, just a shorter replay), it just isn't zero-loss for a power cut the
 * way it is for `reboot`/`poweroff`. With --zstd the feeder→collector DATA
 * stream is zstd-compressed (negotiated in the handshake; ~3–4:1 on the repetitive nav
 * bitstream); ACKs stay plaintext.
 *
 * Wire (must match ../go/internal/wire/wire.go):
 *   stream = "GNF1" then frames [1B type][4B BE len][payload]
 *   HELLO(0x01) {token,station,feed,sw,zstd?} -> WELCOME(0x02){ok,zstd?}
 *   DATA(0x03)  [8B BE seq][record];  ACK(0x04) [8B BE seq]    (DATA zstd-streamed if negotiated)
 *   the DATA record = [8B BE recv_unix_ns][gnssId][svId][sigId][freqId][frame_type][raw…]
 *   raw = the native nav words serialized big-endian (the collector reads them back BE).
 *   A telemetry record (frame_type < 0x10, §6.2) reuses the same DATA frame with a zeroed
 *   gnssId/svId/sigId/freqId and a type-specific body (see emit_monrf/emit_navsat).
 *
 * Still deferred: SBF/RTCM source modes (the fleet is u-blox; the collector's push path wires
 * ubx today), mTLS enrollment tooling, and the ATECC SIGNED_DATA (0x07) hardware tier.
 */
#define _POSIX_C_SOURCE 200809L
#define _DEFAULT_SOURCE   /* glibc: expose usleep() + cfmakeraw() under -std=c11 */
#define _DARWIN_C_SOURCE  /* macOS: expose cfmakeraw() under -std=c11 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdarg.h>
#include <stdatomic.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <time.h>
#include <termios.h>
#include <pthread.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <sys/time.h>
#include <netdb.h>
#include <openssl/ssl.h>
#include <openssl/err.h>
#include <openssl/x509v3.h>
#include <zstd.h>

#define MAGIC "GNF1"
#define F_HELLO 0x01
#define F_WELCOME 0x02
#define F_DATA 0x03
#define F_ACK 0x04
#define F_PING 0x05
#define F_PONG 0x06
#define MAX_FRAME (1u << 20)
#define RECORD_HDR 13         /* recv_ns(8) + gnssId + svId + sigId + freqId + frame_type */
#define MAX_RAW 1024          /* numWords is a u8 → ≤ 1020 raw bytes; round up */
#define GNF_RECORD (RECORD_HDR + MAX_RAW)
#define DRAIN_BATCH 512
#define KEEPALIVE_S 30        /* PING when idle this long, to stay under the collector's idle timeout */
#define USEFUL_CONN_S 3       /* a source connection must survive this long, or emit >=1 frame,
                                * before it resets producer_thread's backoff (else an accept-then-close
                                * peer retries at ~1 Hz forever) */

/* UBX protocol constants (u-blox interface description). SFRBX carries raw nav words
 * per gnssId/sigId (docs/CONSTELLATIONS.md §2.1); MON-RF/MON-HW/NAV-SAT carry the RF-front-end
 * and per-SV telemetry the collector's PNT-defense layer needs (docs/DEFENSE-PNT.md §1). */
#define UBX_SYNC1 0xB5
#define UBX_SYNC2 0x62
#define UBX_CLASS_NAV 0x01
#define UBX_CLASS_RXM 0x02
#define UBX_CLASS_MON 0x0A
#define UBX_ID_SFRBX 0x13
#define UBX_ID_NAVSAT 0x35
#define UBX_ID_MONHW 0x09
#define UBX_ID_MONRF 0x38
#define UBX_MAX_PAYLOAD (1u << 14)

/* GNF1 telemetry record types (docs/CONSTELLATIONS.md §6.2): receiver-side metadata that
 * rides the same DATA stream as raw-nav frames, discriminated by the record's frame_type
 * byte (< 0x10). The body layouts MUST match ../go/internal/ingest/telemetry.go. */
#define F_T_RECEPTION 0x01    /* NAV-SAT per-SV C/N0 + elevation */
#define F_T_JAMMING   0x05    /* MON-RF / MON-HW AGC/jamming/antenna */
#define TELEM_VERSION 1
#define MAX_TELEM_SATS 200    /* fits a ReceptionData body in MAX_RAW (3 + 5*200 = 1003) */

struct opts {
	const char *server_host, *server_port;
	const char *source;    /* /dev/ttyACM0 (serial) or host:port (TCP bridge) */
	int baud;              /* serial baud when --source is a device path */
	const char *token, *station, *feed, *ca;
	const char *cert, *key; /* mTLS client cert + key (PEM); one DNS SAN = the station  */
	const char *spool_file; /* NULL = in-memory only (drop-oldest on overflow) */
	int insecure;
	int zstd; /* request zstd stream compression (the collector must confirm) */
	size_t spool_cap;
	uint64_t disk_max_bytes;
};

/* conn is the write side of a collector connection: a TLS socket, optionally with a zstd
 * compression stream over the feeder→collector (DATA) direction. ACKs back are plaintext
 * and read separately. One conn lives per connection in the consumer thread (no locking). */
struct conn {
	SSL *ssl;
	ZSTD_CCtx *cctx;     /* NULL = plaintext */
	unsigned char *obuf; /* compression output staging */
	size_t obuf_cap;
	uint64_t raw, comp;  /* bytes in / on the wire, for the compression-ratio log */
};

/* spool: a bounded ring of unacked frames, ordered by ascending seq. Each frame is one
 * complete GNF1 DATA record (RECORD_HDR + raw words); the seq is assigned on append and
 * prepended by send_data, exactly as radiolistener spools a line. */
struct frame {
	uint64_t seq;
	uint32_t len;
	unsigned char *data;
};
struct spool {
	struct frame *ring;
	size_t cap, head, count;
	uint64_t seq;     /* last assigned sequence */
	uint64_t acked;   /* last sequence acked by the collector */
	uint64_t dropped; /* frames lost to overflow (no disk, or disk full) */
	/* disk tier (opt-in): on ring overflow, the OLDEST frame spills here instead of
	 * being dropped — so an outage longer than the ring is still lossless. The disk
	 * always holds seqs older than the ring; the consumer drains disk before ring. */
	const char *path;
	FILE *disk_w;
	int disk_append_disabled; /* repair/delete failed: never append past an untrusted boundary */
	uint64_t disk_max_seq, disk_bytes, disk_max_bytes, disk_dropped;
	pthread_mutex_t mu;
};

static struct spool g_spool;
// g_disconnected is written by the reader thread and read by the consumer/drain
// threads. `volatile` is not a C11 synchronization primitive (concurrent unsynchronized
// access is UB); atomic_int gives well-defined cross-thread visibility. Plain =/== on an
// atomic_int are seq-cst atomic operations, so the existing call sites need no change.
static atomic_int g_disconnected;

static void die(const char *m) { fprintf(stderr, "navfeeder: %s\n", m); exit(2); }

static void log_msg(const char *fmt, ...) {
	char ts[32];
	time_t t = time(NULL);
	struct tm tm;
	gmtime_r(&t, &tm);
	strftime(ts, sizeof ts, "%Y-%m-%dT%H:%M:%SZ", &tm);
	// the producer, consumer and reader threads all log; without a lock the three
	// independently-locked stdio calls below can interleave mid-line (a reconnect log spliced
	// into a source-retry log), garbling the logd/journal the deploy relies on. flockfile
	// holds stderr's lock across all three so each line is emitted atomically.
	flockfile(stderr);
	fprintf(stderr, "%s navfeeder: ", ts);
	va_list ap; va_start(ap, fmt); vfprintf(stderr, fmt, ap); va_end(ap);
	fputc('\n', stderr);
	funlockfile(stderr);
}

static uint64_t now_unix_ns(void) {
	struct timespec ts;
	clock_gettime(CLOCK_REALTIME, &ts);
	return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

/* ── byte order ──────────────────────────────────────────────────────────── */

static void be16(unsigned char *b, uint16_t v) { b[0]=v>>8; b[1]=v; }
static void be32(unsigned char *b, uint32_t v) { b[0]=v>>24; b[1]=v>>16; b[2]=v>>8; b[3]=v; }
static uint32_t rd_be32(const unsigned char *b) {
	return ((uint32_t)b[0]<<24)|((uint32_t)b[1]<<16)|((uint32_t)b[2]<<8)|b[3];
}
/* UBX telemetry fields are little-endian on the wire; the collector reads the GNF1 body
 * big-endian (matching ../go/internal/ingest/telemetry.go), so we byte-swap on emit. */
static uint16_t rd_le16(const unsigned char *b) { return (uint16_t)b[0] | ((uint16_t)b[1]<<8); }
static uint32_t rd_le32(const unsigned char *b) {
	return (uint32_t)b[0]|((uint32_t)b[1]<<8)|((uint32_t)b[2]<<16)|((uint32_t)b[3]<<24);
}
static void be64(unsigned char *b, uint64_t v) { for (int i=7;i>=0;i--){ b[i]=v&0xff; v>>=8; } }
static uint64_t rd_be64(const unsigned char *b) {
	uint64_t v=0; for (int i=0;i<8;i++) v=(v<<8)|b[i]; return v;
}

/* ── spool ───────────────────────────────────────────────────────────────── */

/* spool_recover replays a spool file left by a previous run (a feeder restart mid-outage,
 * e.g. a router reboot): it scans the records, truncates any torn tail from an unclean
 * exit, and resumes the sequence so new frames continue past the recovered ones. */
static void spool_recover(struct spool *s) {
	FILE *r = fopen(s->path, "rb");
	if (!r) return; /* no prior spool — first overflow opens it lazily */
	uint64_t max_seq = 0, good_bytes = 0, count = 0;
	for (;;) {
		unsigned char hdr[12];
		if (fread(hdr, 1, 12, r) != 12) break;
		uint64_t seq = rd_be64(hdr);
		uint32_t len = rd_be32(hdr + 8);
		if (len > GNF_RECORD) break;
		if (len) { unsigned char tmp[GNF_RECORD]; if (fread(tmp, 1, len, r) != len) break; }
		max_seq = seq;
		good_bytes += 12 + len;
		count++;
	}
	fclose(r);
	if (count == 0) {
		if (unlink(s->path) != 0 && errno != ENOENT) {
			s->disk_append_disabled = 1;
			log_msg("empty/torn disk spool could not be removed (%s); disk overflow disabled",
				strerror(errno));
		}
		return;
	}
	/* if the tail truncate fails (EROFS/EACCES/EIO), do NOT unlink the whole spool and
	 * restart seq at 0 — that re-enters the seq-reuse regime  where the
	 * collector's (source_id, feeder_seq) ledger discards fresh frames as replays. Instead
	 * resume seq/disk_max_seq/disk_bytes from the recovered good prefix and leave disk_w NULL
	 * (no appends to an unrepairable file), logging loudly. The recovered frames still replay
	 * on connect; only new overflow-to-disk is disabled until the fs is writable again. */
	if (truncate(s->path, (off_t)good_bytes) != 0) {
		s->seq = s->disk_max_seq = max_seq;
		s->disk_bytes = good_bytes;
		s->disk_w = NULL;
		s->disk_append_disabled = 1;
		log_msg("disk spool truncate failed (%s); resuming seq %llu read-only, disk overflow disabled",
			strerror(errno), (unsigned long long)max_seq);
		return;
	}
	s->seq = s->disk_max_seq = max_seq;
	s->disk_bytes = good_bytes;
	s->disk_w = fopen(s->path, "ab");
	log_msg("recovered disk spool: %llu frame(s) up to seq %llu (will replay on connect)",
		(unsigned long long)count, (unsigned long long)max_seq);
}

static void spool_init(struct spool *s, size_t cap, const char *path, uint64_t disk_max_bytes) {
	s->ring = calloc(cap, sizeof *s->ring);
	if (!s->ring) die("out of memory for spool");
	s->cap = cap;
	s->head = s->count = 0;
	s->seq = s->acked = s->dropped = 0;
	s->path = path;
	s->disk_w = NULL;
	s->disk_append_disabled = 0;
	s->disk_max_seq = s->disk_bytes = s->disk_dropped = 0;
	s->disk_max_bytes = disk_max_bytes;
	pthread_mutex_init(&s->mu, NULL);
	if (path) spool_recover(s); /* resume a spool left by a prior run */
}

/* disk_put appends one frame to the disk spool (caller holds the mutex). Each record is
 * fflush()'d into the OS page cache but NOT fsync()'d (regression fix, deliberate): fsync-per-frame
 * on the fleet's flash storage would be a real wear/latency cost for a spill path that is
 * the overflow case, not the common one. This makes the "lossless across reboot" claim in
 * the file header true only for an orderly `reboot`/`poweroff` (page cache flushed on
 * shutdown) — an unclean power loss can drop the not-yet-written-back tail. spool_recover's
 * torn-record scan handles that safely (a partial trailing record is discarded, not
 * misparsed), so this is a bounded durability/wear trade, not a correctness bug. */
/* disk_rollback repairs the spool file back to the last known-good boundary (disk_bytes)
 * after a failed append. ENOSPC/EIO can leave a torn partial record past that
 * boundary; if a later successful append then wrote *after* the tear, the drain scan —
 * which is not self-resynchronizing — would misparse it and stall ALL delivery silently.
 * We close the writer (discarding any buffered bytes), truncate the file to the good
 * boundary, and leave disk_w NULL so the next disk_put reopens "ab" at that clean end.
 * A repair failure is logged rate-limited and the writer stays closed. */
static void disk_rollback(struct spool *s) {
	if (s->disk_w) { fclose(s->disk_w); s->disk_w = NULL; }
	if (truncate(s->path, (off_t)s->disk_bytes) != 0) {
		s->disk_append_disabled = 1;
		static time_t last_warn;
		time_t nowt = time(NULL);
		if (nowt - last_warn >= 60) {
			last_warn = nowt;
			log_msg("disk spool repair (truncate to boundary %llu) failed: %s",
				(unsigned long long)s->disk_bytes, strerror(errno));
		}
	}
}

static void disk_put(struct spool *s, uint64_t seq, const unsigned char *data, uint32_t len) {
	/* regression fix/regression fix verification correction: disk_w == NULL normally means "reopen lazily",
	 * so it cannot also represent a failed repair. Once the on-disk tail/boundary is
	 * untrusted, keep dropping-and-counting overflow until the file is safely deleted (or
	 * the process restarts and recovery succeeds); never append a good record past a tear. */
	if (s->disk_append_disabled) { s->disk_dropped++; return; }
	if (!s->disk_w) {
		s->disk_w = fopen(s->path, "ab");
		if (!s->disk_w) { s->dropped++; return; }
	}
	uint64_t rec = 12 + (uint64_t)len;
	if (s->disk_bytes + rec > s->disk_max_bytes) { s->disk_dropped++; return; }
	unsigned char hdr[12];
	be64(hdr, seq);
	be32(hdr + 8, len);
	/* Transactional append : advance the known-good boundary (disk_bytes/
	 * disk_max_seq) ONLY after the whole record is flushed. fflush's return is now
	 * checked — a buffered-but-unwritten record (ENOSPC on flush) must not poison the
	 * high-water mark. On any fwrite/fflush failure, roll the file back to the last good
	 * boundary so no torn record persists mid-file. */
	if (fwrite(hdr, 1, 12, s->disk_w) != 12 ||
	    (len && fwrite(data, 1, len, s->disk_w) != len) ||
	    fflush(s->disk_w) != 0) {
		s->disk_dropped++;
		disk_rollback(s);
		return;
	}
	s->disk_bytes += rec;
	s->disk_max_seq = seq;
}

/* spool_append copies a record in, assigns the next seq, and on overflow spills the oldest
 * frame to disk (if a spool file is set) or drops it. On a transient allocation failure
 * (regression fix — "the receiver must never go down"), the record is dropped and counted rather
 * than killing the process: malloc is attempted first, before any state mutation, so a
 * failure leaves the ring/seq counter untouched (no phantom seq gap, no wrongful
 * eviction) — mirroring the ESP32 sibling's drop-and-count behavior. */
static uint64_t spool_append(struct spool *s, const unsigned char *data, uint32_t len) {
	pthread_mutex_lock(&s->mu);
	unsigned char *copy = malloc(len);
	if (!copy) {
		s->dropped++;
		pthread_mutex_unlock(&s->mu);
		return 0;
	}
	memcpy(copy, data, len);
	if (s->count == s->cap) {
		struct frame *ev = &s->ring[s->head];
		if (s->path)
			disk_put(s, ev->seq, ev->data, ev->len);
		else
			s->dropped++;
		free(ev->data);
		s->head = (s->head + 1) % s->cap;
		s->count--;
	}
	uint64_t seq = ++s->seq;
	size_t idx = (s->head + s->count) % s->cap;
	s->ring[idx].seq = seq;
	s->ring[idx].len = len;
	s->ring[idx].data = copy;
	s->count++;
	pthread_mutex_unlock(&s->mu);
	return seq;
}

/* spool_ack drops every frame with seq <= n. */
static void spool_ack(struct spool *s, uint64_t n) {
	pthread_mutex_lock(&s->mu);
	/* clamp the ack to the highest assigned sequence — a collector acking beyond what
	 * was sent would otherwise push acked past every future append seq, muting the feeder
	 * until reboot (serve() seeds sent_upto = spool_acked() and collect filters seq <= after). */
	if (n > s->seq) n = s->seq;
	while (s->count > 0 && s->ring[s->head].seq <= n) {
		free(s->ring[s->head].data);
		s->head = (s->head + 1) % s->cap;
		s->count--;
	}
	if (n > s->acked) s->acked = n;
	pthread_mutex_unlock(&s->mu);
}

/* spool_collect copies up to max frames with seq > after into out (caller frees data), and
 * reports the spool's current disk_max_seq under the same lock. Without this, a
 * race exists between disk_drain's own (separately locked) snapshot of disk_max_seq and this
 * call: the producer can evict a ring frame with seq > after to disk in between (advancing
 * disk_max_seq past what disk_drain already saw and observed as "nothing new"), and that
 * frame is then gone from the ring for THIS call to find — the batch silently skips it, the
 * caller advances sent_upto past it anyway, and it is never sent. Reporting disk_max_seq here
 * lets the caller detect that gap and re-run disk_drain (which will see the now-current
 * disk_max_seq) before trusting the ring batch. On a transient allocation failure  it
 * stops and returns the partial batch collected so far rather than dying — the frame that
 * failed to copy, and everything after it in this round, is simply not yet collected; it
 * stays in the ring and is retried the next round. */
static size_t spool_collect(struct spool *s, uint64_t after, struct frame *out, size_t max,
                            uint64_t *disk_max_seq_out) {
	pthread_mutex_lock(&s->mu);
	if (disk_max_seq_out) *disk_max_seq_out = s->disk_max_seq;
	size_t n = 0;
	for (size_t i = 0; i < s->count && n < max; i++) {
		struct frame *f = &s->ring[(s->head + i) % s->cap];
		if (f->seq <= after) continue;
		unsigned char *copy = malloc(f->len);
		if (!copy) break;
		out[n].seq = f->seq;
		out[n].len = f->len;
		out[n].data = copy;
		memcpy(out[n].data, f->data, f->len);
		n++;
	}
	pthread_mutex_unlock(&s->mu);
	return n;
}

static uint64_t spool_acked(struct spool *s) {
	pthread_mutex_lock(&s->mu);
	uint64_t a = s->acked;
	pthread_mutex_unlock(&s->mu);
	return a;
}

/* spool_stats reads the counters under the lock. */
static void spool_stats(struct spool *s, uint64_t *seq, uint64_t *dropped, size_t *count, uint64_t *disk_dropped) {
	pthread_mutex_lock(&s->mu);
	if (seq) *seq = s->seq;
	if (dropped) *dropped = s->dropped;
	if (count) *count = s->count;
	if (disk_dropped) *disk_dropped = s->disk_dropped;
	pthread_mutex_unlock(&s->mu);
}

/* ── net + wire ──────────────────────────────────────────────────────────── */

static int tcp_dial(const char *host, const char *port, int rcv_timeout_s) {
	struct addrinfo hints, *res, *rp;
	memset(&hints, 0, sizeof hints);
	hints.ai_family = AF_UNSPEC;
	hints.ai_socktype = SOCK_STREAM;
	if (getaddrinfo(host, port, &hints, &res) != 0) return -1;
	int fd = -1;
	for (rp = res; rp; rp = rp->ai_next) {
		fd = socket(rp->ai_family, rp->ai_socktype, rp->ai_protocol);
		if (fd < 0) continue;
		if (connect(fd, rp->ai_addr, rp->ai_addrlen) == 0) break;
		close(fd); fd = -1;
	}
	freeaddrinfo(res);
	if (fd >= 0 && rcv_timeout_s > 0) {
		struct timeval tv = { rcv_timeout_s, 0 };
		if (setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv) != 0) {
			/* regression fix verification correction: without the timeout a quiet/wedged TCP
			 * source can block the producer forever, so fail this dial and retry. */
			log_msg("failed to set receive timeout on %s:%s: %s", host, port, strerror(errno));
			close(fd);
			fd = -1;
		}
	}
	return fd;
}

static int ssl_write_all(SSL *ssl, const void *buf, size_t n) {
	const unsigned char *p = buf;
	while (n > 0) {
		int w = SSL_write(ssl, p, (int)n);
		if (w <= 0) return -1;
		p += w; n -= (size_t)w;
	}
	return 0;
}

/* ssl_read_full reads exactly n bytes. Returns 0 on success, -1 on a real error or clean
 * EOF, or -2  if the socket's SO_RCVTIMEO elapsed with *no bytes of this read
 * consumed* — a "nothing to read yet, keep waiting" signal distinct from a dead connection.
 * A timeout after partial consumption returns -1 instead: -2 would make the caller restart
 * its frame parse from a torn header/payload, desyncing the ACK stream (a misparsed F_ACK
 * seq could prune unacked frames), so a peer that stalls mid-record for a full receive
 * timeout is treated as dead and the connection torn down. A blocking socket's receive
 * timeout usually surfaces through OpenSSL as SSL_ERROR_SYSCALL with errno EAGAIN/EWOULDBLOCK,
 * but some builds/BIOs report SSL_ERROR_WANT_READ for the same condition — both are treated
 * as a timeout here. */
static int ssl_read_full(SSL *ssl, void *buf, size_t n) {
	unsigned char *p = buf;
	size_t want = n;
	while (n > 0) {
		int r = SSL_read(ssl, p, (int)n);
		if (r <= 0) {
			int err = SSL_get_error(ssl, r);
			if (err == SSL_ERROR_WANT_READ ||
			    (err == SSL_ERROR_SYSCALL && (errno == EAGAIN || errno == EWOULDBLOCK)))
				return n == want ? -2 : -1;
			return -1;
		}
		p += r; n -= (size_t)r;
	}
	return 0;
}

static int send_frame(SSL *ssl, uint8_t type, const void *payload, uint32_t len) {
	unsigned char hdr[5];
	hdr[0] = type; be32(hdr+1, len);
	if (ssl_write_all(ssl, hdr, 5) != 0) return -1;
	if (len && ssl_write_all(ssl, payload, len) != 0) return -1;
	return 0;
}

/* conn_write writes len bytes to the collector, compressing through the zstd stream if
 * one is set (flushing per call so the collector decodes each frame promptly while the
 * window keeps compressing across frames). */
static int conn_write(struct conn *c, const unsigned char *buf, size_t len) {
	if (!c->cctx) return ssl_write_all(c->ssl, buf, len);
	ZSTD_inBuffer in = { buf, len, 0 };
	size_t rem;
	do {
		ZSTD_outBuffer out = { c->obuf, c->obuf_cap, 0 };
		rem = ZSTD_compressStream2(c->cctx, &out, &in, ZSTD_e_flush);
		if (ZSTD_isError(rem)) return -1;
		if (out.pos && ssl_write_all(c->ssl, c->obuf, out.pos) != 0) return -1;
		c->comp += out.pos;
	} while (rem > 0);
	c->raw += len;
	return 0;
}

/* send_data sends one DATA frame: [F_DATA][4B len][8B seq][record]. The whole frame goes
 * through conn_write, so when compression is on the framing is compressed too and the
 * collector's decoder yields exactly the bytes wire.ReadFrame expects. */
static int send_data(struct conn *c, uint64_t seq, const unsigned char *data, uint32_t len) {
	unsigned char frame[5 + 8 + GNF_RECORD];
	if (len > GNF_RECORD) len = GNF_RECORD;
	frame[0] = F_DATA;
	be32(frame + 1, 8 + len);
	be64(frame + 5, seq);
	memcpy(frame + 13, data, len);
	return conn_write(c, frame, 13 + (size_t)len);
}

/* send_ping keeps an idle connection alive — a stalled receiver can go minutes with nothing
 * to send, and the collector drops a connection with no frames within its idle timeout. */
static int send_ping(struct conn *c) {
	unsigned char frame[5] = { F_PING, 0, 0, 0, 0 };
	return conn_write(c, frame, 5);
}

/* read_frame reads one wire frame. Returns 0 on success, -1 on a real error, or -2 
 * on a receive timeout with the frame boundary intact — i.e. only from the header read with
 * zero bytes consumed, so reader_thread's loop can tell "idle" from "dead" apart. Once the
 * header has been consumed, a payload-read timeout is mid-frame: it is converted to -1 so
 * the caller never resumes parsing from a torn frame. */
static int read_frame(SSL *ssl, uint8_t *type, unsigned char *buf, uint32_t cap, uint32_t *len) {
	unsigned char hdr[5];
	int rc = ssl_read_full(ssl, hdr, 5);
	if (rc != 0) return rc;
	uint32_t n = rd_be32(hdr+1);
	if (n > MAX_FRAME || n > cap) return -1;
	if (n && ssl_read_full(ssl, buf, n) != 0) return -1;
	*type = hdr[0]; *len = n;
	return 0;
}

/* reader thread: apply ACKs (pruning the spool) until the connection drops. The collector
 * socket carries an SO_RCVTIMEO (regression fix, set in tls_connect's tcp_dial call), so a genuinely
 * half-open peer (vanished, no RST) no longer leaves this thread blocked in SSL_read for the
 * full TCP retransmit window — it wakes on each timeout, checks g_disconnected (set by the
 * consumer side on its own write failure), and returns promptly instead of stalling teardown
 * while the spool fills. A timeout alone is not a disconnect signal; only a real read error
 * or clean EOF ends the loop. */
static void *reader_thread(void *arg) {
	SSL *ssl = arg;
	unsigned char buf[256];
	uint8_t type; uint32_t len;
	for (;;) {
		int rc = read_frame(ssl, &type, buf, sizeof buf, &len);
		if (rc == -2) {
			if (g_disconnected) break;
			continue;
		}
		if (rc != 0) break;
		if (type == F_ACK && len >= 8) spool_ack(&g_spool, rd_be64(buf));
	}
	g_disconnected = 1;
	return NULL;
}

/* ── producer: UBX source -> spool (forever) ─────────────────────────────── */

/* frame_type maps (gnssId, sigId) to the GNF1 nav message type byte (docs/CONSTELLATIONS.md
 * §6), mirroring RawFrame.NavType() in the collector so the historian's msg_type is right.
 * The collector dispatches decoding on (gnssId, sigId), so an unmapped type (0) is still
 * decoded — the byte is a forensic label, not the dispatch key. */
static uint8_t frame_type(unsigned gnssId, unsigned sigId) {
	switch (gnssId) {
	case 0: return sigId == 0 ? 0x10 : 0x11;                 /* GPS: LNAV / CNAV */
	case 5:                                                   /* QZSS: shipped LNAV/CNAV only */
		if (sigId == 0) return 0x50;
		if (sigId == 4 || sigId == 5 || sigId == 8 || sigId == 9) return 0x51;
		return 0;
	case 2: return (sigId == 3 || sigId == 4) ? 0x21 : 0x20; /* Galileo: F/NAV / I/NAV */
	case 3:                                                  /* BeiDou: shipped B1I D1 + B2a B-CNAV2 only */
		if (sigId == 0) return 0x30;
		if (sigId == 8) return 0x33;
		return 0; /* D2/B2I/B-CNAV1/B2a-companion planned: no verified decoder  */
	case 6: return 0x40;                                     /* GLONASS */
	case 7: return 0;                                        /* NavIC planned: capture without false type */
	case 1: return 0x70;                                     /* SBAS */
	default: return 0;
	}
}

/* emit_sfrbx builds one GNF1 DATA record from a UBX-RXM-SFRBX payload and appends it to the
 * spool. Layout (F9/M9): gnssId, svId, sigId, freqId, numWords, reserved, version, reserved,
 * then numWords little-endian 32-bit dwrds each holding one native nav word right-aligned.
 * We re-serialize each word big-endian (the collector reads them back with BE, matching its
 * RawBytes()/bytesToWords round-trip), and stamp the host reception time. */
static void emit_sfrbx(const unsigned char *p, unsigned len) {
	if (len < 8) return;
	unsigned gnssId = p[0], svId = p[1], sigId = p[2], freqId = p[3], numWords = p[4];
	if (numWords == 0 || 8u + numWords * 4u > len) return;
	unsigned rawlen = numWords * 4u;
	if (rawlen > MAX_RAW) return;

	unsigned char rec[GNF_RECORD];
	be64(rec, now_unix_ns());
	rec[8]  = (unsigned char)gnssId;
	rec[9]  = (unsigned char)svId;
	rec[10] = (unsigned char)sigId;
	rec[11] = (unsigned char)freqId;
	rec[12] = frame_type(gnssId, sigId);
	for (unsigned i = 0; i < numWords; i++) {
		const unsigned char *w = p + 8 + i * 4; /* u-blox stores the dwrd little-endian */
		uint32_t v = (uint32_t)w[0] | ((uint32_t)w[1] << 8) | ((uint32_t)w[2] << 16) | ((uint32_t)w[3] << 24);
		be32(rec + RECORD_HDR + i * 4, v);
	}
	spool_append(&g_spool, rec, RECORD_HDR + rawlen);
}

/* emit_telem builds one GNF1 telemetry record (frame_type < 0x10 — a §6.2 receiver-side
 * sample) and spools it exactly like a nav frame: telemetry rides the same DATA/seq/ack/
 * replay stream. The record's gnssId/svId/sigId/freqId are zero (the sample is station-
 * scoped, keyed by the observer at the collector); the body is the type-specific payload
 * and MUST match ../go/internal/ingest/telemetry.go so the C↔Go cross-oracle stays exact. */
static void emit_telem(uint8_t type, const unsigned char *body, unsigned bodylen) {
	if (bodylen > MAX_RAW) return;
	unsigned char rec[GNF_RECORD];
	be64(rec, now_unix_ns());
	rec[8] = rec[9] = rec[10] = rec[11] = 0; /* gnssId/svId/sigId/freqId: unused for telemetry */
	rec[12] = type;
	memcpy(rec + RECORD_HDR, body, bodylen);
	spool_append(&g_spool, rec, RECORD_HDR + bodylen);
}

/* emit_monrf converts a UBX-MON-RF payload (F9+ RF-front-end telemetry) into a JammingStats
 * (0x05) record. Layout: version U1, nBlocks U1, reserved U1[2], then nBlocks × 24-byte
 * blocks — blockId U1, flags X1 (bits 0-1 = jammingState), antStatus U1, antPower U1,
 * postStatus U4 @4, reserved U1[4] @8, noisePerMS U2 @12, agcCnt U2 @14, jamInd U1 @16
 * (previously read @14/@16/@20 — off by the 2-byte antStatus/antPower pair, so
 * NoiseLevel got agcCnt, AGC got jamInd|ofsI<<8, and CW got magQ). Body: [ver][nBands]
 * then per band [block][agc BE16][noise BE16][cw][jamState][antStatus]. Bounds-checked. */
static void emit_monrf(const unsigned char *p, unsigned len) {
	if (len < 4) return;
	unsigned nBlocks = p[1];
	if (nBlocks == 0 || 4u + nBlocks * 24u > len) return;
	unsigned bodylen = 2u + nBlocks * 8u;
	if (bodylen > MAX_RAW) return;
	unsigned char body[MAX_RAW];
	body[0] = TELEM_VERSION;
	body[1] = (unsigned char)nBlocks;
	for (unsigned i = 0; i < nBlocks; i++) {
		const unsigned char *b = p + 4 + i * 24;
		unsigned o = 2 + i * 8;
		body[o]     = b[0];                       /* blockId */
		be16(body + o + 1, rd_le16(b + 14));      /* agcCnt */
		be16(body + o + 3, rd_le16(b + 12));      /* noisePerMS */
		body[o + 5] = b[16];                      /* jamInd (CW) */
		body[o + 6] = (unsigned char)(b[1] & 0x03); /* jammingState */
		body[o + 7] = b[2];                       /* antStatus */
	}
	emit_telem(F_T_JAMMING, body, bodylen);
}

/* emit_monhw converts a legacy UBX-MON-HW payload (60 bytes) into a single-band JammingStats
 * record: noisePerMS U2 @16, agcCnt U2 @18, aStatus U1 @20, flags X1 @22 (jammingState in
 * bits 2-3), jamInd U1 @45. Bounds-checked. */
static void emit_monhw(const unsigned char *p, unsigned len) {
	if (len < 60) return;
	unsigned char body[2 + 8];
	body[0] = TELEM_VERSION;
	body[1] = 1;
	body[2] = 0;                            /* block 0 */
	be16(body + 3, rd_le16(p + 18));        /* agcCnt */
	be16(body + 5, rd_le16(p + 16));        /* noisePerMS */
	body[7] = p[45];                        /* jamInd (CW) */
	body[8] = (unsigned char)((p[22] >> 2) & 0x03); /* jammingState */
	body[9] = p[20];                        /* aStatus */
	emit_telem(F_T_JAMMING, body, sizeof body);
}

/* emit_navsat converts a UBX-NAV-SAT payload into a ReceptionData (0x01) record for the
 * C/N0-vs-elevation spoofing gate. Layout: iTOW U4, version U1, numSvs U1 @5, reserved U1[2],
 * then numSvs × 12-byte blocks — gnssId U1, svId U1, cno U1 @2, elev I1 @3, …, flags X4 @8
 * (bit 3 = svUsed). Body: [ver][nSats BE16] then per sat [gnssId][svId][cno][elev i8][flags];
 * capped at MAX_TELEM_SATS. Bounds-checked. */
static void emit_navsat(const unsigned char *p, unsigned len) {
	if (len < 8) return;
	unsigned numSvs = p[5];
	if (numSvs == 0 || 8u + numSvs * 12u > len) return;
	unsigned n = numSvs > MAX_TELEM_SATS ? MAX_TELEM_SATS : numSvs;
	unsigned char body[3 + MAX_TELEM_SATS * 5];
	body[0] = TELEM_VERSION;
	be16(body + 1, (uint16_t)n);
	for (unsigned i = 0; i < n; i++) {
		const unsigned char *s = p + 8 + i * 12;
		unsigned o = 3 + i * 5;
		body[o]     = s[0];                 /* gnssId */
		body[o + 1] = s[1];                 /* svId */
		body[o + 2] = s[2];                 /* cno */
		body[o + 3] = s[3];                 /* elev (I1, forwarded verbatim) */
		body[o + 4] = (rd_le32(s + 8) & 0x08) ? 0x01 : 0x00; /* svUsed */
	}
	emit_telem(F_T_RECEPTION, body, 3u + n * 5u);
}

/* rdbuf is a small buffered reader over the source fd (serial or TCP). */
struct rdbuf { int fd; size_t pos, len; int quiet; unsigned char buf[4096]; };

/* RB_QUIET_MAX bounds how many consecutive source read-timeouts (5s each, regression fix) may elapse
 * before rb_getc gives up and lets the producer re-dial — ~5 min of total silence. */
#define RB_QUIET_MAX 60

static int rb_getc(struct rdbuf *b) {
	while (b->pos >= b->len) {
		ssize_t r = read(b->fd, b->buf, sizeof b->buf);
		if (r > 0) { b->len = (size_t)r; b->pos = 0; b->quiet = 0; break; }
		if (r == 0) return -1;                       /* EOF / device closed */
		if (errno == EINTR) continue;
		/* a source SO_RCVTIMEO expiry (EAGAIN/EWOULDBLOCK) is NOT EOF — a healthy TCP
		 * receiver can legitimately go quiet for >5s (messages not yet enabled, mid-reboot,
		 * ser2net momentarily detached). Keep waiting rather than tearing the connection into
		 * a no-backoff reconnect churn; only give up after RB_QUIET_MAX consecutive quiet
		 * periods so a silently-wedged source is still eventually re-dialed. The serial path
		 * uses a blocking read (VMIN=1), so it never reaches this branch. */
		if (errno == EAGAIN || errno == EWOULDBLOCK) {
			if (++b->quiet >= RB_QUIET_MAX) return -1;
			continue;
		}
		return -1;                                   /* read error */
	}
	return b->buf[b->pos++];
}

static int rb_read(struct rdbuf *b, unsigned char *dst, unsigned n) {
	for (unsigned i = 0; i < n; i++) {
		int c = rb_getc(b);
		if (c < 0) return -1;
		dst[i] = (unsigned char)c;
	}
	return 0;
}

/* sync_ubx consumes bytes until the two-byte 0xB5 0x62 sync is found. A lone 0xB5 followed
 * by another 0xB5 re-examines the second as a fresh sync candidate (mirrors the collector's
 * scanUBX resync). Returns 0 on sync, -1 on stream end. */
static int sync_ubx(struct rdbuf *b) {
	int c = rb_getc(b);
	for (;;) {
		if (c < 0) return -1;
		if (c != UBX_SYNC1) { c = rb_getc(b); continue; }
		c = rb_getc(b);
		if (c < 0) return -1;
		if (c == UBX_SYNC2) return 0;
		/* not 0x62; loop — if c is itself 0xB5 it becomes the next sync candidate */
	}
}

/* run_ubx reads a UBX byte stream, validates each message's Fletcher checksum, and emits
 * every UBX-RXM-SFRBX to the spool. It returns when the source ends (reconnect trigger). A
 * corrupt frame is dropped and the reader resynchronises — a mid-stream connect never
 * derails it. Untrusted-input discipline (docs/INTEGRITY.md §9): every length and index is
 * bounds-checked before use. Returns nonzero if this connection proved "useful" — it
 * emitted >=1 frame, or survived USEFUL_CONN_S — 0 otherwise : a TCP bridge that
 * accepts and instantly closes (ser2net with the tty missing, port busy) makes this return
 * immediately on the very first read; producer_thread uses the return value to decide
 * whether resetting backoff is warranted, mirroring go/internal/ingest's regression fix fix. */
static int run_ubx(int fd) {
	struct rdbuf rb; rb.fd = fd; rb.pos = rb.len = 0; rb.quiet = 0;
	unsigned char head[4], payload[UBX_MAX_PAYLOAD], ck[2];
	time_t start = time(NULL);
	unsigned long frames = 0;
	for (;;) {
		if (sync_ubx(&rb) != 0) { log_msg("source closed"); break; }
		if (rb_read(&rb, head, 4) != 0) break;                /* class, id, len(2, LE) */
		unsigned len = (unsigned)head[2] | ((unsigned)head[3] << 8);
		if (len > UBX_MAX_PAYLOAD) continue;                  /* implausible length → resync */
		if (rb_read(&rb, payload, len) != 0) break;
		if (rb_read(&rb, ck, 2) != 0) break;
		uint8_t a = 0, bb = 0;                                 /* 8-bit Fletcher over class..payload */
		a += head[0]; bb += a; a += head[1]; bb += a;
		a += head[2]; bb += a; a += head[3]; bb += a;
		for (unsigned i = 0; i < len; i++) { a += payload[i]; bb += a; }
		if (a != ck[0] || bb != ck[1]) continue;              /* bad checksum → drop, resync */
		if (head[0] == UBX_CLASS_RXM && head[1] == UBX_ID_SFRBX)
			{ emit_sfrbx(payload, len); frames++; }
		else if (head[0] == UBX_CLASS_MON && head[1] == UBX_ID_MONRF)
			{ emit_monrf(payload, len); frames++; }
		else if (head[0] == UBX_CLASS_MON && head[1] == UBX_ID_MONHW)
			{ emit_monhw(payload, len); frames++; }
		else if (head[0] == UBX_CLASS_NAV && head[1] == UBX_ID_NAVSAT)
			{ emit_navsat(payload, len); frames++; }
	}
	return frames > 0 || (time(NULL) - start) >= USEFUL_CONN_S;
}

/* ── source open (serial or TCP) ─────────────────────────────────────────── */

static speed_t baud_to_speed(int baud) {
	switch (baud) {
#ifdef B9600
	case 9600: return B9600;
#endif
#ifdef B19200
	case 19200: return B19200;
#endif
#ifdef B38400
	case 38400: return B38400;
#endif
#ifdef B57600
	case 57600: return B57600;
#endif
#ifdef B115200
	case 115200: return B115200;
#endif
#ifdef B230400
	case 230400: return B230400;
#endif
#ifdef B460800
	case 460800: return B460800;
#endif
#ifdef B921600
	case 921600: return B921600;
#endif
	default: return 0;
	}
}

/* open_serial opens a receiver device in raw mode at the configured baud. u-blox USB CDC-ACM
 * ignores the line rate, but a real UART bridge needs it (the fleet runs 460800). */
static int open_serial(const char *path, int baud) {
	int fd = open(path, O_RDONLY | O_NOCTTY);
	if (fd < 0) return -1;
	struct termios t;
	if (tcgetattr(fd, &t) != 0) { close(fd); return -1; }
	cfmakeraw(&t);
	speed_t sp = baud_to_speed(baud);
	if (sp == 0) { close(fd); log_msg("unsupported --baud %d", baud); return -1; }
	cfsetispeed(&t, sp);
	cfsetospeed(&t, sp);
	t.c_cflag |= (CLOCAL | CREAD);
	t.c_cc[VMIN] = 1;   /* block for at least one byte */
	t.c_cc[VTIME] = 0;
	if (tcsetattr(fd, TCSANOW, &t) != 0) { close(fd); return -1; }
	return fd;
}

/* open_source opens the receiver: a device path (leading '/') is a serial port; otherwise a
 * host:port TCP bridge (ser2net / a receiver's raw TCP port). */
static int open_source(const struct opts *o) {
	if (o->source[0] == '/') return open_serial(o->source, o->baud);
	char hp[256];
	snprintf(hp, sizeof hp, "%s", o->source);
	char *colon = strrchr(hp, ':');
	if (!colon) { log_msg("--source %s: expected /dev/... or host:port", o->source); return -1; }
	*colon = 0;
	return tcp_dial(hp, colon + 1, 5);
}

static void *producer_thread(void *arg) {
	const struct opts *o = arg;
	int backoff = 1;
	for (;;) {
		int fd = open_source(o);
		if (fd < 0) {
			log_msg("source open failed (%s); retry in %ds", o->source, backoff);
			sleep(backoff); if ((backoff *= 2) > 30) backoff = 30;
			continue;
		}
		log_msg("source open (ubx): %s", o->source);
		int useful = run_ubx(fd);
		close(fd);
		if (useful) {
			backoff = 1;
		} else {
			log_msg("source %s closed instantly with no data; retry in %ds", o->source, backoff);
			sleep(backoff); if ((backoff *= 2) > 30) backoff = 30;
		}
	}
	return NULL;
}

/* ── consumer: spool -> collector, replaying unacked on every reconnect ───── */

static SSL *tls_connect(SSL_CTX *ctx, const struct opts *o, int *out_fd) {
	/* a receive timeout (2x KEEPALIVE_S) so reader_thread wakes periodically
	 * instead of blocking in SSL_read indefinitely on a half-open peer; long enough
	 * that a normally-idle link (waiting on ACKs between our own KEEPALIVE_S pings)
	 * is never mistaken for dead — see ssl_read_full/reader_thread. */
	int fd = tcp_dial(o->server_host, o->server_port, 2 * KEEPALIVE_S);
	if (fd < 0) return NULL;
	/* bound the WRITE side too (regression fix only bounded reads). If the collector stops
	 * reading while TCP stays alive (its decode stage stalls; zero-window probes keep the
	 * link up), SSL_write would block forever — PINGs stop, the spool fills then drops, and
	 * the feeder never reconnects. A send timeout turns that into a normal SSL_write<=0
	 * teardown + reconnect-with-replay. Collector socket only; the source socket (tcp_dial's
	 * other caller) is read-only. */
	{
		struct timeval snd = { 2 * KEEPALIVE_S, 0 };
		if (setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &snd, sizeof snd) != 0) {
			/* regression fix verification correction: continuing would silently restore the
			 * indefinite SSL_write hang the finding requires us to eliminate. */
			log_msg("failed to set collector send timeout: %s", strerror(errno));
			close(fd);
			return NULL;
		}
	}
	SSL *ssl = SSL_new(ctx);
	if (!ssl) { close(fd); return NULL; }
	SSL_set_fd(ssl, fd);
	if (!o->insecure) {
		/* SSL_set_tlsext_host_name() only sets SNI (which name to request); it does
		 * NOT enable peer hostname verification. Without SSL_set1_host(),
		 * SSL_get_verify_result() below only checks the chain, not the name, so any
		 * attacker holding any publicly-trusted cert for any domain passes. */
		SSL_set_hostflags(ssl, X509_CHECK_FLAG_NO_PARTIAL_WILDCARDS);
		if (SSL_set1_host(ssl, o->server_host) != 1) {
			log_msg("failed to set expected TLS hostname");
			SSL_free(ssl); close(fd); return NULL;
		}
		SSL_set_tlsext_host_name(ssl, o->server_host);
	}
	if (SSL_connect(ssl) != 1) { SSL_free(ssl); close(fd); return NULL; }
	if (!o->insecure && SSL_get_verify_result(ssl) != X509_V_OK) {
		log_msg("server certificate verification failed");
		SSL_free(ssl); close(fd); return NULL;
	}
	*out_fd = fd;
	return ssl;
}

/* json_escape copies src into dst as a JSON string body (no surrounding quotes), escaping
 * '"', '\', and control characters : an operator-supplied token/station/feed
 * containing '"' or '\' would otherwise produce invalid JSON, and the collector's
 * ParseHello failing on it manifests as a confusing permanent auth-reject/reconnect loop
 * rather than a clear error at the source. Truncates cleanly (never overruns) if the
 * escaped form would not fit dstcap; dst is always NUL-terminated when dstcap > 0.
 * Well-formed inputs (no '"', '\', or control chars) are copied byte-identical. */
static void json_escape(char *dst, size_t dstcap, const char *src) {
	if (dstcap == 0) return;
	size_t di = 0;
	for (const unsigned char *s = (const unsigned char *)src; *s; s++) {
		unsigned char c = *s;
		char ubuf[7];
		const char *esc = NULL;
		switch (c) {
		case '"':  esc = "\\\""; break;
		case '\\': esc = "\\\\"; break;
		case '\n': esc = "\\n"; break;
		case '\r': esc = "\\r"; break;
		case '\t': esc = "\\t"; break;
		default:
			if (c < 0x20) {
				snprintf(ubuf, sizeof ubuf, "\\u%04x", c);
				esc = ubuf;
			}
		}
		size_t elen = esc ? strlen(esc) : 1;
		if (di + elen + 1 > dstcap) break; /* would overflow: truncate cleanly */
		if (esc) { memcpy(dst + di, esc, elen); } else { dst[di] = (char)c; }
		di += elen;
	}
	dst[di] = 0;
}

static int handshake(SSL *ssl, const struct opts *o, int *zstd_ok) {
	*zstd_ok = 0;
	if (ssl_write_all(ssl, MAGIC, 4) != 0) return -1;
	char tok_esc[512], station_esc[512], feed_esc[128];
	json_escape(tok_esc, sizeof tok_esc, o->token ? o->token : "");
	json_escape(station_esc, sizeof station_esc, o->station);
	json_escape(feed_esc, sizeof feed_esc, o->feed);
	char hello[1024];
	int n = snprintf(hello, sizeof hello,
		"{\"token\":\"%s\",\"station\":\"%s\",\"feed\":\"%s\",\"sw\":\"navfeeder/1\"%s}",
		tok_esc, station_esc, feed_esc, o->zstd ? ",\"zstd\":true" : "");
	if (n < 0 || (size_t)n >= sizeof hello) return -1;
	if (send_frame(ssl, F_HELLO, hello, (uint32_t)n) != 0) return -1;

	unsigned char buf[1024]; uint8_t type; uint32_t len;
	if (read_frame(ssl, &type, buf, sizeof buf, &len) != 0) return -1;
	if (type != F_WELCOME) return -1;
	buf[len < sizeof buf ? len : sizeof buf - 1] = 0;
	if (!strstr((char *)buf, "\"ok\":true")) {
		log_msg("collector rejected handshake: %.*s", (int)len, buf);
		return -2;
	}
	*zstd_ok = (strstr((char *)buf, "\"zstd\":true") != NULL); /* only compress if confirmed */
	return 0;
}

/* disk_maybe_delete removes the spool file once everything in it has been acked. */
static void disk_maybe_delete(struct spool *s) {
	pthread_mutex_lock(&s->mu);
	uint64_t cleared_bytes = 0;
	int delete_err = 0;
	if (s->path && s->disk_max_seq != 0 && s->acked >= s->disk_max_seq) {
		if (s->disk_w) { fclose(s->disk_w); s->disk_w = NULL; }
		if (unlink(s->path) == 0 || errno == ENOENT) {
			cleared_bytes = s->disk_bytes;
			s->disk_max_seq = s->disk_bytes = 0;
			s->disk_append_disabled = 0;
		} else {
			/* Keeping the old accounting and disabling appends is safer than resetting
			 * disk_bytes then reopening an undeleted file at an unknown boundary. */
			delete_err = errno;
			s->disk_append_disabled = 1;
		}
	}
	pthread_mutex_unlock(&s->mu);
	if (delete_err)
		log_msg("acked disk spool could not be removed (%s); disk overflow disabled",
			strerror(delete_err));
	if (cleared_bytes >= 64 * 1024)
		log_msg("disk spool delivered and cleared (%llu KiB)", (unsigned long long)(cleared_bytes / 1024));
}

/* disk_drain sends disk-spooled frames with seq > *sent_upto (oldest first). Returns the
 * count sent, or -1 on a send failure. The disk always holds seqs older than the ring. */
static int disk_drain(struct spool *s, struct conn *c, uint64_t *sent_upto) {
	pthread_mutex_lock(&s->mu);
	const char *path = s->path;
	uint64_t dmax = s->disk_max_seq;
	pthread_mutex_unlock(&s->mu);
	if (!path || dmax <= *sent_upto) return 0;

	FILE *r = fopen(path, "rb");
	if (!r) {
		/* regression fix follow-up: an unopenable spool file (fd exhaustion, external
		 * unlink) is otherwise indistinguishable from "nothing to drain" while
		 * disk_max_seq stays ahead of sent_upto — the caller paces its retry
		 * (see the drain loop), and this rate-limited log makes the stall
		 * diagnosable instead of silent. Only serve_collector's thread calls
		 * this, so the static is single-threaded. */
		static time_t last_warn;
		time_t nowt = time(NULL);
		if (nowt - last_warn >= 60) {
			last_warn = nowt;
			log_msg("disk spool open failed (frames pending past seq %llu): %s",
				(unsigned long long)*sent_upto, strerror(errno));
		}
		return 0;
	}
	int sent = 0;
	for (;;) {
		unsigned char hdr[12];
		if (fread(hdr, 1, 12, r) != 12) break;
		uint64_t seq = rd_be64(hdr);
		uint32_t len = rd_be32(hdr + 8);
		if (len > GNF_RECORD) break; /* truncated/corrupt tail record */
		unsigned char data[GNF_RECORD];
		if (len && fread(data, 1, len, r) != len) break;
		if (seq > *sent_upto) {
			if (send_data(c, seq, data, len) != 0) { fclose(r); g_disconnected = 1; return -1; }
			*sent_upto = seq;
			sent++;
		}
	}
	fclose(r);
	/* the scan ended before reaching disk_max_seq on an openable file — a
	 * torn/short record (which the disk_put rollback fix now prevents from persisting,
	 * but a spool written by an older build or an external corruption could still show).
	 * Log it rate-limited (mirroring the fopen-failure path) so the otherwise-silent
	 * ping-only stall is diagnosable rather than invisible. */
	if (*sent_upto < dmax) {
		static time_t last_warn;
		time_t nowt = time(NULL);
		if (nowt - last_warn >= 60) {
			last_warn = nowt;
			log_msg("disk spool drain reached seq %llu but disk_max_seq is %llu (torn record?)",
				(unsigned long long)*sent_upto, (unsigned long long)dmax);
		}
	}
	return sent;
}

/* serve_collector connects once and drains the spool until disconnect. Returns -2 on auth
 * rejection (back off hard), -1 otherwise. */
static int serve_collector(SSL_CTX *ctx, const struct opts *o) {
	int tls_fd;
	SSL *ssl = tls_connect(ctx, o, &tls_fd);
	if (!ssl) { log_msg("collector TLS connect failed"); return -1; }

	int zstd_ok = 0;
	int hs = handshake(ssl, o, &zstd_ok);
	if (hs != 0) { SSL_free(ssl); close(tls_fd); return hs == -2 ? -2 : -1; }

	struct conn c = { ssl, NULL, NULL, 0, 0, 0 };
	if (zstd_ok) {
		/* regression fix (partial, see technical validation): kept as die() here, not downgraded
		 * to a silent plaintext fallback. By this point handshake() has already
		 * exchanged "zstd":true with the collector, which commits it to wrapping
		 * its reader in a zstd decompressor for the rest of THIS connection
		 * (internal/ingest/push.go's useZstd path is stream-, not frame-, scoped).
		 * Switching c.cctx to NULL here would silently write plaintext into that
		 * decompressor and desync the wire — worse than a clean process exit. The
		 * safe equivalent (probe the allocation before advertising zstd in the
		 * HELLO, or fail this connection attempt and let the outer loop
		 * reconnect) is a real fix but is not what this exact line can do without
		 * restructuring handshake(), so it's deliberately left out of this pass. */
		c.cctx = ZSTD_createCCtx();
		c.obuf_cap = ZSTD_CStreamOutSize();
		c.obuf = malloc(c.obuf_cap);
		if (!c.cctx || !c.obuf) die("out of memory for zstd stream");
		ZSTD_CCtx_setParameter(c.cctx, ZSTD_c_compressionLevel, 3);
	}

	g_disconnected = 0;
	pthread_t rt;
	if (pthread_create(&rt, NULL, reader_thread, ssl) != 0) {
		/* a failed thread create left `rt` indeterminate, and the unconditional
		 * pthread_join(rt, NULL) at the end of this function is UB on it; treat this
		 * exactly like a failed connect (log + tear down + let the outer loop retry). */
		log_msg("failed to start reader thread; treating as a connection error");
		if (c.cctx) ZSTD_freeCCtx(c.cctx);
		free(c.obuf);
		SSL_free(ssl);
		close(tls_fd);
		return -1;
	}

	/* Replay-on-reconnect: resume from the last acked sequence. */
	uint64_t sent_upto = spool_acked(&g_spool);
	uint64_t replay_base = 0;
	spool_stats(&g_spool, &replay_base, NULL, NULL, NULL);
	if (replay_base > sent_upto)
		log_msg("connected: station=%s feed=%s zstd=%d; replaying %llu unacked frame(s)",
			o->station, o->feed, zstd_ok, (unsigned long long)(replay_base - sent_upto));
	else
		log_msg("connected: station=%s feed=%s zstd=%d -> %s:%s",
			o->station, o->feed, zstd_ok, o->server_host, o->server_port);

	struct frame batch[DRAIN_BATCH];
	int rc = -1;
	time_t last_tx = time(NULL);
	while (!g_disconnected) {
		disk_maybe_delete(&g_spool);
		int d = disk_drain(&g_spool, &c, &sent_upto);  /* oldest unacked first (disk) */
		if (d < 0) break;                              /* disconnected during disk replay */
		if (d > 0) { last_tx = time(NULL); continue; } /* re-check disk before the ring */
		uint64_t disk_max_seq_now = 0;
		size_t n = spool_collect(&g_spool, sent_upto, batch, DRAIN_BATCH, &disk_max_seq_now);
		if (disk_max_seq_now > sent_upto) {
			/* a frame > sent_upto may have been evicted to disk between
			 * disk_drain's snapshot (above) and this collect. Discard whatever was
			 * gathered from the ring and loop back to disk_drain first, which will
			 * see the now-current disk_max_seq and send it — safe even if nothing
			 * was actually evicted (disk_drain then sees dmax <= sent_upto and this
			 * check falls through next time). Pace the retry and keep the
			 * keepalive flowing: if disk_drain cannot make progress (unopenable
			 * spool file — see its fopen failure path), a bare continue would spin
			 * this loop at 100% CPU forever and starve PINGs. */
			for (size_t i = 0; i < n; i++) free(batch[i].data);
			if (time(NULL) - last_tx >= KEEPALIVE_S) {
				if (send_ping(&c) != 0) { g_disconnected = 1; break; }
				last_tx = time(NULL);
			}
			usleep(50 * 1000);
			continue;
		}
		if (n == 0) {                                  /* caught up; wait for the producer */
			if (time(NULL) - last_tx >= KEEPALIVE_S) {
				if (send_ping(&c) != 0) { g_disconnected = 1; break; }
				last_tx = time(NULL);
			}
			usleep(50 * 1000);
			continue;
		}
		for (size_t i = 0; i < n; i++) {
			if (!g_disconnected && send_data(&c, batch[i].seq, batch[i].data, batch[i].len) == 0)
				sent_upto = batch[i].seq;
			else
				g_disconnected = 1;
			free(batch[i].data);
		}
		last_tx = time(NULL);
	}

	SSL_shutdown(ssl);
	pthread_join(rt, NULL);
	SSL_free(ssl); close(tls_fd);
	if (c.cctx) ZSTD_freeCCtx(c.cctx);
	if (c.raw)
		log_msg("zstd: %llu -> %llu bytes on the wire (%.1f%% of raw)",
			(unsigned long long)c.raw, (unsigned long long)c.comp, 100.0 * (double)c.comp / (double)c.raw);
	free(c.obuf);
	uint64_t dropped = 0, disk_dropped = 0; size_t spooled = 0;
	spool_stats(&g_spool, NULL, &dropped, &spooled, &disk_dropped);
	log_msg("disconnected (spooled=%zu dropped=%llu disk_dropped=%llu)",
		spooled, (unsigned long long)dropped, (unsigned long long)disk_dropped);
	return rc;
}

/* ── setup ───────────────────────────────────────────────────────────────── */

static SSL_CTX *make_ctx(const struct opts *o) {
	SSL_CTX *ctx = SSL_CTX_new(TLS_client_method());
	if (!ctx) die("SSL_CTX_new failed");
	SSL_CTX_set_min_proto_version(ctx, TLS1_2_VERSION);
	/* Pin to TLS 1.2 (radiolistener regression fix). The reader thread runs SSL_read while the consumer
	 * thread runs SSL_write on the SAME SSL object with no lock. OpenSSL's one-reader/one-writer
	 * pattern is only safe when SSL_read can't have to write: under TLS 1.3 a post-handshake
	 * KeyUpdate processed inside SSL_read needs the write path, racing the concurrent SSL_write
	 * and corrupting the connection. TLS 1.2 has no KeyUpdate and the Go collector never
	 * renegotiates, so capping the max version keeps the split threading model correct without a
	 * per-SSL mutex (which would deadlock the blocking reader). TLS 1.2 with modern ciphers is
	 * secure. */
	SSL_CTX_set_max_proto_version(ctx, TLS1_2_VERSION);
	if (o->insecure) {
		SSL_CTX_set_verify(ctx, SSL_VERIFY_NONE, NULL);
	} else {
		SSL_CTX_set_verify(ctx, SSL_VERIFY_PEER, NULL);
		if (o->ca) {
			if (SSL_CTX_load_verify_locations(ctx, o->ca, NULL) != 1) die("failed to load --ca file");
		} else if (SSL_CTX_set_default_verify_paths(ctx) != 1) {
			die("failed to load system CA paths");
		}
	}
	/* mTLS: present a client certificate. regression fix/the collector matches exactly one DNS
	 * SAN equal to the station id and REJECTS CN-only certs — mint the cert with a DNS SAN. */
	if (o->cert && o->key) {
		if (SSL_CTX_use_certificate_chain_file(ctx, o->cert) != 1) die("failed to load --cert file");
		if (SSL_CTX_use_PrivateKey_file(ctx, o->key, SSL_FILETYPE_PEM) != 1) die("failed to load --key file");
		if (SSL_CTX_check_private_key(ctx) != 1) die("--cert and --key do not match");
	}
	return ctx;
}

/* read_token_file loads the bearer token from a file so it never appears in argv (visible in
 * ps / /proc/<pid>/cmdline). The rc/systemd unit points here. */
static const char *read_token_file(const char *path) {
	FILE *f = fopen(path, "r");
	if (!f) die("cannot open --token-file");
	static char tok[512];
	if (!fgets(tok, sizeof tok, f)) die("empty --token-file");
	fclose(f);
	size_t n = strlen(tok);
	while (n && (tok[n-1] == '\n' || tok[n-1] == '\r' || tok[n-1] == ' ' || tok[n-1] == '\t'))
		tok[--n] = 0;
	if (n == 0) die("empty --token-file");
	return tok;
}

static const char *need(const char *v, const char *name) {
	if (!v) { fprintf(stderr, "navfeeder: missing required --%s\n", name); exit(2); }
	return v;
}

static const char *split_hostport(char *s) {
	char *c = strrchr(s, ':');
	if (!c) die("--server: expected host:port");
	*c = 0;
	return c + 1;
}

static void usage(void) {
	fprintf(stderr,
		"navfeeder — navlistener edge feeder (UBX raw nav frames over GNF1/TLS)\n"
		"usage: navfeeder --server host:port --source (/dev/ttyACM0 | host:port) --station ID\n"
		"                 (--token TOK | --token-file F) [--cert C --key K] [options]\n\n"
		"  --server host:port    the collector's authenticated push endpoint\n"
		"  --source SRC          /dev/ttyACM0 (serial) or host:port (TCP bridge to the receiver)\n"
		"  --baud N              serial baud when --source is a device path (default 460800)\n"
		"  --station ID          this observer's station id (also the mTLS cert's DNS SAN)\n"
		"  --feed ubx            feed type (only 'ubx' is implemented today; default ubx)\n"
		"  --token TOK           bearer token (prefer --token-file so it stays out of argv)\n"
		"  --token-file F        read the bearer token from a file\n"
		"  --cert C --key K      mTLS client certificate + private key (PEM)\n"
		"  --ca F                CA bundle to verify the collector (default: system store)\n"
		"  --spool N             in-memory ring capacity in frames (default 65536)\n"
		"  --spool-file F        disk spool path (lossless past the RAM ring; survives an\n"
		"                        orderly reboot only on PERSISTENT storage — not tmpfs, regression fix)\n"
		"  --spool-disk-mb N     disk spool cap in MiB (default 256)\n"
		"  --zstd                request zstd DATA-stream compression (collector must confirm)\n"
		"  --insecure            skip TLS verification (dev only)\n");
}

/* signal_thread waits for SIGTERM/SIGINT (blocked in every other thread) and, on an orderly
 * stop/reboot, spills every ring-resident (unacked) frame to the disk spool oldest-first,
 * then one final fsync, then _exit(0). Without this, `service stop`/`systemctl
 * restart`/an orderly reboot delivers SIGTERM whose default action kills the process
 * instantly, losing every unacked RAM-ring frame — up to the full spool_cap newest backlog
 * when stopped mid-outage (the disk tier holds only the OLDEST overflow, so the newest
 * frames die with the process). The single shutdown fsync is a wear-acceptable one-off; the
 * work is bounded (≤ spool_cap frames) and fits the 90 s systemd/procd stop timeout. */
static void *signal_thread(void *arg) {
	(void)arg;
	sigset_t set;
	sigemptyset(&set);
	sigaddset(&set, SIGTERM);
	sigaddset(&set, SIGINT);
	int sig = 0;
	sigwait(&set, &sig);
	log_msg("shutdown signal %d; flushing unacked ring to disk spool", sig);
	if (g_spool.path) {
		pthread_mutex_lock(&g_spool.mu);
		size_t idx = g_spool.head;
		for (size_t i = 0; i < g_spool.count; i++) {
			struct frame *fr = &g_spool.ring[idx];
			disk_put(&g_spool, fr->seq, fr->data, fr->len);
			idx = (idx + 1) % g_spool.cap;
		}
		if (g_spool.disk_w) { fflush(g_spool.disk_w); fsync(fileno(g_spool.disk_w)); }
		pthread_mutex_unlock(&g_spool.mu);
	}
	_exit(0);
	return NULL;
}

int main(int argc, char **argv) {
	// OpenSSL's socket BIO writes with plain write()/send(), no
	// MSG_NOSIGNAL; a write to a collector that has already closed its end
	// (routine restart/deploy) raises SIGPIPE, whose default action kills the
	// process outright, losing the entire unacked RAM ring. Ignore it so the
	// write instead fails with EPIPE and the normal reconnect path handles it.
	signal(SIGPIPE, SIG_IGN);
	// block SIGTERM/SIGINT in main (and thus every thread it later spawns) so they are
	// delivered only to the dedicated signal_thread, which flushes the ring to disk before
	// exiting. Set before any pthread_create so the block is inherited.
	sigset_t block;
	sigemptyset(&block);
	sigaddset(&block, SIGTERM);
	sigaddset(&block, SIGINT);
	if (pthread_sigmask(SIG_BLOCK, &block, NULL) != 0)
		die("failed to block shutdown signals for the flush thread");
	struct opts o; memset(&o, 0, sizeof o);
	o.feed = "ubx";
	o.baud = 460800;
	o.spool_cap = 65536;
	o.disk_max_bytes = 256ull * 1024 * 1024;
	char *server = NULL;
	for (int i = 1; i < argc; i++) {
		const char *a = argv[i];
		if (!strcmp(a, "--server") && i+1 < argc) server = argv[++i];
		else if (!strcmp(a, "--source") && i+1 < argc) o.source = argv[++i];
		else if (!strcmp(a, "--baud") && i+1 < argc) o.baud = atoi(argv[++i]);
		else if (!strcmp(a, "--token") && i+1 < argc) o.token = argv[++i];
		else if (!strcmp(a, "--token-file") && i+1 < argc) o.token = read_token_file(argv[++i]);
		else if (!strcmp(a, "--station") && i+1 < argc) o.station = argv[++i];
		else if (!strcmp(a, "--feed") && i+1 < argc) o.feed = argv[++i];
		else if (!strcmp(a, "--ca") && i+1 < argc) o.ca = argv[++i];
		else if (!strcmp(a, "--cert") && i+1 < argc) o.cert = argv[++i];
		else if (!strcmp(a, "--key") && i+1 < argc) o.key = argv[++i];
		else if (!strcmp(a, "--spool") && i+1 < argc) o.spool_cap = strtoul(argv[++i], NULL, 10);
		else if (!strcmp(a, "--spool-file") && i+1 < argc) o.spool_file = argv[++i];
		else if (!strcmp(a, "--spool-disk-mb") && i+1 < argc) o.disk_max_bytes = strtoull(argv[++i], NULL, 10) * 1024 * 1024;
		else if (!strcmp(a, "--zstd")) o.zstd = 1;
		else if (!strcmp(a, "--insecure")) o.insecure = 1;
		else if (!strcmp(a, "-h") || !strcmp(a, "--help")) { usage(); return 0; }
		else { fprintf(stderr, "navfeeder: unknown arg %s\n", a); usage(); return 2; }
	}
	server = (char *)need(server, "server");
	need(o.source, "source");
	need(o.station, "station");
	if (strcmp(o.feed, "ubx") != 0)
		die("only --feed ubx is implemented today (sbf/rtcm source modes are deferred)");
	/* a bearer token is ALWAYS required; mTLS (--cert/--key) is additive, raising the
	 * trust tier (DESIGN §3: "token + optional mTLS"). The collector's only authenticator
	 * matches the token hash, so a cert-only feeder would send "token":"" and be rejected
	 * `unauthorized` in a permanent 30 s loop. */
	if (!o.token) die("a bearer token (--token/--token-file) is required; --cert/--key adds mTLS on top");
	if ((o.cert != NULL) != (o.key != NULL)) die("--cert and --key must be given together");
	if (o.spool_cap < 1) o.spool_cap = 1;
	o.server_port = split_hostport(server); o.server_host = server;

	SSL_library_init();
	SSL_load_error_strings();
	SSL_CTX *ctx = make_ctx(&o);
	spool_init(&g_spool, o.spool_cap, o.spool_file, o.disk_max_bytes);

	/* start the shutdown-flush signal thread once the spool exists. SIGTERM/SIGINT
	 * remain blocked in every other thread, so proceeding after a create failure would make
	 * the process immune to graceful shutdown as well as losing the promised ring flush.
	 * Retry transient boot-time resource failures before starting the receiver producer. */
	pthread_t sigthr;
	int sig_backoff = 1;
	while (pthread_create(&sigthr, NULL, signal_thread, NULL) != 0) {
		log_msg("failed to start signal thread; retrying in %ds", sig_backoff);
		sleep(sig_backoff);
		if ((sig_backoff *= 2) > 30) sig_backoff = 30;
	}

	/* a failed pthread_create here (OOM at boot) previously left `prod`
	 * indeterminate and the process running with no source thread ever reading the
	 * receiver -- silently half-dead while still reconnect-logging like a healthy
	 * feeder. Retry with backoff instead of die()ing: boot-time OOM is typically
	 * transient (heap fragmentation easing as the OS settles), and "never exit" is
	 * this file's rule throughout. */
	pthread_t prod;
	int prod_backoff = 1;
	while (pthread_create(&prod, NULL, producer_thread, &o) != 0) {
		log_msg("failed to start producer thread; retrying in %ds", prod_backoff);
		sleep(prod_backoff);
		if ((prod_backoff *= 2) > 30) prod_backoff = 30;
	}

	int backoff = 1;
	for (;;) {
		time_t t0 = time(NULL);
		int rc = serve_collector(ctx, &o);
		/* a session that authenticated and ran at least USEFUL_CONN_S was useful —
		 * reset the backoff so a routine collector restart weeks later reconnects in ~1s
		 * rather than the 30s cap the backoff otherwise monotonically climbs to over the
		 * process lifetime (it was reset only on an auth reject before). Mirrors the producer
		 * thread's regression fix usefulness reset and the Go collector's regression fix dial-side reset. Auth
		 * reject (-2) still backs off hard. */
		if (rc != -2 && (time(NULL) - t0) >= USEFUL_CONN_S) backoff = 1;
		int wait = rc == -2 ? 30 : backoff;
		log_msg("reconnecting in %ds", wait);
		sleep(wait);
		if (rc == -2) backoff = 1;
		else { if ((backoff *= 2) > 30) backoff = 30; }
	}
	return 0;
}
