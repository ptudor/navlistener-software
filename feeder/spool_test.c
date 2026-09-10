/* Deterministic regression fix filesystem faults around the real feeder source. */
#define _POSIX_C_SOURCE 200809L
#define _DEFAULT_SOURCE
#define _DARWIN_C_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <errno.h>
#include <string.h>
#include <unistd.h>
#include <assert.h>
#include <pthread.h>
#include <zstd.h>
static FILE *fault_file;
static int fail_open, fail_read, reads, read_error;
static const char *fault_path;
static FILE *test_fopen(const char *, const char *);
static size_t test_fread(void *, size_t, size_t, FILE *);
static int test_ferror(FILE *);
static int test_unlink(const char *);
/* fail_mutex_init injects each documented pthread_mutex_init
 * failure code (EAGAIN out of locks, ENOMEM, EPERM) so spool_init's new
 * fail-closed path is exercised for the primary and every recovered spool. */
static int fail_mutex_init;
static int test_pthread_mutex_init(pthread_mutex_t *, const pthread_mutexattr_t *);
/* durability faults for the shutdown ring flush. Each is scoped to
 * the live spool WRITER (never the recovery reader), so a test can prove that a
 * failed write, a deferred stream error, a failed flush or a failed fsync all
 * produce a nonzero exit and an explicit incomplete-flush diagnostic. */
static int test_fflush(FILE *);
static int test_fsync(int);
static size_t test_fwrite(const void *, size_t, size_t, FILE *);
/* fail the zstd compressor's two allocations independently and
 * transiently. NAVFEEDER_TEST_FAIL_ZSTD_CTX / _BUF give the number of leading
 * connection attempts whose ZSTD_createCCtx / output-buffer allocation must fail,
 * so a test can prove the process survives the failures AND that a later
 * successful allocation replays the retained spool in order. */
static ZSTD_CCtx *test_ZSTD_createCCtx(void);
static size_t test_ZSTD_CStreamOutSize(void);
#define fopen test_fopen
#define fread test_fread
#define ferror test_ferror
#define unlink test_unlink
#define pthread_mutex_init test_pthread_mutex_init
#define fflush test_fflush
#define fsync test_fsync
#define fwrite test_fwrite
#define ZSTD_createCCtx test_ZSTD_createCCtx
#define ZSTD_CStreamOutSize test_ZSTD_CStreamOutSize
#define main navfeeder_main
#include "navfeeder.c"
#undef main
#undef fopen
#undef fread
#undef ferror
#undef unlink
#undef pthread_mutex_init
#undef fflush
#undef fsync
#undef fwrite
/* g_spool is defined by navfeeder.c above, so these can scope faults to the
 * live spool writer and leave every other stream untouched. */
