#include "update_state.h"
#include <string.h>

static uint64_t be64(const uint8_t *p){uint64_t n=0;for(unsigned i=0;i<8;i++)n=(n<<8)|p[i];return n;}
static void put(uint8_t *p,uint64_t n,unsigned size){for(unsigned i=0;i<size;i++)p[i]=(uint8_t)(n>>(8*(size-1-i)));}
bool nvf_update_decode_command(const uint8_t *p,size_t length,nvf_update_command_t *out) {
    if(!p || length!=36 || p[0]!=1 || p[1]<UP_CHECK || p[1]>UP_CANCEL || p[2] || p[3]>1 || (p[3] && p[1]!=UP_CHECK))return false;
    *out=(nvf_update_command_t){.action=p[1],.hint=p[3]!=0,.id=be64(p+4),.generation=be64(p+12),.release=be64(p+20),.expires=be64(p+28)};
    return out->id && out->expires && ((out->action!=UP_STAGE && out->action!=UP_APPLY) || (out->generation && out->release));
}
int nvf_update_accept_command(nvf_update_status_t *s,const nvf_update_command_t *c,uint64_t now) {
    if(now<1704067200 || !c->id || c->id<s->last_command || c->expires<=now ||
       c->action<UP_CHECK || c->action>UP_CANCEL || (c->hint && (c->action!=UP_CHECK || s->mode==UP_MANUAL)) ||
       ((c->action==UP_STAGE || c->action==UP_APPLY) && (!c->generation || !c->release)))return -1;
    if(c->id==s->last_command) {
        const nvf_update_command_t *old=&s->command;
        return old->action==c->action && old->hint==c->hint && old->generation==c->generation && old->release==c->release && old->expires==c->expires?0:-1;
    }
    s->command=*c;s->last_command=c->id;return 1;
}
uint64_t nvf_update_weekly(uint64_t now,const uint8_t eui[8],unsigned channel,uint32_t jitter,const nvf_tuf_io_t *io) {
    uint8_t seed[10],hash[32];memcpy(seed,eui,8);seed[8]=channel;seed[9]=1;
    if(!io->sha256(seed,sizeof seed,hash))return 0;
    uint64_t slot=be64(hash)%604800;
    uint64_t start=(now-345600)/604800*604800+345600;
    uint64_t next=start+slot;if(next<=now)next+=604800;
    return next+jitter%301;
}
uint64_t nvf_update_retry(uint64_t now,unsigned attempt,uint32_t random) {
    uint64_t delay=attempt==0?3600:attempt==1?21600:86400;
    return now+delay+random%(delay/4+1);
}
void nvf_update_encode_status(const nvf_update_status_t *s,uint8_t p[140]) {
    memset(p,0,140);p[0]=1;p[1]=s->mode;p[2]=s->channel+1;p[3]=s->state;p[4]=s->security;
    put(p+6,s->layout,2);put(p+8,s->running,8);put(p+16,s->available.sequence,8);put(p+24,s->staged.sequence,8);put(p+32,s->failed,8);
    put(p+40,s->received,4);put(p+44,s->staged.sequence?s->staged.length:s->available.length,4);
    put(p+48,s->last_check,8);put(p+56,s->next_check,8);put(p+64,s->last_command,8);
    put(p+72,s->error/1000,2);put(p+74,s->error%1000,2);
    const nvf_update_release_t *r=s->staged.sequence?&s->staged:&s->available;
    memcpy(p+76,r->boot_key,32);memcpy(p+108,r->release_key,32);
}
const char *nvf_update_error_name(unsigned error) {
    switch(error) {
#define NVF_UPDATE_ERROR(symbol,domain,reason,name) case symbol:return name;
#include "update_errors.inc"
#undef NVF_UPDATE_ERROR
    default:return "UPDATE_UNKNOWN_ERROR";
    }
}
