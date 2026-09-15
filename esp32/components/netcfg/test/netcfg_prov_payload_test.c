#include "netcfg_prov_payload.h"

#include <assert.h>
#include <stdio.h>
#include <string.h>

static size_t request(uint8_t *out, size_t cap, unsigned port,
                      const char *host, const char *station, const char *token)
{
    size_t hl = strlen(host), sl = strlen(station), tl = strlen(token);
    size_t size = 10 + hl + sl + tl;
    assert(size <= cap && hl <= 255 && sl <= 255 && tl <= 255);
    memcpy(out, "NVF1", 4);
    out[4] = 0;
    out[5] = (uint8_t)(port >> 8);
    out[6] = (uint8_t)port;
    out[7] = (uint8_t)hl;
    out[8] = (uint8_t)sl;
    out[9] = (uint8_t)tl;
    memcpy(out + 10, host, hl);
    memcpy(out + 10 + hl, station, sl);
    memcpy(out + 10 + hl + sl, token, tl);
    return size;
}

int main(void)
{
    uint8_t data[256];
    size_t size = request(data, sizeof data, 5580,
                          "collector.example", "roof", "enrollment-token");
    netcfg_t config;
    char reason[NETCFG_ERR_CAP];
    assert(netcfg_prov_decode(data, size, &config, reason, sizeof reason));
    assert(!strcmp(config.host, "collector.example") && config.port == 5580);
    assert(!strcmp(config.station, "roof") && !strcmp(config.token, "enrollment-token"));
    assert(!config.wifi_ssid[0] && !config.wifi_pass[0] && !config.insecure);

    // Every truncated prefix is rejected; the handler cannot accept a frame
    // whose length bytes refer beyond the encrypted BLE message.
    for (size_t n = 0; n < size; n++)
        assert(!netcfg_prov_decode(data, n, &config, reason, sizeof reason));

    uint8_t saved;
    saved = data[0]; data[0] = 'X';
    assert(!netcfg_prov_decode(data, size, &config, reason, sizeof reason)); data[0] = saved;
    saved = data[4]; data[4] = 1;
    assert(!netcfg_prov_decode(data, size, &config, reason, sizeof reason)); data[4] = saved;
    saved = data[7]; data[7]++;
    assert(!netcfg_prov_decode(data, size, &config, reason, sizeof reason)); data[7] = saved;
    saved = data[10]; data[10] = 0;
    assert(!netcfg_prov_decode(data, size, &config, reason, sizeof reason)); data[10] = saved;
    saved = data[10]; data[10] = 0x80;
    assert(!netcfg_prov_decode(data, size, &config, reason, sizeof reason)); data[10] = saved;
    saved = data[5]; data[5] = 0; data[6] = 0;
    assert(!netcfg_prov_decode(data, size, &config, reason, sizeof reason));
    data[5] = saved; data[6] = (uint8_t)5580;

    // Maximum legal field widths fit below the transport's 480-byte Security 2
    // message limit and round-trip without truncation.
    char host[64], station[33], token[129];
    memset(host, 'h', 63); host[63] = 0;
    memset(station, 's', 32); station[32] = 0;
    memset(token, 't', 128); token[128] = 0;
    size = request(data, sizeof data, 65535, host, station, token);
    assert(size == 233 && netcfg_prov_decode(data, size, &config, reason, sizeof reason));
    assert(strlen(config.host) == 63 && strlen(config.station) == 32 &&
           strlen(config.token) == 128 && config.port == 65535);

    uint8_t response[NETCFG_PROV_RESPONSE_SIZE];
    for (int status = NETCFG_PROV_WAITING_FOR_WIFI;
         status <= NETCFG_PROV_SAVED_RESTART_REQUIRED; status++) {
        netcfg_prov_encode_response(response, (netcfg_prov_status_t)status);
        assert(!memcmp(response, "NVR1", 4) && response[4] == status);
    }

    puts("netcfg BLE payload: bounds, truncation, text, port, maxima, and responses PASS");
    return 0;
}
