#pragma once
#include "update_json.h"
#include "../../../../common/board_uid.h"

#define NVF_TUF_ROOT_CAP 8192
#define NVF_UPDATE_PATH_CAP 193
#define NVF_UPDATE_VERSION 1
typedef enum {
#define NVF_UPDATE_ERROR(symbol,domain,reason,name) symbol=(domain)*1000+(reason),
#include "update_errors.inc"
#undef NVF_UPDATE_ERROR
} nvf_update_error_t;
enum { UP_ROOT, UP_TIMESTAMP, UP_SNAPSHOT, UP_TARGETS, UP_RELEASES, UP_STABLE, UP_CANARY, UP_LAB, UP_ROLES };
// One trust profile is compiled into each build. A root and every release
// manifest name exactly one, and none can authorize firmware for another.
enum { UP_PROFILE_TRUSTED=1, UP_PROFILE_OPEN=2, UP_PROFILE_TEST=3 };
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
    bool withdrawn;
    uint8_t profile;
} nvf_update_release_t;
typedef struct {
    uint64_t now, running_sequence;
    uint8_t board_uid[NVF_BOARD_UID_SIZE];
    uint16_t hardware_revision, layout;
    bool hardware_known;
    uint8_t profile;
    const char *board_family; // this image's board (nvf_board.h); a release must name the same one
} nvf_update_device_t;
typedef struct {
    void *context;
    // Return malloc-owned bytes, with exact length <= limit. 404 alone means
    // UP_NOT_FOUND. The transport must verify TLS and disallow redirects.
    int (*fetch)(void *,const char *path,size_t limit,char **bytes,size_t *length);
    bool (*sha256)(const void *,size_t,uint8_t[32]);
    bool (*public_key)(const char *public_pem); // Require a valid NIST P-256 public key.
    bool (*verify)(const char *public_pem,const uint8_t *sig,size_t siglen,const void *message,size_t length);
    // Atomically persist each accepted role before fetching its descendants.
    bool (*save)(void *,const nvf_tuf_trust_t *trust);
} nvf_tuf_io_t;
int nvf_tuf_initialize(nvf_tuf_trust_t *,const char *root,size_t length,unsigned profile,const nvf_tuf_io_t *);
const char *nvf_update_profile_name(unsigned profile);
int nvf_tuf_refresh(nvf_tuf_trust_t *,unsigned channel,const nvf_update_device_t *,nvf_update_release_t *,const nvf_tuf_io_t *);
bool nvf_update_target_path(const char *path);
