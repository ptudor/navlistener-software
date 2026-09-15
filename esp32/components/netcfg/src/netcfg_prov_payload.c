#include "netcfg_prov_payload.h"

#include <stdio.h>
#include <string.h>

#define REQUEST_HEADER_SIZE 10

static bool fail(char *error, size_t cap, const char *message)
{
    if (error && cap) snprintf(error, cap, "%s", message);
    return false;
}

static bool printable(const uint8_t *data, size_t size)
{
    for (size_t i = 0; i < size; i++)
        if (data[i] < 0x20 || data[i] > 0x7e) return false;
    return true;
}

bool netcfg_prov_decode(const uint8_t *data, size_t size, netcfg_t *out,
                        char *error, size_t error_cap)
{
    if (!data || !out) return fail(error, error_cap, "missing request");
    memset(out, 0, sizeof *out);
    if (size < REQUEST_HEADER_SIZE || memcmp(data, "NVF1", 4))
        return fail(error, error_cap, "bad request header");
    if (data[4] != 0) return fail(error, error_cap, "unsupported flags");

    const unsigned port = (unsigned)data[5] << 8 | data[6];
    const size_t host_len = data[7];
    const size_t station_len = data[8];
    const size_t token_len = data[9];
    if (!host_len || host_len >= sizeof out->host ||
        !station_len || station_len >= sizeof out->station ||
        !token_len || token_len >= sizeof out->token)
        return fail(error, error_cap, "invalid field length");
    if (size != REQUEST_HEADER_SIZE + host_len + station_len + token_len)
        return fail(error, error_cap, "request length mismatch");

    const uint8_t *host = data + REQUEST_HEADER_SIZE;
    const uint8_t *station = host + host_len;
    const uint8_t *token = station + station_len;
    if (!printable(host, host_len) || !printable(station, station_len) ||
        !printable(token, token_len))
        return fail(error, error_cap, "fields must be printable ASCII");

    memcpy(out->host, host, host_len);
    memcpy(out->station, station, station_len);
    memcpy(out->token, token, token_len);
    out->port = (int)port;
    out->insecure = false;

    // Reuse the same completeness rule as the browser and boot paths. A local
    // placeholder isolates validation of the three app fields from Wi-Fi,
    // which arrives through the standard provisioning endpoint.
    netcfg_t complete = *out;
    snprintf(complete.wifi_ssid, sizeof complete.wifi_ssid, "pending");
    if (!netcfg_validate(&complete, error, error_cap)) {
        memset(out, 0, sizeof *out);
        return false;
    }
    return true;
}

void netcfg_prov_encode_response(uint8_t out[NETCFG_PROV_RESPONSE_SIZE],
                                 netcfg_prov_status_t status)
{
    memcpy(out, "NVR1", 4);
    out[4] = (uint8_t)status;
}
