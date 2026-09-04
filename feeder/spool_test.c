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
static FILE *fault_file;
static int fail_open, fail_read, reads, read_error;
static const char *fault_path;
static FILE *test_fopen(const char *, const char *);
static size_t test_fread(void *, size_t, size_t, FILE *);
static int test_ferror(FILE *);
static int test_unlink(const char *);
#define fopen test_fopen
#define fread test_fread
#define ferror test_ferror
#define unlink test_unlink
#define main navfeeder_main
#include "navfeeder.c"
#undef main
#undef fopen
#undef fread
#undef ferror
#undef unlink
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
static int test_ferror(FILE *f) { return (f == fault_file && read_error) || ferror(f); }
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
    struct spool old; spool_init(&old, 1, path, 4096);
    assert(spool_recover(&old) == expected);
    assert(bytes(path, after) == n && !memcmp(before, after, n));
    assert(!strcmp(g_session, "fresh"));
    free(old.ring); pthread_mutex_destroy(&old.mu);
}
int main(int argc, char **argv) {
    if (argc > 1 && !strcmp(argv[1], "feeder")) return navfeeder_main(argc-1, argv+1);
    assert(argc == 2);
    char path[PATH_MAX]; snprintf(path, sizeof path, "%s/spool", argv[1]);
    strcpy(g_session, "fresh"); fault_path = path;
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
    struct spool old; spool_init(&old, 1, path, 4096);
    assert(spool_recover(&old) == 1 && old.seq == 2);
    unsigned char b[1024]; assert(bytes(path,b) == SPOOL_HDR_LEN + 2*(12+RECORD_HDR));
    assert(!strcmp(g_session, "fresh") && !strcmp(old.session, "old"));
    free(old.ring); pthread_mutex_destroy(&old.mu);
    /* A short current-format session header is uncertain, not disposable. */
    fixture(path,0); assert(!truncate(path, 20)); check_preserved(path,-1);
    /* Full migration on repeated restarts never changes an old payload's key. */
    fixture(path,0);
    spool_init(&g_spool, 1, path, 4096); spool_recover_all(&g_spool);
    assert(g_replays && !strcmp(g_replays->session,"old") && g_replays->seq == 3);
    unsigned char data[RECORD_HDR] = {0};
    assert(spool_append(&g_spool,data,sizeof data) == 1);
    assert(!strcmp(g_spool.session,"fresh"));
    assert(!spool_append(g_replays,data,sizeof data));
    puts("spool recovery faults, corruption, torn tail, retry and session isolation PASS");
    return 0;
}
