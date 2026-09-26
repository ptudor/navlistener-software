#include "ota_policy.h"
#include "nvf_board.h"
#include <string.h>
static int unhex(char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    return -1;
}
bool nvf_ota_unhex(const char *hex, size_t len, uint8_t *out)
{
    if (!hex || !out || len % 2) return false;
    for (size_t i = 0; i < len; i += 2) {
        int a = unhex(hex[i]), b = unhex(hex[i+1]);
        if (a < 0 || b < 0) return false;
        out[i/2] = (uint8_t)((a << 4) | b);
    }
    return true;
}
void nvf_ota_hex(const uint8_t *bytes, size_t len, char *out)
{
    const char *digits = "0123456789abcdef";
    for (size_t i = 0; i < len; i++) { out[2*i] = digits[bytes[i] >> 4]; out[2*i+1] = digits[bytes[i] & 15]; }
    out[2*len] = 0;
}
bool nvf_ota_parse_request(const char *body, size_t len, char url[NVF_OTA_URL_CAP], uint8_t hash[32])
{
    if (!body || len <= 73 || len >= NVF_OTA_BODY_CAP || body[64] != '\n' ||
        !nvf_ota_unhex(body, 64, hash)) return false;
    size_t n = len - 65;
    const char *u = body + 65;
    if (n >= NVF_OTA_URL_CAP || memcmp(u, "https://", 8)) return false;
    // No userinfo, fragments, control bytes, backslashes or URL-parser ambiguity.
    for (size_t i = 0; i < n; i++)
        if ((unsigned char)u[i] <= 32 || (unsigned char)u[i] >= 127 ||
            u[i] == '@' || u[i] == '#' || u[i] == '\\') return false;
    if (u[8] == '/' || u[8] == '?' || u[8] == ':') return false;
    memcpy(url, u, n); url[n] = 0;
    return true;
}
bool nvf_ota_image_compatible(const uint8_t *p, size_t len, uint16_t chip,
                            uint8_t board, const char *project)
{
    if (!p || !project || len < NVF_OTA_PREFIX_SIZE || p[0] != 0xe9 ||
        p[1] == 0 || p[1] > 16 || (p[12] | ((uint16_t)p[13] << 8)) != chip) return false;
    // ESP image header (24), first segment header (8), esp_app_desc_t (256).
    if (memcmp(p + 32, "\x32\x54\xcd\xab", 4) || strlen(project) >= 32 ||
        memcmp(p + 80, project, strlen(project) + 1)) return false;
    const uint8_t *m = p + 288;
    // The universal image fits every board; another is for the one board the device is.
    bool fits = m[9] == NVF_BOARD_ID_UNIVERSAL || (board != NVF_BOARD_ID_NONE && m[9] == board);
    return memcmp(m, "NVFOTA1", 8) == 0 && m[8] == 2 && fits &&
           m[10] == 1 && m[11] == 0 && m[12] == 3 && !m[13] && !m[14] && !m[15];
    // Schema 2 requires layout 3. Older firmware rejects it before flash writes;
    // the changed bootloader/table/app offsets require an attended USB baseline.
}

bool nvf_ota_consume_nonce(char active[65], int64_t deadline, const char *claimed,
                           int64_t now, bool mac_valid)
{
    if (!active || !claimed || !mac_valid || now >= deadline ||
        strlen(active) != 64 || strlen(claimed) != 64 || strcmp(active, claimed)) return false;
    active[0] = 0;
    return true;
}
