#include "update_memory.h"
#include "sdkconfig.h"
#include "update_runtime.h"
#if CONFIG_NVF_OTA
#include "ota_download.h"
#include "../../../../common/endpoint_fallback.h"
#include <stdatomic.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <stdio.h>
#include <time.h>
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/queue.h"
#include "freertos/semphr.h"
#include "esp_crt_bundle.h"
#include "esp_flash_encrypt.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_ota_ops.h"
#include "esp_random.h"
#include "esp_rom_crc.h"
#include "esp_secure_boot.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "esp32s3/rom/secure_boot.h"
#include "esp32s3/rom/rsa_pss.h"
#include "mbedtls/pk.h"
#include "mbedtls/sha256.h"
#include "nvs_flash.h"
#include "nvs.h"
#include "spool.h"
#include "journal.h"

#ifndef CONFIG_NVF_UPDATE_TEST_KEYS
#define CONFIG_NVF_UPDATE_TEST_KEYS 0
#endif
#ifndef NVF_BUILD_NUMBER
#define NVF_BUILD_NUMBER 1
#endif
extern const char nvf_update_root[];
extern const size_t nvf_update_root_length;
static const char *TAG="update";
static const char *const origins[]={"https://firmware.intsat.net/firmware/v1/","https://firmware.intsat.space/firmware/v1/"};
typedef struct {
    uint32_t magic,version;
    nvf_update_status_t status;
    nvf_tuf_trust_t trust;
    uint32_t staged_address;
    bool command_pending;
    uint32_t crc;
} update_record;
_Static_assert(sizeof(update_record)*4<128*1024,"NVS transaction and metadata exceed update partition budget");
typedef struct {unsigned action,mode,channel,epoch;uint64_t release;bool discard,collector;nvf_update_command_t command;} job;
static update_record record;
static nvf_update_status_t view;
static nvf_update_hooks_t hooks;
static SemaphoreHandle_t view_lock;
static QueueHandle_t jobs;
static atomic_bool confirmed,busy;
static atomic_uint cancel_epoch;
static unsigned operation_epoch;
static QueueHandle_t discard_results,discard_replies;
static bool is_cancelled(void){return atomic_load(&cancel_epoch)!=operation_epoch;}
static nvs_handle_t storage;
static bool storage_ready,secondary;
static uint64_t clock_now(void){time_t t=time(NULL);return t>0?(uint64_t)t:0;}
static void publish(void) {
    xSemaphoreTake(view_lock,portMAX_DELAY);view=record.status;xSemaphoreGive(view_lock);
}
static bool persist(void) {
    record.crc=esp_rom_crc32_le(0,(const uint8_t*)&record,offsetof(update_record,crc));
    bool ok=storage_ready && nvs_set_blob(storage,"state_v1",&record,sizeof record)==ESP_OK && nvs_commit(storage)==ESP_OK;
    if(!ok){record.status.error=UP_STORAGE;record.status.state=UP_FAILED;record.status.mode=UP_MANUAL;}
    publish();return ok;
}
static bool save_trust(void *context,const nvf_tuf_trust_t *trust){(void)context;record.trust=*trust;return persist();}
static bool sha256(const void *data,size_t length,uint8_t hash[32]) {return mbedtls_sha256(data,length,hash,0)==0;}
static bool verify(const char *pem,const uint8_t *sig,size_t n,const void *data,size_t length) {
    uint8_t hash[32];mbedtls_pk_context key;mbedtls_pk_init(&key);
    bool ok=sha256(data,length,hash) && mbedtls_pk_parse_public_key(&key,(const uint8_t*)pem,strlen(pem)+1)==0 &&
        mbedtls_pk_can_do(&key,MBEDTLS_PK_ECDSA) && mbedtls_pk_get_bitlen(&key)==256 &&
        mbedtls_pk_ec(key)->MBEDTLS_PRIVATE(grp).id==MBEDTLS_ECP_DP_SECP256R1 &&
        mbedtls_pk_verify(&key,MBEDTLS_MD_SHA256,hash,sizeof hash,sig,n)==0;
    mbedtls_pk_free(&key);return ok;
}
static esp_err_t response_header(esp_http_client_event_t *event) {
    if(event->event_id==HTTP_EVENT_ON_HEADER && !strcasecmp(event->header_key,"Content-Encoding") &&
       strcasecmp(event->header_value,"identity"))*(bool*)event->user_data=true;
    return ESP_OK;
}
static int fetch_one(unsigned origin,const char *path,size_t limit,char **bytes,size_t *length) {
    char url[384];if(snprintf(url,sizeof url,"%s%s",origins[origin],path)>=(int)sizeof url)return UP_META_INVALID;
    bool encoded=false;
    esp_http_client_config_t cfg={.event_handler=response_header,.user_data=&encoded,.url=url,.crt_bundle_attach=esp_crt_bundle_attach,.transport_type=HTTP_TRANSPORT_OVER_SSL,
        .disable_auto_redirect=true,.timeout_ms=10000,.buffer_size=2048};
    esp_http_client_handle_t client=esp_http_client_init(&cfg);if(!client)return UP_STORAGE;
    int err=UP_NETWORK;char *data=NULL;size_t received=0;int64_t deadline=esp_timer_get_time()+30000000;
    esp_http_client_set_header(client,"Accept-Encoding","identity");
    if(esp_http_client_open(client,0)!=ESP_OK)goto done;
    int64_t size=esp_http_client_fetch_headers(client);int status=esp_http_client_get_status_code(client);
    if(status==404){err=UP_NOT_FOUND;goto done;}
    if(status!=200 || size<=0 || size>(int64_t)limit || esp_http_client_is_chunked_response(client))goto done;
    if(encoded)goto done;
    data=nvf_update_alloc((size_t)size+1);if(!data){err=UP_STORAGE;goto done;}
    while(received<(size_t)size) {
        if(is_cancelled()||esp_timer_get_time()>deadline)goto done;
        int got=esp_http_client_read(client,data+received,(size_t)size-received);if(got<=0)goto done;received+=got;
        vTaskDelay(1);
    }
    if(!esp_http_client_is_complete_data_received(client))goto done;
    data[received]=0;*bytes=data;*length=received;data=NULL;err=UP_OK;
done:
    free(data);esp_http_client_cleanup(client);return err;
}
static int fetch(void *context,const char *path,size_t limit,char **bytes,size_t *length) {
    (void)context;int err=fetch_one(secondary?1:0,path,limit,bytes,length);
    if(err && !secondary)err=fetch_one(1,path,limit,bytes,length);
    return err;
}
static const nvf_tuf_io_t io={.fetch=fetch,.sha256=sha256,.verify=verify,.save=save_trust};
static void failure(unsigned error,bool retry) {
    record.status.error=error;record.status.error_time=clock_now();
    if(record.status.staged.sequence)record.status.state=UP_STAGED;
    else record.status.state=UP_FAILED;
    if(retry)record.status.next_check=nvf_update_retry(clock_now(),record.status.retry++,esp_random());
    if(error/1000==5)record.status.mode=UP_MANUAL;
    journal_event(JOURNAL_OTA_FAILED,error);persist();
    ESP_LOGW(TAG,"%s",nvf_update_error_name(error));
}
static bool check(void) {
    if(!hooks.online || !hooks.online()){failure(UP_NETWORK,true);return false;}
    record.status.state=UP_CHECKING;record.status.error=UP_OK;publish();
    nvf_update_device_t device=hooks.device;device.now=clock_now();device.running_sequence=record.status.running;
    nvf_update_release_t release;secondary=false;
    int err=nvf_tuf_refresh(&record.trust,record.status.channel,&device,&release,&io);
    // A transport success with stale/corrupt content also gets a second origin.
    // Its metadata must independently pass the complete trust chain.
    if(err && err/1000!=3 && err!=UP_STORAGE && !is_cancelled()) {
        secondary=true;err=nvf_tuf_refresh(&record.trust,record.status.channel,&device,&release,&io);
    }
    if(err){
        if(err/1000==3){
            record.status.available=release;memset(&record.status.staged,0,sizeof record.status.staged);
            record.staged_address=0;record.status.last_check=device.now;
        }
        failure(err,true);return false;
    }
    record.status.available=release;record.status.last_check=device.now;record.status.retry=0;
    record.status.error=UP_OK;record.status.next_check=nvf_update_weekly(device.now,device.eui,record.status.channel,esp_random(),&io);
    if(record.status.staged.sequence && (record.status.staged.sequence!=release.sequence ||
       memcmp(record.status.staged.hash,release.hash,32))) {
        memset(&record.status.staged,0,sizeof record.status.staged);record.staged_address=0;record.status.received=0;
    }
    if(record.status.staged.sequence)record.status.staged=release;
    record.status.state=record.status.staged.sequence?UP_STAGED:release.sequence>record.status.running?UP_AVAILABLE:UP_IDLE;
    return persist();
}
static bool progress(size_t received,size_t total,void *context) {
    (void)context;(void)total;record.status.received=received;publish();
    if(is_cancelled())return false;
    size_t used,capacity;bool psram;
    spool_memory_stats(&used,&capacity,&psram);
    int64_t deadline=esp_timer_get_time()+30000000;
    while(capacity && used>capacity*3/4) {
        if(is_cancelled() || esp_timer_get_time()>=deadline)return false;
        vTaskDelay(pdMS_TO_TICKS(100));spool_memory_stats(&used,&capacity,&psram);
    }
    vTaskDelay(1);return true;
}
static bool partition_hash(const esp_partition_t *slot,size_t length,uint8_t out[32]) {
    uint8_t buffer[1024];mbedtls_sha256_context hash;mbedtls_sha256_init(&hash);
    bool ok=mbedtls_sha256_starts(&hash,0)==0;
    for(size_t offset=0;ok && offset<length;offset+=sizeof buffer) {
        size_t n=length-offset<sizeof buffer?length-offset:sizeof buffer;
        ok=esp_partition_read(slot,offset,buffer,n)==ESP_OK && mbedtls_sha256_update(&hash,buffer,n)==0;
        if(is_cancelled())ok=false;
        vTaskDelay(1);
    }
    if(ok)ok=mbedtls_sha256_finish(&hash,out)==0;
    mbedtls_sha256_free(&hash);return ok;
}
static bool verify_boot_signature(const esp_partition_t *slot,const nvf_update_release_t *release) {
    if(release->length<8192 || release->length%4096 || release->length>slot->size)return false;
    uint32_t padded=(uint32_t)release->length-4096;uint8_t image_hash[32],artifact_hash[32];
    if(!partition_hash(slot,release->length,artifact_hash) || memcmp(artifact_hash,release->hash,32) ||
       !partition_hash(slot,padded,image_hash))return false;
    ets_secure_boot_sig_block_t *block=nvf_update_alloc(sizeof *block);if(!block)return false;
    bool valid=false;
    for(unsigned i=0;i<3;i++) {
        if(esp_partition_read(slot,padded+i*sizeof *block,block,sizeof *block)!=ESP_OK)break;
        uint8_t key_hash[32],verified[32];
        if(block->magic_byte!=0xe7 || block->version!=2 ||
           esp_rom_crc32_le(0,(uint8_t*)block,offsetof(ets_secure_boot_sig_block_t,block_crc))!=block->block_crc ||
           memcmp(block->image_digest,image_hash,32)||!sha256(&block->key,sizeof block->key,key_hash)||memcmp(key_hash,release->boot_key,32))continue;
        if(ets_rsa_pss_verify(&block->key,block->signature,image_hash,verified)){valid=true;break;}
    }
    free(block);return valid;
}
static bool stage(uint64_t sequence) {
    if(!check())return false;
    if(record.status.staged.sequence && record.status.staged.sequence==sequence)return true;
    nvf_update_release_t *release=&record.status.available;
    if(!release->sequence || release->sequence<=record.status.running || (sequence && sequence!=release->sequence)){failure(UP_INELIGIBLE,false);return false;}
    nvf_ota_request_t request;char hex[65];nvf_ota_hex(release->hash,32,hex);
    const char *name=strrchr(release->artifact,'/');if(!name){failure(UP_META_INVALID,false);return false;}
    int n=snprintf(request.url,sizeof request.url,"%stargets/%.*s/%s.%s",origins[0],(int)(name-release->artifact),release->artifact,hex,name+1);
    if(n<0 || n>=(int)sizeof request.url){failure(UP_META_INVALID,false);return false;}
    memcpy(request.hash,release->hash,32);record.status.state=UP_DOWNLOADING;record.status.received=0;
    if(!persist())return false;
    esp_err_t err=nvf_ota_stage(&request,progress,NULL);
    if(is_cancelled()){record.status.state=UP_IDLE;record.status.received=0;persist();return false;}
    if(err!=ESP_OK){failure(err==ESP_ERR_TIMEOUT?UP_NETWORK:UP_ARTIFACT,true);return false;}
    const esp_partition_t *slot=esp_ota_get_next_update_partition(NULL);
    if(!slot || !verify_boot_signature(slot,release)){failure(UP_ARTIFACT,false);return false;}
    record.status.staged=*release;record.staged_address=slot->address;record.status.state=UP_STAGED;
    journal_event(JOURNAL_OTA_READY,0);return persist();
}
static bool install(uint64_t sequence,bool discard,bool automatic) {
    if(!record.status.staged.sequence || (sequence && sequence!=record.status.staged.sequence)){failure(UP_INELIGIBLE,false);return false;}
    nvf_update_release_t saved=record.status.staged;
    bool fresh=check();
    if(!fresh && (automatic || record.status.error/1000!=1))return false;
    if(fresh && (!record.status.staged.sequence || record.status.available.sequence!=saved.sequence)){failure(UP_INELIGIBLE,false);return false;}
    if(is_cancelled())return false;
    const esp_partition_t *slot=esp_ota_get_next_update_partition(NULL);
    if(!slot || slot->address!=record.staged_address || !verify_boot_signature(slot,&saved)){failure(UP_ARTIFACT,false);return false;}
    record.status.state=UP_WAITING_SAFE;publish();
    if(!discard && (!hooks.durable_link || !hooks.durable_link())){failure(UP_DURABILITY,false);record.status.state=UP_WAITING_SAFE;persist();return false;}
    uint64_t final=0;size_t depth=0;spool_stats(NULL,NULL,&depth);
    if(!discard && depth){failure(UP_ACK_TIMEOUT,false);record.status.state=UP_WAITING_SAFE;persist();return false;}
    if(!hooks.pause || !hooks.resume || !hooks.pause(&final)){failure(UP_ACK_TIMEOUT,false);return false;}
    record.status.state=UP_QUIESCING;publish();
    bool drained=discard;
    for(unsigned i=0;!discard && i<50;i++) {
        if(is_cancelled()||!hooks.durable_link())break;
        if(spool_acked()>=final){drained=true;break;}
        vTaskDelay(pdMS_TO_TICKS(100));
    }
    if(!drained){hooks.resume();failure(UP_ACK_TIMEOUT,false);record.status.state=UP_WAITING_SAFE;persist();return false;}
    if(discard){spool_stats(NULL,NULL,&depth);record.status.discarded=depth;}
    if(discard) {
        uint32_t count=record.status.discarded;bool sent=false;
        xQueueOverwrite(discard_results,&count);
        if(xQueueReceive(discard_replies,&sent,pdMS_TO_TICKS(5000))!=pdTRUE || !sent){hooks.resume();failure(UP_ACK_TIMEOUT,false);return false;}
    }
    if(is_cancelled()){hooks.resume();return false;}
    record.status.state=UP_REBOOT_PENDING;record.command_pending=false;
    if(!persist()){hooks.resume();return false;}
    if(esp_ota_set_boot_partition(slot)!=ESP_OK){hooks.resume();failure(UP_STORAGE,false);return false;}
    ESP_LOGW(TAG,"installing release %llu; explicitly discarded records=%lu",(unsigned long long)saved.sequence,(unsigned long)record.status.discarded);
    vTaskDelay(pdMS_TO_TICKS(250));esp_restart();return true;
}
static void do_job(job *j) {
    if(!atomic_load(&confirmed) || !nvf_update_claim()){
        xQueueSend(jobs,j,0);vTaskDelay(pdMS_TO_TICKS(250));return;
    }
    operation_epoch=j->epoch;
    if(is_cancelled() && j->action!=UP_CANCEL && (!j->collector || j->command.action!=UP_CANCEL))goto finished;
    if(j->collector) {
        int accepted=nvf_update_accept_command(&record.status,&j->command,clock_now());
        if(accepted<=0){publish();goto finished;}
        record.command_pending=true;if(!persist())goto finished;
        j->action=j->command.action;j->release=j->command.release;
        if(j->command.hint){record.status.next_check=clock_now()+esp_random()%1801;record.command_pending=false;persist();goto finished;}
        if((j->action==UP_STAGE || j->action==UP_APPLY) && j->command.generation!=record.status.available.generation) {
            if(!check() || j->command.generation!=record.status.available.generation){record.command_pending=false;failure(UP_INELIGIBLE,false);goto finished;}
        }
    }
    if(j->mode) {
        record.status.mode=j->mode;record.status.channel=j->channel;record.status.next_check=clock_now()+300+esp_random()%1501;
        memset(&record.status.staged,0,sizeof record.status.staged);record.staged_address=0;record.status.received=0;record.status.state=UP_IDLE;persist();
    } else switch(j->action) {
    case UP_CHECK:check();break;
    case UP_STAGE:stage(j->release);break;
    case UP_APPLY:install(j->release,j->discard,false);break;
    case UP_CANCEL:
        if(!j->release || j->release==record.status.staged.sequence || j->release==record.status.available.sequence) {
            memset(&record.status.staged,0,sizeof record.status.staged);record.staged_address=0;record.status.received=0;record.status.received=0;record.status.state=UP_IDLE;persist();
        }
        break;
    default:break;
    }
    record.command_pending=false;persist();
finished:
    nvf_update_release();
}
static void worker(void *unused) {
    (void)unused;
    if(record.command_pending) {
        job j={.action=record.status.command.action,.release=record.status.command.release,.epoch=atomic_load(&cancel_epoch)};
        // Revalidate the accepted generation after reboot, without accepting its
        // ID a second time. Expired requests have no delayed side effects.
        if(record.status.command.expires>clock_now() && !record.status.command.hint &&
           (j.action==UP_CHECK || j.action==UP_CANCEL || record.status.command.generation==record.status.available.generation))do_job(&j);
        else {record.command_pending=false;persist();}
    }
    uint64_t next_install=0;
    for(;;) {
        job j;
        if(xQueueReceive(jobs,&j,pdMS_TO_TICKS(1000))==pdTRUE){do_job(&j);continue;}
        if(atomic_load(&confirmed) && record.status.state==UP_TRIAL_BOOT) {
            record.status.state=UP_CONFIRMED;record.status.running=NVF_BUILD_NUMBER;record.status.staged.sequence=0;record.staged_address=0;persist();
        }
        if(!atomic_load(&confirmed) || !hooks.online || !hooks.online() || record.status.mode==UP_MANUAL || clock_now()<1704067200)continue;
        if(!record.status.next_check){record.status.next_check=clock_now()+300+esp_random()%1501;persist();}
        if(clock_now()>=record.status.next_check && nvf_update_claim()) {
            operation_epoch=atomic_load(&cancel_epoch);
            if(check() && record.status.available.sequence>record.status.running && record.status.mode>=UP_DOWNLOAD)stage(record.status.available.sequence);
            nvf_update_release();
        }
        if(record.status.mode==UP_INSTALL && record.status.staged.sequence && clock_now()>=next_install && nvf_update_claim()) {
            operation_epoch=atomic_load(&cancel_epoch);next_install=clock_now()+300;install(record.status.staged.sequence,false,true);nvf_update_release();
        }
    }
}
esp_err_t nvf_update_start(const nvf_update_hooks_t *config) {
    hooks=*config;hooks.device.test_build=CONFIG_NVF_UPDATE_TEST_KEYS;
    view_lock=xSemaphoreCreateMutex();if(!view_lock)return ESP_ERR_NO_MEM;
    record.magic=0x3150554e;record.version=1;record.status.mode=UP_MANUAL;record.status.running=NVF_BUILD_NUMBER;
    const esp_partition_t *partition=esp_partition_find_first(ESP_PARTITION_TYPE_DATA,ESP_PARTITION_SUBTYPE_DATA_NVS,"update_meta");
    bool layout=partition && partition->address==0x620000 && partition->size==0x20000;
    hooks.device.layout=layout?2:1;record.status.layout=hooks.device.layout;
    uint8_t security=layout?16:0;
    if(esp_secure_boot_enabled())security|=1;
    if(esp_get_flash_encryption_mode()==ESP_FLASH_ENC_MODE_RELEASE)security|=2;
    esp_err_t err=ESP_ERR_NOT_FOUND;
    if(layout) {
#if CONFIG_NVS_ENCRYPTION
        const esp_partition_t *keys=esp_partition_find_first(ESP_PARTITION_TYPE_DATA,ESP_PARTITION_SUBTYPE_DATA_NVS_KEYS,NULL);
        nvs_sec_cfg_t cfg;
        err=keys?nvs_flash_read_security_cfg(keys,&cfg):ESP_ERR_NOT_FOUND;
        if(err==ESP_OK){security|=4;err=nvs_flash_secure_init_partition("update_meta",&cfg);if(err==ESP_OK)security|=8;memset(&cfg,0,sizeof cfg);}
#else
        err=nvs_flash_init_partition("update_meta");
#endif
        if(err==ESP_OK)err=nvs_open_from_partition("update_meta","updater",NVS_READWRITE,&storage);
    }
    if(err!=ESP_OK){record.status.error=UP_STORAGE;publish();return err;}
    storage_ready=true;size_t size=sizeof record;update_record *saved=nvf_update_alloc(sizeof record);
    if(!saved)return ESP_ERR_NO_MEM;
    err=nvs_get_blob(storage,"state_v1",saved,&size);
    if(err==ESP_OK && size==sizeof record && saved->magic==record.magic && saved->version==1 &&
       saved->crc==esp_rom_crc32_le(0,(uint8_t*)saved,offsetof(update_record,crc)) && saved->status.mode>=UP_MANUAL && saved->status.mode<=UP_INSTALL && saved->status.channel<3 && saved->status.state<=UP_FAILED && saved->trust.root_length<=NVF_TUF_ROOT_CAP &&
       saved->status.available.length<=0x200000 && saved->status.staged.length<=0x200000)record=*saved;
    else if(err!=ESP_ERR_NVS_NOT_FOUND){free(saved);storage_ready=false;record.status.error=UP_STORAGE;publish();return ESP_ERR_INVALID_STATE;}
    free(saved);record.status.security=security;record.status.layout=hooks.device.layout;record.status.running=NVF_BUILD_NUMBER;
    // Test roots require an explicit unfused test build; production requires
    // the complete security profile. Ordinary development stays service-only.
    if(!CONFIG_NVF_UPDATE_TEST_KEYS && security!=31){record.status.error=UP_TRUST_UNCONFIGURED;record.status.mode=UP_MANUAL;publish();return ESP_ERR_NOT_SUPPORTED;}
    if(!record.trust.root_length) {
        int result=nvf_tuf_initialize(&record.trust,nvf_update_root,nvf_update_root_length,CONFIG_NVF_UPDATE_TEST_KEYS,&io);
        if(result){record.status.error=result;publish();return ESP_ERR_INVALID_STATE;}
    }
    const esp_partition_t *running=esp_ota_get_running_partition();
    if(record.status.state==UP_REBOOT_PENDING || record.status.state==UP_TRIAL_BOOT) {
        record.command_pending=false;
        if(running && running->address==record.staged_address && record.status.staged.sequence==NVF_BUILD_NUMBER)record.status.state=UP_TRIAL_BOOT;
        else {record.status.failed=record.status.staged.sequence;record.status.staged.sequence=0;record.staged_address=0;record.status.state=UP_ROLLED_BACK;record.status.error=UP_TRIAL_FAILED;}
    } else if(record.status.state==UP_DOWNLOADING || record.status.state==UP_CHECKING || record.status.state==UP_QUIESCING)record.status.state=UP_IDLE;
    if(!persist())return ESP_FAIL;
    discard_results=xQueueCreate(1,sizeof(uint32_t));discard_replies=xQueueCreate(1,sizeof(bool));
    jobs=xQueueCreate(4,sizeof(job));if(!jobs || !discard_results || !discard_replies)return ESP_ERR_NO_MEM;
    if(xTaskCreate(worker,"update",16384,NULL,2,NULL)!=pdPASS){vQueueDelete(jobs);jobs=NULL;return ESP_ERR_NO_MEM;}
    return ESP_OK;
}
void nvf_update_confirmed(void){atomic_store(&confirmed,true);}
void nvf_update_status(nvf_update_status_t *out) {
    if(!view_lock){memset(out,0,sizeof *out);out->mode=UP_MANUAL;return;}
    xSemaphoreTake(view_lock,portMAX_DELAY);*out=view;xSemaphoreGive(view_lock);
}
bool nvf_update_request(unsigned action,uint64_t release,bool discard) {
    if(!jobs || action<UP_CHECK || action>UP_CANCEL || (discard && action!=UP_APPLY))return false;
    nvf_update_status_t current;nvf_update_status(&current);
    bool matching=!release || release==current.staged.sequence || release==current.available.sequence;
    if(action==UP_CANCEL && !matching)return false;
    if(discard){xQueueReset(discard_results);xQueueReset(discard_replies);}
    job j={.action=action,.release=release,.discard=discard,.epoch=atomic_load(&cancel_epoch)};
    if(xQueueSend(jobs,&j,0)!=pdTRUE)return false;
    if(action==UP_CANCEL)atomic_fetch_add(&cancel_epoch,1);
    return true;
}
bool nvf_update_policy(unsigned mode,unsigned channel) {
    if(!jobs || mode<UP_MANUAL || mode>UP_INSTALL || channel>2 || (mode!=UP_MANUAL && !hooks.device.hardware_known))return false;
    job j={.mode=mode,.channel=channel,.epoch=atomic_load(&cancel_epoch)};return xQueueSend(jobs,&j,0)==pdTRUE;
}
void nvf_update_control(const uint8_t *bytes,size_t length) {
    if(!jobs)return;
    job j={.collector=true,.epoch=atomic_load(&cancel_epoch)};
    if(!nvf_update_decode_command(bytes,length,&j.command))return;
    nvf_update_status_t current;nvf_update_status(&current);
    bool matching=!j.command.release || j.command.release==current.staged.sequence || j.command.release==current.available.sequence;
    if(nvf_update_accept_command(&current,&j.command,clock_now())<0)return;
    if(xQueueSend(jobs,&j,0)==pdTRUE && j.command.action==UP_CANCEL && matching)atomic_fetch_add(&cancel_epoch,1);
}
bool nvf_update_wire(uint8_t out[140]){if(!view_lock)return false;nvf_update_status_t s;nvf_update_status(&s);nvf_update_encode_status(&s,out);return true;}
bool nvf_update_busy(void){return atomic_load(&busy);}
bool nvf_update_claim(void){bool expected=false;return atomic_compare_exchange_strong(&busy,&expected,true);}
void nvf_update_release(void){atomic_store(&busy,false);}
bool nvf_update_discard_result(uint32_t *count,unsigned timeout){return discard_results && xQueueReceive(discard_results,count,pdMS_TO_TICKS(timeout))==pdTRUE;}
void nvf_update_discard_response(bool sent){if(discard_replies)xQueueOverwrite(discard_replies,&sent);}
#else
esp_err_t nvf_update_start(const nvf_update_hooks_t *h){(void)h;return ESP_ERR_NOT_SUPPORTED;}
void nvf_update_confirmed(void){}
void nvf_update_status(nvf_update_status_t *s){*s=(nvf_update_status_t){.mode=UP_MANUAL};}
bool nvf_update_request(unsigned a,uint64_t r,bool d){(void)a;(void)r;(void)d;return false;}
bool nvf_update_policy(unsigned m,unsigned c){(void)m;(void)c;return false;}
void nvf_update_control(const uint8_t *b,size_t n){(void)b;(void)n;}
bool nvf_update_wire(uint8_t out[140]){(void)out;return false;}
bool nvf_update_busy(void){return false;}
bool nvf_update_claim(void){return false;}
void nvf_update_release(void){}
bool nvf_update_discard_result(uint32_t *c,unsigned t){(void)c;(void)t;return false;}
void nvf_update_discard_response(bool s){(void)s;}
#endif
