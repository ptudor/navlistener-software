/* the provisioning form decoder used strtol(hex, NULL, 16) with
 * neither a hex-digit check nor an end pointer, so "%GG" became a NUL, "%00" did
 * so deliberately, and a short trailing "%" was copied through literally. The
 * later C-string validation then saw only a prefix, so the unit could save a
 * credential or identifier other than the one submitted — and strand itself in a
 * reconnect loop. Malformed input must now be refused outright, with no NVS write. */
#include "netcfg_form.h"

#include <assert.h>
#include <stdio.h>
#include <string.h>

static int failures;

static void expect(int cond, const char *what)
{
    if (!cond) { printf("FAIL: %s\n", what); failures++; }
}

static form_result_t decode(const char *in, char *out, size_t cap)
{
    memset(out, 0xAA, cap);
    return netcfg_url_decode(out, cap, in, strlen(in));
}

int main(void)
{
    char out[64];

    /* Every one of the 256 byte values expressible as a two-hex-digit escape.
     * Printable bytes decode exactly; C0 and DEL are refused. */
    for (unsigned v = 0; v < 256; v++) {
        char esc[8];
        snprintf(esc, sizeof esc, "%%%02X", v);
        form_result_t r = decode(esc, out, sizeof out);
        if (v < 0x20 || v == 0x7f) {
            expect(r == FORM_MALFORMED, "control byte escape must be refused");
        } else {
            expect(r == FORM_OK, "printable byte escape must decode");
            expect((unsigned char)out[0] == v && out[1] == '\0', "escape decoded to the wrong byte");
        }
        /* Lowercase hex must behave identically. */
        snprintf(esc, sizeof esc, "%%%02x", v);
        expect(decode(esc, out, sizeof out) == r, "hex decoding must be case-insensitive");
    }

    /* Malformed escapes: no digits, one digit, non-hex digits, trailing percent. */
    const char *bad[] = { "%", "%A", "%G", "%GG", "%0G", "%G0", "abc%", "abc%A", "a%ZZb", "%%41" };
    for (size_t i = 0; i < sizeof bad / sizeof *bad; i++) {
        expect(decode(bad[i], out, sizeof out) == FORM_MALFORMED, "malformed escape must be refused");
    }
    /* %00 specifically: it used to be decoded into a string-terminating NUL. */
    expect(decode("tok%00en", out, sizeof out) == FORM_MALFORMED, "%00 must be refused");
    expect(decode("%2500", out, sizeof out) == FORM_OK, "a literal %% escape is valid");
    expect(!strcmp(out, "%00"), "%%2500 must decode to the literal text %00");

    /* Valid forms are unchanged: '+' is a space, delimiters and UTF-8 survive. */
    expect(decode("a+b", out, sizeof out) == FORM_OK && !strcmp(out, "a b"), "'+' is a space");
    expect(decode("a%20b", out, sizeof out) == FORM_OK && !strcmp(out, "a b"), "%%20 is a space");
    expect(decode("st%C3%A5tion", out, sizeof out) == FORM_OK && !strcmp(out, "st\xC3\xA5tion"),
           "valid UTF-8 bytes must pass through");
    expect(decode("a=b:c/d", out, sizeof out) == FORM_OK && !strcmp(out, "a=b:c/d"),
           "delimiters inside a value are literal");
    expect(decode("", out, sizeof out) == FORM_OK && out[0] == '\0', "an empty value is valid");

    /* Exact capacity: cap-1 characters fit, cap characters do not. */
    char small[5];
    expect(decode("abcd", small, sizeof small) == FORM_OK && !strcmp(small, "abcd"),
           "a value of exactly cap-1 must fit");
    expect(decode("abcde", small, sizeof small) == FORM_TRUNCATED,
           "a value of cap characters must be refused, not truncated");
    expect(decode("%41%42%43%44", small, sizeof small) == FORM_OK && !strcmp(small, "ABCD"),
           "escapes count as their decoded length");
    expect(decode("%41%42%43%44%45", small, sizeof small) == FORM_TRUNCATED,
           "one escape too many must be refused");

    /* Field extraction: presence, absence, suffix-key safety, and propagation. */
    char field[16];
    memset(field, 0, sizeof field);
    expect(netcfg_form_field("ssid=home&token=abc", "ssid", field, sizeof field) == FORM_OK
           && !strcmp(field, "home"), "field extraction");
    expect(netcfg_form_field("ssid=home&token=abc", "token", field, sizeof field) == FORM_OK
           && !strcmp(field, "abc"), "second field extraction");
    strcpy(field, "kept");
    expect(netcfg_form_field("ssid=home", "station", field, sizeof field) == FORM_ABSENT,
           "an absent field is reported absent");
    expect(!strcmp(field, "kept"), "an absent field must leave dst untouched ");
    expect(netcfg_form_field("xssid=other&ssid=real", "ssid", field, sizeof field) == FORM_OK
           && !strcmp(field, "real"), "a key must not match a suffix of another key");
    /* A malformed value must surface as malformed through form_field, not as OK. */
    expect(netcfg_form_field("ssid=ho%00me", "ssid", field, sizeof field) == FORM_MALFORMED,
           "form_field must propagate a malformed escape");
    expect(netcfg_form_field("ssid=ho%ZZme", "ssid", field, sizeof field) == FORM_MALFORMED,
           "form_field must propagate a bad hex escape");

    /* Operator-facing reasons must never echo submitted bytes. */
    expect(strstr(netcfg_form_error(FORM_MALFORMED), "malformed") != NULL, "malformed reason");
    expect(strstr(netcfg_form_error(FORM_TRUNCATED), "too long") != NULL, "truncated reason");

    printf("netcfg form decoder: %d failure(s)\n", failures);
    return failures != 0;
}
