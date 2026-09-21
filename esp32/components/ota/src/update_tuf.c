#include "update_memory.h"
#include "update_tuf.h"
#include "ota_policy.h"
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

typedef struct { uj_doc doc;char *bytes;size_t length;const uj_node *body; } metadata;
static const char *const role_names[]={"root","timestamp","snapshot","targets","releases","stable","canary","lab"};
static void close_meta(metadata *m){uj_free(&m->doc);free(m->bytes);memset(m,0,sizeof *m);}
static const uj_node *get(const metadata *m,const uj_node *n,const char *key){return uj_get(&m->doc,n,key);}
static const char *str(const metadata *m,const uj_node *n,const char *key){return uj_string(get(m,n,key));}
static bool eq(const char *a,const char *b){return a && b && !strcmp(a,b);}
static bool number(const metadata *m,const uj_node *n,const char *key,uint64_t *v){return uj_uint(get(m,n,key),v);}
static bool boolean(const metadata *m,const uj_node *n,const char *key,bool *v) {
    const uj_node *b=get(m,n,key);if(!b || b->kind!=UJ_BOOL)return false;*v=b->number!=0;return true;
}
static unsigned entries(const metadata *m,const uj_node *n) {
    if(!n || (n->kind!=UJ_OBJECT && n->kind!=UJ_ARRAY))return UINT32_MAX;
    unsigned count=0;for(unsigned i=n->first;i;i=m->doc.nodes[i].next){count++;if(n->kind==UJ_OBJECT)i=m->doc.nodes[i].next;}return count;
}
static int parse(metadata *m,char *bytes,size_t length) {
    m->bytes=bytes;m->length=length;
    if(!bytes || !length || length>UJ_MAX_BYTES)return UP_META_INVALID;
    if(!uj_parse(&m->doc,bytes,length))return UP_META_INVALID;
    const char *const fields[]={"signed","signatures"};
    if(!uj_fields(&m->doc,uj_root(&m->doc),fields,2))return UP_META_INVALID;
    m->body=get(m,uj_root(&m->doc),"signed");
    return m->body && m->body->kind==UJ_OBJECT?UP_OK:UP_META_INVALID;
}
static bool sha_text(const char *text,uint8_t bytes[32]) { return text && strlen(text)==64 && nvf_ota_unhex(text,64,bytes); }
const char *nvf_update_profile_name(unsigned profile) {
    static const char *const names[]={"trusted","open","test"};
    return profile>=UP_PROFILE_TRUSTED && profile<=UP_PROFILE_TEST?names[profile-UP_PROFILE_TRUSTED]:NULL;
}
// The marker is mandatory: an unmarked root or manifest belongs to no profile.
static bool profile_marker(const metadata *m,const char *field,unsigned expected) {
    return eq(str(m,m->body,field),nvf_update_profile_name(expected));
}
static bool expiration(const char *text,uint64_t *result) {
    if(!text || strlen(text)!=20)return false;
    unsigned v[6]={0};const unsigned starts[]={0,5,8,11,14,17},lens[]={4,2,2,2,2,2};
    if(text[4]!='-'||text[7]!='-'||text[10]!='T'||text[13]!=':'||text[16]!=':'||text[19]!='Z')return false;
    for(unsigned i=0;i<6;i++)for(unsigned j=0;j<lens[i];j++){char c=text[starts[i]+j];if(c<'0'||c>'9')return false;v[i]=v[i]*10+c-'0';}
    unsigned y=v[0],month=v[1],day=v[2];
    if(y<1970 || y>9999 || month<1 || month>12 || day<1 || v[3]>23 || v[4]>59 || v[5]>59)return false;
    bool leap=y%4==0 && (y%100!=0 || y%400==0);const unsigned lengths[]={31,28,31,30,31,30,31,31,30,31,30,31};
    if(day>lengths[month-1]+(month==2 && leap))return false;
    uint64_t days=(uint64_t)(y-1970)*365+(y-1)/4-1969/4-(y-1)/100+1969/100+(y-1)/400-1969/400;
    for(unsigned m=1;m<month;m++)days+=lengths[m-1]+(m==2 && leap);
    *result=(days+day-1)*86400+v[3]*3600+v[4]*60+v[5];return true;
}
static int header(const metadata *m,unsigned role,uint64_t now,bool check_expiry,uint64_t *version) {
    const char *type=role>=UP_TARGETS?"targets":role_names[role];
    const char *spec=str(m,m->body,"spec_version");uint64_t expires;
    if(!eq(str(m,m->body,"_type"),type) || !spec || strncmp(spec,"1.0.",4) ||
       !spec[4] || strspn(spec+4,"0123456789")!=strlen(spec+4) ||
       !number(m,m->body,"version",version) || !*version ||
       !expiration(str(m,m->body,"expires"),&expires))return UP_META_INVALID;
    if(check_expiry && expires<=now)return UP_META_EXPIRED;
    const char *const root[]={"_type","spec_version","version","expires","keys","roles","consistent_snapshot","x_navlisten_profile"};
    const char *const meta[]={"_type","spec_version","version","expires","meta"};
    const char *const targets[]={"_type","spec_version","version","expires","targets","delegations"};
    const char *const *fields=role==UP_ROOT?root:role<UP_TARGETS?meta:targets;
    return uj_fields(&m->doc,m->body,fields,role==UP_ROOT?8:role<UP_TARGETS?5:6)?UP_OK:UP_META_INVALID;
}
static int signatures(const metadata *m,const metadata *authority,const uj_node *keys,const uj_node *role,const nvf_tuf_io_t *io,uint8_t first_key[32]) {
    uint64_t threshold;
    const uj_node *ids=get(authority,role,"keyids");unsigned key_count=entries(authority,ids);
    const uj_node *sigs=get(m,uj_root(&m->doc),"signatures");unsigned sig_count=entries(m,sigs);
    if(!number(authority,role,"threshold",&threshold) || !threshold || threshold>key_count || key_count>16 || sig_count>16 ||
       !keys || keys->kind!=UJ_OBJECT || !ids || ids->kind!=UJ_ARRAY || !sigs || sigs->kind!=UJ_ARRAY)return UP_META_INVALID;
    for(unsigned k=ids->first;k;k=authority->doc.nodes[k].next) {
        const char *id=uj_string(&authority->doc.nodes[k]);uint8_t digest[32];
        if(!sha_text(id,digest) || !get(authority,keys,id))return UP_META_INVALID;
        for(unsigned prior=ids->first;prior!=k;prior=authority->doc.nodes[prior].next)
            if(eq(id,uj_string(&authority->doc.nodes[prior])))return UP_META_INVALID;
    }
    size_t length;char *canonical=uj_canonical(&m->doc,m->body,&length);if(!canonical)return UP_STORAGE;
    unsigned valid=0;bool seen[16]={false};
    for(unsigned i=sigs->first;i;i=m->doc.nodes[i].next) {
        const uj_node *signature=&m->doc.nodes[i];const char *id=str(m,signature,"keyid"),*sig=str(m,signature,"sig");
        const char *const sig_fields[]={"keyid","sig"};
        if(!uj_fields(&m->doc,signature,sig_fields,2)||!id||!sig){free(canonical);return UP_META_INVALID;}
        unsigned index=0;
        for(unsigned k=ids->first;k;k=authority->doc.nodes[k].next,index++) {
            if(!eq(uj_string(&authority->doc.nodes[k]),id) || seen[index])continue;
            const uj_node *key=get(authority,keys,id),*value=get(authority,key,"keyval");
            const char *pem=str(authority,value,"public");uint8_t signature_bytes[80];size_t n=strlen(sig);
            const char *const key_fields[]={"keytype","scheme","keyval"},*const value_fields[]={"public"};
            if(!uj_fields(&authority->doc,key,key_fields,3)||!uj_fields(&authority->doc,value,value_fields,1)||
               !eq(str(authority,key,"keytype"),"ecdsa")||!eq(str(authority,key,"scheme"),"ecdsa-sha2-nistp256")||
               !pem || !n || n%2 || n>sizeof signature_bytes*2 || !nvf_ota_unhex(sig,n,signature_bytes))continue;
            if(io->verify(pem,signature_bytes,n/2,canonical,length)) {
                seen[index]=true;valid++;
                if(first_key && valid==1 && !sha_text(id,first_key)){free(canonical);return UP_META_INVALID;}
            }
        }
    }
    free(canonical);return valid>=threshold?UP_OK:UP_META_SIGNATURE;
}
static int root_signature(const metadata *m,const metadata *root,unsigned role,const nvf_tuf_io_t *io) {
    const uj_node *roles=get(root,root->body,"roles");
    return signatures(m,root,get(root,root->body,"keys"),get(root,roles,role_names[role]),io,NULL);
}
static bool key_set(const metadata *m,const uj_node *keys,const nvf_tuf_io_t *io) {
    if(!keys || keys->kind!=UJ_OBJECT || entries(m,keys)>16 || !io->public_key)return false;
    for(unsigned i=keys->first;i;i=m->doc.nodes[m->doc.nodes[i].next].next) {
        const uj_node *key=&m->doc.nodes[m->doc.nodes[i].next],*value=get(m,key,"keyval");
        const char *const fields[]={"keytype","scheme","keyval"},*const values[]={"public"};
        const char *pem=str(m,value,"public");uint8_t expected[32],actual[32];
        if(!sha_text(m->doc.nodes[i].text,expected)||!uj_fields(&m->doc,key,fields,3)||
           !uj_fields(&m->doc,value,values,1)||!eq(str(m,key,"keytype"),"ecdsa")||
           !eq(str(m,key,"scheme"),"ecdsa-sha2-nistp256")||!pem||!io->public_key(pem))return false;
        size_t length;char *canonical=uj_canonical(&m->doc,key,&length);
        bool valid=canonical && io->sha256(canonical,length,actual) && !memcmp(expected,actual,32);
        free(canonical);if(!valid)return false;
    }
    return true;
}
static bool distinct_ids(const metadata *m,const uj_node *keys,const uj_node *ids,const char **used,unsigned *count) {
    if(!ids || ids->kind!=UJ_ARRAY)return false;
    for(unsigned i=ids->first;i;i=m->doc.nodes[i].next) {
        const char *id=uj_string(&m->doc.nodes[i]);
        if(!id || !get(m,keys,id) || *count>=16)return false;
        for(unsigned j=0;j<*count;j++)if(eq(id,used[j]))return false;
        used[(*count)++]=id;
    }
    return true;
}
static bool root_profile(const metadata *root,unsigned profile,const nvf_tuf_io_t *io) {
    bool consistent=false;if(!profile_marker(root,"x_navlisten_profile",profile)||!boolean(root,root->body,"consistent_snapshot",&consistent)||!consistent)return false;
    const uj_node *roles=get(root,root->body,"roles"),*keys=get(root,root->body,"keys");
    const char *const role_fields[]={"keyids","threshold"};
    const char *used[16];unsigned count=0;
    if(entries(root,roles)!=4 || entries(root,keys)!=8 || !key_set(root,keys,io))return false;
    for(unsigned i=0;i<4;i++) {
        const uj_node *r=get(root,roles,role_names[i]);uint64_t threshold;
        unsigned want=i==UP_ROOT||i==UP_TARGETS?2:1;
        if(!uj_fields(&root->doc,r,role_fields,2)||!number(root,r,"threshold",&threshold)||threshold!=want||
           entries(root,get(root,r,"keyids"))!=(want==2?3:1))return false;
        const uj_node *ids=get(root,r,"keyids");
        if(!distinct_ids(root,keys,ids,used,&count))return false;
        for(unsigned k=ids->first;k;k=root->doc.nodes[k].next) {
            const char *id=uj_string(&root->doc.nodes[k]);uint8_t digest[32];
            if(!sha_text(id,digest)||!get(root,keys,id))return false;
            for(unsigned prior=ids->first;prior!=k;prior=root->doc.nodes[prior].next)
                if(eq(id,uj_string(&root->doc.nodes[prior])))return false;
        }
    }
    return true;
}
static bool same_authority(const metadata *a,const metadata *b,unsigned role) {
    const uj_node *ar=get(a,get(a,a->body,"roles"),role_names[role]);
    const uj_node *br=get(b,get(b,b->body,"roles"),role_names[role]);
    size_t an=0,bn=0;char *ac=uj_canonical(&a->doc,ar,&an),*bc=uj_canonical(&b->doc,br,&bn);
    bool same=ac && bc && an==bn && !memcmp(ac,bc,an);free(ac);free(bc);return same;
}
static int remember(nvf_tuf_trust_t *trust,const metadata *m,unsigned role,uint64_t version,const nvf_tuf_io_t *io) {
    size_t length;char *canonical=uj_canonical(&m->doc,m->body,&length);uint8_t hash[32];
    if(!canonical)return UP_STORAGE;
    bool ok=io->sha256(canonical,length,hash);free(canonical);if(!ok)return UP_STORAGE;
    if(version<trust->versions[role] || (version==trust->versions[role] && memcmp(hash,trust->digests[role],32)))return UP_META_ROLLBACK;
    trust->versions[role]=version;memcpy(trust->digests[role],hash,32);return UP_OK;
}
// Accept each verified role durably before requesting its descendants. A later
// missing file, bad signature, or reset must not let a mirror replay an older
// timestamp/snapshot on the next refresh. Unchanged checkpoints avoid NVS writes.
static int checkpoint(nvf_tuf_trust_t *trust,nvf_tuf_trust_t *candidate,uint64_t now,const nvf_tuf_io_t *io) {
    candidate->trusted_time=now;
    if(!memcmp(trust,candidate,sizeof *trust))return UP_OK;
    if(!io->save(io->context,candidate))return UP_STORAGE;
    *trust=*candidate;return UP_OK;
}
int nvf_tuf_initialize(nvf_tuf_trust_t *trust,const char *bytes,size_t length,unsigned profile,const nvf_tuf_io_t *io) {
    if(!bytes || !length || length>NVF_TUF_ROOT_CAP)return UP_TRUST_UNCONFIGURED;
    metadata root={0};char *copy=nvf_update_alloc(length+1);if(!copy)return UP_STORAGE;memcpy(copy,bytes,length);copy[length]=0;
    int err=parse(&root,copy,length);uint64_t version=0;
    if(!err)err=header(&root,UP_ROOT,0,false,&version);
    if(!err && !root_profile(&root,profile,io))err=UP_META_INVALID;
    if(!err)err=root_signature(&root,&root,UP_ROOT,io);
    if(!err){memset(trust,0,sizeof *trust);trust->root_length=length;memcpy(trust->root,bytes,length);err=remember(trust,&root,UP_ROOT,version,io);}
    close_meta(&root);return err;
}
bool nvf_update_target_path(const char *path) {
    if(!path || !*path || strlen(path)>=NVF_UPDATE_PATH_CAP || path[0]=='/' || strstr(path,".."))return false;
    for(const char *p=path;*p;p++)if(!strchr("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._/",*p))return false;
    return !strstr(path,"//") && path[strlen(path)-1]!='/';
}
static bool target_info(const metadata *m,const uj_node *target,uint64_t *length,uint8_t hash[32]) {
    const uj_node *hashes=get(m,target,"hashes");
    const char *const fields[]={"length","hashes"};
    return uj_fields(&m->doc,target,fields,2)&&number(m,target,"length",length)&&*length&&
        entries(m,hashes)==1&&sha_text(str(m,hashes,"sha256"),hash);
}
static int fetch_hash(const char *path,uint64_t length,const uint8_t hash[32],size_t limit,char **out,size_t *size,const nvf_tuf_io_t *io) {
    if(length>limit)return UP_META_INVALID;
    int err=io->fetch(io->context,path,limit,out,size);if(err)return err;
    uint8_t actual[32];
    if(*size!=length || !io->sha256(*out,*size,actual)||memcmp(hash,actual,32)){free(*out);*out=NULL;return UP_ARTIFACT;}
    return UP_OK;
}
static int fetch_role(metadata *m,const metadata *parent,const char *name,size_t limit,uint64_t *version,const nvf_tuf_io_t *io) {
    char key[64],path[96];uint64_t length;uint8_t hash[32];
    snprintf(key,sizeof key,"%s.json",name);
    const uj_node *entry=get(parent,get(parent,parent->body,"meta"),key);
    const char *const fields[]={"version","length","hashes"};
    if(!uj_fields(&parent->doc,entry,fields,3)||!number(parent,entry,"version",version)||!*version||
       !number(parent,entry,"length",&length)||!length||entries(parent,get(parent,entry,"hashes"))!=1||
       !sha_text(str(parent,get(parent,entry,"hashes"),"sha256"),hash))return UP_META_INVALID;
    snprintf(path,sizeof path,"metadata/%llu.%s.json",(unsigned long long)*version,name);
    char *bytes=NULL;size_t size=0;int err=fetch_hash(path,length,hash,limit,&bytes,&size,io);
    if(err)return err;
    return parse(m,bytes,size);
}
static bool delegation_profile(const metadata *targets,const metadata *root,const nvf_tuf_io_t *io) {
    const uj_node *delegations=get(targets,targets->body,"delegations"),*roles=get(targets,delegations,"roles");
    const uj_node *keys=get(targets,delegations,"keys"),*root_keys=get(root,root->body,"keys");
    const char *const delegated_fields[]={"keys","roles"},*const fields[]={"name","keyids","threshold","paths","terminating"};
    if(!uj_fields(&targets->doc,delegations,delegated_fields,2)||!roles||roles->kind!=UJ_ARRAY||entries(targets,roles)!=4)return false;
    if(entries(targets,keys)!=6 || !key_set(targets,keys,io))return false;
    for(unsigned i=keys->first;i;i=targets->doc.nodes[targets->doc.nodes[i].next].next)
        if(get(root,root_keys,targets->doc.nodes[i].text))return false;
    bool seen[4]={false};const char *used[16];unsigned count=0;
    for(unsigned i=roles->first;i;i=targets->doc.nodes[i].next) {
        const uj_node *role=&targets->doc.nodes[i];const char *name=str(targets,role,"name");unsigned which;
        for(which=0;which<4;which++)if(eq(name,role_names[UP_RELEASES+which]))break;
        bool terminating=false;uint64_t threshold;const uj_node *paths=get(targets,role,"paths");
        if(which==4||seen[which]||!uj_fields(&targets->doc,role,fields,5)||!boolean(targets,role,"terminating",&terminating)||!terminating||
           !number(targets,role,"threshold",&threshold)||threshold!=(which?1:2)||
           entries(targets,get(targets,role,"keyids"))!=(which?1:3)||!paths||paths->kind!=UJ_ARRAY||entries(targets,paths)!=(which?1:3))return false;
        seen[which]=true;
        if(!distinct_ids(targets,keys,get(targets,role,"keyids"),used,&count))return false;
        const char *expected[]={"releases/*","artifacts/*","notes/*"};unsigned k=0;
        for(unsigned p=paths->first;p;p=targets->doc.nodes[p].next,k++) {
            char channel[48];snprintf(channel,sizeof channel,"channels/%s.json",name);
            if(!eq(uj_string(&targets->doc.nodes[p]),which?channel:expected[k]))return false;
        }
    }return true;
}
static int delegated_signature(const metadata *m,const metadata *targets,unsigned role,const nvf_tuf_io_t *io,uint8_t key[32]) {
    const uj_node *d=get(targets,targets->body,"delegations"),*roles=get(targets,d,"roles");
    for(unsigned i=roles->first;i;i=targets->doc.nodes[i].next) {
        const uj_node *r=&targets->doc.nodes[i];if(eq(str(targets,r,"name"),role_names[role]))
            return signatures(m,targets,get(targets,d,"keys"),r,io,key);
    }return UP_META_INVALID;
}
static bool target_role_profile(const metadata *m,unsigned role,const metadata *root,const nvf_tuf_io_t *io) {
    const uj_node *targets=get(m,m->body,"targets");if(entries(m,targets)>32)return false;
    if(role!=UP_TARGETS && get(m,m->body,"delegations"))return false;
    if(role==UP_TARGETS)return entries(m,targets)==0 && delegation_profile(m,root,io);
    for(unsigned i=targets->first;i;i=m->doc.nodes[m->doc.nodes[i].next].next) {
        const char *path=m->doc.nodes[i].text;uint64_t length;uint8_t hash[32];char expected[48];
        snprintf(expected,sizeof expected,"channels/%s.json",role_names[role]);
        if(!nvf_update_target_path(path)||!target_info(m,&m->doc.nodes[m->doc.nodes[i].next],&length,hash))return false;
        if(role==UP_RELEASES) {if(strncmp(path,"releases/",9)&&strncmp(path,"artifacts/",10)&&strncmp(path,"notes/",6))return false;}
        else if(strcmp(path,expected))return false;
    }return true;
}
static int target(metadata *m,const metadata *role,const char *path,size_t limit,const nvf_tuf_io_t *io) {
    if(!nvf_update_target_path(path))return UP_META_INVALID;
    uint64_t length;uint8_t hash[32];
    if(!target_info(role,get(role,get(role,role->body,"targets"),path),&length,hash))return UP_META_INVALID;
    const char *name=strrchr(path,'/');char url[280],hex[65];nvf_ota_hex(hash,32,hex);
    if(name)snprintf(url,sizeof url,"targets/%.*s/%s.%s",(int)(name-path),path,hex,name+1);
    else snprintf(url,sizeof url,"targets/%s.%s",hex,path);
    char *bytes=NULL;size_t size=0;int err=fetch_hash(url,length,hash,limit,&bytes,&size,io);if(err)return err;
    m->bytes=bytes;m->length=size;
    if(!uj_parse(&m->doc,bytes,size))return UP_META_INVALID;
    m->body=uj_root(&m->doc);return m->body && m->body->kind==UJ_OBJECT?UP_OK:UP_META_INVALID;
}
static bool copy_string(char *out,size_t cap,const char *s){if(!s||strlen(s)>=cap)return false;strcpy(out,s);return true;}
static bool related_target(const metadata *releases,const metadata *manifest,const char *field,const char *digest) {
    const char *path=str(manifest,manifest->body,field);uint64_t length;uint8_t hash[32],expected[32];
    return nvf_update_target_path(path)&&sha_text(str(manifest,manifest->body,digest),expected)&&
        target_info(releases,get(releases,get(releases,releases->body,"targets"),path),&length,hash)&&!memcmp(hash,expected,32);
}
static int release_info(const metadata *channel,const metadata *manifest,const metadata *releases,
                        const nvf_update_device_t *device,nvf_update_release_t *out) {
    const char *const fields[]={"schema","release_sequence","version","build_number","source_revision","published","chip","board_family",
        "hardware_revision_min","hardware_revision_max","partition_layout_id","minimum_updater_version","artifact","length","sha256",
        "secure_boot_key_id","security_version","provenance","provenance_sha256","licenses","licenses_sha256","notes","notes_sha256","collector_capability","profile"};
    uint64_t schema,sequence,build,lo,hi,layout,updater,length,security;
    const char *revision=str(manifest,manifest->body,"source_revision");
    if(!uj_fields(&manifest->doc,manifest->body,fields,sizeof fields/sizeof fields[0])||
       !number(manifest,manifest->body,"schema",&schema)||schema!=1||
       !number(manifest,manifest->body,"release_sequence",&sequence)||!sequence||
       !number(manifest,manifest->body,"build_number",&build)||build!=sequence||
       !revision || strlen(revision)!=40 || strspn(revision,"0123456789abcdef")!=40 ||
       !number(manifest,manifest->body,"hardware_revision_min",&lo)||
       !number(manifest,manifest->body,"hardware_revision_max",&hi)||lo>hi||hi>UINT16_MAX||
       !number(manifest,manifest->body,"partition_layout_id",&layout)||layout>UINT16_MAX||
       !number(manifest,manifest->body,"minimum_updater_version",&updater)||!updater||
       !number(manifest,manifest->body,"security_version",&security)||security!=0||
       !profile_marker(manifest,"profile",device->profile)||
       !expiration(str(manifest,manifest->body,"published"),&out->published)||out->published>device->now||
       !copy_string(out->version,sizeof out->version,str(manifest,manifest->body,"version"))||
       !sha_text(str(manifest,manifest->body,"sha256"),out->hash)||
       !sha_text(str(manifest,manifest->body,"secure_boot_key_id"),out->boot_key)||
       !copy_string(out->artifact,sizeof out->artifact,str(manifest,manifest->body,"artifact"))||
       !copy_string(out->notes,sizeof out->notes,str(manifest,manifest->body,"notes"))||
       !nvf_update_target_path(out->artifact)||strncmp(out->artifact,"artifacts/",10)||
       !number(manifest,manifest->body,"length",&out->length)||!out->length||out->length>0x400000||
       !related_target(releases,manifest,"provenance","provenance_sha256")||
       !related_target(releases,manifest,"licenses","licenses_sha256")||
       !related_target(releases,manifest,"notes","notes_sha256"))return UP_META_INVALID;
    uint8_t hash[32];
    if(!target_info(releases,get(releases,get(releases,releases->body,"targets"),out->artifact),&length,hash)||
       length!=out->length||memcmp(hash,out->hash,32)||sequence!=out->sequence)return UP_META_INVALID;
    out->layout=layout;out->profile=device->profile;
    if(!device->hardware_known)return UP_HARDWARE;
    const char *capability=str(manifest,manifest->body,"collector_capability");
    if(!eq(str(manifest,manifest->body,"chip"),"esp32s3")||!eq(str(manifest,manifest->body,"board_family"),"gnss-color-neo")||
       layout!=device->layout||lo>device->hardware_revision||hi<device->hardware_revision||updater>NVF_UPDATE_VERSION||
       (capability && *capability && !eq(capability,"durable_ack")))return UP_INELIGIBLE;
    (void)channel;return UP_OK;
}
int nvf_tuf_refresh(nvf_tuf_trust_t *trust,unsigned channel_index,const nvf_update_device_t *device,nvf_update_release_t *out,const nvf_tuf_io_t *io) {
    if(channel_index>=3 || !trust->root_length || trust->root_length>NVF_TUF_ROOT_CAP)return UP_TRUST_UNCONFIGURED;
    if(device->now<1704067200)return UP_TIME;
    uint64_t now=device->now>trust->trusted_time?device->now:trust->trusted_time;
    metadata root={0},timestamp={0},snapshot={0},targets={0},releases={0},channel={0},choice={0},manifest={0};
    nvf_tuf_trust_t *candidate=nvf_update_alloc(sizeof *candidate);if(!candidate)return UP_STORAGE;*candidate=*trust;
    memset(out,0,sizeof *out);
    char *bytes=nvf_update_alloc(trust->root_length+1);if(!bytes){free(candidate);return UP_STORAGE;}
    memcpy(bytes,trust->root,trust->root_length+1);int err=parse(&root,bytes,trust->root_length);uint64_t version=0;
    if(err)goto done;
    if(!root_profile(&root,device->profile,io)){err=UP_META_INVALID;goto done;}
    // Sequential roots are durable independently, so interruption can continue.
    for(unsigned rotation=0;rotation<32;rotation++) {
        char path[80];uint64_t next=candidate->versions[UP_ROOT]+1;
        if(!next){err=UP_META_ROLLBACK;goto done;}
        snprintf(path,sizeof path,"metadata/%llu.root.json",(unsigned long long)next);
        bytes=NULL;size_t size=0;err=io->fetch(io->context,path,NVF_TUF_ROOT_CAP,&bytes,&size);
        if(err==UP_NOT_FOUND){err=UP_OK;break;}
        if(err)goto done;
        metadata newer={0};err=parse(&newer,bytes,size);
        if(size>NVF_TUF_ROOT_CAP)err=UP_META_INVALID;
        if(!err)err=header(&newer,UP_ROOT,now,false,&version);
        if(!err && (version!=next || !root_profile(&newer,device->profile,io)))err=UP_META_INVALID;
        if(!err)err=root_signature(&newer,&root,UP_ROOT,io);
        if(!err)err=root_signature(&newer,&newer,UP_ROOT,io);
        if(!err)err=remember(candidate,&newer,UP_ROOT,version,io);
        if(err){close_meta(&newer);goto done;}
        // TUF resets these rollback floors only when that role's authority
        // changes. An expiry renewal with the same keys cannot permit replay.
        for(unsigned role=UP_TIMESTAMP;role<=UP_SNAPSHOT;role++) {
            if(same_authority(&root,&newer,role))continue;
            candidate->versions[role]=0;memset(candidate->digests[role],0,32);
            if(role==UP_SNAPSHOT)memset(candidate->snapshot_versions,0,sizeof candidate->snapshot_versions);
        }
        candidate->root_length=size;memcpy(candidate->root,bytes,size);candidate->root[size]=0;
        if(!io->save(io->context,candidate)){close_meta(&newer);err=UP_STORAGE;goto done;}
        *trust=*candidate;close_meta(&root);root=newer;
        if(rotation==31){err=UP_ROOT_LIMIT;goto done;}
    }
    err=header(&root,UP_ROOT,now,true,&version);if(err)goto done;
    bytes=NULL;size_t size=0;
    err=io->fetch(io->context,"metadata/timestamp.json",4096,&bytes,&size);if(err)goto done;
    err=parse(&timestamp,bytes,size);if(err)goto done;
    if(size>4096){err=UP_META_INVALID;goto done;}
    err=header(&timestamp,UP_TIMESTAMP,now,true,&version);if(err)goto done;
    err=root_signature(&timestamp,&root,UP_TIMESTAMP,io);if(err)goto done;
    err=remember(candidate,&timestamp,UP_TIMESTAMP,version,io);if(err)goto done;
    if(entries(&timestamp,get(&timestamp,timestamp.body,"meta"))!=1){err=UP_META_INVALID;goto done;}
    err=checkpoint(trust,candidate,now,io);if(err)goto done;
    uint64_t expected;
    err=fetch_role(&snapshot,&timestamp,"snapshot",8192,&expected,io);if(err)goto done;
    err=header(&snapshot,UP_SNAPSHOT,now,true,&version);if(err)goto done;
    if(version!=expected){err=UP_META_INVALID;goto done;}
    err=root_signature(&snapshot,&root,UP_SNAPSHOT,io);if(err)goto done;
    err=remember(candidate,&snapshot,UP_SNAPSHOT,version,io);if(err)goto done;
    if(entries(&snapshot,get(&snapshot,snapshot.body,"meta"))!=5){err=UP_META_INVALID;goto done;}
    for(unsigned i=0;i<5;i++) {
        char name[32];snprintf(name,sizeof name,"%s.json",role_names[UP_TARGETS+i]);
        const uj_node *entry=get(&snapshot,get(&snapshot,snapshot.body,"meta"),name);
        const char *const fields[]={"version","length","hashes"};uint64_t ref,size;uint8_t hash[32];
        if(!uj_fields(&snapshot.doc,entry,fields,3)||!number(&snapshot,entry,"version",&ref)||!ref||
           !number(&snapshot,entry,"length",&size)||!size||size>12288||
           entries(&snapshot,get(&snapshot,entry,"hashes"))!=1||
           !sha_text(str(&snapshot,get(&snapshot,entry,"hashes"),"sha256"),hash)){err=UP_META_INVALID;goto done;}
        if(ref<candidate->snapshot_versions[i] || ref<candidate->versions[UP_TARGETS+i]){err=UP_META_ROLLBACK;goto done;}
        candidate->snapshot_versions[i]=ref;
    }
    err=checkpoint(trust,candidate,now,io);if(err)goto done;
    metadata *roles[]={&targets,&releases,&channel};unsigned indices[]={UP_TARGETS,UP_RELEASES,UP_STABLE+channel_index};
    for(unsigned i=0;i<3;i++) {
        unsigned role=indices[i];metadata *m=roles[i];
        err=fetch_role(m,&snapshot,role_names[role],12288,&expected,io);if(err)goto done;
        err=header(m,role,now,true,&version);if(err)goto done;
        if(version!=expected || !target_role_profile(m,role,&root,io)){err=UP_META_INVALID;goto done;}
        err=i?delegated_signature(m,&targets,role,io,role==UP_RELEASES?out->release_key:NULL):root_signature(m,&root,role,io);
        if(err)goto done;
        err=remember(candidate,m,role,version,io);if(err)goto done;
        err=checkpoint(trust,candidate,now,io);if(err)goto done;
    }
    char channel_path[48];snprintf(channel_path,sizeof channel_path,"channels/%s.json",role_names[UP_STABLE+channel_index]);
    err=target(&choice,&channel,channel_path,4096,io);if(err)goto done;
    const char *const choice_fields[]={"schema","generation","release_sequence","release_manifest","priority","rollout_salt","percentage","withdrawn","advisory"};
    uint64_t schema,percent;uint8_t salt[32];const uj_node *withdrawn=get(&choice,choice.body,"withdrawn");
    const uj_node *advisory=get(&choice,choice.body,"advisory");const char *const advisory_fields[]={"classification","summary"};
    if(!uj_fields(&choice.doc,choice.body,choice_fields,9)||!number(&choice,choice.body,"schema",&schema)||schema!=1||
       !number(&choice,choice.body,"generation",&out->generation)||!out->generation||
       !number(&choice,choice.body,"release_sequence",&out->sequence)||
       !number(&choice,choice.body,"percentage",&percent)||percent>100||
       !sha_text(str(&choice,choice.body,"rollout_salt"),salt)||
       !withdrawn||withdrawn->kind!=UJ_ARRAY||entries(&choice,withdrawn)>32||
       !uj_fields(&choice.doc,advisory,advisory_fields,2)||
       !copy_string(out->advisory,sizeof out->advisory,str(&choice,advisory,"summary"))){err=UP_META_INVALID;goto done;}
    const char *priority=str(&choice,choice.body,"priority"),*classification=str(&choice,advisory,"classification");
    if((!eq(priority,"normal")&&!eq(priority,"urgent"))||
       (!eq(classification,"info")&&!eq(classification,"warning")&&!eq(classification,"security"))){err=UP_META_INVALID;goto done;}
    uint8_t choice_hash[32];if(!io->sha256(choice.bytes,choice.length,choice_hash)){err=UP_STORAGE;goto done;}
    if(out->generation<candidate->generations[channel_index] ||
       (out->generation==candidate->generations[channel_index] && memcmp(choice_hash,candidate->generation_digests[channel_index],32))){err=UP_META_ROLLBACK;goto done;}
    memcpy(candidate->generation_digests[channel_index],choice_hash,32);
    candidate->generations[channel_index]=out->generation;candidate->trusted_time=now;
    for(unsigned i=withdrawn->first;i;i=choice.doc.nodes[i].next) {
        uint64_t seq;if(!uj_uint(&choice.doc.nodes[i],&seq)||!seq){err=UP_META_INVALID;goto done;}
        if(seq==out->sequence)out->withdrawn=true;
    }
    err=checkpoint(trust,candidate,now,io);if(err)goto done;
    // Withdrawal is authoritative even if the release manifest is unavailable.
    // Report eligibility, so an attended offline install cannot treat that
    // missing manifest as permission to boot an already withdrawn staged image.
    if(out->withdrawn){err=UP_INELIGIBLE;goto done;}
    if(out->sequence) {
        if(!copy_string(out->manifest,sizeof out->manifest,str(&choice,choice.body,"release_manifest"))||strncmp(out->manifest,"releases/",9)){err=UP_META_INVALID;goto done;}
        err=target(&manifest,&releases,out->manifest,8192,io);if(err)goto done;
        err=release_info(&choice,&manifest,&releases,device,out);
        if(err && err!=UP_INELIGIBLE && err!=UP_HARDWARE)goto done;
        if(!err) {
            uint8_t cohort[32+NVF_BOARD_UID_SIZE+8],digest[32];memcpy(cohort,salt,32);memcpy(cohort+32,device->board_uid,NVF_BOARD_UID_SIZE);
            for(unsigned i=0;i<8;i++)cohort[32+NVF_BOARD_UID_SIZE+i]=(uint8_t)(out->sequence>>(56-8*i));
            if(!io->sha256(cohort,sizeof cohort,digest)){err=UP_STORAGE;goto done;}
            unsigned bucket=((unsigned)digest[0]<<8|digest[1])%100;
            if(bucket>=percent)err=UP_ROLLOUT;
            if(out->withdrawn)err=UP_INELIGIBLE;
        }
    }
done:
    close_meta(&root);close_meta(&timestamp);close_meta(&snapshot);close_meta(&targets);close_meta(&releases);close_meta(&channel);close_meta(&choice);close_meta(&manifest);free(candidate);return err;
}
