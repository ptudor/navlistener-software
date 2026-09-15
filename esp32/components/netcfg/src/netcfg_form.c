#include "netcfg_form.h"

#include <stdbool.h>
#include <stdio.h>
#include <string.h>

static int hex_digit(unsigned char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static form_result_t url_decode(char *dst, size_t cap, const char *src, size_t srclen, bool multiline)
{
    if (cap == 0) return FORM_TRUNCATED;
    size_t o = 0;
    for (size_t i = 0; i < srclen; i++) {
        if (o + 1 >= cap) {
            dst[o] = '\0';
            return FORM_TRUNCATED;
        }
        unsigned char c = (unsigned char)src[i];
        unsigned char out;
        if (c == '+') {
            out = ' ';
        } else if (c == '%') {
            // Exactly two hex digits, or the escape is malformed. strtol with no
            // digit check turned "%GG" into 0 (a NUL) and a short trailing "%"
            // was copied through literally.
            if (i + 2 >= srclen) {
                dst[o] = '\0';
                return FORM_MALFORMED;
            }
            int hi = hex_digit((unsigned char)src[i + 1]);
            int lo = hex_digit((unsigned char)src[i + 2]);
            if (hi < 0 || lo < 0) {
                dst[o] = '\0';
                return FORM_MALFORMED;
            }
            out = (unsigned char)((hi << 4) | lo);
            i += 2;
        } else {
            out = c;
        }
        // No configuration field may contain a C0 control or DEL, and a decoded
        // NUL would end the C string early — later validation would then see only
        // a prefix of what was submitted. A textarea legitimately carries line
        // structure, so only tab, CR and LF pass there.
        bool line_break = multiline && (out == '\t' || out == '\r' || out == '\n');
        if ((out < 0x20 && !line_break) || out == 0x7f) {
            dst[o] = '\0';
            return FORM_MALFORMED;
        }
        dst[o++] = (char)out;
    }
    dst[o] = '\0';
    return FORM_OK;
}

form_result_t netcfg_url_decode(char *dst, size_t cap, const char *src, size_t srclen)
{
    return url_decode(dst, cap, src, srclen, false);
}

static form_result_t field(const char *body, const char *name, char *dst, size_t cap, bool multiline)
{
    char key[24];
    int kn = snprintf(key, sizeof key, "%s=", name);
    if (kn < 0 || (size_t)kn >= sizeof key) return FORM_MALFORMED;
    const char *p = body;
    while ((p = strstr(p, key)) != NULL) {
        // Must be at the start or right after '&' to avoid matching a suffix.
        if (p == body || p[-1] == '&') {
            const char *v = p + kn;
            const char *end = strchr(v, '&');
            size_t vlen = end ? (size_t)(end - v) : strlen(v);
            return url_decode(dst, cap, v, vlen, multiline);
        }
        p += kn;
    }
    return FORM_ABSENT;
}

form_result_t netcfg_form_field(const char *body, const char *name, char *dst, size_t cap)
{
    return field(body, name, dst, cap, false);
}

form_result_t netcfg_form_field_text(const char *body, const char *name, char *dst, size_t cap)
{
    return field(body, name, dst, cap, true);
}

const char *netcfg_form_error(form_result_t result)
{
    switch (result) {
    case FORM_MALFORMED:
        return "malformed percent-encoding or control character in a form field";
    case FORM_TRUNCATED:
        return "form field too long";
    default:
        return "invalid form";
    }
}
