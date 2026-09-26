#include "ota_policy.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>
int main(void)
{
    uint8_t hash[32], image[304] = {0}; char url[NVF_OTA_URL_CAP], body[900];
    memset(body, 'a', 64); strcpy(body+64, "\nhttps://example.invalid/firmware.bin");
    assert(nvf_ota_parse_request(body, strlen(body), url, hash));
    assert(!strcmp(url, "https://example.invalid/firmware.bin"));
    const char *bad[] = {"http://example.invalid/x", "https:///x", "https://user@example.invalid/x",
        "https://example.invalid/#x", "https://example.invalid/\rX:yes", "https://example.invalid/\\x"};
    for (unsigned i = 0; i < sizeof(bad)/sizeof(*bad); i++) {
        strcpy(body+65, bad[i]); assert(!nvf_ota_parse_request(body, strlen(body), url, hash));
    }
    strcpy(body+65, "https://example.invalid/x"); body[0] = 'g';
    assert(!nvf_ota_parse_request(body, strlen(body), url, hash));
    assert(!nvf_ota_unhex("abc", 3, hash));
    image[0] = 0xe9; image[1] = 1; image[12] = 9;
    memcpy(image+32, "\x32\x54\xcd\xab", 4); strcpy((char*)image+80, "navfeeder-esp");
    memcpy(image+288, "NVFOTA1", 8); image[296]=2;image[297]=image[298]=1;image[300]=3;
    assert(nvf_ota_image_compatible(image, sizeof image, 9, 1, "navfeeder-esp"));
    assert(!nvf_ota_image_compatible(image, 303, 9, 1, "navfeeder-esp"));
    assert(!nvf_ota_image_compatible(image, sizeof image, 13, 1, "navfeeder-esp"));
    assert(!nvf_ota_image_compatible(image, sizeof image, 9, 0, "navfeeder-esp"));
    assert(!nvf_ota_image_compatible(image, sizeof image, 9, 2, "navfeeder-esp"));
    // The universal image (board 0) fits every device, including one whose manifest names none.
    image[297] = 0;
    for (uint8_t board = 0; board <= 3; board++)
        assert(nvf_ota_image_compatible(image, sizeof image, 9, board, "navfeeder-esp"));
    image[297] = 1;
    assert(!nvf_ota_image_compatible(image, sizeof image, 9, 1, "another-app"));
    image[299] = 1; assert(!nvf_ota_image_compatible(image, sizeof image, 9, 1, "navfeeder-esp"));
    image[299] = 0; image[298] = 0;
    assert(!nvf_ota_image_compatible(image, sizeof image, 9, 1, "navfeeder-esp"));
    char active[65], claimed[65]; memset(active, 'a', 64); active[64] = 0;
    strcpy(claimed, active);
    assert(!nvf_ota_consume_nonce(active, 100, claimed, 99, false));
    assert(active[0] == 'a');
    assert(!nvf_ota_consume_nonce(active, 100, claimed, 100, true));
    claimed[0] = 'b'; assert(!nvf_ota_consume_nonce(active, 100, claimed, 99, true));
    claimed[0] = 'a'; assert(nvf_ota_consume_nonce(active, 100, claimed, 99, true));
    assert(!nvf_ota_consume_nonce(active, 100, claimed, 99, true)); // replay
    puts("OTA policy: malformed requests, target, project, boot confirmation and factory-write rejection passed");
}
