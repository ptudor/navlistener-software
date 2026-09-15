// The real send loop and RAM spool, with only ESP-IDF transport/time/task shims.
#define _POSIX_C_SOURCE 200809L
#include <assert.h>
#include <stdio.h>
#include "idf_stubs.h"
#define select test_select
#include "../src/pusher.c"
#undef select

static int64_t clock_us, start_us, restore_us, next_slow_us;
static uint64_t produced, durable, lost, slow_limit;
static bool persisted[10000], finish_allowed, finished;
static int phase, connections, hellos, pings;
static enum { STALL, IDLE, SLOW, FLOOD } mode;
static uint8_t inbound[65536];
static size_t head, tail;
static esp_tls_t transport;

static void queue(uint8_t type, const uint8_t *body, size_t len) {
    assert(tail+GNF1_FRAME_HDR+len <= sizeof inbound);
    gnf1_frame_header(inbound+tail,type,(uint32_t)len);tail+=GNF1_FRAME_HDR;
    if(len)memcpy(inbound+tail,body,len);tail+=len;
}
static void ack(void){uint8_t b[8];gnf1_be64(b,durable);queue(GNF1_F_ACK,b,8);}
static void produce(void){uint8_t data[GNF1_RECORD_HDR]={0};produced=spool_append(data,sizeof data);assert(produced<10000);}
int64_t esp_timer_get_time(void){return clock_us;}
ssize_t esp_tls_conn_write(esp_tls_t *tls,const void *buf,size_t n){
    (void)tls;const uint8_t *b=buf;
    if(phase==0){assert(n==4&&!memcmp(b,"GNF1",4));phase++;return (ssize_t)n;}
    if(phase==1){assert(n==5&&b[0]==GNF1_F_HELLO);phase++;return (ssize_t)n;}
    if(phase==2){assert(strstr((const char *)b,"\"session\":\"same-boot\""));hellos++;phase++;
        const uint8_t welcome[]="{\"ok\":true}";queue(GNF1_F_WELCOME,welcome,sizeof welcome-1);return (ssize_t)n;}
    if(b[0]==GNF1_F_DATA){
        uint64_t seq=gnf1_rd_be64(b+5);assert(seq<=produced);
        if(mode==STALL && (seq!=lost||clock_us>=restore_us))persisted[seq]=true;
        if(mode==STALL){while(persisted[durable+1])durable++;ack();queue(GNF1_F_PONG,NULL,0);}
    }else {assert(b[0]==GNF1_F_PING);pings++;queue(GNF1_F_PONG,NULL,0);}
    return (ssize_t)n; // the connection and every write remain healthy
}
ssize_t esp_tls_conn_read(esp_tls_t *tls,void *buf,size_t n){
    (void)tls;
    // FLOOD: a saturating peer always has another unchanged ACK and PONG queued,
    // and each refill costs 1 ms of wall time. Only a bounded drain lets the
    // durability watchdog in the outer loop run at all.
    if(head==tail&&mode==FLOOD&&phase>=3){ack();queue(GNF1_F_PONG,NULL,0);clock_us+=1000;
        assert(clock_us-start_us<200000000);} // an unbounded drain never reaches select(): fail here, not hang
    if(head==tail)return -1;
    if(n>tail-head)n=tail-head;memcpy(buf,inbound+head,n);head+=n;
    if(head==tail)head=tail=0;return (ssize_t)n;
}
int esp_tls_get_bytes_avail(esp_tls_t *tls){
    (void)tls;
    if(mode==FLOOD&&phase>=3)return 1;
    if(finish_allowed&&mode==STALL&&durable==produced&&spool_acked()==produced)finished=true;
    return (int)(tail-head)+(finished?1:0);
}
int test_select(int n,fd_set *r,fd_set *w,fd_set *e,struct timeval *timeout){
    (void)n;(void)r;(void)w;(void)e;
    int64_t step=timeout->tv_sec*1000000LL+timeout->tv_usec;
    clock_us+=step;
    if(step&&mode==STALL&&!finish_allowed)produce(); // 10 frames/sec; default 1024-frame ring
    if(mode==IDLE&&clock_us-start_us>=120000000)finished=true;
    if(mode==SLOW&&clock_us>=next_slow_us){durable++;persisted[durable]=true;ack();next_slow_us+=5000000;
        if(durable==slow_limit)finished=true;}
    assert(clock_us-start_us<200000000); // missing watchdog must fail, not hang
    if(mode==FLOOD&&phase>=3)return 1;
    return tail>head||finished;
}
esp_tls_t *esp_tls_init(void){phase=0;head=tail=0;finished=false;return &transport;}
int esp_tls_conn_new_sync(const char *h,int l,int p,const esp_tls_cfg_t *c,esp_tls_t *t){(void)h;(void)l;(void)p;(void)c;(void)t;connections++;return 1;}
void esp_tls_conn_destroy(esp_tls_t *t){(void)t;}
esp_err_t esp_tls_get_conn_sockfd(esp_tls_t *t,int *fd){(void)t;*fd=1;return ESP_OK;}
int esp_crt_bundle_attach(void *p){(void)p;return 0;}
void vTaskDelay(int ticks){clock_us+=(int64_t)ticks*1000;}
uint32_t esp_random(void){return 0;}
int xTaskCreate(void (*f)(void*),const char *n,int sz,void *a,int pri,void *h){(void)f;(void)n;(void)sz;(void)a;(void)pri;(void)h;return pdPASS;}

int main(void){
    assert(spool_init(1024));
    s_cfg=(pusher_cfg_t){.host="collector",.port=443,.token="token",.station="station",.feed="ubx",.session="same-boot"};
    for(int outage=0;outage<3;outage++){
        mode=STALL;finish_allowed=false;start_us=clock_us;restore_us=clock_us+15000000;lost=produced+1;produce();
        int before=connections;assert(serve(s_cfg.host, NULL, false)==0);
        assert(connections==before+1&&!finished&&durable==lost-1);
        assert(clock_us-start_us>=ACK_STALL_US&&clock_us-start_us<ACK_STALL_US+1000000);
        uint64_t dropped;size_t count;spool_stats(NULL,&dropped,&count);assert(!dropped&&count<1024);
        finish_allowed=true;assert(serve(s_cfg.host, NULL, false)<=0); // same boot, replay from the durable watermark
        assert(spool_acked()==produced&&durable==produced&&persisted[lost]);
        spool_stats(NULL,&dropped,&count);assert(!dropped&&!count);
    }
    mode=IDLE;start_us=clock_us;int before=connections;assert(serve(s_cfg.host, NULL, false)==0);
    assert(connections==before+1&&clock_us-start_us>=120000000&&pings>0);
    mode=SLOW;start_us=clock_us;next_slow_us=clock_us+5000000;
    for(int i=0;i<20;i++)produce();slow_limit=produced;
    before=connections;assert(serve(s_cfg.host, NULL, false)==0);
    assert(connections==before+1&&durable==produced&&clock_us-start_us>=100000000);
    // A saturating peer (endless unchanged ACKs/PONGs, one record outstanding
    // and never persisted) cannot starve the watchdog: the bounded drain yields
    // to it every DRAIN_BATCH frames and the stall still fires on time.
    mode=FLOOD;start_us=clock_us;produce();before=connections;
    assert(serve(s_cfg.host, NULL, false)==0);
    assert(connections==before+1&&clock_us-start_us>=ACK_STALL_US&&clock_us-start_us<ACK_STALL_US+2000000);
    assert(hellos==connections);
    puts("ACK-stall replay, healthy heartbeats, repeated outages, idle, slow and flooding ACK progress PASS");
    return 0;
}
