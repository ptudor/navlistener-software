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
 * "the receiver must never go down"). The collector ACKs the DURABLE watermark ( * regression fix, 2026-07-31): the highest seq through which every received frame has been durably
 * resolved — committed to the historian, deduped against an already-committed claim, or
 * classified unfixable/never-persistable. Pruning the ring up to that seq is therefore
 * safe against a collector-side DB outage: the ack simply stalls, this spool holds the
 * frames, and the ACK_STALL_S watchdog cycles the connection so reconnect replay
 * redelivers them once the collector recovers. (A collector running without a historian
 * acks on receipt — live-only mode; the spool contract is then best-effort by explicit
 * configuration, not by accident.) gap rule is unchanged: acks skip past
 * sequences the collector never received. On overflow the OLDEST
 * frame spills to a disk spool (--spool-file) rather
 * than being dropped; the spool is recovered and replayed on restart, so an outage longer
 * than RAM, or an ORDERLY router reboot, still loses nothing. Disk-spooled frames are
 * fflush()'d but deliberately not fsync()'d — a bounded flash-wear trade for the
 * fleet's mips/SBC hardware, not an oversight — so an UNCLEAN power loss can still drop the
 * page-cache tail that hadn't reached disk yet; spool_recover's torn-tail scan handles that
 * cleanly (no corruption, just a shorter replay), it just isn't zero-loss for a power cut the
 * way it is for `reboot`/`poweroff`. With --zstd the feeder→collector DATA
 * stream is zstd-compressed (negotiated in the handshake; ~3–4:1 on the repetitive nav
 * bitstream); ACKs stay plaintext.
 *
 * Wire (must match ../go/internal/wire/wire.go):
 *   stream = "GNF1" then frames [1B type][4B BE len][payload]
 *   HELLO(0x01) {token,station,feed,sw,session,zstd?} -> WELCOME(0x02){ok,zstd?}
 * session (regression fix, REQUIRED): the boot identity half of the collector's
 *     replay-dedup key (observer, session, seq) — fresh per sequence-space restart,
 *     persisted in the disk spool header across restarts that recover the spool.
 *   DATA(0x03)  [8B BE seq][record];  ACK(0x04) [8B BE seq]    (DATA zstd-streamed if negotiated)
 *   the DATA record = [8B BE recv_unix_ns][gnssId][svId][sigId][freqId][frame_type][raw…]
 *   raw = the native nav words serialized big-endian (the collector reads them back BE).
 *   A telemetry record (frame_type < 0x10, §6.2) reuses the same DATA frame with a zeroed
 *   gnssId/svId/sigId/freqId and a type-specific body (see emit_monrf/emit_navsat).
 *
 * Still deferred in this executable: SBF/RTCM source parsers (the standalone feeder reads
 * u-blox only, although the collector's GNF1 endpoint also accepts RTCM records), mTLS
 * enrollment tooling, and the ATECC SIGNED_DATA (0x07) hardware tier.
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
#include <limits.h>
#include <poll.h>
#include <signal.h>
#include <time.h>
#include <termios.h>
#include <pthread.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <sys/time.h>
#include <sys/stat.h>
#include <dirent.h>
#include <netdb.h>
#include <arpa/inet.h>
#include <openssl/ssl.h>
#include <openssl/err.h>
#include <openssl/x509v3.h>
#include <zstd.h>
#include "../common/endpoint_fallback.h"

#define MAGIC "GNF1"
#define F_HELLO 0x01
#define F_WELCOME 0x02
#define F_DATA 0x03
#define F_ACK 0x04
#define F_PING 0x05
#define F_PONG 0x06
#define MAX_FRAME (1u << 20)
#define RECORD_HDR 13         /* recv_ns(8) + gnssId + svId + sigId + freqId + frame_type */
/* HELLO_CAP mirrors the collector's reception-side pre-auth cap
 * (internal/ingest/push.go helloMaxLen). TOKEN_CAP is the documented maximum raw
 * bearer token; both replace the old 512-byte escape buffer and 1024-byte HELLO
 * buffer that silently shortened credentials the collector explicitly accepts
 *. Neither enlarges the wire maximum: 4096 is what the collector
 * already accepts. */
#define HELLO_CAP 4096
#define TOKEN_CAP 1024

#define MAX_RAW 1024          /* numWords is a u8 → ≤ 1020 raw bytes; round up */
#define GNF_RECORD (RECORD_HDR + MAX_RAW)
#define DRAIN_BATCH 512
#define KEEPALIVE_S 30        /* PING when idle this long, to stay under the collector's idle timeout */
#define ACK_STALL_S 600 /* cycle the connection when frames are outstanding and the
                               * durable-ack watermark has not moved this long — reconnect replay is the
                               * only path that redelivers batches the collector shed during a DB outage.
                               * 10 min: far above the collector's ~35 s flush-retry budget and ack
                               * cadence (no churn on a healthy or briefly-blipping historian), small
                               * enough that recovery-to-redelivery latency stays minutes-scale. */
#define USEFUL_CONN_S 3 /* a source connection must survive this long, or emit >=1 frame,
                                * before it resets producer_thread's backoff (else an accept-then-close
                                * peer retries at ~1 Hz forever) */
#define UBX_STATS_S 300 /* cadence of the producer's recognized/delivered stats line.
                                * 5 min is quiet enough for syslog on a router that runs for months,
                                * and fast enough that "link alive, nothing spooling" is noticed
                                * within one operator glance rather than at the next disconnect. */

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
	int configure_ubx;     /* 0 = passive; 1/2/3 = receiver UART1/UART2/USB */
	const char *token, *station, *feed, *ca;
	const char *cert, *key; /* mTLS client cert + key (PEM); one DNS SAN = the station */
	const char *spool_file; /* NULL = in-memory only (drop-oldest on overflow) */
	int insecure;
	int zstd; /* request zstd stream compression (the collector must confirm) */
	size_t spool_cap;
	uint64_t disk_max_bytes;
};

/* tls_io serializes every operation on the shared SSL object. The reader polls the socket
 * without this lock and holds it only for SSL_pending/SSL_read, so it never monopolizes the
 * writer while waiting for ACK traffic. */
struct tls_io {
	SSL *ssl;
	int fd;
	pthread_mutex_t mu;
};

/* conn is the write side of a collector connection: a TLS socket, optionally with a zstd
 * compression stream over the feeder→collector (DATA) direction. ACKs back are plaintext
 * and read separately. One conn lives per connection in the consumer thread. */
struct conn {
	struct tls_io *io;
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
	char session[65]; /* immutable: recovered spools are replay-only */
	struct spool *next;
	uint64_t retained_bytes; /* reservation against the shared disk budget */
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
	/* disk_gen counts file shrink/replace events — the unlink in disk_maybe_delete and
	 * the boundary repair in disk_rollback. disk_drain's per-connection read
	 * cursor is tagged with the generation it scanned; a mismatch forces a full rescan,
	 * since bytes below the cursor may have been truncated away and re-appended with
	 * different records. Mutated under mu. */
	uint64_t disk_gen;
	/* replay spools only: monotonic second the file was first found fully acked
	 * yet not removable (see replay_retire_stuck); 0 = not stuck */
	time_t retire_stuck_since;
	pthread_mutex_t mu;
	/* set only after pthread_mutex_init succeeded. spool_free must
	 * never pthread_mutex_destroy an object that was never initialized, and no
	 * lock/unlock may run against one either — that is undefined behavior across
	 * the producer, consumer, ACK and shutdown threads. */
	int mu_ready;
};

static struct spool g_spool;
static struct spool *g_replays;
// g_disconnected is written by the reader thread and read by the consumer/drain
// threads. `volatile` is not a C11 synchronization primitive (concurrent unsynchronized
// access is UB); atomic_int gives well-defined cross-thread visibility. Plain =/== on an
// atomic_int are seq-cst atomic operations, so the existing call sites need no change.
static atomic_int g_disconnected;

/* g_session belongs only to this process's newly captured records. Recovered files
 * retain their own session and are never extended with new observations. */
static char g_session[65];

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

/* monotonic_s is the clock for every INTERVAL comparison — keepalive pacing, the
 * useful-connection windows, backoff-reset decisions, log-throttle spacing.
 * The fleet is RTC-less OpenWrt/mips/SBC hardware whose CLOCK_REALTIME is STEPPED
 * (possibly backward, possibly by hours) at the first NTP/GNSS sync after boot: with
 * wall-clock intervals, a backward step makes `now - last_tx` negative, so no F_PING is
 * sent for the step's magnitude and the collector's idle timeout drops the connection;
 * the same window misclassifies healthy sessions as not-useful and climbs backoff to
 * the cap. CLOCK_MONOTONIC is immune to steps by definition. now_unix_ns() deliberately
 * stays CLOCK_REALTIME — it is the on-wire reception timestamp (a forensic wall-clock
 * fact) — as do log_msg's human-readable header timestamps. */
static time_t monotonic_s(void) {
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return ts.tv_sec;
}

/* sleep_with_jitter sleeps s seconds plus 0..25% : a collector redeploy fails
 * every connected feeder at the same instant, and identical deterministic backoff
 * ladders would then re-attempt TLS handshakes (the most expensive per-connection event
 * on both ends) in fleet-synchronized bursts. The monotonic clock's tv_nsec is a
 * per-process phase — enough entropy for herd-breaking without PRNG state. Only the
 * collector reconnect path uses this; the receiver-source backoff stays plain sleep()
 * (a local device, no herd to break). */
/* RECONNECT_BACKOFF_MAX_S is the collector reconnect ladder's ceiling: the doubling
 * cap and the auth-reject wait in main(), and the clamp sleep_with_jitter applies to
 * its own argument. Named so the helper's bound and the ladder that feeds
 * it cannot drift apart. */
#define RECONNECT_BACKOFF_MAX_S 30

