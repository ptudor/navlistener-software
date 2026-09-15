#ifndef NVF_OTA_POLICY_H
#define NVF_OTA_POLICY_H
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#define NVF_OTA_URL_CAP 768
#define NVF_OTA_PREFIX_SIZE 304
#define NVF_OTA_BODY_CAP (NVF_OTA_URL_CAP + 65)
bool nvf_ota_unhex(const char *hex, size_t len, uint8_t *out);
void nvf_ota_hex(const uint8_t *bytes, size_t len, char *out);
bool nvf_ota_parse_request(const char *body, size_t len, char url[NVF_OTA_URL_CAP], uint8_t hash[32]);
bool nvf_ota_image_compatible(const uint8_t *prefix, size_t len, uint16_t chip,
                            uint8_t board, const char *project);
// Successful MAC verification consumes the matching live nonce exactly once.
bool nvf_ota_consume_nonce(char active[65], int64_t deadline, const char *claimed,
                           int64_t now, bool mac_valid);
#endif
