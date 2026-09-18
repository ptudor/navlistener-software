// Test-only references for the mcu_identity host tests: FIPS 180-4 SHA-256, the RSA public
// operation for e = 65537, and EMSA-PSS-VERIFY (RFC 8017 §9.1.2). They are written
// independently of the code under test so the fixtures, produced by the Go reference
// implementation, arbitrate between the two. Nothing here is built into the firmware.

#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

static inline uint32_t ref_rotr(uint32_t x, unsigned n) { return x >> n | x << (32 - n); }

static bool ref_sha256(const void *data, size_t size, uint8_t out[32])
{
    static const uint32_t k[64] = {
        0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
        0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
        0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
        0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
        0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
        0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
        0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
        0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2 };
    uint32_t h[8] = { 0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19 };
    const uint8_t *p = data;
    size_t total = size + 1 + 8, blocks = (total + 63) / 64;
    for (size_t b = 0; b < blocks; b++) {
        uint8_t block[64] = {0};
        for (size_t i = 0; i < 64; i++) {
            size_t at = b * 64 + i;
            if (at < size) block[i] = p[at];
            else if (at == size) block[i] = 0x80;
        }
        if (b == blocks - 1) for (unsigned i = 0; i < 8; i++) block[63 - i] = (uint8_t)((uint64_t)size * 8 >> (8 * i));
        uint32_t w[64], v[8];
        for (unsigned i = 0; i < 16; i++)
            w[i] = (uint32_t)block[4*i] << 24 | (uint32_t)block[4*i+1] << 16 | (uint32_t)block[4*i+2] << 8 | block[4*i+3];
        for (unsigned i = 16; i < 64; i++) {
            uint32_t s0 = ref_rotr(w[i-15], 7) ^ ref_rotr(w[i-15], 18) ^ (w[i-15] >> 3);
            uint32_t s1 = ref_rotr(w[i-2], 17) ^ ref_rotr(w[i-2], 19) ^ (w[i-2] >> 10);
            w[i] = w[i-16] + s0 + w[i-7] + s1;
        }
        memcpy(v, h, sizeof v);
        for (unsigned i = 0; i < 64; i++) {
            uint32_t s1 = ref_rotr(v[4], 6) ^ ref_rotr(v[4], 11) ^ ref_rotr(v[4], 25);
            uint32_t ch = (v[4] & v[5]) ^ (~v[4] & v[6]);
            uint32_t t1 = v[7] + s1 + ch + k[i] + w[i];
            uint32_t s0 = ref_rotr(v[0], 2) ^ ref_rotr(v[0], 13) ^ ref_rotr(v[0], 22);
            uint32_t maj = (v[0] & v[1]) ^ (v[0] & v[2]) ^ (v[1] & v[2]);
            v[7] = v[6]; v[6] = v[5]; v[5] = v[4]; v[4] = v[3] + t1;
            v[3] = v[2]; v[2] = v[1]; v[1] = v[0]; v[0] = t1 + s0 + maj;
        }
        for (unsigned i = 0; i < 8; i++) h[i] += v[i];
    }
    for (unsigned i = 0; i < 8; i++) { out[4*i] = h[i] >> 24; out[4*i+1] = h[i] >> 16; out[4*i+2] = h[i] >> 8; out[4*i+3] = h[i]; }
    return true;
}

// --- RSA public operation, e = 65537, 3072-bit modulus -------------------------------------
// Little-endian 32-bit words with one spare word so a sum of two residues never overflows.
#define REF_WORDS 96
typedef struct { uint32_t w[REF_WORDS + 1]; } ref_bn;

