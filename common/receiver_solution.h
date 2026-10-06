/* Receiver-solution telemetry (GNF1 telemetry type 0x03), shared by the ESP32
 * firmware and the C feeder. One record carries one navigation epoch of the
 * receiver's own solution: the position, velocity and time block (from UBX-NAV-PVT),
 * the clock block (UBX-NAV-CLOCK) and the status block (UBX-NAV-STATUS). The body
 * layout and its validation match go/internal/ingest/solution.go byte for byte and
 * docs/proposals/STATION-ASSURANCE.md §2; testdata/receiver_solution_v1.txt is the
 * shared golden case.
 *
 * Header-only and I/O-free. The u-blox receiver emits the three messages of an
 * epoch separately; rs_assembler joins them by GPS time of week and completes the
 * epoch when a later epoch starts or NAV-EOE ends it. Every input length and field
 * the collector interprets is checked here, so an out-of-range message is dropped at
 * the edge rather than sent and rejected. */
#ifndef NAVLISTENER_RECEIVER_SOLUTION_H
#define NAVLISTENER_RECEIVER_SOLUTION_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

#define RS_TELEM_TYPE 0x03
#define RS_VERSION    1
#define RS_HAS_PVT    0x01
#define RS_HAS_CLOCK  0x02
#define RS_HAS_STATUS 0x04
#define RS_PVT_LEN    64
#define RS_CLOCK_LEN  20
#define RS_STATUS_LEN 16
#define RS_BODY_MAX   (2 + RS_PVT_LEN + RS_CLOCK_LEN + RS_STATUS_LEN)
#define RS_WEEK_MS    604800000u

/* u-blox NAV message ids and exact payload lengths. */
#define RS_UBX_CLASS_NAV   0x01
#define RS_UBX_ID_STATUS   0x03
#define RS_UBX_ID_PVT      0x07
#define RS_UBX_ID_CLOCK    0x22
#define RS_UBX_ID_EOE      0x61
#define RS_UBX_PVT_LEN     92
#define RS_UBX_CLOCK_LEN   20
#define RS_UBX_STATUS_LEN  16
#define RS_UBX_EOE_LEN     4

/* One epoch's encoded blocks. */
typedef struct {
	uint8_t present; /* RS_HAS_* */
	uint32_t tow;    /* GPS time of week of the epoch, ms */
	/* The caller's clock when the epoch's first block arrived. A record is stamped
	 * with it, not the completion time, which without NAV-EOE is the next epoch. */
	uint64_t arrived_ns;
	uint8_t pvt[RS_PVT_LEN];
	uint8_t clock[RS_CLOCK_LEN];
	uint8_t status[RS_STATUS_LEN];
} rs_epoch_t;

typedef struct {
	rs_epoch_t pending;
	bool have_pending;
	/* The last completed epoch: a later block with its time of week (a polled
	 * repeat) is dropped rather than sent twice. */
	uint32_t last_done;
	bool have_done;
} rs_assembler_t;

static inline uint16_t rs_le16(const uint8_t *b) { return (uint16_t)(b[0] | (b[1] << 8)); }
static inline uint32_t rs_le32(const uint8_t *b)
{
	return (uint32_t)b[0] | ((uint32_t)b[1] << 8) | ((uint32_t)b[2] << 16) | ((uint32_t)b[3] << 24);
}
static inline void rs_be16(uint8_t *b, uint16_t v) { b[0] = (uint8_t)(v >> 8); b[1] = (uint8_t)v; }
static inline void rs_be32(uint8_t *b, uint32_t v)
{
	b[0] = (uint8_t)(v >> 24); b[1] = (uint8_t)(v >> 16); b[2] = (uint8_t)(v >> 8); b[3] = (uint8_t)v;
}

/* rs_pvt_block validates a UBX-NAV-PVT payload and encodes the 64-byte PVT block.
 * Bits the record does not carry (validMag, psmState, headVehValid, the correction
 * age) are dropped; a payload the collector would reject returns false. */