static int spool_writer(FILE *f) { return f && f == g_spool.disk_w; }
static int test_fflush(FILE *f) {
    if (spool_writer(f) && getenv("NAVFEEDER_TEST_FAIL_FFLUSH")) { errno = ENOSPC; return EOF; }
    return fflush(f);
}
static int test_fsync(int fd) {
    if (g_spool.disk_w && fd == fileno(g_spool.disk_w) && getenv("NAVFEEDER_TEST_FAIL_FSYNC")) {
        errno = EIO; return -1;
    }
    return fsync(fd);
}
static size_t test_fwrite(const void *p, size_t size, size_t n, FILE *f) {
    /* A short write is how a full disk actually presents to stdio. */
    if (spool_writer(f) && getenv("NAVFEEDER_TEST_SHORT_WRITE")) {
        if (n > 1) return fwrite(p, size, n / 2, f);
        return 0;
    }
    return fwrite(p, size, n, f);
}
#undef ZSTD_createCCtx
#undef ZSTD_CStreamOutSize
static int envfaults(const char *name) {
    const char *v = getenv(name);
    return v ? atoi(v) : 0;
}
static ZSTD_CCtx *test_ZSTD_createCCtx(void) {
    static int left = -1;
    if (left < 0) left = envfaults("NAVFEEDER_TEST_FAIL_ZSTD_CTX");
    if (left > 0) { left--; return NULL; }
    return ZSTD_createCCtx();
}
static size_t test_ZSTD_CStreamOutSize(void) {
    static int left = -1;
    if (left < 0) left = envfaults("NAVFEEDER_TEST_FAIL_ZSTD_BUF");
    /* SIZE_MAX makes the caller's malloc fail deterministically, exercising the
     * output-buffer half independently of the context half. */
    if (left > 0) { left--; return (size_t)-1; }
    return ZSTD_CStreamOutSize();
}
static int test_pthread_mutex_init(pthread_mutex_t *m, const pthread_mutexattr_t *a) {
    if (fail_mutex_init) { int e = fail_mutex_init; return e; }
    return pthread_mutex_init(m, a);
}
static FILE *test_fopen(const char *path, const char *mode) {
    if (fault_path && !strcmp(path, fault_path) && !strcmp(mode, "rb")) {
        if (fail_open) { errno = EACCES; return NULL; }
        fault_file = fopen(path, mode); reads = read_error = 0; return fault_file;
    }
    return fopen(path, mode);
}
static size_t test_fread(void *p, size_t size, size_t n, FILE *f) {
    if (f == fault_file && fail_read && ++reads == fail_read) {
        size_t got = fread(p, size, n / 2, f);
        read_error = 1; errno = EIO; return got;
    }
    return fread(p, size, n, f);
}
static int test_ferror(FILE *f) {
    if (spool_writer(f) && getenv("NAVFEEDER_TEST_DEFER_FERROR")) return 1;
    return (f == fault_file && read_error) || ferror(f);
}
static int test_unlink(const char *path) {
    const char *keep = getenv("NAVFEEDER_TEST_KEEP_ACKED");
    if (keep && g_spool.disk_max_seq && !strcmp(keep, path)) { errno = EACCES; return -1; }
    return unlink(path);
}
static void fixture(const char *path, int malformed) {
    unsigned char hdr[SPOOL_HDR_LEN] = {0}, rec[12+RECORD_HDR] = {0};
    memcpy(hdr, SPOOL_MAGIC, 8); hdr[8] = 3; memcpy(hdr+9, "old", 3);
    FILE *f = fopen(path, "wb"); assert(f);
    assert(fwrite(hdr, 1, sizeof hdr, f) == sizeof hdr);
    for (int i = 1; i <= 3; i++) {
        be64(rec, malformed == 3 && i == 3 ? 1 : (uint64_t)i);
        be32(rec+8, malformed == 1 && i == 3 ? 0 : malformed == 2 && i == 3 ? GNF_RECORD+1 : RECORD_HDR);
        assert(fwrite(rec, 1, sizeof rec, f) == sizeof rec);
    }
    assert(!fclose(f));
}
static size_t bytes(const char *path, unsigned char *p) {
    FILE *f = fopen(path, "rb"); assert(f);
    size_t n = fread(p, 1, 1024, f); assert(!ferror(f)); fclose(f); return n;
}
static void check_preserved(const char *path, int expected) {
    unsigned char before[1024], after[1024]; size_t n = bytes(path, before);
    struct spool old; assert(spool_init(&old, 1, path, 4096) == 0);
    assert(spool_recover(&old) == expected);
    assert(bytes(path, after) == n && !memcmp(before, after, n));
    assert(!strcmp(g_session, "fresh"));
    free(old.ring); pthread_mutex_destroy(&old.mu);
}
/* spool_init used to return void and ignore pthread_mutex_init's
 * result, so an exhausted lock/memory budget produced a spool whose every later
 * lock, unlock and destroy ran against an uninitialized mutex -- undefined
 * behavior shared by the producer, consumer, ACK and shutdown threads. Each
 * documented failure code must now leave no usable object, release the ring, and
 * preserve every spool file byte-for-byte. */
static void spool_free_stack(struct spool *s);
static void mutex_init_faults(const char *path) {
    int codes[] = { EAGAIN, ENOMEM, EPERM, EBUSY };
    unsigned char before[1024], after[1024];
    size_t n = bytes(path, before);
    for (size_t i = 0; i < sizeof codes / sizeof *codes; i++) {
        fail_mutex_init = codes[i];

        /* A direct init must fail closed and publish nothing. */
        struct spool s;
        assert(spool_init(&s, 1, path, 4096) == -1);
        assert(s.ring == NULL);   /* the partial allocation was released */
        assert(s.mu_ready == 0);  /* so spool_free never destroys this mutex */
        assert(s.cap == 0 && s.path == NULL);
        spool_free_stack(&s);     /* must not touch the uninitialized mutex */

        /* Recovery must fail closed too: files untouched, disk append disabled. */
        struct spool current;
        fail_mutex_init = 0;
        assert(spool_init(&current, 1, strdup(path), 4096) == 0);
        fail_mutex_init = codes[i];
        spool_recover_all(&current);
        assert(current.disk_append_disabled == 1);
        assert(g_replays == NULL); /* no partially built spool was published */
        assert(bytes(path, after) == n && !memcmp(before, after, n));
        fail_mutex_init = 0;
        free(current.ring); pthread_mutex_destroy(&current.mu); free((void *)current.path);
    }
    fail_mutex_init = 0;
    assert(bytes(path, after) == n && !memcmp(before, after, n));
}

/* spool_free without the free(s): the fault cases above use stack objects. */
static void spool_free_stack(struct spool *s) {
    if (s->mu_ready) pthread_mutex_destroy(&s->mu);
    free(s->ring);
}

