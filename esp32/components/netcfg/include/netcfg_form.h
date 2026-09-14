// netcfg_form — the provisioning portal's form decoder.
//
// Deliberately split out and kept PURE (no ESP-IDF, no NVS, no httpd, no
// FreeRTOS), the same reasoning as netcfg_check: this is untrusted input parsing
// reachable from the captive portal, so it is the part of netcfg that most needs
// exhaustive host tests, and regression fix is exactly the class of bug that hides
// when such code sits inline in a handler.

#ifndef NETCFG_FORM_H
#define NETCFG_FORM_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

// FORM_MALFORMED is distinct from FORM_TRUNCATED so save_post can
// refuse a body that is *wrong* as clearly as one that is too long, and in both
// cases write nothing to NVS.
typedef enum {
    FORM_MALFORMED = -2,
    FORM_TRUNCATED = -1,
    FORM_ABSENT = 0,
    FORM_OK = 1,
} form_result_t;

// netcfg_url_decode decodes application/x-www-form-urlencoded text into dst.
//
// '+' is a space and percent escapes are case-insensitive, but an escape must be
// exactly two ASCII hex digits: `%GG`, `%0G`, `%A` and a trailing `%` are all
// FORM_MALFORMED rather than being turned into a NUL or copied through literally,
// which is what strtol(hex, NULL, 16) with no digit or end-pointer check used to
// do. A decoded NUL or any other C0/DEL control is refused for the same reason:
// it would terminate the C string early, so the subsequent validation would see
// only a prefix and the device could save a credential or identifier other than
// the one the operator submitted.
form_result_t netcfg_url_decode(char *dst, size_t cap, const char *src, size_t srclen);

// netcfg_form_field extracts one urlencoded field ("name=value&...") into dst,
// decoded. FORM_ABSENT leaves dst untouched, so a caller that seeded dst
// from the current config keeps that value for a field a partial POST omitted;
// callers using a fresh buffer must zero it themselves.
form_result_t netcfg_form_field(const char *body, const char *name, char *dst, size_t cap);

// netcfg_form_error maps a negative result to a fixed operator-facing reason. It
// never echoes submitted bytes back, so a credential cannot leak through an
// error page.
const char *netcfg_form_error(form_result_t result);

#ifdef __cplusplus
}
#endif

#endif
