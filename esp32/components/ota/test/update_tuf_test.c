#include "update_tuf.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <openssl/evp.h>
#include <openssl/pem.h>
#include <openssl/core_names.h>

static const char *directory;
static unsigned saves;
static nvf_tuf_trust_t persisted;
static int fetch(void *ctx,const char *path,size_t cap,char **data,size_t *length) {
    (void)ctx;char full[1024];snprintf(full,sizeof full,"%s/%s",directory,path);
    FILE *f=fopen(full,"rb");if(!f)return UP_NOT_FOUND;
    *data=malloc(cap+1);assert(*data);*length=fread(*data,1,cap+1,f);fclose(f);
    if(*length>cap){free(*data);*data=NULL;return UP_META_INVALID;}return UP_OK;
}
static bool sha(const void *data,size_t n,uint8_t hash[32]) {return EVP_Digest(data,n,hash,NULL,EVP_sha256(),NULL)==1;}
static bool public_key(const char *pem) {
    BIO *bio=BIO_new_mem_buf(pem,-1);EVP_PKEY *key=PEM_read_bio_PUBKEY(bio,NULL,NULL,NULL);BIO_free(bio);
    char group[80];size_t length=0;
    bool ok=key && EVP_PKEY_is_a(key,"EC") && EVP_PKEY_get_utf8_string_param(key,OSSL_PKEY_PARAM_GROUP_NAME,group,sizeof group,&length)==1 &&
        (!strcmp(group,"prime256v1") || !strcmp(group,"P-256"));
    EVP_PKEY_free(key);return ok;
}
static bool verify(const char *pem,const uint8_t *sig,size_t size,const void *bytes,size_t length) {
    BIO *bio=BIO_new_mem_buf(pem,-1);EVP_PKEY *key=PEM_read_bio_PUBKEY(bio,NULL,NULL,NULL);BIO_free(bio);
    EVP_MD_CTX *ctx=EVP_MD_CTX_new();bool ok=public_key(pem) && key && ctx && EVP_DigestVerifyInit(ctx,NULL,EVP_sha256(),NULL,key)==1 && EVP_DigestVerify(ctx,sig,size,bytes,length)==1;
    EVP_MD_CTX_free(ctx);EVP_PKEY_free(key);return ok;
}
static bool save(void *ctx,const nvf_tuf_trust_t *trust){(void)ctx;assert(trust->root_length>0 && trust->root_length<=8192);persisted=*trust;saves++;return true;}
int main(int argc,char **argv) {
    if(argc==4 && !strcmp(argv[1],"--json")) {
        uj_doc doc;bool valid=uj_parse(&doc,argv[2],strlen(argv[2]));
        if(valid){size_t length;char *canonical=uj_canonical(&doc,uj_root(&doc),&length);assert(canonical);fwrite(canonical,1,length,stdout);free(canonical);uj_free(&doc);}
        return valid==(atoi(argv[3])!=0)?0:1;
    }
    assert(argc>=3);directory=argv[1];char *bytes=NULL;size_t length=0;
    assert(fetch(NULL,"metadata/1.root.json",8192,&bytes,&length)==UP_OK);
    nvf_tuf_trust_t trust;nvf_tuf_io_t io={.fetch=fetch,.sha256=sha,.public_key=public_key,.verify=verify,.save=save};
    int err=nvf_tuf_initialize(&trust,bytes,length,true,&io);free(bytes);
    if(err){printf("initialize=%d\n",err);return err==atoi(argv[2])?0:1;}
    persisted=trust;
    nvf_update_device_t device={.now=1800000000,.hardware_known=true,.hardware_revision=1,.layout=1,.test_build=true,.eui={1,2,3,4,5,6,7,8}};
    nvf_update_release_t result;
    err=nvf_tuf_refresh(&trust,2,&device,&result,&io);
    printf("refresh=%d sequence=%llu saves=%u\n",err,(unsigned long long)result.sequence,saves);
    if(err!=atoi(argv[2]))return 1;
    if(argc>3){trust=persisted;directory=argv[3];err=nvf_tuf_refresh(&trust,2,&device,&result,&io);printf("second=%d\n",err);return err==atoi(argv[4])?0:1;}
    return 0;
}
