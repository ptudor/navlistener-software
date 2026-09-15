// Service aliases operated as identical endpoints. Keep this allowlist explicit:
// unrelated domains (including the two apex websites) need not serve the same data.
#ifndef NAV_ENDPOINT_FALLBACK_H
#define NAV_ENDPOINT_FALLBACK_H
#include <stdbool.h>
#include <stddef.h>
#include <string.h>

static inline bool nav_endpoint_name_equal(const char *a, const char *b)
{
    while (*a && *b) {
        unsigned char c = (unsigned char)*a++;
        if (c >= 'A' && c <= 'Z') c += 'a' - 'A';
        if (c != (unsigned char)*b++) return false;
    }
    return (!*a || (*a == '.' && !a[1])) && !*b;
}

static inline bool nav_endpoint_secondary(const char *host, char *out, size_t cap)
{
    static const char *const pairs[][2] = {
        {"in.intsat.net", "in.intsat.space"},
        {"firmware.intsat.net", "firmware.intsat.space"},
        {"klax1-navlistener.intsat.net", "klax1-navlistener.intsat.space"},
    };
    if (!out || !cap) return false;
    out[0] = 0;
    if (!host) return false;
    for (size_t i = 0; i < sizeof pairs / sizeof pairs[0]; i++) {
        for (unsigned side = 0; side < 2; side++) {
            if (!nav_endpoint_name_equal(host, pairs[i][side])) continue;
            const char *other = pairs[i][1 - side];
            if (strlen(other) >= cap) return false;
            strcpy(out, other);
            return true;
        }
    }
    return false;
}

// Change only an approved HTTPS authority; preserve the port and resource bytes.
static inline bool nav_endpoint_secondary_url(const char *url, char *out, size_t cap)
{
    char host[64], other[64];
    if (!out || !cap) return false;
    out[0] = 0;
    if (!url || strncmp(url, "https://", 8)) return false;
    size_t n = strcspn(url + 8, ":/?#");
    if (!n || n >= sizeof host) return false;
    memcpy(host, url + 8, n); host[n] = 0;
    if (!nav_endpoint_secondary(host, other, sizeof other)) return false;
    const char *rest = url + 8 + n;
    if (*rest == ':') {
        const char *port = rest + 1;
        size_t digits = strspn(port, "0123456789");
        if (!digits || (port[digits] && port[digits] != '/')) return false;
    }
    if (8 + strlen(other) + strlen(rest) >= cap) return false;
    strcpy(out, "https://"); strcat(out, other); strcat(out, rest);
    return true;
}
#endif
