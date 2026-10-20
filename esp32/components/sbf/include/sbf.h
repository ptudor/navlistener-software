// sbf: Septentrio Binary Format framing and the blocks the observer reads from a mosaic
// receiver's COM2 (mosaic-G5 firmware reference guide v1.0.1, chapter 4).
//
// A block is the sync "$@", a CRC-16-CCITT (polynomial 0x1021, seed 0, no reflection or final
// XOR) over everything after the CRC field, a 16-bit ID (bits 0-12 the block number, 13-15
// the revision) and the block's total length, a multiple of 4; all little-endian (4.1.1).
// The reader takes the port's bytes as they arrive. It returns each CRC-checked block, and
// passes the ASCII the receiver writes between blocks (command replies, which are atomic,
// 3.1.3) as lines and prompts. A sync that fails its length or CRC check is rescanned from the
// next byte, as 4.1.12 recommends, so a "$@" inside a block or in noise costs nothing.
//
// The decoders are pure and bounds-check every field against the block's Length and each
// sub-block's SBLength; padding and reserved fields are ignored, and fields added in later
// revisions are read only when the revision and length carry them (4.1.5, 4.1.6, 4.1.12).
#ifndef NVF_SBF_H
#define NVF_SBF_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define SBF_HEADER    8
#define SBF_MAX_BLOCK 8192 // larger blocks are skipped; a full MeasEpoch fits with room
#define SBF_LINE_MAX  2048 // the longest command line is 2000 characters (3.1.2)

// The blocks the observer configures (4.2). Only bits 0-12 of the ID identify a block.
enum {
    SBF_MEAS_EPOCH = 4027,
    SBF_PVT_GEODETIC = 4007,
    SBF_RECEIVER_TIME = 5914,
    SBF_SAT_VISIBILITY = 4012,
    SBF_RECEIVER_STATUS = 4014,
    SBF_RF_STATUS = 4092,
    SBF_RECEIVER_SETUP = 5902,
};
#define SBF_TOW_UNKNOWN 4294967295u
#define SBF_WNC_UNKNOWN 65535u

uint16_t sbf_crc16(const uint8_t *p, size_t n, uint16_t crc);

typedef void (*sbf_block_fn)(const uint8_t *block, size_t length, void *ctx);
// A line of ASCII outside any block, without its CR/LF: a command reply's lines.
typedef void (*sbf_line_fn)(const char *line, void *ctx);
// A command prompt such as "COM2>", which ends every reply (3.1).
typedef void (*sbf_prompt_fn)(const char *prompt, void *ctx);

typedef struct {
    sbf_block_fn block;
    sbf_line_fn line;
    sbf_prompt_fn prompt;
    void *ctx;
    size_t used;
    uint8_t buffer[SBF_MAX_BLOCK + 512];
    size_t line_used;
    bool line_overflow;
    char line_text[SBF_LINE_MAX + 1];
    // Counters for the logs, owned by the reading task.
    uint32_t bytes, blocks, crc_errors, bad_lengths;
} sbf_reader_t;

void sbf_reader_init(sbf_reader_t *r, sbf_block_fn block, sbf_line_fn line, sbf_prompt_fn prompt, void *ctx);
void sbf_reader_feed(sbf_reader_t *r, const uint8_t *data, size_t n);

static inline uint16_t sbf_u16(const uint8_t *p) { return (uint16_t)(p[0] | p[1] << 8); }
static inline uint32_t sbf_u32(const uint8_t *p)
{ return (uint32_t)p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 | (uint32_t)p[3] << 24; }
static inline uint16_t sbf_number(const uint8_t *block) { return sbf_u16(block + 4) & 0x1fff; }
static inline uint8_t sbf_revision(const uint8_t *block) { return (uint8_t)(sbf_u16(block + 4) >> 13); }
static inline uint32_t sbf_tow(const uint8_t *block) { return sbf_u32(block + 8); }
static inline uint16_t sbf_wnc(const uint8_t *block) { return sbf_u16(block + 12); }

// PVTGeodetic (4007). position: the solution type is one computed from GNSS signals (not 0 "no
// PVT", nor 3 "fixed location", which reports entered coordinates) with Error 0 and in-range
// coordinates. Accuracy fields arrived in revision 1.
typedef struct {
    uint32_t tow;
    uint16_t wnc;
    uint8_t mode, error, nr_sv;
    bool position;
    double latitude_deg, longitude_deg, height_m;
    bool accuracy;
    uint16_t h_accuracy_cm, v_accuracy_cm; // 2DRMS and 2-sigma, 95 %; 65535 unknown
} sbf_pvt_t;
bool sbf_pvt_geodetic(const uint8_t *block, size_t length, sbf_pvt_t *out);

