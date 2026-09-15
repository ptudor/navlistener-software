#pragma once
#include "update_json.h"

#define NVF_TUF_ROOT_CAP 8192
#define NVF_UPDATE_PATH_CAP 193
#define NVF_UPDATE_VERSION 1
typedef enum {
#define NVF_UPDATE_ERROR(symbol,domain,reason,name) symbol=(domain)*1000+(reason),
#include "update_errors.inc"
#undef NVF_UPDATE_ERROR
} nvf_update_error_t;
enum { UP_ROOT, UP_TIMESTAMP, UP_SNAPSHOT, UP_TARGETS, UP_RELEASES, UP_STABLE, UP_CANARY, UP_LAB, UP_ROLES };
typedef struct {
    uint32_t root_length;
    char root[NVF_TUF_ROOT_CAP+1];
    uint64_t versions[UP_ROLES], generations[3], trusted_time;
    uint8_t digests[UP_ROLES][32];
    uint8_t generation_digests[3][32];
    uint64_t snapshot_versions[5];
} nvf_tuf_trust_t;
typedef struct {
    uint64_t sequence, generation, length, published;
    uint16_t layout;
    uint8_t hash[32], boot_key[32], release_key[32];
    char artifact[NVF_UPDATE_PATH_CAP], manifest[NVF_UPDATE_PATH_CAP];
    char notes[NVF_UPDATE_PATH_CAP], version[32], advisory[513];
    bool withdrawn, test_only;
} nvf_update_release_t;
typedef struct {
    uint64_t now, running_sequence;
    uint8_t eui[8];
    uint16_t hardware_revision, layout;
    bool hardware_known, test_build;
} nvf_update_device_t;
typedef struct {
    void *context;
    // Return malloc-owned bytes, with exact length <= limit. 404 alone means
    // UP_NOT_FOUND. The transport must verify TLS and disallow redirects.
    int (*fetch)(void *,const char *path,size_t limit,char **bytes,size_t *length);
    bool (*sha256)(const void *,size_t,uint8_t[32]);
    bool (*verify)(const char *public_pem,const uint8_t *sig,size_t siglen,const void *message,size_t length);
    // Atomically persist accepted root progress / completed refresh before use.
    bool (*save)(void *,const nvf_tuf_trust_t *trust);
} nvf_tuf_io_t;
int nvf_tuf_initialize(nvf_tuf_trust_t *,const char *root,size_t length,bool test_build,const nvf_tuf_io_t *);
int nvf_tuf_refresh(nvf_tuf_trust_t *,unsigned channel,const nvf_update_device_t *,nvf_update_release_t *,const nvf_tuf_io_t *);
bool nvf_update_target_path(const char *path);
