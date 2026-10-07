// netcfg_portal — the browser portal's form-to-record decoder.
//
// Deliberately split out of the httpd handler and kept PURE (no ESP-IDF, no NVS, no httpd,
// no FreeRTOS), the same reasoning as netcfg_form and netcfg_check: this is the one place
// that turns untrusted POST bytes into a configuration record, so it is host-tested. Every
// field of the record starts empty. A field the form omits stays empty, never a compiled-in
// development credential: seeding the record from netcfg_load() let a POST without `token`
// (or `host`, or `pass`) commit whatever the bench Kconfig held, and carried the compiled
// NVF_INSECURE default into NVS as a stored TLS-downgrade bit.

#ifndef NETCFG_PORTAL_H
#define NETCFG_PORTAL_H

#include <stdbool.h>
#include <stddef.h>

#include "netcfg_check.h"

#ifdef __cplusplus
extern "C" {
#endif

// NETCFG_PORTAL_ERR_CAP bounds the operator-facing reason. The form decoder's fixed
// messages are the longest; none echoes submitted bytes back.
#define NETCFG_PORTAL_ERR_CAP 96

// netcfg_portal_decode fills out from an application/x-www-form-urlencoded body and
// validates it with netcfg_validate, the rule the boot path applies. The only stored value
// carried over is the tunnel profile: a body with no `wg` field keeps `stored` (NULL means
// none), a present-but-blank one turns the tunnel off, anything else must parse whole.
// `insecure` is false unless the build compiles the portal checkbox in
// (CONFIG_NVF_ALLOW_INSECURE_PORTAL) and the checkbox was posted. A commissioned board
// refuses a posted station id and stores none. On any refusal out is zeroed, err (when
// given) holds the reason and false is returned; on success err is cleared.
bool netcfg_portal_decode(const char *body, const netcfg_tunnel_t *stored, netcfg_t *out,
                          char *err, size_t errcap);

#ifdef __cplusplus
}
#endif

#endif // NETCFG_PORTAL_H