static inline bool rs_pvt_block(const uint8_t *p, size_t len, uint8_t out[RS_PVT_LEN], uint32_t *tow)
{
	if (len != RS_UBX_PVT_LEN) return false;
	uint32_t itow = rs_le32(p);
	uint16_t year = rs_le16(p + 4);
	uint8_t month = p[6], day = p[7], hour = p[8], minute = p[9], second = p[10];
	uint8_t valid = p[11] & 0x07;
	int32_t nano = (int32_t)rs_le32(p + 16);
	uint8_t fix_type = p[20], flags = p[21] & 0xC3;
	int32_t lon = (int32_t)rs_le32(p + 24), lat = (int32_t)rs_le32(p + 28);
	uint16_t pos_flags = rs_le16(p + 78) & 0x0001;
	if (itow >= RS_WEEK_MS || fix_type > 5 || (flags & 0xC0) == 0xC0 ||
	    lat < -900000000 || lat > 900000000 || lon < -1800000000 || lon > 1800000000 ||
	    nano <= -1000000000 || nano >= 1000000000)
		return false;
	if ((valid & 0x01) && (year < 1999 || year > 2200 || month < 1 || month > 12 || day < 1 || day > 31))
		return false;
	if ((valid & 0x02) && (hour > 23 || minute > 59 || second > 60)) return false;
	memset(out, 0, RS_PVT_LEN);
	rs_be32(out, itow);
	rs_be16(out + 4, year);
	out[6] = month; out[7] = day; out[8] = hour; out[9] = minute; out[10] = second;
	out[11] = valid;
	rs_be32(out + 12, rs_le32(p + 12));          /* tAcc */
	rs_be32(out + 16, (uint32_t)nano);
	out[20] = fix_type; out[21] = flags; out[22] = p[23]; /* numSV; out[23] reserved */
	rs_be32(out + 24, (uint32_t)lon);
	rs_be32(out + 28, (uint32_t)lat);
	rs_be32(out + 32, rs_le32(p + 32));          /* height above ellipsoid, mm */
	rs_be32(out + 36, rs_le32(p + 40));          /* hAcc */
	rs_be32(out + 40, rs_le32(p + 44));          /* vAcc */
	rs_be32(out + 44, rs_le32(p + 48));          /* velN */
	rs_be32(out + 48, rs_le32(p + 52));          /* velE */
	rs_be32(out + 52, rs_le32(p + 56));          /* velD */
	rs_be32(out + 56, rs_le32(p + 68));          /* sAcc */
	rs_be16(out + 60, rs_le16(p + 76));          /* pDOP */
	rs_be16(out + 62, pos_flags);
	*tow = itow;
	return true;
}

/* rs_clock_block validates a UBX-NAV-CLOCK payload and encodes the clock block. */
static inline bool rs_clock_block(const uint8_t *p, size_t len, uint8_t out[RS_CLOCK_LEN], uint32_t *tow)
{
	if (len != RS_UBX_CLOCK_LEN || rs_le32(p) >= RS_WEEK_MS) return false;
	for (unsigned i = 0; i < 5; i++) rs_be32(out + 4 * i, rs_le32(p + 4 * i));
	*tow = rs_le32(p);
	return true;
}

/* rs_status_block validates a UBX-NAV-STATUS payload and encodes the status block;
 * the spoofing state is flags2 bits 3..4. */
static inline bool rs_status_block(const uint8_t *p, size_t len, uint8_t out[RS_STATUS_LEN], uint32_t *tow)
{
	if (len != RS_UBX_STATUS_LEN || rs_le32(p) >= RS_WEEK_MS || p[4] > 5) return false;
	rs_be32(out, rs_le32(p));
	out[4] = p[4]; out[5] = p[5]; out[6] = p[6]; out[7] = (uint8_t)((p[7] >> 3) & 0x03);
	rs_be32(out + 8, rs_le32(p + 8));
	rs_be32(out + 12, rs_le32(p + 12));
	*tow = rs_le32(p);
	return true;
}

static inline void rs_assembler_init(rs_assembler_t *a) { memset(a, 0, sizeof *a); }

/* rs_assembler_add merges one encoded block that arrived at now_ns. It returns true
 * and fills done when the block begins a new epoch, completing the previous one. A
 * block of the epoch just completed is a repeat and is dropped. */
