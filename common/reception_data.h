/* Reception telemetry (GNF1 telemetry type 0x01, body version 2), shared by the ESP32
 * firmware and the C feeder. One record carries one UBX-NAV-SAT epoch: for each
 * satellite its C/N0, elevation, azimuth and pseudorange residual, the receiver's
 * quality indicator and health, and whether the receiver used it in its solution.
 * The body layout and its validation match go/internal/ingest/telemetry.go byte for
 * byte; testdata/reception_data_v2.txt is the shared golden case.
 *
 * Body: [version = 2][count U2], then 9 bytes per satellite:
 *   gnssId U1, svId U1, C/N0 U1 (dB-Hz), elevation I1 (degrees, forwarded verbatim,
 *   including NAV-SAT's out-of-range "unknown"), azimuth I2 (degrees, verbatim),
 *   pseudorange residual I2 (0.1 m), flags U1: bit 0 used in the solution, bits 1-3
 *   the quality indicator (u-blox scale 0-7), bits 4-5 health (0 unknown, 1 healthy,
 *   2 unhealthy; the reserved value 3 is sent as unknown), bits 6-7 zero.
 * Multi-byte fields are big-endian. Tracked satellites (C/N0 above zero) come first,
 * in receiver order, then untracked ones, up to RD_MAX_SATS, which fills the record
 * body limit; a receiver lists far fewer tracked satellites than that.
 *
 * Signal identity is not carried: NAV-SAT reports each satellite once, whatever
 * signals the receiver tracks from it.
 *
 * Header-only and I/O-free. */
#ifndef NAVLISTENER_RECEPTION_DATA_H
#define NAVLISTENER_RECEPTION_DATA_H

#include <stddef.h>
#include <stdint.h>

#define RD_TELEM_TYPE 0x01
#define RD_VERSION    2
#define RD_ENTRY_LEN  9
#define RD_MAX_SATS   113 /* 3 + 9 * 113 = 1020 bytes */
#define RD_BODY_MAX   (3 + RD_ENTRY_LEN * RD_MAX_SATS)

/* u-blox NAV-SAT: class, id, message version, header and per-satellite block. */
#define RD_UBX_CLASS_NAV   0x01
#define RD_UBX_ID_SAT      0x35
#define RD_UBX_SAT_VERSION 1
#define RD_UBX_SAT_HDR     8
#define RD_UBX_SAT_BLOCK   12

/* rd_put_entry writes one body entry from a 12-byte NAV-SAT block: gnssId, svId,
 * cno, elev I1, azim I2, prRes I2, flags X4 (qualityInd bits 0-2, svUsed bit 3,
 * health bits 4-5), little-endian. */
static inline void rd_put_entry(uint8_t *o, const uint8_t *s)
{
	uint32_t flags = (uint32_t)s[8] | (uint32_t)s[9] << 8 | (uint32_t)s[10] << 16 | (uint32_t)s[11] << 24;
	uint8_t health = (uint8_t)((flags >> 4) & 0x03);
	if (health == 3) health = 0;
	o[0] = s[0];
	o[1] = s[1];
	o[2] = s[2];
	o[3] = s[3];
	o[4] = s[5]; /* azimuth */
	o[5] = s[4];
	o[6] = s[7]; /* pseudorange residual */
	o[7] = s[6];
	o[8] = (uint8_t)(((flags >> 3) & 0x01) | (flags & 0x07) << 1 | (uint32_t)health << 4);
}

/* rd_encode_navsat converts a UBX-NAV-SAT payload into a body in out, which must hold
 * RD_BODY_MAX bytes, and returns the body length: 0 when the payload is malformed or
 * lists no satellite. */
static inline size_t rd_encode_navsat(const uint8_t *p, size_t len, uint8_t *out)
{
	if (len < RD_UBX_SAT_HDR || p[4] != RD_UBX_SAT_VERSION) return 0;
	size_t num = p[5];
	if (num == 0 || len != RD_UBX_SAT_HDR + num * RD_UBX_SAT_BLOCK) return 0;
	size_t n = 0;
	for (int tracked = 1; tracked >= 0; tracked--) {
		for (size_t i = 0; i < num && n < RD_MAX_SATS; i++) {
			const uint8_t *s = p + RD_UBX_SAT_HDR + i * RD_UBX_SAT_BLOCK;
			if ((s[2] > 0) != tracked) continue;
			rd_put_entry(out + 3 + n * RD_ENTRY_LEN, s);
			n++;
		}
	}
	out[0] = RD_VERSION;
	out[1] = (uint8_t)(n >> 8);
	out[2] = (uint8_t)n;
	return 3 + n * RD_ENTRY_LEN;
}

#endif
