#include <assert.h>
#include <stdio.h>
#include "../../../../common/board_identity.h"

typedef struct { unsigned model[2]; bool fault, bad_id, unstable; unsigned serial_reads; } bus;
static int read_id(void *ctx, uint8_t address, uint16_t reg, uint8_t width, uint8_t *out, size_t n) {
    bus *b=ctx;
    if (b->fault) return NVF_ID_IO_ERROR;
    if (address==0x7c) {
        assert(width==1 && n==3 && (reg==0xa0 || reg==0xa2));
        if (b->model[(reg-0xa0)/2]!=3) return NVF_ID_NACK;
        memcpy(out, "\x00\xd0\xb8", 3); if(b->bad_id) out[2]^=8;
        return NVF_ID_READ_OK;
    }
    if (address==0x58 || address==0x59) {
        assert(width==2 && reg==0x800 && n==16 && b->model[address-0x58]==3);
        for(unsigned i=0;i<16;i++) out[i]=(uint8_t)i;
        if(b->unstable)out[15]+=(uint8_t)b->serial_reads++;
        return NVF_ID_READ_OK;
    }
    assert(address==0x50 || address==0x51);
    unsigned model=b->model[address-0x50];
    if(!model)return NVF_ID_ABSENT;
    assert(model!=3); /* A 16-bit EEPROM must never see an 8-bit pointer. */
    assert(width==1 && n==8 && reg==0xf8);
    if(model!=1)return NVF_ID_NACK;
    memcpy(out,"\x00\x04\xa3\x01\x02\x03\x04\x05",8);
    return NVF_ID_READ_OK;
}
int main(void) {
    nvf_board_identity_t out; uint8_t adopted[NVF_BOARD_UID_SIZE];
    bus b={.model={3,0}};
    assert(!nvf_board_discover(read_id,&b,NULL,0,&out));
    assert(out.board_valid && out.eeprom_cs128 && out.board_address==0x50 && out.board_uid[2]==16);
    memcpy(adopted,out.board_uid,sizeof adopted);
    char id[NVF_BOARD_OBSERVER_SIZE]; assert(nvf_uid_observer(adopted,id));
    assert(!strcmp(id,"board-0003-000102030405060708090a0b0c0d0e0f"));
    b.model[0]=0;b.model[1]=3;
    assert(!nvf_board_discover(read_id,&b,adopted,0,&out) && out.board_address==0x51);
    b.model[0]=1;b.model[1]=0; assert(nvf_board_discover(read_id,&b,adopted,0,&out));
    assert(!nvf_board_discover(read_id,&b,NULL,0,&out) && nvf_uid_kind(out.board_uid)==1);
    memcpy(adopted,out.board_uid,sizeof adopted);
    b.model[0]=0;b.model[1]=1;
    assert(!nvf_board_discover(read_id,&b,adopted,0,&out) && nvf_uid_kind(out.board_uid)==1);
    b.model[1]=0;
    assert(nvf_board_discover(read_id,&b,adopted,0,&out));
    b.model[0]=99;b.model[1]=1;assert(nvf_board_discover(read_id,&b,NULL,0,&out));
    b.model[0]=3;b.model[1]=0;b.bad_id=true;assert(nvf_board_discover(read_id,&b,NULL,0,&out));b.bad_id=false;
    b.unstable=true;assert(nvf_board_discover(read_id,&b,NULL,0,&out));b.unstable=false;
    b.fault=true;assert(nvf_board_discover(read_id,&b,NULL,0,&out));b.fault=false;
    b.model[1]=3;assert(nvf_board_discover(read_id,&b,NULL,0,&out));
    b.model[0]=b.model[1]=1;assert(nvf_board_discover(read_id,&b,NULL,0,&out));
    adopted[1]=2;assert(!nvf_uid_valid(adopted));adopted[1]=1;
    adopted[34]=1;assert(!nvf_uid_valid(adopted));
    puts("typed UID and read-only discovery: CS128, EUI64, faults and adoption PASS");
}