static void ref_bn_from_be(ref_bn *x, const uint8_t *b, size_t n)
{
    memset(x, 0, sizeof *x);
    for (size_t i = 0; i < n; i++) x->w[i / 4] |= (uint32_t)b[n - 1 - i] << (8 * (i % 4));
}
static void ref_bn_to_be(const ref_bn *x, uint8_t *b, size_t n)
{
    for (size_t i = 0; i < n; i++) b[n - 1 - i] = (uint8_t)(x->w[i / 4] >> (8 * (i % 4)));
}
static int ref_bn_cmp(const ref_bn *a, const ref_bn *b)
{
    for (int i = REF_WORDS; i >= 0; i--) if (a->w[i] != b->w[i]) return a->w[i] > b->w[i] ? 1 : -1;
    return 0;
}
static void ref_bn_addmod(ref_bn *r, const ref_bn *a, const ref_bn *n)
{
    uint64_t carry = 0;
    for (int i = 0; i <= REF_WORDS; i++) { carry += (uint64_t)r->w[i] + a->w[i]; r->w[i] = (uint32_t)carry; carry >>= 32; }
    if (ref_bn_cmp(r, n) >= 0) {
        uint64_t borrow = 0;
        for (int i = 0; i <= REF_WORDS; i++) {
            uint64_t d = (uint64_t)r->w[i] - n->w[i] - borrow;
            r->w[i] = (uint32_t)d; borrow = d >> 32 & 1;
        }
    }
}
static void ref_bn_mulmod(ref_bn *out, const ref_bn *a, const ref_bn *b, const ref_bn *n)
{
    ref_bn r = {{0}};
    for (int bit = REF_WORDS * 32 - 1; bit >= 0; bit--) {
        ref_bn twice = r;
        ref_bn_addmod(&r, &twice, n);
        if (b->w[bit / 32] >> (bit % 32) & 1) ref_bn_addmod(&r, a, n);
    }
    *out = r;
}
// ref_rsa_public writes sig^65537 mod n, big-endian, all REF_WORDS*4 bytes.
static bool ref_rsa_public(const uint8_t *modulus, const uint8_t *sig, uint8_t *out)
{
    ref_bn n, s, x;
    ref_bn_from_be(&n, modulus, REF_WORDS * 4);
    ref_bn_from_be(&s, sig, REF_WORDS * 4);
    if (ref_bn_cmp(&s, &n) >= 0) return false;
    x = s;
    for (int i = 0; i < 16; i++) { ref_bn square = x; ref_bn_mulmod(&x, &square, &square, &n); }
    ref_bn_mulmod(&x, &x, &s, &n);
    ref_bn_to_be(&x, out, REF_WORDS * 4);
    return true;
}
// ref_spki_modulus finds the 3072-bit modulus inside a DER SubjectPublicKeyInfo and checks
// that the public exponent is 65537. Minimal by design: it accepts exactly the key shape the
// commissioning statement allows.
static const uint8_t *ref_spki_modulus(const uint8_t *der, size_t len)
{
    static const uint8_t integer[] = { 0x02, 0x82, 0x01, 0x81, 0x00 }; // INTEGER, 385 bytes, leading zero
    static const uint8_t exponent[] = { 0x02, 0x03, 0x01, 0x00, 0x01 };
    for (size_t i = 0; i + sizeof integer + REF_WORDS * 4 + sizeof exponent <= len; i++)
        if (!memcmp(der + i, integer, sizeof integer) &&
            !memcmp(der + i + sizeof integer + REF_WORDS * 4, exponent, sizeof exponent) &&
            i + sizeof integer + REF_WORDS * 4 + sizeof exponent == len)
            return der + i + sizeof integer;
    return NULL;
}

// --- EMSA-PSS-VERIFY, SHA-256, MGF1-SHA-256, 32-byte salt, emBits = 8*em_len - 1 ------------
static bool ref_pss_verify(const uint8_t digest[32], const uint8_t *em, size_t em_len)
{
    enum { H = 32, SALT = 32 };
    if (em_len < H + SALT + 2 || em_len > 512 || em[em_len - 1] != 0xbc || (em[0] & 0x80)) return false;
    size_t db_len = em_len - H - 1;
    const uint8_t *h = em + db_len;
    uint8_t db[512], seed[H + 4], mask[H];
    memcpy(seed, h, H);
    for (size_t at = 0, counter = 0; at < db_len; at += H, counter++) {
        seed[H] = (uint8_t)(counter >> 24); seed[H+1] = (uint8_t)(counter >> 16);
        seed[H+2] = (uint8_t)(counter >> 8); seed[H+3] = (uint8_t)counter;
        ref_sha256(seed, sizeof seed, mask);
        for (size_t i = 0; i < H && at + i < db_len; i++) db[at + i] = em[at + i] ^ mask[i];
    }
    db[0] &= 0x7f;
    for (size_t i = 0; i < db_len - SALT - 1; i++) if (db[i]) return false;
    if (db[db_len - SALT - 1] != 0x01) return false;
    uint8_t m[8 + H + SALT] = {0}, expect[H];
    memcpy(m + 8, digest, H); memcpy(m + 8 + H, db + db_len - SALT, SALT);
    ref_sha256(m, sizeof m, expect);
    return !memcmp(expect, h, H);
}
