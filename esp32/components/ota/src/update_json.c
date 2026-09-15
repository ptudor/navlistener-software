#include "update_memory.h"
#include "update_json.h"
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

#define UJ_MAX_NODES 2048u
typedef struct { uj_doc *d; const unsigned char *p, *end; unsigned depth; } parser;
static void spaces(parser *p) { while (p->p < p->end && strchr(" \t\r\n", *p->p)) p->p++; }
static int hex(unsigned char c) {
    if (c >= '0' && c <= '9') return c-'0';
    if (c >= 'a' && c <= 'f') return c-'a'+10;
    if (c >= 'A' && c <= 'F') return c-'A'+10;
    return -1;
}
static bool four(parser *p, uint32_t *cp) {
    if (p->end-p->p < 4) return false;
    *cp=0;
    for (int i=0;i<4;i++) { int n=hex(*p->p++); if(n<0)return false; *cp=(*cp<<4)|(unsigned)n; }
    return true;
}
static bool string(parser *p, const char **out) {
    if (p->p == p->end || *p->p++ != '"') return false;
    size_t start=p->d->stored;
    *out=p->d->strings+start;
    while (p->p < p->end) {
        uint32_t cp=*p->p++;
        bool escaped=cp=='\\';
        if (cp == '"') { p->d->strings[p->d->stored++]=0; return true; }
        if (cp == '\\') {
            if(p->p == p->end)return false;
            cp=*p->p++;
            if (cp == 'u') {
                if(!four(p,&cp))return false;
                if(cp>=0xd800 && cp<=0xdbff) {
                    uint32_t low;
                    if(p->end-p->p<6 || *p->p++!='\\' || *p->p++!='u' || !four(p,&low) || low<0xdc00 || low>0xdfff)return false;
                    cp=0x10000+((cp-0xd800)<<10)+(low-0xdc00);
                } else if(cp>=0xdc00 && cp<=0xdfff)return false;
            } else if(cp=='n')cp='\n';
            else if(cp=='r')cp='\r';
            else if(cp=='t')cp='\t';
            else if(cp=='b')cp='\b';
            else if(cp=='f')cp='\f';
            else if(cp!='"' && cp!='\\' && cp!='/')return false;
        } else if(cp>=0x80) {
            unsigned extra; uint32_t minimum;
            if(cp>=0xc2 && cp<=0xdf){extra=1;minimum=0x80;cp&=0x1f;}
            else if(cp>=0xe0 && cp<=0xef){extra=2;minimum=0x800;cp&=0xf;}
            else if(cp>=0xf0 && cp<=0xf4){extra=3;minimum=0x10000;cp&=7;}
            else return false;
            for(unsigned i=0;i<extra;i++) { if(p->p==p->end || (*p->p&0xc0)!=0x80)return false;cp=(cp<<6)|(*p->p++&0x3f); }
            if(cp<minimum || cp>0x10ffff || (cp>=0xd800 && cp<=0xdfff))return false;
        }
        if(cp==0 || (cp<32 && !escaped))return false;
        unsigned char bytes[4]; size_t n;
        if(cp<0x80){n=1;bytes[0]=cp;}
        else if(cp<0x800){n=2;bytes[0]=0xc0|(cp>>6);bytes[1]=0x80|(cp&63);}
        else if(cp<0x10000){n=3;bytes[0]=0xe0|(cp>>12);bytes[1]=0x80|((cp>>6)&63);bytes[2]=0x80|(cp&63);}
        else {n=4;bytes[0]=0xf0|(cp>>18);bytes[1]=0x80|((cp>>12)&63);bytes[2]=0x80|((cp>>6)&63);bytes[3]=0x80|(cp&63);}
        if(p->d->stored-start+n>512)return false;
        memcpy(p->d->strings+p->d->stored,bytes,n);p->d->stored+=n;
    }
    return false;
}
static unsigned value(parser *p) {
    spaces(p);
    if(p->p==p->end || p->d->used==p->d->limit || p->depth>=16)return 0;
    unsigned id=++p->d->used; uj_node *node=&p->d->nodes[id];
    unsigned char c=*p->p;
    if(c=='{' || c=='[') {
        node->kind=c=='{'?UJ_OBJECT:UJ_ARRAY; p->p++;p->depth++;
        unsigned last=0; spaces(p);
        unsigned char close=c=='{'?'}':']';
        if(p->p<p->end && *p->p==close){p->p++;p->depth--;return id;}
        do {
            unsigned child=value(p); if(!child)return 0;
            if(node->kind==UJ_OBJECT) {
                uj_node *key=&p->d->nodes[child];
                if(key->kind!=UJ_STRING)return 0;
                for(unsigned i=node->first;i;i=p->d->nodes[p->d->nodes[i].next].next)
                    if(!strcmp(p->d->nodes[i].text,key->text))return 0;
                spaces(p);if(p->p==p->end || *p->p++!=':')return 0;
                unsigned val=value(p);if(!val)return 0;key->next=val;
            }
            if(last)p->d->nodes[last].next=child; else node->first=child;
            last=node->kind==UJ_OBJECT?p->d->nodes[child].next:child;
            spaces(p);if(p->p==p->end)return 0;
            if(*p->p==close){p->p++;p->depth--;return id;}
            if(*p->p++!=',')return 0;
        } while(true);
    }
    if(c=='"'){node->kind=UJ_STRING;return string(p,&node->text)?id:0;}
    if(c>='0' && c<='9') {
        node->kind=UJ_UINT;node->number=0;
        const unsigned char *start=p->p;
        do { unsigned digit=*p->p++-'0';if(node->number>(UINT64_MAX-digit)/10)return 0;node->number=node->number*10+digit; }
        while(p->p<p->end && *p->p>='0' && *p->p<='9');
        if(p->p-start>1 && *start=='0')return 0;
        return id;
    }
    const char *literal=c=='t'?"true":c=='f'?"false":c=='n'?"null":NULL;
    if(!literal || (size_t)(p->end-p->p)<strlen(literal) || memcmp(p->p,literal,strlen(literal)))return 0;
    p->p+=strlen(literal);node->kind=c=='n'?UJ_NULL:UJ_BOOL;node->number=c=='t';return id;
}
bool uj_parse(uj_doc *d,const char *data,size_t length) {
    memset(d,0,sizeof *d);
    if(!data || !length || length>UJ_MAX_BYTES || memchr(data,0,length))return false;
    d->limit=length/2+1;if(d->limit>UJ_MAX_NODES)d->limit=UJ_MAX_NODES;
    d->nodes=nvf_update_alloc((d->limit+1)*sizeof *d->nodes);
    if(d->nodes)memset(d->nodes,0,(d->limit+1)*sizeof *d->nodes);
    d->strings=nvf_update_alloc(length+1);
    if(!d->nodes || !d->strings){uj_free(d);return false;}
    parser p={.d=d,.p=(const unsigned char*)data,.end=(const unsigned char*)data+length};
    unsigned root=value(&p);spaces(&p);
    if(root!=1 || p.p!=p.end){uj_free(d);return false;}return true;
}
void uj_free(uj_doc *d){free(d->nodes);free(d->strings);memset(d,0,sizeof *d);}
const uj_node *uj_root(const uj_doc *d){return d->used?&d->nodes[1]:NULL;}
const uj_node *uj_get(const uj_doc *d,const uj_node *o,const char *name) {
    if(!o || o->kind!=UJ_OBJECT)return NULL;
    for(unsigned i=o->first;i;i=d->nodes[d->nodes[i].next].next)
        if(!strcmp(d->nodes[i].text,name))return &d->nodes[d->nodes[i].next];
    return NULL;
}
const char *uj_string(const uj_node *n){return n && n->kind==UJ_STRING?n->text:NULL;}
bool uj_uint(const uj_node *n,uint64_t *v){if(!n || n->kind!=UJ_UINT)return false;*v=n->number;return true;}
bool uj_fields(const uj_doc *d,const uj_node *o,const char *const *allowed,size_t count) {
    if(!o || o->kind!=UJ_OBJECT)return false;
    for(unsigned i=o->first;i;i=d->nodes[d->nodes[i].next].next) {
        bool found=false;for(size_t j=0;j<count;j++)if(!strcmp(d->nodes[i].text,allowed[j]))found=true;
        if(!found)return false;
    }return true;
}
typedef struct { char *out;size_t used,cap; } writer;
static bool put(writer *w,const char *s,size_t n){if(n>w->cap-w->used)return false;memcpy(w->out+w->used,s,n);w->used+=n;return true;}
static bool quoted(writer *w,const char *s) {
    if(!put(w,"\"",1))return false;
    for(;*s;s++){if((*s=='"' || *s=='\\')&&!put(w,"\\",1))return false;if(!put(w,s,1))return false;}
    return put(w,"\"",1);
}
static int compare(const void *a,const void *b){return strcmp((*(const uj_node*const*)a)->text,(*(const uj_node*const*)b)->text);}
static bool emit(const uj_doc *d,const uj_node *n,writer *w) {
    if(n->kind==UJ_STRING)return quoted(w,n->text);
    if(n->kind==UJ_UINT){char b[24];int size=snprintf(b,sizeof b,"%llu",(unsigned long long)n->number);return size>0 && put(w,b,size);}
    if(n->kind==UJ_BOOL)return put(w,n->number?"true":"false",n->number?4:5);
    if(n->kind==UJ_NULL)return put(w,"null",4);
    // Each recursive frame owns only its actual key list. A fixed 128-pointer
    // array at every nesting level can exhaust the embedded task's stack.
    const uj_node **keys=NULL;unsigned count=0;
    if(n->kind==UJ_OBJECT) {
        for(unsigned i=n->first;i;i=d->nodes[d->nodes[i].next].next)if(++count>128)return false;
        keys=nvf_update_alloc((count?count:1)*sizeof *keys);if(!keys)return false;
        unsigned j=0;for(unsigned i=n->first;i;i=d->nodes[d->nodes[i].next].next)keys[j++]=&d->nodes[i];
        qsort(keys,count,sizeof keys[0],compare);
    }
    bool ok=put(w,n->kind==UJ_OBJECT?"{":"[",1);
    if(n->kind==UJ_OBJECT) {
        for(unsigned i=0;ok&&i<count;i++) {
            ok=(!i||put(w,",",1))&&quoted(w,keys[i]->text)&&put(w,":",1)&&emit(d,&d->nodes[keys[i]->next],w);
        }
    } else for(unsigned i=n->first;ok&&i;i=d->nodes[i].next) {
        ok=(i==n->first||put(w,",",1))&&emit(d,&d->nodes[i],w);
    }
    free(keys);return ok&&put(w,n->kind==UJ_OBJECT?"}":"]",1);
}
char *uj_canonical(const uj_doc *d,const uj_node *n,size_t *length) {
    writer w={.out=nvf_update_alloc(UJ_MAX_BYTES*2+1),.cap=UJ_MAX_BYTES*2};
    if(!w.out || !n || !emit(d,n,&w)){free(w.out);return NULL;}
    w.out[w.used]=0;*length=w.used;return w.out;
}
