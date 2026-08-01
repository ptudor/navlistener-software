// hello_test — host unit test for gnf1_build_hello() and gnf1_session_valid().
//
// The HELLO is the one frame whose exact JSON the collector parses before it will accept
// anything, and regression fix made `session` a REQUIRED field of it (contract revision
// 2026-07-31): a HELLO without a valid session is rejected with
// `{"ok":false,"error":"missing or invalid session"}`, which on a feeder presents as an
// endless reconnect loop rather than an obvious error. That makes the built payload worth
// pinning byte-for-byte here — the field is easy to drop in a refactor and impossible to
// notice missing without a live collector.
//
// gnf1 is pure buffer code with no ESP-IDF dependency, so this runs on the host:
//
//   make -C esp32/components/gnf1/test        # build + run
//
// Also run by `make -C esp32 host-test`, which `make -C go check` invokes.

#include <stdio.h>
#include <string.h>

#include "gnf1.h"

static int failures;

#define CHECK(cond, ...)                           \
    do {                                           \
        if (!(cond)) {                             \
            fprintf(stderr, "FAIL: " __VA_ARGS__); \
            fputc('\n', stderr);                   \
            failures++;                            \
        }                                          \
    } while (0)

// The reference session shape both feeders mint: 16 random bytes as 32 hex characters.
static const char *SESSION = "0f1e2d3c4b5a69788796a5b4c3d2e1f0";

static void test_hello_exact(void)
{
    // Byte-for-byte expectation, not a substring probe: field ORDER and the compact spelling
    // match navfeeder.c's handshake() (session after "sw", before the optional zstd flag), so
    // the two feeders' HELLOs are diffable in one packet capture. `sw` differs by design —
    // it is what tells the collector which implementation is talking.
    char out[512];
    int n = gnf1_build_hello(out, sizeof out, "tok3n", "navfeeder-AABBCC", "ubx", SESSION, false);
    const char *want = "{\"token\":\"tok3n\",\"station\":\"navfeeder-AABBCC\",\"feed\":\"ubx\","
                       "\"sw\":\"navfeeder-esp/1\",\"session\":\"0f1e2d3c4b5a69788796a5b4c3d2e1f0\"}";
    CHECK(n == (int)strlen(want), "hello length = %d, want %d", n, (int)strlen(want));
    CHECK(n > 0 && strcmp(out, want) == 0, "hello = %s\n            want %s", out, want);

    n = gnf1_build_hello(out, sizeof out, "tok3n", "navfeeder-AABBCC", "ubx", SESSION, true);
    const char *want_z = "{\"token\":\"tok3n\",\"station\":\"navfeeder-AABBCC\",\"feed\":\"ubx\","
                         "\"sw\":\"navfeeder-esp/1\",\"session\":\"0f1e2d3c4b5a69788796a5b4c3d2e1f0\","
                         "\"zstd\":true}";
    CHECK(n > 0 && strcmp(out, want_z) == 0, "hello(zstd) = %s\n                  want %s", out,
          want_z);
}

static void test_session_required(void)
{
    // no legacy sessionless tier exists on the wire, so building one is a bug we
    // refuse locally instead of shipping a frame the collector will certainly reject.
    char out[512];
    CHECK(gnf1_build_hello(out, sizeof out, "t", "s", "ubx", NULL, false) < 0,
          "NULL session accepted, want rejected");
    CHECK(gnf1_build_hello(out, sizeof out, "t", "s", "ubx", "", false) < 0,
          "empty session accepted, want rejected");
}

static void test_session_charset(void)
{
    // The session is emitted UNESCAPED (it is charset-constrained instead), so a value
    // carrying a quote/backslash/control byte would produce malformed JSON — and, worse than
    // malformed, a token-smuggling shape. Domain must match wire.ValidSession.
    CHECK(gnf1_session_valid(SESSION), "32 hex chars rejected");
    CHECK(gnf1_session_valid("a"), "single char rejected");
    CHECK(gnf1_session_valid("Ab9._-"), "[A-Za-z0-9._-] rejected");
    CHECK(!gnf1_session_valid(NULL), "NULL accepted");
    CHECK(!gnf1_session_valid(""), "empty accepted");
    CHECK(!gnf1_session_valid("bad\"quote"), "embedded quote accepted");
    CHECK(!gnf1_session_valid("back\\slash"), "embedded backslash accepted");
    CHECK(!gnf1_session_valid("has space"), "embedded space accepted");
    CHECK(!gnf1_session_valid("new\nline"), "embedded newline accepted");
    CHECK(!gnf1_session_valid("semi;colon"), "SQL metacharacter accepted");

    char max[GNF1_SESSION_MAX + 1];
    memset(max, 'a', sizeof max - 1);
    max[sizeof max - 1] = 0;
    CHECK(gnf1_session_valid(max), "%d-char session rejected, want accepted", GNF1_SESSION_MAX);

    char over[GNF1_SESSION_MAX + 2];
    memset(over, 'a', sizeof over - 1);
    over[sizeof over - 1] = 0;
    CHECK(!gnf1_session_valid(over), "%d-char session accepted, want rejected",
          GNF1_SESSION_MAX + 1);

    // And a bad session must not leak into the payload even if a caller ignores the return.
    char out[512];
    memset(out, 'X', sizeof out);
    CHECK(gnf1_build_hello(out, sizeof out, "t", "s", "ubx", "bad\"quote", false) < 0,
          "session with a quote accepted into the HELLO");
}

static void test_truncation(void)
{
    // Truncation must be reported, never silently emitted: a half-written HELLO is an
    // unparsable frame the collector drops, and the regression fix session is the last field before
    // the closing brace, so it is the first thing a too-small buffer loses.
    char small[32];
    int n = gnf1_build_hello(small, sizeof small, "tok3n", "navfeeder-AABBCC", "ubx", SESSION,
                             false);
    CHECK(n < 0, "truncated hello returned %d, want -1", n);
}

static void test_json_escape_still_applies(void)
{
    // the operator-supplied fields are still escaped (only the session skips it).
    char out[512];
    int n = gnf1_build_hello(out, sizeof out, "a\"b", "s\\t", "ubx", SESSION, false);
    CHECK(n > 0 && strstr(out, "\"token\":\"a\\\"b\"") != NULL, "token not escaped: %s", out);
    CHECK(n > 0 && strstr(out, "\"station\":\"s\\\\t\"") != NULL, "station not escaped: %s", out);
}

int main(void)
{
    test_hello_exact();
    test_session_required();
    test_session_charset();
    test_truncation();
    test_json_escape_still_applies();

    if (failures) {
        fprintf(stderr, "hello_test: %d failure(s)\n", failures);
        return 1;
    }
    printf("hello_test: OK\n");
    return 0;
}