static inline bool rs_assembler_add(rs_assembler_t *a, uint8_t block, uint32_t tow, const uint8_t *data,
                                    uint64_t now_ns, rs_epoch_t *done)
{
	bool completed = false;
	if (a->have_done && tow == a->last_done) return false;
	if (a->have_pending && a->pending.tow != tow) {
		*done = a->pending;
		a->have_pending = false;
		a->last_done = done->tow;
		a->have_done = completed = true;
	}
	if (!a->have_pending) {
		memset(&a->pending, 0, sizeof a->pending);
		a->pending.tow = tow;
		a->pending.arrived_ns = now_ns;
		a->have_pending = true;
	}
	switch (block) {
	case RS_HAS_PVT:    memcpy(a->pending.pvt, data, RS_PVT_LEN); break;
	case RS_HAS_CLOCK:  memcpy(a->pending.clock, data, RS_CLOCK_LEN); break;
	case RS_HAS_STATUS: memcpy(a->pending.status, data, RS_STATUS_LEN); break;
	default: return completed;
	}
	a->pending.present |= block;
	return completed;
}

/* rs_assembler_end completes the pending epoch when NAV-EOE names it. */
static inline bool rs_assembler_end(rs_assembler_t *a, uint32_t tow, rs_epoch_t *done)
{
	if (!a->have_pending || a->pending.tow != tow) return false;
	*done = a->pending;
	a->have_pending = false;
	a->last_done = tow;
	a->have_done = true;
	return true;
}

/* rs_ubx_feed routes one checksum-valid UBX message, received at now_ns, through the
 * assembler. It returns true and fills done when an epoch completes; bad reports
 * whether the message was a solution message the record could not carry. */
static inline bool rs_ubx_feed(rs_assembler_t *a, uint8_t cls, uint8_t id, const uint8_t *p, size_t len,
                               uint64_t now_ns, rs_epoch_t *done, bool *bad)
{
	uint8_t block[RS_PVT_LEN];
	uint32_t tow = 0;
	*bad = false;
	if (cls != RS_UBX_CLASS_NAV) return false;
	switch (id) {
	case RS_UBX_ID_PVT:
		if (!rs_pvt_block(p, len, block, &tow)) { *bad = true; return false; }
		return rs_assembler_add(a, RS_HAS_PVT, tow, block, now_ns, done);
	case RS_UBX_ID_CLOCK:
		if (!rs_clock_block(p, len, block, &tow)) { *bad = true; return false; }
		return rs_assembler_add(a, RS_HAS_CLOCK, tow, block, now_ns, done);
	case RS_UBX_ID_STATUS:
		if (!rs_status_block(p, len, block, &tow)) { *bad = true; return false; }
		return rs_assembler_add(a, RS_HAS_STATUS, tow, block, now_ns, done);
	case RS_UBX_ID_EOE:
		if (len != RS_UBX_EOE_LEN) { *bad = true; return false; }
		return rs_assembler_end(a, rs_le32(p), done);
	}
	return false;
}

/* rs_encode writes the record body: [version][present][blocks in order]. It returns
 * the body length, or 0 when cap is too small or no block is present. */
static inline size_t rs_encode(const rs_epoch_t *e, uint8_t *out, size_t cap)
{
	size_t n = 2;
	if (e->present & RS_HAS_PVT) n += RS_PVT_LEN;
	if (e->present & RS_HAS_CLOCK) n += RS_CLOCK_LEN;
	if (e->present & RS_HAS_STATUS) n += RS_STATUS_LEN;
	if (!(e->present & (RS_HAS_PVT | RS_HAS_CLOCK | RS_HAS_STATUS)) || n > cap) return 0;
	out[0] = RS_VERSION;
	out[1] = e->present & (RS_HAS_PVT | RS_HAS_CLOCK | RS_HAS_STATUS);
	size_t o = 2;
	if (e->present & RS_HAS_PVT) { memcpy(out + o, e->pvt, RS_PVT_LEN); o += RS_PVT_LEN; }
	if (e->present & RS_HAS_CLOCK) { memcpy(out + o, e->clock, RS_CLOCK_LEN); o += RS_CLOCK_LEN; }
	if (e->present & RS_HAS_STATUS) { memcpy(out + o, e->status, RS_STATUS_LEN); }
	return n;
}

#endif
