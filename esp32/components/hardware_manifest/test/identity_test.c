#include <assert.h>
#include <stdio.h>
#include "../../../../common/board_identity.h"

typedef struct { unsigned model[2], cs; bool fault, bad_id, bad_revision, unstable; unsigned serial_reads; } bus;
static const uint8_t cs_ids[3][3] = {{0x00,0xd0,0xb8}, {0x00,0xd0,0xc0}, {0x00,0xd0,0xc8}}; /* 24CS128/256/512 */
static int read_id(void *ctx, uint8_t address, uint16_t reg, uint8_t width, uint8_t *out, size_t n) {
    bus *b=ctx;
    if (b->fault) return NVF_ID_IO_ERROR;
    if (address==0x7c) {
        assert(width==1 && n==3 && (reg==0xa0 || reg==0xa2));
        if (b->model[(reg-0xa0)/2]!=3) return NVF_ID_NACK;
        memcpy(out, cs_ids[b->cs], 3); if(b->bad_id) out[2]^=8; if(b->bad_revision) out[2]|=1;
        return NVF_ID_READ_OK;
    }
    if (address==0x58 || address==0x59) {
        unsigned model=b->model[address-0x58];
        if (model != 3 && model != 4) return NVF_ID_ABSENT;
        assert(width==2 && n==16 && reg==(model==3 ? 0x800 : 0));
        for(unsigned i=0;i<16;i++) out[i]=(uint8_t)i;
        if(model==4) { memcpy(out,"\x20\xe0\x0e\xff",4); if(b->bad_id)out[2]^=1; }
        if(b->unstable)out[15]+=(uint8_t)b->serial_reads++;
        return NVF_ID_READ_OK;
    }
    assert(address==0x50 || address==0x51);
    unsigned model=b->model[address-0x50];
    if(!model)return NVF_ID_ABSENT;
    assert(model!=3 && model!=4); /* A 16-bit EEPROM must never see an 8-bit pointer. */
    assert(width==1 && n==8 && reg==0xf8);
    if(model!=1)return NVF_ID_NACK;
    memcpy(out,"\x00\x04\xa3\x01\x02\x03\x04\x05",8);
    return NVF_ID_READ_OK;
}
int main(void) {
    nvf_board_identity_t out; uint8_t adopted[NVF_BOARD_UID_SIZE];
    bus b={.model={3,0}};
    assert(!nvf_board_discover(read_id,&b,NULL,0,&out));
    assert(out.board_valid && out.eeprom_kind == NVF_UID_SERIAL128 && out.eeprom_kbit == 128 && out.board_address==0x50 && out.board_uid[2]==16);
    assert(!strcmp(nvf_uid_kind_name(out.eeprom_kind), "serial128"));
    memcpy(adopted,out.board_uid,sizeof adopted);
    char id[NVF_BOARD_OBSERVER_SIZE]; assert(nvf_uid_observer(adopted,id));
    assert(!strcmp(id,"board-0003-000102030405060708090a0b0c0d0e0f"));
    b.model[0]=0;b.model[1]=3;
    assert(!nvf_board_discover(read_id,&b,adopted,0,&out) && out.board_address==0x51);
    b.model[0]=1;b.model[1]=0; assert(nvf_board_discover(read_id,&b,adopted,0,&out));
    assert(!nvf_board_discover(read_id,&b,NULL,0,&out) && nvf_uid_kind(out.board_uid)==NVF_UID_EUI64 && out.eeprom_kbit==2);
    assert(!strcmp(nvf_uid_kind_name(NVF_UID_EUI64), "eui64"));
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
    b=(bus){.model={4,0}};
    assert(!nvf_board_discover(read_id,&b,NULL,0,&out));
    assert(out.eeprom_kind==NVF_UID_ST_UID128 && out.eeprom_kbit==128 && out.board_uid[2]==16);
    assert(!strcmp(nvf_uid_kind_name(out.eeprom_kind), "st_uid128"));
    memcpy(adopted,out.board_uid,sizeof adopted);
    assert(nvf_uid_observer(adopted,id));
    assert(!strcmp(id,"board-0004-20e00eff0405060708090a0b0c0d0e0f"));
    b.model[0]=0;b.model[1]=4;
    assert(!nvf_board_discover(read_id,&b,adopted,0,&out) && out.board_address==0x51);
    assert(nvf_board_discover(read_id,&b,adopted,NVF_UID_SERIAL128,&out));
    b.bad_id=true;assert(nvf_board_discover(read_id,&b,NULL,0,&out));b.bad_id=false;
    b.unstable=true;assert(nvf_board_discover(read_id,&b,NULL,0,&out));b.unstable=false;
    b.model[0]=3;assert(nvf_board_discover(read_id,&b,NULL,0,&out));
    b.model[0]=1;assert(nvf_board_discover(read_id,&b,NULL,0,&out));
    b.model[0]=4;assert(nvf_board_discover(read_id,&b,NULL,0,&out));
    b.model[0]=0;b.model[1]=3;assert(nvf_board_discover(read_id,&b,adopted,0,&out));
    b.model[1]=0;assert(nvf_board_discover(read_id,&b,adopted,0,&out));
    assert(!nvf_board_discover(read_id,&b,NULL,0,&out) && !out.board_valid);
    /* 24CS256 and 24CS512: the same 128-bit serial kind, with their density. */
    for (unsigned cs = 1; cs < 3; cs++) {
        b=(bus){.model={3,0},.cs=cs};
        assert(!nvf_board_discover(read_id,&b,NULL,0,&out));
        assert(out.board_valid && out.eeprom_kind==NVF_UID_SERIAL128 && out.eeprom_kbit==(cs==1 ? 256 : 512));
        assert(nvf_uid_observer(out.board_uid,id) && !strcmp(id,"board-0003-000102030405060708090a0b0c0d0e0f"));
        b.bad_revision=true;assert(nvf_board_discover(read_id,&b,NULL,0,&out));
    }
    b=(bus){.model={3,0}};b.bad_revision=true;assert(nvf_board_discover(read_id,&b,NULL,0,&out));
    adopted[1]=2;assert(!nvf_uid_valid(adopted));adopted[1]=4;
    adopted[34]=1;assert(!nvf_uid_valid(adopted));
    puts("typed UID and read-only discovery: 24CS128/256/512 serial128, ST UID128, EUI64, faults and adoption PASS");
}