static void sleep_with_jitter(unsigned s) {
	struct timespec ts;
	/* bound s inside the helper rather than trusting the caller. The jitter
	 * base below computes s * 250 in a signed long; on the ILP32 fleet targets (mips,
	 * armhf) long is 32-bit, so s > ~8.59M seconds overflows it — signed overflow is
	 * UB, not a wrap. Unreachable from today's only caller (the ladder caps wait at
	 * RECONNECT_BACKOFF_MAX_S), which is exactly why a future second caller would not
	 * think to check. Clamping is free and keeps the helper self-contained. */
	if (s > (unsigned)RECONNECT_BACKOFF_MAX_S) s = (unsigned)RECONNECT_BACKOFF_MAX_S;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	/* Modulo the FULL tv_nsec range (verification finding, this pass): dividing down
	 * to milliseconds first would cap the entropy at [0,999] ms, flattening the
	 * jitter on exactly the top backoff rungs a post-outage fleet sits at (25% of
	 * 30 s should be up to 7.5 s, not 1 s). tv_nsec <= 999999999 fits 32-bit long. */
	unsigned jitter_ms = (unsigned)(ts.tv_nsec % ((long)s * 250L + 1L));
	struct timespec d = { (time_t)s + jitter_ms / 1000, (long)(jitter_ms % 1000) * 1000000L };
	while (nanosleep(&d, &d) == -1 && errno == EINTR)
		;
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

/* NAVSPO01 remains byte-compatible: [magic:8][session length:1][session:64],
 * then [seq:8 BE][len:4 BE][record]. regression fix migrates valid prior-run files to
 * <path>.replay.<session> and replays them on separate GNF1 connections. New captures
 * always use a fresh session at <path>; no recovered sequence space is extended.
 * Pre-session headerless files remain unsupported by the documented policy. */
#define SPOOL_MAGIC "NAVSPO01"
#define SPOOL_MAGIC_LEN 8
#define SPOOL_SESSION_CAP 64
#define SPOOL_HDR_LEN (SPOOL_MAGIC_LEN + 1 + SPOOL_SESSION_CAP)
#define MAX_REPLAY_FILES 256

/* Do not substitute a time/PID identity when entropy is unavailable: RTC-less
 * restarts can repeat both. Retry before capturing anything in an uncertain space. */
static void session_init(void) {
	unsigned char b[16];
	for (;;) {
		FILE *f = fopen("/dev/urandom", "rb");
		int ok = f && fread(b, 1, sizeof b, f) == sizeof b;
		if (f) fclose(f);
		if (ok) break;
		log_msg("session entropy unavailable; waiting before new captures");
		sleep(1);
	}
	for (size_t i = 0; i < sizeof b; i++)
		snprintf(g_session + 2 * i, 3, "%02x", b[i]);
}

/* session_charset_ok mirrors the collector's wire.ValidSession ([A-Za-z0-9._-]):
 * a session adopted from a disk header lands verbatim inside the HELLO JSON, so
 * a corrupt header must never smuggle quotes/control bytes into the handshake. */
static int session_charset_ok(const char *s, size_t n) {
	if (n == 0 || n > SPOOL_SESSION_CAP) return 0;
	for (size_t i = 0; i < n; i++) {
		char c = s[i];
		if (!((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		      (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'))
			return 0;
	}
	return 1;
}

/* Recovery returns 1 for a validated replay-only prefix, 0 for absent/empty/
 * explicitly unsupported legacy files, and -1 for uncertainty. Only clean EOF can
 * authorize torn-tail repair. Read errors and corruption preserve every original
 * byte and disable this file's recovery, with an operator-visible diagnostic. */
static int spool_recover(struct spool *s) {
	FILE *r = fopen(s->path, "rb");
	if (!r) {
		if (errno == ENOENT) return 0;
		log_msg("disk spool recovery open failed (%s): %s; preserving evidence", s->path, strerror(errno));
		return -1;
	}
	unsigned char fhdr[SPOOL_HDR_LEN];
	size_t got = fread(fhdr, 1, sizeof fhdr, r);
	if (ferror(r)) goto uncertain;
	if (got < 8 || memcmp(fhdr, SPOOL_MAGIC, 8) != 0) {
		/* Recognize only a plausible pre-session record header as unsupported
		 * legacy. Unknown magic/truncated NAVSPO headers may be corrupt evidence. */
		if (got < 12 || !rd_be64(fhdr) || rd_be32(fhdr+8) == 0 || rd_be32(fhdr+8) > GNF_RECORD)
			goto corrupt;
		/* Validate the entire recognizable legacy stream before discarding it;
		 * an I/O error later in that file is still unread evidence. */
		if (fseeko(r, 0, SEEK_SET) != 0) goto uncertain;
		uint64_t previous = 0;
		for (;;) {
			unsigned char hdr[12], data[GNF_RECORD];
			got = fread(hdr, 1, sizeof hdr, r);
			if (ferror(r)) goto uncertain;
			if (!got) break;
			if (got != sizeof hdr || rd_be64(hdr) <= previous || !rd_be32(hdr+8) || rd_be32(hdr+8) > GNF_RECORD) goto corrupt;
			previous = rd_be64(hdr);
			got = fread(data, 1, rd_be32(hdr+8), r);
			if (ferror(r)) goto uncertain;
			if (got != rd_be32(hdr+8)) goto corrupt;
		}
		fclose(r);
		log_msg("disk spool lacks a valid session header (pre-session build); discarding unsupported legacy file");
		return (unlink(s->path) == 0 || errno == ENOENT) ? 0 : -1;
	}
	if (got != sizeof fhdr || !session_charset_ok((char *)fhdr+9, fhdr[8])) goto corrupt;
	uint64_t last = 0, good = SPOOL_HDR_LEN, count = 0;
	int torn = 0;
	for (;;) {
		unsigned char hdr[12], data[GNF_RECORD];
		got = fread(hdr, 1, sizeof hdr, r);
		if (ferror(r)) goto uncertain;
		if (got != sizeof hdr) { torn = got != 0; break; }
		uint64_t seq = rd_be64(hdr);
		uint32_t len = rd_be32(hdr+8);
		if (seq <= last || len < RECORD_HDR || len > GNF_RECORD) goto corrupt;
		got = fread(data, 1, len, r);
		if (ferror(r)) goto uncertain;
		if (got != len) { torn = 1; break; }
		last = seq;
		good += sizeof hdr + len;
		count++;
	}
	fclose(r);
	if (!count) {
		log_msg("disk spool has no complete record; discarding empty/torn file (fresh session kept)");
		return (unlink(s->path) == 0 || errno == ENOENT) ? 0 : -1;
	}
	if (torn && truncate(s->path, (off_t)good) != 0) {
		log_msg("disk spool tail repair failed (%s): %s; preserving evidence", s->path, strerror(errno));
		return -1;
	}
	memcpy(s->session, fhdr+9, fhdr[8]);
	s->session[fhdr[8]] = 0;
	s->seq = s->disk_max_seq = last;
	s->disk_bytes = s->retained_bytes = good;
	s->disk_append_disabled = 1;
	log_msg("recovered disk spool: %llu frame(s), session %s, replay only", (unsigned long long)count, s->session);
	return 1;
corrupt:
	log_msg("disk spool corrupt (%s); recovery disabled, preserving evidence", s->path);
	fclose(r);
	return -1;
uncertain:
	log_msg("disk spool recovery read failed (%s): %s; preserving evidence", s->path, strerror(errno));
	fclose(r);
	return -1;
}

/* spool_init returns 0 on success, -1 on failure, and publishes nothing usable
 * unless BOTH the ring allocation and the mutex creation succeeded
 *. It previously returned void and ignored pthread_mutex_init's
 * result, so resource exhaustion produced a spool whose every later lock, unlock
 * and destroy ran against an uninitialized mutex — undefined behavior shared by
 * the producer, consumer, ACK and shutdown threads, i.e. a recoverable startup
 * failure turned into possible crashes or spool/ring corruption. On failure the
 * partial allocation is released and `path` is left for the caller to free, so no
 * spool file is touched. */
static int spool_init(struct spool *s, size_t cap, const char *path, uint64_t disk_max_bytes) {
	memset(s, 0, sizeof *s);
	s->ring = calloc(cap, sizeof *s->ring);
	if (!s->ring) return -1;
	if (pthread_mutex_init(&s->mu, NULL) != 0) {
		free(s->ring);
		s->ring = NULL;
		return -1;
	}
	s->mu_ready = 1;
	s->cap = cap;
	s->path = path;
	s->disk_max_bytes = disk_max_bytes;
	memcpy(s->session, g_session, sizeof s->session);
	return 0;
}

/* sync_parent_dir durably commits the directory entry of `path`.
 * fsync on a file makes its CONTENT durable, not its NAME: a spool file created
 * by this flush can still vanish on an unclean shutdown unless the containing
 * directory is synced too, and recovery finds the spool only by name. Returns 0
 * on success. */
static int sync_parent_dir(const char *path) {
	char buf[PATH_MAX];
	if (snprintf(buf, sizeof buf, "%s", path) >= (int)sizeof buf) return -1;
	char *slash = strrchr(buf, '/');
	if (!slash) { buf[0] = '.'; buf[1] = 0; }
	else if (slash == buf) buf[1] = 0; /* the file lives in "/" */
	else *slash = 0;
	int fd = open(buf, O_RDONLY);
	if (fd < 0) return -1;
	int rc = fsync(fd);
	close(fd);
	return rc;
}

/* Link + directory fsync before unlink keeps the old name recoverable until its
 * replay name is durable. A crash between the two names only causes dedup-safe
 * replay. Never overwrite an existing archive or append to a file we cannot move. */
static int archive_spool(struct spool *s, const char *dir) {
	char archived[PATH_MAX];
	if (snprintf(archived, sizeof archived, "%s.replay.%s", s->path, s->session) >= (int)sizeof archived) return -1;
	if (link(s->path, archived) != 0) {
		struct stat a, b;
		if (errno != EEXIST || stat(s->path, &a) || stat(archived, &b) || a.st_dev != b.st_dev || a.st_ino != b.st_ino) return -1;
	}
	int fd = open(dir, O_RDONLY);
	if (fd < 0) return -1;
	int rc = fsync(fd);
	if (!rc) rc = unlink(s->path);
	if (!rc) rc = fsync(fd);
	close(fd);
	if (rc) return -1;
	free((void *)s->path);
	s->path = strdup(archived);
	if (!s->path) die("out of memory for replay path");
	return 0;
}

static void spool_free(struct spool *s) {
	if (s->mu_ready) pthread_mutex_destroy(&s->mu); /* regression fix */
	free(s->ring);
	free((void *)s->path);
	free(s);
}

/* Single-threaded startup. Bound retained replay metadata and reserve all archive
 * bytes against the same configured disk cap. On any uncertain file/directory,
 * preserve it and continue fresh RAM capture with disk overflow disabled. */
static void spool_recover_all(struct spool *current) {
	if (!current->path) return;
	char dir[PATH_MAX], prefix[PATH_MAX];
	if (strlen(current->path) >= sizeof dir) goto failed;
	strcpy(dir, current->path);
	char *slash = strrchr(dir, '/');
	const char *base = slash ? slash+1 : current->path;
	if (snprintf(prefix, sizeof prefix, "%s.replay.", base) >= (int)sizeof prefix) goto failed;
	if (slash) { if (slash == dir) slash[1] = 0; else *slash = 0; }
	else strcpy(dir, ".");
	DIR *d = opendir(dir);
	if (!d) goto failed;
	unsigned files = 0;
	uint64_t retained = 0;
	int failed = 0;
	struct dirent *e;
	for (;;) {
		errno = 0;
		e = readdir(d);
		if (!e) { if (errno) failed = 1; break; }
		if (strncmp(e->d_name, prefix, strlen(prefix))) continue;
		if (++files > MAX_REPLAY_FILES) { failed = 1; break; }
		char path[PATH_MAX];
		if (snprintf(path, sizeof path, "%s/%s", dir, e->d_name) >= (int)sizeof path) { failed = 1; break; }
		struct spool *old = calloc(1, sizeof *old);
		if (!old) { failed = 1; break; }
		char *dup = strdup(path);
		/* fail closed rather than publish a spool with no usable
		 * mutex. The archive file itself is untouched, so a later start retries it;
		 * `failed` disables disk append for this run. */
		if (!dup || spool_init(old, 1, dup, 0) != 0) {
			free(dup);
			free(old);
			failed = 1;
			break;
		}
		int rc = spool_recover(old);
		if (rc == 1) { old->next = g_replays; g_replays = old; retained += old->retained_bytes; }
		else { if (rc < 0) failed = 1; spool_free(old); }
	}
	closedir(d);
	struct spool *old = calloc(1, sizeof *old);
	if (!old) goto failed;
	char *curdup = strdup(current->path);
	if (!curdup || spool_init(old, 1, curdup, 0) != 0) { /* regression fix */
		free(curdup);
		free(old);
		goto failed;
	}
	int rc = spool_recover(old);
	if (rc == 1 && files < MAX_REPLAY_FILES && archive_spool(old, dir) == 0) {
		/* The archive may already be listed after a crash between link/unlink.
		 * Duplicate sessions replay the same immutable file; keep only one. */
		int duplicate = 0;
		for (struct spool *p = g_replays; p; p = p->next)
			if (!strcmp(p->session, old->session)) duplicate = 1;
		if (duplicate) spool_free(old);
		else { old->next = g_replays; g_replays = old; retained += old->retained_bytes; }
	} else { if (rc != 0) failed = 1; spool_free(old); }
	current->disk_max_bytes = retained < current->disk_max_bytes ? current->disk_max_bytes - retained : 0;
	/* A random collision must never extend a recovered identity. */
	for (;;) {
		int collision = 0;
		for (struct spool *p = g_replays; p; p = p->next)
			if (!strcmp(p->session, current->session)) collision = 1;
		if (!collision) break;
		/* Loud, because a degenerate entropy source (a minimal container, broken
		 * early-boot RNG) would otherwise spin here silently forever. */
		log_msg("fresh session collided with a recovered replay session; minting another");
		session_init();
		memcpy(current->session, g_session, sizeof current->session);
	}
	if (!failed) return;
failed:
	current->disk_append_disabled = 1;
	log_msg("spool recovery incomplete; original files preserved, new captures use fresh RAM only; repair filesystem and restart");
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
	/* any repair invalidates drain cursors — a reader may have scanned (and
	 * even sent) a record the truncate below removes, and later appends would reuse
	 * those offsets for different records; without the generation bump the cursor
	 * would seek past them and they would never be sent. Bump even if the truncate
	 * fails (appends get disabled; a rescan is harmless and conservative). */
	s->disk_gen++;
	if (truncate(s->path, (off_t)s->disk_bytes) != 0) {
		s->disk_append_disabled = 1;
		/* throttle on the monotonic clock (a realtime step must not mute or
		 * burst the warning). last_warn == 0 means "never warned" — CLOCK_MONOTONIC
		 * starts near 0 at boot, so a plain `nowt - 0 >= 60` would suppress the first
		 * warning for the first minute of uptime on a freshly-booted router. (A warn
		 * fired in system-uptime second 0 leaves last_warn == 0 and would not throttle
		 * a second event that same second — one benign extra line, accepted. Applies
		 * to all three throttle sites.) */
		static time_t last_warn;
		time_t nowt = monotonic_s();
		if (last_warn == 0 || nowt - last_warn >= 60) {
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
	uint64_t hdr_need = (s->disk_bytes == 0) ? SPOOL_HDR_LEN : 0;
	if (s->disk_bytes + hdr_need + rec > s->disk_max_bytes) { s->disk_dropped++; return; }
	if (hdr_need) {
		/* Fresh file (first spill, or the post-ack unlink reset the accounting):
		 * write the regression fix session header before any record, under the same
		 * transactional-append rules as records — the boundary (disk_bytes)
		 * advances only after the flush succeeds, and a failure rolls the file
		 * back to empty so no torn header persists. */
		unsigned char fhdr[SPOOL_HDR_LEN];
		memset(fhdr, 0, sizeof fhdr);
		memcpy(fhdr, SPOOL_MAGIC, SPOOL_MAGIC_LEN);
		size_t slen = strlen(s->session);
		if (slen > SPOOL_SESSION_CAP) slen = SPOOL_SESSION_CAP; /* unreachable; defensive */
		fhdr[SPOOL_MAGIC_LEN] = (unsigned char)slen;
		memcpy(fhdr + SPOOL_MAGIC_LEN + 1, s->session, slen);
		if (fwrite(fhdr, 1, sizeof fhdr, s->disk_w) != sizeof fhdr || fflush(s->disk_w) != 0) {
			s->disk_dropped++;
			disk_rollback(s);
			return;
		}
		s->disk_bytes = SPOOL_HDR_LEN;
	}
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
	if (s != &g_spool || s->seq == UINT64_MAX) { s->dropped++; pthread_mutex_unlock(&s->mu); return 0; }
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
 * disk_max_seq) before trusting the ring batch. On a transient allocation failure it
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

/* CONNECT_TIMEOUT_S bounds a single connect() attempt (regression fix, the regression fix deferred
 * follow-on): a routable-but-down host (SYN silently dropped) otherwise blocks the
 * calling thread for the OS SYN-retry window (~127 s on Linux defaults) per attempt —
 * the producer or consumer sits dark that long before its retry loop even runs. The
 * bound is deliberately PER getaddrinfo CANDIDATE (a multi-homed host can take
 * N x 10 s worst-case): each address gets a full window every round instead of the
 * first black-holed one starving the rest, and the never-exit retry loop absorbs the
 * total. 10 s echoes the collector side's dialTimeout (go/internal/ingest/ingest.go),
 * which is a total budget there. */
#define CONNECT_TIMEOUT_S 10

/* parse_authority is the ONE endpoint parser for both --server and a TCP
 * --source. Both used to split at the LAST colon with no
 * bracket awareness, so a standard `[2001:db8::1]:5580` left the brackets in the
 * name handed to getaddrinfo (which then never resolves), and an unbracketed
 * `2001:db8::1:5580` silently parsed the last hextet as the port. --source
 * additionally copied into a fixed 256-byte buffer with an unchecked snprintf,
 * so a longer authority was truncated and a DIFFERENT endpoint was dialled.
 *
 * Accepts `host:port` for DNS names and IPv4, and `[v6]:port` for IPv6 literals.
 * Brackets are stripped here, so callers get the bare name resolution and
 * certificate verification want. Returns 0 on success, -1 with *why set.
 */
static int parse_authority(const char *in, char *host, size_t hostcap,
			   char *port, size_t portcap, const char **why) {
	*why = NULL;
	if (!in || !*in) { *why = "empty"; return -1; }
	for (const unsigned char *p = (const unsigned char *)in; *p; p++)
		if (*p < 0x20 || *p == 0x7f) { *why = "contains control characters"; return -1; }

	const char *hstart, *hend, *colon;
	if (in[0] == '[') {
		const char *close = strchr(in, ']');
		if (!close) { *why = "unterminated '[' in an IPv6 authority"; return -1; }
		hstart = in + 1;
		hend = close;
		colon = close + 1;
		if (*colon != ':') { *why = "expected ':port' after ']'"; return -1; }
	} else {
		colon = strrchr(in, ':');
		if (!colon) { *why = "expected host:port"; return -1; }
		/* More than one colon and no brackets is an unbracketed IPv6 literal:
		 * ambiguous, and splitting at the last colon silently eats a hextet. */
		if (strchr(in, ':') != colon) {
			*why = "ambiguous unbracketed IPv6 address; write it as [address]:port";
			return -1;
		}
		hstart = in;
		hend = colon;
	}
	size_t hlen = (size_t)(hend - hstart);
	if (hlen == 0) { *why = "empty host"; return -1; }
	if (hlen >= hostcap) { *why = "host is too long"; return -1; }

	const char *pstart = colon + 1;
	size_t plen = strlen(pstart);
	if (plen == 0) { *why = "empty port"; return -1; }
	if (plen >= portcap) { *why = "port is too long"; return -1; }
	long value = 0;
	for (const char *p = pstart; *p; p++) {
		if (*p < '0' || *p > '9') { *why = "port must be decimal"; return -1; }
		value = value * 10 + (*p - '0');
		if (value > 65535) { *why = "port out of range (1-65535)"; return -1; }
	}
	if (value < 1) { *why = "port out of range (1-65535)"; return -1; }

	memcpy(host, hstart, hlen);
	host[hlen] = 0;
	memcpy(port, pstart, plen);
	port[plen] = 0;
	return 0;
}

/* numeric_host reports whether host is an IP literal rather than a DNS name.
 * RFC 6066 §3 forbids a literal address in the TLS server_name extension, so SNI
 * is omitted for these. Certificate verification is unaffected:
 * SSL_set1_host() recognizes an IP literal and matches it against the
 * certificate's iPAddress SANs -- confirmed empirically against OpenSSL 3.6.4,
 * where a matching IP SAN verifies and a non-matching one fails with
 * X509_V_ERR_IP_ADDRESS_MISMATCH (64). */
static int numeric_host(const char *host) {
	unsigned char buf[16];
	return inet_pton(AF_INET, host, buf) == 1 || inet_pton(AF_INET6, host, buf) == 1;
}

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
		/* non-blocking connect + poll(POLLOUT) + SO_ERROR per candidate,
		 * keeping the multi-address iteration; on success the socket is returned to
		 * blocking mode (every later read/write relies on blocking semantics plus
		 * SO_RCVTIMEO/SO_SNDTIMEO). POSIX: an EINTR'd connect keeps completing
		 * asynchronously — poll for it exactly like EINPROGRESS. */
		int fl = fcntl(fd, F_GETFL);
		if (fl < 0 || fcntl(fd, F_SETFL, fl | O_NONBLOCK) != 0) { close(fd); fd = -1; continue; }
		int rc = connect(fd, rp->ai_addr, rp->ai_addrlen);
		if (rc != 0 && (errno == EINPROGRESS || errno == EINTR)) {
			struct pollfd pfd = { .fd = fd, .events = POLLOUT };
			do { rc = poll(&pfd, 1, CONNECT_TIMEOUT_S * 1000); } while (rc < 0 && errno == EINTR);
			if (rc > 0) {
				int soerr = 0; socklen_t sl = sizeof soerr;
				rc = (getsockopt(fd, SOL_SOCKET, SO_ERROR, &soerr, &sl) == 0 && soerr == 0) ? 0 : -1;
			} else {
				rc = -1; /* poll timeout (rc==0) or poll error */
			}
		}
		if (rc == 0 && fcntl(fd, F_SETFL, fl) == 0) break; /* connected, blocking restored */
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

static int ssl_write_all(struct tls_io *io, const void *buf, size_t n) {
	const unsigned char *p = buf;
	while (n > 0) {
		pthread_mutex_lock(&io->mu);
		int w = SSL_write(io->ssl, p, (int)n);
		if (w <= 0) (void)SSL_get_error(io->ssl, w);
		pthread_mutex_unlock(&io->mu);
		if (w <= 0) return -1;
		p += w; n -= (size_t)w;
	}
	return 0;
}

/* Wait without holding the per-SSL mutex. SSL_pending is inspected under the mutex because
 * buffered plaintext belongs to the same shared SSL object and must be drained even when the
 * kernel fd is no longer readable. */
static int ssl_wait_readable(struct tls_io *io) {
	pthread_mutex_lock(&io->mu);
	int pending = SSL_pending(io->ssl);
	pthread_mutex_unlock(&io->mu);
	if (pending > 0) return 1;

	struct pollfd pfd = { .fd = io->fd, .events = POLLIN };
	int rc;
	do {
		rc = poll(&pfd, 1, 2 * KEEPALIVE_S * 1000);
	} while (rc < 0 && errno == EINTR);
	if (rc <= 0) return rc;
	return (pfd.revents & (POLLIN | POLLERR | POLLHUP | POLLNVAL)) ? 1 : 0;
}

/* ssl_read_full reads exactly n bytes. Returns 0 on success, -1 on a real error or clean
 * EOF, or -2 if the socket's SO_RCVTIMEO elapsed with *no bytes of this read
 * consumed* — a "nothing to read yet, keep waiting" signal distinct from a dead connection.
 * A timeout after partial consumption returns -1 instead: -2 would make the caller restart
 * its frame parse from a torn header/payload, desyncing the ACK stream (a misparsed F_ACK
 * seq could prune unacked frames), so a peer that stalls mid-record for a full receive
 * timeout is treated as dead and the connection torn down. A blocking socket's receive
 * timeout usually surfaces through OpenSSL as SSL_ERROR_SYSCALL with errno EAGAIN/EWOULDBLOCK,
 * but some builds/BIOs report SSL_ERROR_WANT_READ for the same condition — both are treated
 * as a timeout here. */
static int ssl_read_full(struct tls_io *io, void *buf, size_t n) {
	unsigned char *p = buf;
	size_t want = n;
	while (n > 0) {
		int ready = ssl_wait_readable(io);
		if (ready <= 0) return n == want && ready == 0 ? -2 : -1;
		pthread_mutex_lock(&io->mu);
		int r = SSL_read(io->ssl, p, (int)n);
		int err = r <= 0 ? SSL_get_error(io->ssl, r) : SSL_ERROR_NONE;
		pthread_mutex_unlock(&io->mu);
		if (r <= 0) {
			if (err == SSL_ERROR_WANT_READ ||
			    (err == SSL_ERROR_SYSCALL && (errno == EAGAIN || errno == EWOULDBLOCK)))
				return n == want ? -2 : -1;
			return -1;
		}
		p += r; n -= (size_t)r;
	}
	return 0;
}

static int send_frame(struct tls_io *io, uint8_t type, const void *payload, uint32_t len) {
	unsigned char hdr[5];
	hdr[0] = type; be32(hdr+1, len);
	if (ssl_write_all(io, hdr, 5) != 0) return -1;
	if (len && ssl_write_all(io, payload, len) != 0) return -1;
	return 0;
}

/* conn_write writes len bytes to the collector, compressing through the zstd stream if
 * one is set (flushing per call so the collector decodes each frame promptly while the
 * window keeps compressing across frames). */
static int conn_write(struct conn *c, const unsigned char *buf, size_t len) {
	if (!c->cctx) return ssl_write_all(c->io, buf, len);
	ZSTD_inBuffer in = { buf, len, 0 };
	size_t rem;
	do {
		ZSTD_outBuffer out = { c->obuf, c->obuf_cap, 0 };
		rem = ZSTD_compressStream2(c->cctx, &out, &in, ZSTD_e_flush);
		if (ZSTD_isError(rem)) return -1;
		if (out.pos && ssl_write_all(c->io, c->obuf, out.pos) != 0) return -1;
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
static int read_frame(struct tls_io *io, uint8_t *type, unsigned char *buf, uint32_t cap, uint32_t *len) {
	unsigned char hdr[5];
	int rc = ssl_read_full(io, hdr, 5);
	if (rc != 0) return rc;
	uint32_t n = rd_be32(hdr+1);
	if (n > MAX_FRAME || n > cap) return -1;
	if (n && ssl_read_full(io, buf, n) != 0) return -1;
	*type = hdr[0]; *len = n;
	return 0;
}

/* reader thread: apply ACKs (pruning the spool) until the connection drops. ACK payloads
 * may carry trailing extension bytes; only the first eight bytes are the acknowledged
 * sequence, matching wire.DecodeAck on the collector side. The collector
 * socket carries an SO_RCVTIMEO (regression fix, set in tls_connect's tcp_dial call), so a genuinely
 * half-open peer (vanished, no RST) no longer leaves this thread blocked in SSL_read for the
 * full TCP retransmit window — it wakes on each timeout, checks g_disconnected (set by the
 * consumer side on its own write failure), and returns promptly instead of stalling teardown
 * while the spool fills. A timeout alone is not a disconnect signal; only a real read error
 * or clean EOF ends the loop. */
struct reader_args { struct tls_io *io; struct spool *spool; };
static void *reader_thread(void *arg) {
	struct reader_args *args = arg;
	struct tls_io *io = args->io;
	unsigned char buf[256];
	uint8_t type; uint32_t len;
	for (;;) {
		int rc = read_frame(io, &type, buf, sizeof buf, &len);
		if (rc == -2) {
			if (g_disconnected) break;
			continue;
		}
		if (rc != 0) break;
		if (type == F_ACK && len >= 8) spool_ack(args->spool, rd_be64(buf));
	}
	g_disconnected = 1;
	return NULL;
}

/* ── producer: UBX source -> spool (forever) ─────────────────────────────── */

/* frame_type maps (gnssId, sigId) to the GNF1 nav message type byte (docs/CONSTELLATIONS.md
 * §6), mirroring RawFrame.NavType() in the collector so the historian's msg_type is right.
 * The collector dispatches decoding on (gnssId, sigId), so an unmapped type (0) is still
 * decoded — the byte is a forensic label, not the dispatch key.
 *
 * this is an EXACT allow-list, not a set of constellation-family defaults. Every
 * arm must match ../testdata/gnf1_frame_type.tsv row-for-row — the golden matrix generated
 * from RawFrame.NavType(), which the collector-side test TestNavfeederFrameTypeMatrix
 * enforces against this binary end-to-end. Adding a signal here without a shipped,
 * capture-verified decoder persists frames that replay will re-decode through the wrong
 * layout; the correct label for anything unverified is 0. */
static uint8_t frame_type(unsigned gnssId, unsigned sigId) {
	switch (gnssId) {
	case 0:
		if (sigId == 0) return 0x10;
		if (sigId == 3 || sigId == 4 || sigId == 6 || sigId == 7) return 0x11;
		return 0;
	case 5:                                                   /* QZSS: shipped LNAV/CNAV only */
		if (sigId == 0) return 0x50;
		if (sigId == 4 || sigId == 5 || sigId == 8 || sigId == 9) return 0x51;
		return 0;
	case 2: /* Galileo: 0x20 labels the I/NAV page LAYOUT, which E1-B and E5b-I share;
	         * the collector dispatches on (gnssId,sigId) and defers E5b */
		if (sigId == 0 || sigId == 1 || sigId == 5 || sigId == 6) return 0x20;
		if (sigId == 3 || sigId == 4) return 0x21;
		return 0;
	case 3:                                                  /* BeiDou: shipped B1I D1 + B2a B-CNAV2 only */
		if (sigId == 0) return 0x30;
		if (sigId == 8) return 0x33;
		return 0; /* D2/B2I/B-CNAV1/B2a-companion planned: no verified decoder */
	case 6: return (sigId == 0 || sigId == 2) ? 0x40 : 0;    /* GLONASS L1/L2 OF */
	case 7: return 0;                                        /* NavIC planned: capture without false type */
	case 1: return (sigId == 0) ? 0x70 : 0; /* SBAS L1 C/A only. this arm
	                                                          * returned 0x70 for EVERY sigId, so an
	                                                          * L5-capable receiver's DFMC frames (a
	                                                          * different 250-bit layout, 0x71, doc-
	                                                          * reserved and unshipped) would have been
	                                                          * persisted as L1 — exactly what regression fix
	                                                          * exists to prevent, live in the deployed
	                                                          * feeder rather than hypothetical. */
	default: return 0;
	}
}

/* emit_sfrbx builds one GNF1 DATA record from a UBX-RXM-SFRBX payload and appends it to the
 * spool. Layout (F9/M9): gnssId, svId, sigId, freqId, numWords, reserved, version, reserved,
 * then numWords little-endian 32-bit dwrds each holding one native nav word right-aligned.
 * We re-serialize each word big-endian (the collector reads them back with BE, matching its
 * RawBytes()/bytesToWords round-trip), and stamp the host reception time.
 * Returns 1 if a record was appended to the spool, 0 if the inner payload was rejected or the
 * append failed — run_ubx counts those separately from recognized messages. */
static int emit_sfrbx(const unsigned char *p, unsigned len) {
	if (len < 8) return 0;
	unsigned gnssId = p[0], svId = p[1], sigId = p[2], freqId = p[3], numWords = p[4];
	if (numWords == 0 || 8u + numWords * 4u > len) return 0;
	unsigned rawlen = numWords * 4u;
	if (rawlen > MAX_RAW) return 0;

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
	/* report whether a record actually reached the spool. spool_append returns 0
	 * only when the record could not be stored at all (malloc failure — counted as dropped
	 * there), so this is a true delivered/not-delivered signal, distinct from run_ubx's
	 * recognized-message count. */
	return spool_append(&g_spool, rec, RECORD_HDR + rawlen) != 0;
}

/* emit_telem builds one GNF1 telemetry record (frame_type < 0x10 — a §6.2 receiver-side
 * sample) and spools it exactly like a nav frame: telemetry rides the same DATA/seq/ack/
 * replay stream. The record's gnssId/svId/sigId/freqId are zero (the sample is station-
 * scoped, keyed by the observer at the collector); the body is the type-specific payload
 * and MUST match ../go/internal/ingest/telemetry.go so the C↔Go cross-oracle stays exact.
 * Returns 1 if a record was appended. */
static int emit_telem(uint8_t type, const unsigned char *body, unsigned bodylen) {
	if (bodylen > MAX_RAW) return 0;
	unsigned char rec[GNF_RECORD];
	be64(rec, now_unix_ns());
	rec[8] = rec[9] = rec[10] = rec[11] = 0; /* gnssId/svId/sigId/freqId: unused for telemetry */
	rec[12] = type;
	memcpy(rec + RECORD_HDR, body, bodylen);
	return spool_append(&g_spool, rec, RECORD_HDR + bodylen) != 0; /* regression fix */
}

/* emit_monrf converts a UBX-MON-RF payload (F9+ RF-front-end telemetry) into a JammingStats
 * (0x05) record. Layout: version U1, nBlocks U1, reserved U1[2], then nBlocks × 24-byte
 * blocks — blockId U1, flags X1 (bits 0-1 = jammingState), antStatus U1, antPower U1,
 * postStatus U4 @4, reserved U1[4] @8, noisePerMS U2 @12, agcCnt U2 @14, jamInd U1 @16
 * (previously read @14/@16/@20 — off by the 2-byte antStatus/antPower pair, so
 * NoiseLevel got agcCnt, AGC got jamInd|ofsI<<8, and CW got magQ). Body: [ver][nBands]
 * then per band [block][agc BE16][noise BE16][cw][jamState][antStatus]. Bounds-checked.
 * Returns 1 if a record was appended. */
static int emit_monrf(const unsigned char *p, unsigned len) {
	if (len < 4) return 0;
	unsigned nBlocks = p[1];
	if (nBlocks == 0 || 4u + nBlocks * 24u > len) return 0;
	unsigned bodylen = 2u + nBlocks * 8u;
	if (bodylen > MAX_RAW) return 0;
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
	return emit_telem(F_T_JAMMING, body, bodylen);
}

/* emit_monhw converts a legacy UBX-MON-HW payload (60 bytes) into a single-band JammingStats
 * record: noisePerMS U2 @16, agcCnt U2 @18, aStatus U1 @20, flags X1 @22 (jammingState in
 * bits 2-3), jamInd U1 @45. Bounds-checked. Returns 1 if a record was appended. */
static int emit_monhw(const unsigned char *p, unsigned len) {
	if (len < 60) return 0;
	unsigned char body[2 + 8];
	body[0] = TELEM_VERSION;
	body[1] = 1;
	body[2] = 0;                            /* block 0 */
	be16(body + 3, rd_le16(p + 18));        /* agcCnt */
	be16(body + 5, rd_le16(p + 16));        /* noisePerMS */
	body[7] = p[45];                        /* jamInd (CW) */
	body[8] = (unsigned char)((p[22] >> 2) & 0x03); /* jammingState */
	body[9] = p[20];                        /* aStatus */
	return emit_telem(F_T_JAMMING, body, sizeof body);
}

/* emit_navsat converts a UBX-NAV-SAT payload into a ReceptionData (0x01) record for the
 * C/N0-vs-elevation spoofing gate. Layout: iTOW U4, version U1, numSvs U1 @5, reserved U1[2],
 * then numSvs × 12-byte blocks — gnssId U1, svId U1, cno U1 @2, elev I1 @3, …, flags X4 @8
 * (bit 3 = svUsed). Body: [ver][nSats BE16] then per sat [gnssId][svId][cno][elev i8][flags];
 * capped at MAX_TELEM_SATS. Bounds-checked. Returns 1 if a record was appended. */
static int emit_navsat(const unsigned char *p, unsigned len) {
	if (len < 8) return 0;
	unsigned numSvs = p[5];
	if (numSvs == 0 || 8u + numSvs * 12u > len) return 0;
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
	return emit_telem(F_T_RECEPTION, body, 3u + n * 5u);
}

/* Receiver setup is opt-in and writes only the volatile configuration layer. */
#include "ubx_config.h"

/* rdbuf is a small buffered reader over the source fd (serial or TCP). */
struct rdbuf {
	int fd; size_t pos, len; int quiet;
	struct ubx_config config;
	unsigned char buf[4096];
};

/* RB_QUIET_MAX bounds how many consecutive quiet periods (RB_POLL_TIMEOUT_S each — the
 * poll() window below, matched by the TCP SO_RCVTIMEO) may elapse before rb_getc gives up
 * and lets the producer re-dial — ~5 min of total silence. */
#define RB_POLL_TIMEOUT_S 5
#define RB_QUIET_MAX 60

static int rb_getc(struct rdbuf *b) {
	while (b->pos >= b->len) {
		/* Tick while scanning NMEA too: a receiver can reset without its UART
		 * bridge disconnecting, leaving sync_ubx waiting indefinitely. */
		if (ubx_config_tick(&b->config, b->fd, monotonic_s()) != 0) return -1;
		/* regression fix (the residual own comment named): gate EVERY source read behind
		 * poll(), so silence is bounded for BOTH source types. Ttys have no SO_RCVTIMEO,
		 * so the serial path's VMIN=1 blocking read was uncovered by the regression fix EAGAIN
		 * watchdog: a u-blox whose GNSS engine hangs while its USB CDC-ACM interface
		 * stays enumerated — or a wired UART that simply goes quiet — blocked the
		 * producer thread in read() indefinitely, while the independent consumer kept
		 * the collector connection alive with PINGs: a healthy-looking station carrying
		 * zero frames, forever, with no log and no re-dial. N consecutive quiet poll
		 * windows force run_ubx to return so producer_thread reopens the device.
		 * Deliberately NOT VMIN=0/VTIME=N instead: read()==0 is EOF to this reader, so a
		 * VTIME expiry would be indistinguishable from the device vanishing. */
		struct pollfd pfd = { .fd = b->fd, .events = POLLIN };
		int pr;
		do { pr = poll(&pfd, 1, RB_POLL_TIMEOUT_S * 1000); } while (pr < 0 && errno == EINTR);
		if (pr < 0) return -1;                       /* poll error */
		if (pr == 0) {                               /* quiet window, no data */
			if (++b->quiet >= RB_QUIET_MAX) {
				log_msg("source quiet for ~%d s; reopening", RB_QUIET_MAX * RB_POLL_TIMEOUT_S);
				return -1;
			}
			continue;
		}
		/* Readable — or POLLERR/POLLHUP, which read() resolves to 0/-1 below. */
		ssize_t r = read(b->fd, b->buf, sizeof b->buf);
		if (r > 0) { b->len = (size_t)r; b->pos = 0; b->quiet = 0; break; }
		if (r == 0) return -1;                       /* EOF / device closed */
		if (errno == EINTR) continue;
		/* an SO_RCVTIMEO expiry or a spurious poll wakeup (EAGAIN/EWOULDBLOCK)
		 * is NOT EOF — a healthy TCP receiver can legitimately go quiet (messages not
		 * yet enabled, mid-reboot, ser2net momentarily detached). Count it as one quiet
		 * period rather than tearing the connection into no-backoff reconnect churn;
		 * the SO_RCVTIMEO stays on the TCP dial as a second layer under the poll(). */
		if (errno == EAGAIN || errno == EWOULDBLOCK) {
			if (++b->quiet >= RB_QUIET_MAX) {
				log_msg("source quiet for ~%d s; reopening", RB_QUIET_MAX * RB_POLL_TIMEOUT_S);
				return -1;
			}
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

/* log_ubx_stats prints the producer's two counters. They are deliberately distinct:
 * `recognized` is checksum-valid UBX of a class/id we handle — the liveness/backoff signal —
 * while `delivered` is records that actually reached the spool. recognized > 0 with
 * delivered == 0 is the one combination an operator cannot otherwise see: the link is healthy
 * and the receiver is talking, but every inner payload is being rejected (a firmware/protocol
 * mismatch, a truncating bridge), so nothing is being forwarded. Call it out in words rather
 * than leaving it to be inferred from two numbers. */
static void log_ubx_stats(const char *what, unsigned long recognized, unsigned long delivered) {
	if (recognized > 0 && delivered == 0)
		log_msg("%s: recognized=%lu delivered=%lu — source is alive but NOTHING is being spooled",
			what, recognized, delivered);
	else
		log_msg("%s: recognized=%lu delivered=%lu", what, recognized, delivered);
}

/* run_ubx reads a UBX byte stream, validates each message's Fletcher checksum, and emits
 * every UBX-RXM-SFRBX to the spool. It returns when the source ends (reconnect trigger). A
 * corrupt frame is dropped and the reader resynchronises — a mid-stream connect never
 * derails it. Untrusted-input discipline (docs/INTEGRITY.md §9): every length and index is
 * bounds-checked before use. Returns nonzero if this connection proved "useful" — it
 * received >=1 checksummed UBX message of a recognized class/id, or survived
 * USEFUL_CONN_S — 0 otherwise. The counter advances after dispatch, even when a
 * message's inner payload validation makes emit_* decline to spool it; it is a
 * reconnect-backoff signal, not a delivered-frame count. A TCP bridge that
 * accepts and instantly closes (ser2net with the tty missing, port busy) makes this return
 * immediately on the very first read; producer_thread uses the return value to decide
 * whether resetting backoff is warranted, mirroring go/internal/ingest's regression fix fix.
 *
 * regression fix re-examined that policy and KEPT it: backoff exists to stop hammering a dead or
 * instantly-closing source, and a source delivering checksum-valid, recognized-class UBX is
 * alive. Reconnecting more slowly cannot fix an undecodable inner payload, so keying backoff
 * on delivered records would only punish a live-but-degraded source for something a reconnect
 * never repairs. What WAS missing is the operator's ability to see "link alive, nothing
 * spooling": `delivered` counts the records that actually reached the spool, separately, and
 * both counts are logged (periodically and at source close). Do not merge the two counters. */
static int run_ubx(int fd, int configure_ubx) {
	struct rdbuf rb = { .fd = fd, .config = { .port = configure_ubx } };
	unsigned char head[4], payload[UBX_MAX_PAYLOAD], ck[2];
	time_t start = monotonic_s(); /* interval, not wall-clock */
	time_t last_stats = start;
	unsigned long frames = 0; /* recognized class/id messages — the regression fix backoff signal */
	unsigned long delivered = 0; /* of those, records actually appended to the spool */
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
		ubx_config_ack(&rb.config, head[0], head[1], payload, len);
		if (head[0] == UBX_CLASS_RXM && head[1] == UBX_ID_SFRBX)
			{ delivered += emit_sfrbx(payload, len); frames++; }
		else if (head[0] == UBX_CLASS_MON && head[1] == UBX_ID_MONRF)
			{ delivered += emit_monrf(payload, len); frames++; }
		else if (head[0] == UBX_CLASS_MON && head[1] == UBX_ID_MONHW)
			{ delivered += emit_monhw(payload, len); frames++; }
		else if (head[0] == UBX_CLASS_NAV && head[1] == UBX_ID_NAVSAT)
			{ delivered += emit_navsat(payload, len); frames++; }
		else
			continue; /* unrecognized class/id: not counted, no stats tick needed */
		ubx_config_observed(&rb.config, monotonic_s());
		if (monotonic_s() - last_stats >= UBX_STATS_S) {
			log_ubx_stats("ubx", frames, delivered);
			last_stats = monotonic_s();
		}
	}
	log_ubx_stats("ubx session", frames, delivered);
	return frames > 0 || (monotonic_s() - start) >= USEFUL_CONN_S;
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
static int open_serial(const char *path, int baud, int configure_ubx) {
	int fd = open(path, (configure_ubx ? O_RDWR : O_RDONLY) | O_NOCTTY | O_NONBLOCK);
	if (fd < 0) return -1;
	struct termios t;
	if (tcgetattr(fd, &t) != 0) { close(fd); return -1; }
	cfmakeraw(&t);
	/* cfmakeraw() does not touch flow control (glibc or BSD). An inherited
	 * CRTSCTS on the common 3-wire hookup (RTS/CTS unwired) holds RX off waiting for a
	 * CTS that never asserts — read() stalls with a healthy receiver, presenting as a
	 * dead station at startup on a subset of the fleet. cfmakeraw already clears IXON;
	 * clear IXOFF too so the port never software-throttles the receiver either. */
#ifdef CRTSCTS
	t.c_cflag &= (tcflag_t)~CRTSCTS;
#endif
	t.c_iflag &= (tcflag_t)~IXOFF;
	speed_t sp = baud_to_speed(baud);
	if (sp == 0) { close(fd); log_msg("unsupported --baud %d", baud); return -1; }
	cfsetispeed(&t, sp);
	cfsetospeed(&t, sp);
	t.c_cflag |= (CLOCAL | CREAD);
	t.c_cc[VMIN] = 1;   /* block for at least one byte */
	t.c_cc[VTIME] = 0;
	if (tcsetattr(fd, TCSANOW, &t) != 0) { close(fd); return -1; }
	int fl = fcntl(fd, F_GETFL);
	if (fl < 0 || fcntl(fd, F_SETFL, fl & ~O_NONBLOCK) != 0) { close(fd); return -1; }
	return fd;
}

/* open_source opens the receiver: a device path (leading '/') is a serial port; otherwise a
 * host:port TCP bridge (ser2net / a receiver's raw TCP port). */
static int open_source(const struct opts *o) {
	if (o->source[0] == '/') return open_serial(o->source, o->baud, o->configure_ubx);
	/* one strict parser, and no fixed-buffer copy that could
	 * silently truncate a long authority into a different endpoint. */
	char host[NI_MAXHOST], port[16];
	const char *why = NULL;
	if (parse_authority(o->source, host, sizeof host, port, sizeof port, &why) != 0) {
		log_msg("--source %s: %s (expected /dev/... , host:port, or [v6]:port)", o->source, why);
		return -1;
	}
	return tcp_dial(host, port, 5);
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
		int useful = run_ubx(fd, o->configure_ubx);
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
			log_msg("failed to set expected TLS peer name");
			SSL_free(ssl); close(fd); return NULL;
		}
		/* RFC 6066 §3 forbids a literal address in server_name, and
		 * some servers reject an SNI that carries one. Verification is unaffected —
		 * SSL_set1_host() recognizes an IP literal and matches it against the
		 * certificate's iPAddress SANs (see numeric_host). */
		if (!numeric_host(o->server_host)) SSL_set_tlsext_host_name(ssl, o->server_host);
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
/* json_escape returns 0 on success and -1 when the escaped form would not fit.
 * it used to truncate silently, and handshake ignored that — so a
 * long bearer token was quietly shortened and presented as a DIFFERENT
 * credential, producing an endless "unauthorized" reconnect loop with nothing
 * saying why. Identity material is never truncated; the caller reports the
 * failure instead. */
static int json_escape(char *dst, size_t dstcap, const char *src) {
	if (dstcap == 0) return -1;
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
		if (di + elen + 1 > dstcap) { dst[0] = 0; return -1; }
		if (esc) { memcpy(dst + di, esc, elen); } else { dst[di] = (char)c; }
		di += elen;
	}
	dst[di] = 0;
	return 0;
}

/* build_hello renders the HELLO exactly as it goes on the wire, returning its
 * length or -1 if any part would not fit. this is one function so
 * a startup precheck and the live handshake cannot disagree about whether a
 * configured credential is presentable. The buffers are sized against the
 * collector's own documented reception cap (HELLO_CAP) rather than the old
 * 512-byte token buffer, which silently shortened credentials the collector
 * explicitly accepts and tests (a 900-byte token). Nothing here logs the token. */
static int build_hello(char *out, size_t cap, const struct opts *o, const char *session,
		       const char **why) {
	char tok_esc[TOKEN_CAP * 2 + 1], station_esc[512], feed_esc[128];
	if (json_escape(tok_esc, sizeof tok_esc, o->token ? o->token : "") != 0) {
		*why = "--token/--token-file does not fit the HELLO once JSON-escaped";
		return -1;
	}
	if (json_escape(station_esc, sizeof station_esc, o->station) != 0) {
		*why = "--station does not fit the HELLO once JSON-escaped";
		return -1;
	}
	if (json_escape(feed_esc, sizeof feed_esc, o->feed) != 0) {
		*why = "--feed does not fit the HELLO once JSON-escaped";
		return -1;
	}
	/* session : the boot identity half of the collector's replay-dedup
	 * key — REQUIRED since the 2026-07-31 contract revision (the collector rejects a
	 * HELLO without it). g_session is charset-validated ([A-Za-z0-9._-], both at
	 * mint and at spool-header adoption), so it needs no JSON escaping. */
	int n = snprintf(out, cap,
		"{\"token\":\"%s\",\"station\":\"%s\",\"feed\":\"%s\",\"sw\":\"navfeeder/1\",\"session\":\"%s\"%s}",
		tok_esc, station_esc, feed_esc, session, o->zstd ? ",\"zstd\":true" : "");
	if (n < 0 || (size_t)n >= cap) {
		*why = "the assembled HELLO exceeds the collector's accepted size";
		return -1;
	}
	return n;
}

static int handshake(struct tls_io *io, const struct opts *o, const char *session, int *zstd_ok) {
	*zstd_ok = 0;
	if (ssl_write_all(io, MAGIC, 4) != 0) return -1;
	char hello[HELLO_CAP];
	const char *why = NULL;
	int n = build_hello(hello, sizeof hello, o, session, &why);
	if (n < 0) {
		/* Unreachable: main prechecks the same construction at startup. Loud
		 * rather than a silently wrong credential in a reconnect loop. */
		log_msg("cannot build HELLO: %s", why);
		return -2;
	}
	if (send_frame(io, F_HELLO, hello, (uint32_t)n) != 0) return -1;

	unsigned char buf[1024]; uint8_t type; uint32_t len;
	if (read_frame(io, &type, buf, sizeof buf, &len) != 0) return -1;
	if (type != F_WELCOME) return -1;
	buf[len < sizeof buf ? len : sizeof buf - 1] = 0;
	/* This tiny fleet binary deliberately does not carry a JSON parser. It recognizes
	 * the compact `"ok":true` and `"zstd":true` spellings emitted by Go's
	 * encoding/json; a different GNF1 server must preserve those spellings (including
	 * no whitespace around the colon) or the feeder treats the WELCOME as rejected.
	 * Keep this coupling in mind before independently reformatting handshake JSON.
	 * this is no longer just a local note — the compact spelling is NORMATIVE
	 * in the GNF1 wire contract (docs/DESIGN.md §GNF1) and restated at the collector's
	 * write site (wire.MarshalWelcome), so the requirement lives with the spec rather
	 * than only with the code that would break. */
	if (!strstr((char *)buf, "\"ok\":true")) {
		log_msg("collector rejected handshake: %.*s", (int)len, buf);
		return -2;
	}
	/* honor "zstd":true only if WE advertised --zstd in the HELLO. The real
	 * collector only echoes the feeder's request (push.go: Zstd: h.Zstd), but a buggy or
	 * hostile-but-authenticated collector answering true to a non-zstd feeder would
	 * desync the stream (feeder writes plaintext into a collector-side zstd decoder);
	 * gate on our own intent instead of trusting a server field we can validate. */
	*zstd_ok = (o->zstd && strstr((char *)buf, "\"zstd\":true") != NULL);
	return 0;
}

/* REPLAY_RETIRE_STUCK_S bounds how long a fully acknowledged replay spool that
 * cannot be unlinked (read-only or root-squashed spool dir, immutable flag, an
 * NFS permission glitch) may block the live session. Its records are all
 * durably acknowledged, so dropping it from the replay list is dedup-safe: the
 * next process start recovers the file again, replays it (the collector dedups
 * on (observer, session, seq)), lands here again after the same bound, and says
 * so — bounded churn, logged, instead of a feeder that only ever sends PINGs
 * (astra-6 verification of regression fix). */
#define REPLAY_RETIRE_STUCK_S 30

/* replay_retire_stuck forces a stuck replay spool's retirement (disk_max_seq = 0,
 * which ends its connection and pops it from g_replays) once it has been fully
 * acked but unremovable for REPLAY_RETIRE_STUCK_S. Call after disk_maybe_delete. */
static void replay_retire_stuck(struct spool *s) {
	pthread_mutex_lock(&s->mu);
	int stuck = s->disk_max_seq != 0 && s->acked >= s->disk_max_seq; /* delete just failed */
	if (!stuck) { s->retire_stuck_since = 0; pthread_mutex_unlock(&s->mu); return; }
	time_t now = monotonic_s();
	if (s->retire_stuck_since == 0) s->retire_stuck_since = now ? now : 1; /* 0 means "never" */
	if (now - s->retire_stuck_since < REPLAY_RETIRE_STUCK_S) { pthread_mutex_unlock(&s->mu); return; }
	s->disk_max_seq = 0;
	pthread_mutex_unlock(&s->mu);
	log_msg("replay spool %s fully acknowledged but not removable for %ds; retiring it anyway "
		"(records durably acked, re-delivery after a restart is dedup-safe) -- fix the spool directory permissions",
		s->path, REPLAY_RETIRE_STUCK_S);
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
			s->disk_gen++; /* the next spill starts a new file; cursors reset */
		} else {
			/* Keeping the old accounting and disabling appends is safer than resetting
			 * disk_bytes then reopening an undeleted file at an unknown boundary. */
			delete_err = errno;
			s->disk_append_disabled = 1;
		}
	}
	pthread_mutex_unlock(&s->mu);
	if (delete_err) {
		/* regression fix-style throttle on the monotonic clock: the drain loop re-tries
		 * every ~50 ms, and a permanent EACCES must not flood the log. */
		static time_t last_warn;
		time_t nowt = monotonic_s();
		if (last_warn == 0 || nowt - last_warn >= 60) {
			last_warn = nowt ? nowt : 1;
			log_msg("acked disk spool could not be removed (%s); disk overflow disabled",
				strerror(delete_err));
		}
	}
	if (cleared_bytes >= 64 * 1024)
		log_msg("disk spool delivered and cleared (%llu KiB)", (unsigned long long)(cleared_bytes / 1024));
}

/* disk_cursor is disk_drain's per-CONNECTION resume point : the byte offset of
 * the spool file already scanned, tagged with the disk_gen it was scanned under. Owned by
 * serve_collector, initialized {0,0} for every new connection. */
struct disk_cursor { uint64_t off, gen; };

/* disk_drain sends disk-spooled frames with seq > *sent_upto (oldest first). Returns the
 * count sent, or -1 on a send failure. The disk always holds seqs older than the ring.
 *
 * cur resumes the scan where the previous call left off instead of re-reading
 * the file from byte 0 on every call. Without it, a sustained connected-overflow regime
 * (producer outpacing the uplink) re-read up to the whole 256 MiB file once per spill
 * event — severe I/O/CPU amplification on exactly the weakest (mips/flash) hardware.
 * Resuming is safe because sends are strictly forward within one connection: every
 * complete record below cur->off was either sent or skipped as seq <= *sent_upto, and
 * *sent_upto only grows, so it can never be needed again on THIS connection. Records a
 * reconnect must resend (sent-but-unacked) are covered by each connection starting a
 * fresh cursor at 0. In-process file shrink/replace events — the unlink in
 * disk_maybe_delete, the boundary repair in disk_rollback — bump disk_gen, which resets
 * the cursor to a full rescan; otherwise offsets below cur->off could be reused by
 * different records and silently skipped. A torn/short tail record stops the scan
 * WITHOUT advancing the cursor, so the (possibly completed-by-then) record is re-read
 * next call — regression fix/regression fix semantics preserved. */
static int disk_drain(struct spool *s, struct conn *c, uint64_t *sent_upto, struct disk_cursor *cur) {
	pthread_mutex_lock(&s->mu);
	const char *path = s->path;
	uint64_t dmax = s->disk_max_seq;
	uint64_t gen = s->disk_gen;
	pthread_mutex_unlock(&s->mu);
	if (cur->gen != gen) { cur->off = 0; cur->gen = gen; }
	if (!path || dmax <= *sent_upto) return 0;

	FILE *r = fopen(path, "rb");
	if (!r) {
		/* regression fix follow-up: an unopenable spool file (fd exhaustion, external
		 * unlink) is otherwise indistinguishable from "nothing to drain" while
		 * disk_max_seq stays ahead of sent_upto — the caller paces its retry
		 * (see the drain loop), and this rate-limited log makes the stall
		 * diagnosable instead of silent. Only serve_collector's thread calls
		 * this, so the static is single-threaded. Monotonic + first-warn guard:
		 * regression fix (see disk_rollback's throttle comment). */
		static time_t last_warn;
		time_t nowt = monotonic_s();
		if (last_warn == 0 || nowt - last_warn >= 60) {
			last_warn = nowt;
			log_msg("disk spool open failed (frames pending past seq %llu): %s",
				(unsigned long long)*sent_upto, strerror(errno));
		}
		return 0;
	}
	if (cur->off < SPOOL_HDR_LEN)
		cur->off = SPOOL_HDR_LEN; /* records start after the regression fix session header */
	if (fseeko(r, (off_t)cur->off, SEEK_SET) != 0) {
		cur->off = SPOOL_HDR_LEN; /* seek failure: fall back to the always-safe full rescan */
		if (fseeko(r, (off_t)cur->off, SEEK_SET) != 0) {
			fclose(r);
			return 0; /* even the header skip failed; retry next round */
		}
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
			/* Cursor deliberately NOT advanced past this record on failure: the
			 * connection is dead and the next one starts a fresh cursor anyway. */
			if (send_data(c, seq, data, len) != 0) { fclose(r); g_disconnected = 1; return -1; }
			*sent_upto = seq;
			sent++;
		}
		cur->off += 12 + (uint64_t)len; /* complete record handled (sent or skipped) */
	}
	fclose(r);
	/* the scan ended before reaching disk_max_seq on an openable file — a
	 * torn/short record (which the disk_put rollback fix now prevents from persisting,
	 * but a spool written by an older build or an external corruption could still show).
	 * Log it rate-limited (mirroring the fopen-failure path) so the otherwise-silent
	 * ping-only stall is diagnosable rather than invisible. */
	if (*sent_upto < dmax) {
		/* Monotonic + first-warn guard: regression fix (see disk_rollback's throttle comment). */
		static time_t last_warn;
		time_t nowt = monotonic_s();
		if (last_warn == 0 || nowt - last_warn >= 60) {
			last_warn = nowt;
			log_msg("disk spool drain reached seq %llu but disk_max_seq is %llu (torn record?)",
				(unsigned long long)*sent_upto, (unsigned long long)dmax);
		}
	}
	return sent;
}

/* serve_collector connects once and drains the spool until disconnect. Returns 0 when the
 * session authenticated and then ran at least USEFUL_CONN_S (a "useful" session — main
 * resets backoff, regression fix), -2 on auth rejection (back off hard), -1 otherwise. The
 * usefulness clock starts AFTER the handshake (regression fix interaction with regression fix): with
 * connect() now bounded at CONNECT_TIMEOUT_S (10 s) > USEFUL_CONN_S (3 s), the old
 * around-the-call window in main would count every slow-FAILING connect as useful and
 * reset backoff on each attempt against a down collector — the exact no-growth pathology
 * regression fix/regression fix exist to prevent. */
static int serve_collector(SSL_CTX *ctx, const struct opts *o, struct spool *s) {
	int tls_fd;
	SSL *ssl = tls_connect(ctx, o, &tls_fd);
	if (!ssl) { log_msg("collector TLS connect failed"); return -1; }
	struct tls_io io = { .ssl = ssl, .fd = tls_fd };
	if (pthread_mutex_init(&io.mu, NULL) != 0) {
		SSL_free(ssl); close(tls_fd); return -1;
	}

	struct conn c = { &io, NULL, NULL, 0, 0, 0 };
	/* regression fix (completing regression fix): the compressor is allocated BEFORE
	 * handshake() can advertise "zstd":true, which is the restructuring the old
	 * comment here said was the real fix and deferred.
	 *
	 * Why the ordering is the whole point: once the HELLO says "zstd":true and the
	 * collector answers in kind, it wraps its reader in a zstd decompressor for
	 * the rest of THIS connection (push.go's useZstd path is stream-, not
	 * frame-scoped). Allocating afterwards left only two bad options — write
	 * plaintext into that decompressor and desync the wire, or die(). die() calls
	 * exit(2), which bypasses the signal thread's orderly RAM-ring spill, so the
	 * newest unacknowledged frames were lost during exactly the memory-pressure
	 * condition that makes the ring valuable (the disk tier holds the oldest
	 * overflow, not the current ring).
	 *
	 * Allocating first removes the dilemma: nothing has been negotiated yet, so a
	 * failure is just a failed connection attempt. The outer loop's existing
	 * bounded backoff paces the retry, so persistent memory pressure
	 * cannot spin, and the spool keeps every unacknowledged frame meanwhile. The
	 * user's --zstd request is never silently downgraded to plaintext. */
	if (o->zstd) {
		c.cctx = ZSTD_createCCtx();
		c.obuf_cap = ZSTD_CStreamOutSize();
		c.obuf = malloc(c.obuf_cap);
		if (!c.cctx || !c.obuf) {
			if (c.cctx) ZSTD_freeCCtx(c.cctx);
			free(c.obuf);
			log_msg("out of memory for the zstd compressor; dropping this connection attempt "
				"(spool retained, will retry with backoff)");
			pthread_mutex_destroy(&io.mu);
			SSL_free(ssl); close(tls_fd); return -1;
		}
		ZSTD_CCtx_setParameter(c.cctx, ZSTD_c_compressionLevel, 3);
	}

	int zstd_ok = 0;
	int hs = handshake(&io, o, s->session, &zstd_ok);
	if (hs != 0) {
		if (c.cctx) ZSTD_freeCCtx(c.cctx);
		free(c.obuf);
		pthread_mutex_destroy(&io.mu);
		SSL_free(ssl); close(tls_fd); return hs == -2 ? -2 : -1;
	}
	/* regression fix gates zstd_ok on our own advertised intent, so it can only be false
	 * here if the collector declined. Release the unused compressor rather than
	 * carrying it for the life of a plaintext connection; send_data keys off
	 * c.cctx == NULL, so this is the same plaintext path as a non-zstd feeder. */
	if (!zstd_ok && c.cctx) {
		ZSTD_freeCCtx(c.cctx);
		c.cctx = NULL;
		free(c.obuf);
		c.obuf = NULL;
		c.obuf_cap = 0;
	}

	g_disconnected = 0;
	pthread_t rt;
	struct reader_args reader = { &io, s };
	if (pthread_create(&rt, NULL, reader_thread, &reader) != 0) {
		/* a failed thread create left `rt` indeterminate, and the unconditional
		 * pthread_join(rt, NULL) at the end of this function is UB on it; treat this
		 * exactly like a failed connect (log + tear down + let the outer loop retry). */
		log_msg("failed to start reader thread; treating as a connection error");
		if (c.cctx) ZSTD_freeCCtx(c.cctx);
		free(c.obuf);
		pthread_mutex_destroy(&io.mu);
		SSL_free(ssl);
		close(tls_fd);
		return -1;
	}

	time_t session_start = monotonic_s(); /* regression fix/usefulness measured post-handshake */

	/* Replay-on-reconnect: resume from the last acked sequence. */
	uint64_t sent_upto = spool_acked(s);
	uint64_t replay_base = 0;
	spool_stats(s, &replay_base, NULL, NULL, NULL);
	if (replay_base > sent_upto)
		log_msg("connected: station=%s feed=%s zstd=%d; replaying %llu unacked frame(s)",
			o->station, o->feed, zstd_ok, (unsigned long long)(replay_base - sent_upto));
	else
		log_msg("connected: station=%s feed=%s zstd=%d -> %s:%s",
			o->station, o->feed, zstd_ok, o->server_host, o->server_port);

	struct frame batch[DRAIN_BATCH];
	/* last_tx paces the F_PING keepalive and is monotonic : an NTP step
	 * backward would otherwise make `monotonic_s() - last_tx` a wall-clock delta that
	 * goes negative and mutes the keepalive for the step's magnitude, so the
	 * collector's idle timeout (idleReadTimeout, push.go) drops a healthy link. */
	time_t last_tx = monotonic_s();
	struct disk_cursor dcur = {0, 0}; /* fresh per connection */
	/* regression fix ack-stall watchdog: ACK now advances only as the collector
	 * DURABLY resolves frames (historian commit), so a healthy TCP link can carry
	 * a stalled ack stream for as long as the collector's database is down. The
	 * spool holds everything meanwhile; what needs forcing is REDELIVERY —
	 * batches the collector dropped from its RAM writer queue during the outage
	 * are re-sent only by reconnect replay. So: frames outstanding
	 * (sent_upto > acked) with zero ack progress for ACK_STALL_S ⇒ cycle the
	 * connection; the normal reconnect replays everything past the last ack into
	 * the (possibly recovered) collector. Bounded churn: at most one replay per
	 * ACK_STALL_S window during an outage, nothing when acks flow. Monotonic
	 * clock per regression fix. */
	uint64_t stall_acked = spool_acked(s);
	time_t stall_since = monotonic_s();
	while (!g_disconnected) {
		uint64_t acked_now = spool_acked(s);
		if (acked_now != stall_acked) {
			stall_acked = acked_now;
			stall_since = monotonic_s();
		} else if (sent_upto <= acked_now) {
			/* nothing outstanding — keep the stall clock parked. Without
			 * this, a connection idle >= ACK_STALL_S (silent source: receiver
			 * unplugged or no fix) satisfied the age test the instant its FIRST
			 * new frame made sent_upto > acked_now and cycled a healthy link
			 * exactly at receiver recovery. The stall window must start when
			 * frames become outstanding, not at connect. */
			stall_since = monotonic_s();
		} else if (monotonic_s() - stall_since >= ACK_STALL_S) {
			log_msg("no ack progress in %d s with frames outstanding past seq %llu; cycling connection for replay",
				ACK_STALL_S, (unsigned long long)acked_now);
			g_disconnected = 1;
			break;
		}
		disk_maybe_delete(s);
		if (s != &g_spool) replay_retire_stuck(s);
		if (s != &g_spool && s->disk_max_seq == 0) { g_disconnected = 1; break; }
		int d = disk_drain(s, &c, &sent_upto, &dcur); /* oldest unacked first (disk) */
		if (d < 0) break;                              /* disconnected during disk replay */
		if (d > 0) { last_tx = monotonic_s(); continue; } /* re-check disk before the ring */
		uint64_t disk_max_seq_now = 0;
		size_t n = spool_collect(s, sent_upto, batch, DRAIN_BATCH, &disk_max_seq_now);
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
			if (monotonic_s() - last_tx >= KEEPALIVE_S) {
				if (send_ping(&c) != 0) { g_disconnected = 1; break; }
				last_tx = monotonic_s();
			}
			usleep(50 * 1000);
			continue;
		}
		if (n == 0) {                                  /* caught up; wait for the producer */
			if (monotonic_s() - last_tx >= KEEPALIVE_S) {
				if (send_ping(&c) != 0) { g_disconnected = 1; break; }
				last_tx = monotonic_s();
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
		last_tx = monotonic_s();
	}

	/* Wake a healthy reader immediately when a replay file completes. */
	shutdown(tls_fd, SHUT_RDWR);
	pthread_join(rt, NULL);
	pthread_mutex_lock(&io.mu);
	SSL_shutdown(ssl);
	pthread_mutex_unlock(&io.mu);
	pthread_mutex_destroy(&io.mu);
	SSL_free(ssl); close(tls_fd);
	if (c.cctx) ZSTD_freeCCtx(c.cctx);
	if (c.raw)
		log_msg("zstd: %llu -> %llu bytes on the wire (%.1f%% of raw)",
			(unsigned long long)c.raw, (unsigned long long)c.comp, 100.0 * (double)c.comp / (double)c.raw);
	free(c.obuf);
	uint64_t dropped = 0, disk_dropped = 0; size_t spooled = 0;
	spool_stats(s, NULL, &dropped, &spooled, &disk_dropped);
	log_msg("disconnected (spooled=%zu dropped=%llu disk_dropped=%llu)",
		spooled, (unsigned long long)dropped, (unsigned long long)disk_dropped);
	if (s != &g_spool && s->disk_max_seq == 0) return 1;
	return (monotonic_s() - session_start >= USEFUL_CONN_S) ? 0 : -1;
}

/* ── setup ───────────────────────────────────────────────────────────────── */

static SSL_CTX *make_ctx(const struct opts *o) {
	SSL_CTX *ctx = SSL_CTX_new(TLS_client_method());
	if (!ctx) die("SSL_CTX_new failed");
	SSL_CTX_set_min_proto_version(ctx, TLS1_2_VERSION);
	/* Pin to TLS 1.2 (radiolistener regression fix). The per-connection mutex serializes every shared
	 * SSL_read/SSL_write/SSL_shutdown operation, including TLS fatal-alert writes from the read
	 * path; readiness polling happens outside that mutex. Keeping the TLS 1.2 cap also excludes
	 * post-handshake KeyUpdate and preserves the deployed protocol policy. */
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
/* the credential is the file's first line, with trailing spaces
 * and the terminal line ending removed — the long-established trimming rule,
 * preserved deliberately so existing token files keep producing the same bytes.
 * What changed is that everything the old single fgets() could not see is now an
 * ERROR rather than silently ignored: a token longer than the buffer used to be
 * cut to 512 bytes and presented as a different credential, and extra file
 * content was dropped without a word. The token is never logged. */
static const char *read_token_file(const char *path) {
	FILE *f = fopen(path, "r");
	if (!f) die("cannot open --token-file");
	static char tok[TOKEN_CAP + 2];
	size_t n = fread(tok, 1, sizeof tok - 1, f);
	if (ferror(f)) { fclose(f); die("--token-file read failed"); }
	tok[n] = 0;
	int truncated = fgetc(f) != EOF; /* content beyond what the buffer could hold */
	fclose(f);

	char *nl = strpbrk(tok, "\r\n");
	if (nl) {
		/* Only whitespace may follow the credential line; anything else is far
		 * more likely a mistake than a deliberate multi-line credential. */
		for (const char *rest = nl; *rest; rest++)
			if (*rest != '\r' && *rest != '\n' && *rest != ' ' && *rest != '\t')
				die("--token-file has content after the first line");
		*nl = 0;
		truncated = 0;
	}
	size_t len = strlen(tok);
	while (len && (tok[len-1] == ' ' || tok[len-1] == '\t')) tok[--len] = 0;
	if (len == 0) die("empty --token-file");
	if (truncated || len > TOKEN_CAP) die("--token-file token is longer than the supported maximum");
	return tok;
}

static const char *need(const char *v, const char *name) {
	if (!v) { fprintf(stderr, "navfeeder: missing required --%s\n", name); exit(2); }
	return v;
}

static uint64_t parse_positive_decimal(const char *flag, const char *s, uint64_t max) {
	if (!s || !*s) goto bad;
	for (const char *p = s; *p; p++)
		if (*p < '0' || *p > '9') goto bad;
	errno = 0;
	char *end = NULL;
	unsigned long long v = strtoull(s, &end, 10);
	if (errno == ERANGE || !end || *end || v == 0 || v > max) goto bad;
	return (uint64_t)v;
bad:
	fprintf(stderr, "navfeeder: bad --%s value %s: want a positive decimal integer\n",
		flag, s ? s : "(empty)");
	exit(2);
}

static void usage(void) {
	fprintf(stderr,
		"navfeeder — navlistener edge feeder (UBX raw nav frames over GNF1/TLS)\n"
		"usage: navfeeder --server host:port --source (/dev/ttyACM0 | host:port) --station ID\n"
		"                 (--token TOK | --token-file F) [--cert C --key K] [options]\n\n"
		"  --server host:port    the collector's authenticated push endpoint\n"
		"  --source SRC          /dev/ttyACM0 (serial) or host:port (TCP bridge to the receiver)\n"
		"  --baud N              serial baud when --source is a device path (default 460800)\n"
		"  --configure-ubx PORT  enable SFRBX/NAV-SAT/MON-RF in RAM on uart1, uart2, or usb\n"
		"                        (serial only; preserves receiver baud/NMEA; default passive)\n"
		"  --station ID          this observer's station id (also the mTLS cert's DNS SAN)\n"
		"  --feed ubx            feed type (only 'ubx' is implemented today; default ubx)\n"
		"  --token TOK           bearer token (prefer --token-file so it stays out of argv)\n"
		"  --token-file F        read the bearer token from a file\n"
		"  --cert C --key K      mTLS client certificate + private key (PEM)\n"
		"  --ca F                CA bundle to verify the collector (default: system store)\n"
		"  --spool N             in-memory ring capacity in frames (default 65536)\n"
		"  --spool-file F        disk spool path (lossless past the RAM ring; survives an\n"
		" orderly reboot only on PERSISTENT storage — not tmpfs, regression fix)\n"
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
	/* exit status must mean what the service manager will read it
	 * to mean. Every durability outcome is aggregated under the spool lock —
	 * per-record drops, the *other* drop counter disk_put uses when the spool file
	 * cannot even be opened, a deferred stream error, and the final fflush/fsync,
	 * none of which were previously examined. Reporting success while any of those
	 * failed tells the service manager the promised recovery spool was safely
	 * committed when some or all of the newly spilled frames are not durable. */
	int durable = 1;
	uint64_t lost = 0;
	size_t intended = 0;
	const char *why = "";
	if (g_spool.path) {
		pthread_mutex_lock(&g_spool.mu);
		uint64_t disk_dropped_before = g_spool.disk_dropped;
		uint64_t open_failed_before = g_spool.dropped;
		size_t idx = g_spool.head;
		intended = g_spool.count;
		for (size_t i = 0; i < g_spool.count; i++) {
			struct frame *fr = &g_spool.ring[idx];
			disk_put(&g_spool, fr->seq, fr->data, fr->len);
			idx = (idx + 1) % g_spool.cap;
		}
		/* disk_put counts an fopen failure under `dropped`, not `disk_dropped`, so
		 * the old diagnostic could not see a spool that never opened at all. */
		lost = (g_spool.disk_dropped - disk_dropped_before) +
		       (g_spool.dropped - open_failed_before);
		if (lost) { durable = 0; why = "records dropped (disk overflow disabled, budget exhausted, or the spool file could not be opened)"; }
		if (g_spool.disk_w) {
			/* A short write can surface only as a deferred stream error; check it
			 * before trusting the flush, and stop at the first failure so the
			 * reported cause is the real one. */
			if (ferror(g_spool.disk_w)) {
				durable = 0; why = "the spool stream reported a deferred write error";
			} else if (fflush(g_spool.disk_w) != 0) {
				durable = 0; why = "the final spool flush failed";
			} else if (fsync(fileno(g_spool.disk_w)) != 0) {
				durable = 0; why = "the final spool fsync failed";
			} else if (sync_parent_dir(g_spool.path) != 0) {
				/* Recovery finds the spool by name, and this flush may have created
				 * that name: fsync on the file alone does not make its directory
				 * entry durable. */
				durable = 0; why = "the spool directory could not be synced";
			}
		} else if (intended) {
			/* Records were meant to be spilled but no writer is open — the file was
			 * never opened, or a failed append rolled it back. Nothing is durable. */
			durable = 0;
			if (!*why) why = "no spool writer was open after the flush";
		}
		pthread_mutex_unlock(&g_spool.mu);
	}
	/* Never let "flushing" be the last word when the flush was a no-op: after an
	 * incomplete recovery (disk appends disabled) or with the live budget consumed
	 * by retained replay files, every ring frame is dropped here — say so. Frame
	 * contents and credentials are never logged, only counts and a cause. */
	if (!durable) {
		log_msg("shutdown flush INCOMPLETE: %llu of %zu unacked ring frame(s) not durably spooled: %s; "
			"existing spool files left unchanged",
			(unsigned long long)lost, intended, why);
		_exit(1);
	}
	if (intended)
		log_msg("shutdown flush complete: %zu unacked ring frame(s) durably spooled", intended);
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
		else if (!strcmp(a, "--baud") && i+1 < argc)
			o.baud = (int)parse_positive_decimal("baud", argv[++i], INT_MAX);
		else if (!strcmp(a, "--configure-ubx") && i+1 < argc) {
			o.configure_ubx = ubx_config_port(argv[++i]);
			if (!o.configure_ubx) die("--configure-ubx requires uart1, uart2, or usb");
		}
		else if (!strcmp(a, "--token") && i+1 < argc) o.token = argv[++i];
		else if (!strcmp(a, "--token-file") && i+1 < argc) o.token = read_token_file(argv[++i]);
		else if (!strcmp(a, "--station") && i+1 < argc) o.station = argv[++i];
		else if (!strcmp(a, "--feed") && i+1 < argc) o.feed = argv[++i];
		else if (!strcmp(a, "--ca") && i+1 < argc) o.ca = argv[++i];
		else if (!strcmp(a, "--cert") && i+1 < argc) o.cert = argv[++i];
		else if (!strcmp(a, "--key") && i+1 < argc) o.key = argv[++i];
		else if (!strcmp(a, "--spool") && i+1 < argc)
			o.spool_cap = (size_t)parse_positive_decimal("spool", argv[++i], SIZE_MAX);
		else if (!strcmp(a, "--spool-file") && i+1 < argc) o.spool_file = argv[++i];
		else if (!strcmp(a, "--spool-disk-mb") && i+1 < argc)
			o.disk_max_bytes = parse_positive_decimal("spool-disk-mb", argv[++i],
				UINT64_MAX / (1024 * 1024)) * 1024 * 1024;
		else if (!strcmp(a, "--zstd")) o.zstd = 1;
		else if (!strcmp(a, "--insecure")) o.insecure = 1;
		else if (!strcmp(a, "-h") || !strcmp(a, "--help")) { usage(); return 0; }
		else { fprintf(stderr, "navfeeder: unknown arg %s\n", a); usage(); return 2; }
	}
	server = (char *)need(server, "server");
	need(o.source, "source");
	if (o.configure_ubx && o.source[0] != '/')
		die("--configure-ubx requires a local serial device source");
	need(o.station, "station");
	if (strcmp(o.feed, "ubx") != 0)
		die("only --feed ubx is implemented today (sbf/rtcm source modes are deferred)");
	/* a bearer token is ALWAYS required; mTLS (--cert/--key) is additive, raising the
	 * trust tier (DESIGN §3: "token + optional mTLS"). The collector's only authenticator
	 * matches the token hash, so a cert-only feeder would send "token":"" and be rejected
	 * `unauthorized` in a permanent 30 s loop. */
	if (!o.token) die("a bearer token (--token/--token-file) is required; --cert/--key adds mTLS on top");
	if ((o.cert != NULL) != (o.key != NULL)) die("--cert and --key must be given together");
	/* same strict parser as --source. Brackets are stripped, so
	 * getaddrinfo and the certificate check both see the bare address. */
	static char server_host[NI_MAXHOST], server_port[16];
	const char *server_why = NULL;
	if (parse_authority(server, server_host, sizeof server_host,
			    server_port, sizeof server_port, &server_why) != 0)
	{
		char msg[512];
		snprintf(msg, sizeof msg, "--server %s: %s (expected host:port or [v6]:port)",
			 server, server_why);
		die(msg);
	}
	o.server_host = server_host;
	o.server_port = server_port;

	/* prove the configured credential can actually be presented
	 * BEFORE opening a socket. Without this, an over-long token produced an
	 * endless connect/"unauthorized"/reconnect loop with no local diagnostic. The
	 * session id is not minted yet, so a maximum-length placeholder of the same
	 * charset stands in — it is the longest a real one can be. */
	{
		char probe[HELLO_CAP];
		char placeholder[sizeof g_session];
		memset(placeholder, 'a', sizeof placeholder - 1);
		placeholder[sizeof placeholder - 1] = 0;
		const char *why = NULL;
		if (build_hello(probe, sizeof probe, &o, placeholder, &why) < 0) {
			char msg[256];
			snprintf(msg, sizeof msg, "%s", why);
			die(msg);
		}
	}

	SSL_library_init();
	SSL_load_error_strings();
	SSL_CTX *ctx = make_ctx(&o);
	/* New captures and recovered records use separate session spaces. */
	session_init();
	/* no frame has been captured yet, so a failure here costs
	 * nothing — but continuing with an uninitialized mutex would corrupt every
	 * later spool operation. Retry with bounded backoff (boot-time resource
	 * exhaustion is typically transient, and "never exit" is this file's rule)
	 * before any thread that could touch the spool is started. */
	int spool_backoff = 1;
	while (spool_init(&g_spool, o.spool_cap, o.spool_file, o.disk_max_bytes) != 0) {
		log_msg("failed to initialize the spool (out of memory or locks); retrying in %ds", spool_backoff);
		sleep(spool_backoff);
		if ((spool_backoff *= 2) > 30) spool_backoff = 30;
	}
	spool_recover_all(&g_spool);
	log_msg("session %s", g_session);

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
	char secondary_host[64];
	nav_endpoint_secondary(o.server_host, secondary_host, sizeof secondary_host);
	struct opts connection = o; /* producer's configuration remains immutable */
	for (;;) {
		struct spool *sending = g_replays ? g_replays : &g_spool;
		int rc = serve_collector(ctx, &connection, sending);
		if (rc == 1) {
			g_replays = sending->next;
			pthread_mutex_lock(&g_spool.mu);
			uint64_t room = o.disk_max_bytes - g_spool.disk_max_bytes;
			g_spool.disk_max_bytes += sending->retained_bytes < room ? sending->retained_bytes : room;
			pthread_mutex_unlock(&g_spool.mu);
			spool_free(sending);
			backoff = 1;
			continue;
		}
		/* regression fix, tightened by serve_collector itself reports usefulness (rc==0,
		 * authenticated + ran USEFUL_CONN_S, measured post-handshake on the monotonic
		 * clock) — reset the backoff so a routine collector restart weeks later
		 * reconnects in ~1s rather than the 30s cap the ladder otherwise climbs to over
		 * the process lifetime. Mirrors the producer thread's regression fix usefulness reset and
		 * the Go collector's regression fix dial-side reset. Auth reject (-2) still backs off
		 * hard; failed/slow connects (-1) never reset. */
		if (rc == 0) backoff = 1;
		if (rc != -2)
			connection.server_host = rc != 0 && connection.server_host == o.server_host && secondary_host[0]
				? secondary_host : o.server_host;
		/* the same named ceiling sleep_with_jitter clamps to, so raising one
		 * without the other cannot silently shorten the ladder's top rungs. */
		int wait = rc == -2 ? RECONNECT_BACKOFF_MAX_S : backoff;
		log_msg("reconnecting in %ds", wait);
		sleep_with_jitter((unsigned)wait);
		if (rc == -2) backoff = 1;
		else { if ((backoff *= 2) > RECONNECT_BACKOFF_MAX_S) backoff = RECONNECT_BACKOFF_MAX_S; }
	}
	return 0;
}
