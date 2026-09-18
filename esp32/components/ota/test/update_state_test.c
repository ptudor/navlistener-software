#include "update_state.h"
#include "update_api.h"
#include "ota_policy.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

static void fixture(const char *name,uint8_t *out,size_t size) {
    char path[200],hex[281];snprintf(path,sizeof path,"../../../../common/fixtures/%s.hex",name);
    FILE *f=fopen(path,"r");assert(f && fread(hex,1,size*2,f)==size*2);fclose(f);
    assert(nvf_ota_unhex(hex,size*2,out));
}
static bool hash(const void *data,size_t size,uint8_t out[32]) {
    assert(size==10);memset(out,0,32);memcpy(out,data,8);out[7]^=((const uint8_t*)data)[8];return true;
}
static bool parse(const char *method,const char *path,const char *body) {
    nvf_update_api_request_t request;return nvf_update_api_parse(method,path,body,strlen(body),&request);
}
int main(void) {
    uint8_t control[36],expected[140],actual[140];fixture("update-control-v1",control,36);fixture("update-status-v1",expected,140);
    nvf_update_command_t command;assert(nvf_update_decode_command(control,36,&command));
    assert(command.id==UINT64_C(9007199254740993) && command.release==UINT64_MAX-1 && command.generation==UINT64_C(0x102030405060708));
    nvf_update_status_t s={.mode=UP_INSTALL,.channel=1,.state=UP_WAITING_SAFE,.security=31,.layout=2,
        .running=UINT64_C(9007199254740993),.failed=31,.received=8192,.last_check=1800000000,.next_check=1800003600,.error=UP_ACK_TIMEOUT};
    assert(nvf_update_accept_command(&s,&command,1800000000)==1);
    assert(nvf_update_accept_command(&s,&command,1800000000)==0);
    command.release--;assert(nvf_update_accept_command(&s,&command,1800000000)==-1);command.release++;
    assert(nvf_update_accept_command(&s,&command,command.expires)==-1);
    command.id--;assert(nvf_update_accept_command(&s,&command,1800000000)==-1);command.id+=2;
    command.hint=true;command.action=UP_CHECK;s.mode=UP_MANUAL;
    assert(nvf_update_accept_command(&s,&command,1800000000)==-1);
    s.mode=UP_INSTALL;s.available.sequence=s.staged.sequence=UINT64_MAX-1;s.staged.length=12288;
    memset(s.staged.boot_key,0xab,32);memset(s.staged.release_key,0xcd,32);
    nvf_update_encode_status(&s,UP_PROFILE_TRUSTED,actual);assert(!memcmp(expected,actual,140));
    for(size_t n=0;n<36;n++)assert(!nvf_update_decode_command(control,n,&command));
    control[2]=1;assert(!nvf_update_decode_command(control,36,&command));control[2]=0;
    control[3]=1;assert(!nvf_update_decode_command(control,36,&command));
    assert(parse("POST","/ota/v1/check","{}"));
    assert(parse("POST","/ota/v1/install","{\"release_sequence\":\"18446744073709551615\",\"discard_backlog\":false}"));
    assert(parse("PUT","/ota/v1/policy","{\"mode\":\"download\",\"channel\":\"lab\"}"));
    const char *bad[]={"{\"release_sequence\":9007199254740993}","{\"release_sequence\":\"01\"}","{\"release_sequence\":\"18446744073709551616\"}",
        "{\"release_sequence\":\"1\",\"release_sequence\":\"2\"}","{\"release_sequence\":\"0\"}","{\"release_sequence\":\"1\",\"url\":\"https://example.invalid\"}"};
    for(unsigned i=0;i<sizeof bad/sizeof bad[0];i++)assert(!parse("POST","/ota/v1/download",bad[i]));
    assert(!parse("GET","/ota/v1/check","{}"));assert(!parse("POST","/ota/v1/check?x=1","{}"));
    assert(!parse("POST","/ota/v1/install","{\"release_sequence\":\"1\"}"));
    assert(!parse("POST","/ota/v1/check","{\"discard_backlog\":true}"));
    uint64_t now=1800000000;uint8_t eui[8]={1,2,3,4,5,6,7,8};nvf_tuf_io_t io={.sha256=hash};
    uint64_t weekly=nvf_update_weekly(now,eui,1,0,&io);assert(weekly>now && weekly<=now+604800);
    assert(nvf_update_weekly(now,eui,1,300,&io)==weekly+300);
    assert(nvf_update_weekly(weekly,eui,1,0,&io)==weekly+604800);
    assert(nvf_update_retry(now,0,0)==now+3600 && nvf_update_retry(now,1,0)==now+21600 && nvf_update_retry(now,20,0)==now+86400);
    nvf_tuf_trust_t trust={0};s.error=UP_NETWORK;s.staged.generation=7;trust.generations[s.channel]=7;
    assert(nvf_update_offline_install_allowed(&s,&trust));
    // A newer channel may withdraw the staged release and choose a different
    // release whose manifest cannot be downloaded. Restoring these persisted
    // records after a power cut must still prohibit the old offline install.
    trust.generations[s.channel]=8;assert(!nvf_update_offline_install_allowed(&s,&trust));
    trust.generations[s.channel]=7;s.error=UP_META_SIGNATURE;assert(!nvf_update_offline_install_allowed(&s,&trust));
    s.error=UP_NETWORK;s.staged.generation=0;assert(!nvf_update_offline_install_allowed(&s,&trust));
    puts("Update protocol golden bytes, replay protection, scheduling and strict API parsing passed");
}