/* one strict authority parser for --server and a TCP --source.
 * Both used to split at the LAST colon with no bracket awareness, so a standard
 * [v6]:port left brackets in the name given to getaddrinfo and an unbracketed
 * IPv6 literal silently lost its last hextet to the "port"; --source also copied
 * through a fixed 256-byte buffer with an unchecked snprintf, so an overlong
 * authority was truncated into a DIFFERENT endpoint. */
static void authority_matrix(void) {
    char host[NI_MAXHOST], port[16];
    const char *why;
    struct { const char *in, *host, *port; } good[] = {
        {"collector.example:5580", "collector.example", "5580"},
        {"192.0.2.10:5580",        "192.0.2.10",        "5580"},
        {"[2001:db8::1]:5580",     "2001:db8::1",       "5580"},  /* brackets stripped */
        {"[::1]:1",                "::1",               "1"},
        {"h:65535",                "h",                 "65535"},
    };
    for (size_t i = 0; i < sizeof good / sizeof *good; i++) {
        assert(parse_authority(good[i].in, host, sizeof host, port, sizeof port, &why) == 0);
        assert(!strcmp(host, good[i].host));
        assert(!strcmp(port, good[i].port));
    }
    const char *bad[] = {
        "",                     /* empty */
        "collector.example",    /* no port */
        ":5580",                /* empty host */
        "collector.example:",   /* empty port */
        "collector.example:0",  /* port 0 */
        "collector.example:65536",
        "collector.example:-1",
        "collector.example:80x",
        "2001:db8::1:5580",     /* ambiguous unbracketed IPv6 */
        "[2001:db8::1:5580",    /* unterminated bracket */
        "[2001:db8::1]5580",    /* missing ':' after ']' */
        "collector.example\r\nX: 1:5580", /* control characters */
        "collector\t.example:5580",
    };
    for (size_t i = 0; i < sizeof bad / sizeof *bad; i++) {
        assert(parse_authority(bad[i], host, sizeof host, port, sizeof port, &why) == -1);
        assert(why != NULL);
    }
    /* Truncation must be refused, never silently dialled as another endpoint. */
    char overlong[NI_MAXHOST + 32];
    memset(overlong, 'a', sizeof overlong - 1);
    overlong[sizeof overlong - 1] = 0;
    memcpy(overlong + sizeof overlong - 6, ":5580", 6);
    assert(parse_authority(overlong, host, sizeof host, port, sizeof port, &why) == -1);

    /* An IP literal must be recognized so SNI is omitted for it (RFC 6066 §3). */
    assert(numeric_host("192.0.2.10"));
    assert(numeric_host("::1"));
    assert(numeric_host("2001:db8::1"));
    assert(!numeric_host("collector.example"));
    assert(!numeric_host("192.0.2.10.example"));
}

int main(int argc, char **argv) {
    if (argc > 1 && !strcmp(argv[1], "feeder")) return navfeeder_main(argc-1, argv+1);
    assert(argc == 2);
    char path[PATH_MAX]; snprintf(path, sizeof path, "%s/spool", argv[1]);
    strcpy(g_session, "fresh"); fault_path = path;
    authority_matrix();
    fixture(path, 0);
    fail_open = 1; check_preserved(path, -1); fail_open = 0;
    int points[] = {1, 3, 4, 5};
    for (size_t i = 0; i < sizeof points/sizeof *points; i++) {
        fail_read = points[i]; check_preserved(path, -1);
    }
    fail_read = 0; read_error = 0;
    check_preserved(path, 1); /* retry after filesystem recovery */
    for (int bad = 1; bad <= 3; bad++) { fixture(path, bad); check_preserved(path, -1); }
    fixture(path, 0);
    assert(!truncate(path, SPOOL_HDR_LEN + 2*(12+RECORD_HDR) + 15));
    struct spool old; assert(spool_init(&old, 1, path, 4096) == 0);
    assert(spool_recover(&old) == 1 && old.seq == 2);
    unsigned char b[1024]; assert(bytes(path,b) == SPOOL_HDR_LEN + 2*(12+RECORD_HDR));
    assert(!strcmp(g_session, "fresh") && !strcmp(old.session, "old"));
    free(old.ring); pthread_mutex_destroy(&old.mu);
    /* A short current-format session header is uncertain, not disposable. */
    fixture(path,0); assert(!truncate(path, 20)); check_preserved(path,-1);
    /* Full migration on repeated restarts never changes an old payload's key. */
    fixture(path,0);
    mutex_init_faults(path);
    assert(spool_init(&g_spool, 1, path, 4096) == 0); spool_recover_all(&g_spool);
    assert(g_replays && !strcmp(g_replays->session,"old") && g_replays->seq == 3);
    unsigned char data[RECORD_HDR] = {0};
    assert(spool_append(&g_spool,data,sizeof data) == 1);
    assert(!strcmp(g_spool.session,"fresh"));
    assert(!spool_append(g_replays,data,sizeof data));
    puts("spool recovery faults, corruption, torn tail, mutex-init faults, authority parsing, retry and session isolation PASS");
    return 0;
}
