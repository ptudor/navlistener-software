#include "update_api.h"
#include <string.h>

bool nvf_update_decimal(const char *s,uint64_t *out) {
    if(!s || !*s || (s[0]=='0' && s[1]))return false;
    uint64_t n=0;
    for(;*s;s++) {
        if(*s<'0' || *s>'9' || n>(UINT64_MAX-(unsigned)(*s-'0'))/10)return false;
        n=n*10+(unsigned)(*s-'0');
    }
    *out=n;return true;
}
bool nvf_update_api_parse(const char *method,const char *path,const char *body,size_t length,nvf_update_api_request_t *out) {
    uj_doc doc={0};*out=(nvf_update_api_request_t){0};
    if(length>256 || !uj_parse(&doc,body,length))return false;
    const uj_node *root=uj_root(&doc);bool ok=false;
    if(!strcmp(method,"PUT") && !strcmp(path,"/ota/v1/policy")) {
        const char *const fields[]={"mode","channel"};
        const char *mode=uj_string(uj_get(&doc,root,"mode")),*channel=uj_string(uj_get(&doc,root,"channel"));
        const char *const modes[]={"manual","download","install"},*const channels[]={"stable","canary","lab"};
        if(!uj_fields(&doc,root,fields,2)||!mode||!channel)goto done;
        unsigned m,c;for(m=0;m<3;m++)if(!strcmp(mode,modes[m]))break;
        for(c=0;c<3;c++)if(!strcmp(channel,channels[c]))break;
        if(m==3||c==3)goto done;
        out->mode=m+1;out->channel=c;ok=true;
    } else if(!strcmp(method,"POST")) {
        const char *const paths[]={"/ota/v1/check","/ota/v1/download","/ota/v1/install","/ota/v1/cancel"};
        unsigned action;for(action=0;action<4;action++)if(!strcmp(path,paths[action]))break;
        if(action==4)goto done;
        out->action=action+1;
        const char *const fields[]={"release_sequence","discard_backlog"};
        if(!uj_fields(&doc,root,fields,action==0?0:action==2?2:1))goto done;
        if(action && !nvf_update_decimal(uj_string(uj_get(&doc,root,"release_sequence")),&out->release))goto done;
        if((action==1||action==2) && !out->release)goto done;
        if(action==2) {
            const uj_node *discard=uj_get(&doc,root,"discard_backlog");
            if(!discard||discard->kind!=UJ_BOOL)goto done;
            out->discard=discard->number!=0;
        }
        ok=true;
    }
done:
    uj_free(&doc);return ok;
}