// ReceiverTime (5914): the UTC calendar at the time stamp, -128 where unknown, and SyncLevel
// bits WNSET 1, TOWSET 2, FINETIME 4.
typedef struct {
    uint32_t tow;
    uint16_t wnc;
    int8_t year, month, day, hour, minute, second, delta_ls;
    uint8_t sync;
} sbf_time_t;
enum { SBF_SYNC_WNSET = 1, SBF_SYNC_TOWSET = 2, SBF_SYNC_FINETIME = 4 };
bool sbf_receiver_time(const uint8_t *block, size_t length, sbf_time_t *out);

// One signal of a MeasEpoch (4027): master or slave sub-block alike.
typedef struct {
    uint8_t svid, signal, antenna;
    uint8_t cn0;     // raw, 0.25 dB-Hz steps; 255 unknown (see sbf_cn0_centi_dbhz)
    bool code_valid; // the pseudorange is present
} sbf_meas_t;
typedef void (*sbf_meas_fn)(const sbf_meas_t *signal, void *ctx);
// Validates the whole block, then calls fn for every signal in receiver order. False, with no
// call, for a block whose sub-blocks do not fit. common_flags may be NULL.
bool sbf_meas_epoch(const uint8_t *block, size_t length, uint8_t *common_flags, sbf_meas_fn fn, void *ctx);
enum { SBF_MEAS_SCRAMBLED = 0x80 }; // CommonFlags bit 7: no Measurement Availability permission
// C/N0 in 0.01 dB-Hz: raw x 0.25 for the GPS P(Y) signals 1 and 2, raw x 0.25 + 10 otherwise.
int sbf_cn0_centi_dbhz(uint8_t raw, uint8_t signal);

// One SatInfo of a SatVisibility (4012).
typedef struct {
    uint8_t svid;
    int16_t elevation;  // 0.01 degree; -32768 unknown
    uint16_t azimuth;   // 0.01 degree; 65535 unknown
} sbf_sat_t;
typedef void (*sbf_sat_fn)(const sbf_sat_t *sat, void *ctx);
bool sbf_sat_visibility(const uint8_t *block, size_t length, sbf_sat_fn fn, void *ctx);

// RFStatus (4092): Flags bit 0 the receiver's own authenticity tests, bit 1 a navigation
// message failing authentication; and the RF bands with detected interference, mitigated
// (mode 2) or not (mode 8). Notch filters a user set (mode 1) are not interference.
typedef struct {
    uint8_t flags;
    uint8_t mitigated, unmitigated;
} sbf_rf_t;
bool sbf_rf_status(const uint8_t *block, size_t length, sbf_rf_t *out);

// ReceiverStatus (4014).
typedef struct {
    uint8_t cpu_load;
    uint32_t uptime_s, rx_state, rx_error;
} sbf_rx_status_t;
enum { SBF_RXSTATE_ACTIVE_ANTENNA = 1u << 1, SBF_RXERROR_ANTENNA = 1u << 5, SBF_RXERROR_CONGESTION = 1u << 6 };
bool sbf_receiver_status(const uint8_t *block, size_t length, sbf_rx_status_t *out);

// ReceiverSetup (5902): the receiver's identity, NUL-terminated and printable. ProductName
// arrived in revision 3 and is empty before it.
typedef struct {
    char serial[21], name[21], version[21], product[41];
} sbf_setup_t;
bool sbf_receiver_setup(const uint8_t *block, size_t length, sbf_setup_t *out);

// The UBX gnssId and satellite number for an SVID (4.1.9). False for L-band beams, a GLONASS
// satellite whose slot is unknown and undefined values, which 4.1.9 says to ignore.
bool sbf_svid(uint8_t svid, uint8_t *gnss, uint8_t *sv);
// The constellation a signal number belongs to (4.1.10), and its signal ID in the reception
// space of common/reception.h, where the observer's u-blox signal identities live. canonical
// is 255 for a signal without a counterpart there (GPS P(Y), Galileo E6, BeiDou B3I and B2b,
// QZSS L6 and L1C, NavIC L1, SBAS L5); it still counts for its satellite. False for reserved
// numbers and L-band.
bool sbf_signal(uint8_t number, uint8_t *gnss, uint8_t *canonical);
#define SBF_NO_CANONICAL 255u
#endif
