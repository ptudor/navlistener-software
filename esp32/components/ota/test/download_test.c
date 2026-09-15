// Fault injection through the real download/verification/selection code.
// The transport, digest primitive and flash are deterministic facades; this
// tests state transitions, not physical flash electronics or TLS cryptography.
#include "ota_download.h"
#include "idf.h"
#include <assert.h>
#include <setjmp.h>
#include <stdio.h>
#include <string.h>
static uint8_t image[700], flash[700];
static esp_partition_t running = {0, 0, 0x200000}, slot = {0, 16, 0x200000};
static const esp_app_desc_t app = {.project_name = "navfeeder-esp"};
static size_t offset, written;
static int selected, begun, ended, aborted, complete, chunked, http_status;
static int short_read, fail_write, fail_end, steps, cut_at;
static int64_t reported_length, fake_time;
static bool slow;
static int attempts, primary_failure;
static jmp_buf reboot;
static void boundary(void) { if (++steps == cut_at) longjmp(reboot, 1); }
const esp_partition_t *esp_ota_get_running_partition(void) { return &running; }
const esp_partition_t *esp_ota_get_next_update_partition(const esp_partition_t *p) { (void)p; return &slot; }
const esp_partition_t *esp_ota_get_boot_partition(void) { return &running; }
const esp_app_desc_t *esp_app_get_description(void) { return &app; }
esp_err_t esp_ota_begin(const esp_partition_t *p,size_t n,esp_ota_handle_t *h)
{ assert(p == &slot && n == OTA_WITH_SEQUENTIAL_WRITES); boundary(); written=0; begun++; *h = 1; return ESP_OK; }
esp_err_t esp_ota_write(esp_ota_handle_t h,const void *data,size_t n)
{
    assert(h == 1 && begun && written+n <= sizeof flash); boundary();
    if (fail_write) return ESP_FAIL;
    memcpy(flash+written,data,n); written += n; boundary(); return ESP_OK;
}
esp_err_t esp_ota_end(esp_ota_handle_t h)
{ assert(h == 1 && written == sizeof image); boundary(); ended++; return fail_end ? ESP_FAIL : ESP_OK; }
esp_err_t esp_ota_abort(esp_ota_handle_t h) { assert(h == 1); aborted++; return ESP_OK; }
esp_err_t esp_ota_set_boot_partition(const esp_partition_t *p)
{ assert(p == &slot && ended && complete && !memcmp(flash,image,sizeof image)); boundary(); selected=1; boundary(); return ESP_OK; }
int esp_crt_bundle_attach(void *p) { (void)p; return 0; }
esp_http_client_handle_t esp_http_client_init(const esp_http_client_config_t *c)
{
    assert(c->disable_auto_redirect && c->transport_type == HTTP_TRANSPORT_OVER_SSL && c->crt_bundle_attach);
    attempts++; offset=0;
    if (primary_failure) assert(!strcmp(c->url, attempts == 1
        ? "https://firmware.intsat.net:443/firmware/v1/app.bin?build=1"
        : "https://firmware.intsat.space:443/firmware/v1/app.bin?build=1"));
    return (void*)1;
}
esp_err_t esp_http_client_open(esp_http_client_handle_t h,int n)
{ (void)h;(void)n; boundary(); return primary_failure == 1 && attempts == 1 ? ESP_FAIL : ESP_OK; }
int64_t esp_http_client_fetch_headers(esp_http_client_handle_t h) { (void)h; return reported_length; }
int esp_http_client_get_status_code(esp_http_client_handle_t h)
{ (void)h; return primary_failure == 2 && attempts == 1 ? 503 : http_status; }
bool esp_http_client_is_chunked_response(esp_http_client_handle_t h) { (void)h; return chunked; }
int esp_http_client_read(esp_http_client_handle_t h,char *out,int n)
{
    (void)h; boundary(); if (slow) fake_time += 11000000; if (short_read && offset >= 350) return 0;
    if (primary_failure == 3 && attempts == 1 && offset >= 350) return 0;
    if (n > 37) n = 37; // exercise fragmented headers and final short chunks
    if ((size_t)n > sizeof image-offset) n = sizeof image-offset;
    memcpy(out,image+offset,n); offset += n; return n;
}
bool esp_http_client_is_complete_data_received(esp_http_client_handle_t h) { (void)h; return complete && offset == sizeof image; }
void esp_http_client_cleanup(esp_http_client_handle_t h) { (void)h; }
int64_t esp_timer_get_time(void) { return fake_time; }
void vTaskDelay(int n) { (void)n; }
void mbedtls_sha256_init(mbedtls_sha256_context *c) { c->value = 0; }
int mbedtls_sha256_starts(mbedtls_sha256_context *c,int n) { (void)n; c->value = 0; return 0; }
int mbedtls_sha256_update(mbedtls_sha256_context *c,const uint8_t *p,size_t n)
{ for (size_t i=0;i<n;i++) c->value = c->value * 33 + p[i]; return 0; }
int mbedtls_sha256_finish(mbedtls_sha256_context *c,uint8_t *out)
{ for (unsigned i=0;i<32;i++) out[i] = c->value >> ((i%4)*8); return 0; }
void mbedtls_sha256_free(mbedtls_sha256_context *c) { (void)c; }
void spool_stats(uint64_t *n,uint64_t *d,size_t *c) { (void)d;(void)c; *n = 42; }
uint64_t spool_acked(void) { return 42; }
static nvf_ota_request_t request;
static void reset(void)
{
    offset=written=0; fake_time=1000; slow=false; selected=begun=ended=aborted=0; complete=1; chunked=0; http_status=200;
    short_read=fail_write=fail_end=steps=cut_at=attempts=primary_failure=0; reported_length=sizeof image; slot.subtype=16;
    memset(image,0,sizeof image); memset(flash,0xee,sizeof flash);
    image[0]=0xe9; image[1]=1; image[12]=9;
    memcpy(image+32,"\x32\x54\xcd\xab",4); strcpy((char*)image+80,"navfeeder-esp");
    memcpy(image+288,"NVFOTA1",8); image[296]=image[297]=image[298]=1;
    strcpy(request.url,"https://example.invalid/app.bin");
    mbedtls_sha256_context sha; mbedtls_sha256_init(&sha);
    mbedtls_sha256_update(&sha,image,sizeof image); mbedtls_sha256_finish(&sha,request.hash);
}
int main(void)
{
    reset(); assert(nvf_ota_download(&request) == ESP_OK && selected && ended && !aborted);
    int boundaries = steps;
    reset(); request.hash[0]^=1; assert(nvf_ota_download(&request) != ESP_OK && !selected && aborted && !ended);
    reset(); image[12]=13; assert(nvf_ota_download(&request) != ESP_OK && !begun && !selected);
    reset(); image[299]=1; assert(nvf_ota_download(&request) != ESP_OK && !begun);
    reset(); reported_length=0x200001; assert(nvf_ota_download(&request) != ESP_OK && !begun);
    reset(); reported_length=200; assert(nvf_ota_download(&request) != ESP_OK && !begun);
    reset(); http_status=302; assert(nvf_ota_download(&request) != ESP_OK && !begun);
    reset(); chunked=1; assert(nvf_ota_download(&request) != ESP_OK && !begun);
    reset(); slow=true; assert(nvf_ota_download(&request) == ESP_ERR_TIMEOUT && aborted && !selected);
    reset(); short_read=1; assert(nvf_ota_download(&request) != ESP_OK && aborted && !selected);
    reset(); fail_write=1; assert(nvf_ota_download(&request) != ESP_OK && aborted && !selected);
    reset(); fail_end=1; assert(nvf_ota_download(&request) != ESP_OK && ended && !aborted && !selected);
    reset(); complete=0; assert(nvf_ota_download(&request) != ESP_OK && aborted && !selected);
    reset(); slot.subtype=0; assert(nvf_ota_download(&request) != ESP_OK && !begun);
    for (int failure=1; failure<=3; failure++) {
        reset(); primary_failure=failure;
        strcpy(request.url,"https://firmware.intsat.net:443/firmware/v1/app.bin?build=1");
        assert(nvf_ota_download(&request) == ESP_OK && selected && attempts == 2);
        assert(aborted == (failure == 3));
    }
    reset(); strcpy(request.url,"https://firmware.intsat.net/app.bin"); fail_write=1;
    assert(nvf_ota_download(&request) != ESP_OK && attempts == 1 && !selected);
    reset(); strcpy(request.url,"https://firmware.intsat.net/app.bin"); request.hash[0]^=1;
    assert(nvf_ota_download(&request) != ESP_OK && attempts == 2 && !selected && aborted == 2);
    for (int i=1;i<=boundaries;i++) {
        reset(); cut_at=i;
        if (setjmp(reboot) == 0) (void)nvf_ota_download(&request);
        // Either old boot selection, or the fully verified new image. Never
        // select incomplete data, and never write the running/factory slot.
        if (selected) assert(ended && written == sizeof image && !memcmp(flash,image,sizeof image));
    }
    printf("OTA download: errors and %d interrupted boundaries preserve boot selection\n", boundaries);
}
