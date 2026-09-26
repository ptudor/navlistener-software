#ifndef NVF_ICM45686_H
#define NVF_ICM45686_H
// ICM-45686 six-axis IMU (the MAX board's U37 at I2C 0x69), TDK DS-000577 Rev 1.0.
// Accelerometer and gyroscope run in low-noise mode into the FIFO in stream mode, each
// through its UI low-pass filter at a quarter of the output rate, so vibration above it
// is removed rather than aliased. The rate follows motion, like a CPU governor: 12.5 Hz
// while still, the profile's moving rate from the first sample that shows motion until
// ICM45686_QUIET_MS pass without any. INT1 (GPIO16) signals the watermark or a full
// FIFO; INT2 (GPIO17) is driven but unused. Data stays in the default little-endian format.
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct {
    void *ctx;
    bool (*read)(void *ctx, uint8_t reg, uint8_t *data, size_t length);
    bool (*write)(void *ctx, uint8_t reg, const uint8_t *data, size_t length);
    void (*delay_ms)(void *ctx, unsigned ms);
} icm45686_io_t;

enum {
    ICM45686_ADDRESS = 0x69, ICM45686_WHO_AM_I = 0xe9,
    ICM45686_PACKET = 16,        // header, accel, gyro, 1-byte temperature, timestamp
    ICM45686_FIFO_PACKETS = 128, // 2 KB FIFO: 10 s still, 1.28 s at 100 Hz
    ICM45686_WATERMARK = 16,     // packets: 1.28 s still, 0.16 s at 100 Hz
    ICM45686_STILL_DECIHZ = 125, // 12.5 Hz
    ICM45686_QUIET_MS = 30000,
};

// The ranges, fixed for a unit, and the moving rate. One count is range/32768 of a g or
// of a degree per second. A valid sample shows motion when its acceleration is more than
// 0.1 g from 1 g in magnitude or from the tracked gravity vector (a 2 s average, so a
// unit parked on a new slope settles), or its rotation rate is above 5 degrees per
// second. Horizontal acceleration barely changes the magnitude, hence the vector test.
typedef struct {
    uint8_t accel_fs_g, accel_fs_sel;
    uint16_t gyro_fs_dps;
    uint8_t gyro_fs_sel;
    uint16_t moving_decihz;
    uint8_t moving_odr; // the ODR field code, sections 17.26 and 17.27
} icm45686_profile_t;
// Surface (vehicles and vessels): +/-8 g, +/-1000 dps, 50 Hz moving.
// Aerial (aircraft and drones): +/-16 g, +/-2000 dps, 100 Hz moving.
extern const icm45686_profile_t ICM45686_SURFACE, ICM45686_AERIAL;
uint16_t icm45686_period_ms(uint16_t decihz);

// The valid samples in a window: their count and the time they cover, sums weighted by
// that time (so the mean vectors are time averages across rate changes), and the extremes
// of their magnitudes, squared, in counts.
typedef struct {
    bool valid;
    uint32_t samples, span_ms;
    int64_t accel_sum[3], gyro_sum[3]; // count x milliseconds
    uint32_t accel_min_sq, accel_max_sq, gyro_max_sq;
} icm45686_window_t;
enum { ICM45686_WINDOW_REPORT, ICM45686_WINDOW_SNAPSHOT };

typedef struct {
    // Set by icm45686_stats_start from the profile.
    uint32_t accel_low_sq, accel_high_sq, gyro_motion_sq;
    int64_t reference_motion_sq; // (0.1 g x 256)^2
    // The tracked gravity vector, counts x 256; valid from the first sample.
    bool reference_valid;
    int32_t reference[3];
    // Governor: the rate in force, the time motion was last seen, changes since boot.
    bool moving;
    int64_t last_motion_ms;
    uint32_t rate_changes;
    bool motion_seen;           // the last service decoded a sample showing motion
    uint16_t period_ms;         // of the packets being decoded
    uint16_t pending_period_ms; // after a rate change, from the packet that flags it
    // Counters since boot.
    uint32_t packets;       // accelerometer-and-gyroscope packets decoded
    uint32_t overflows;     // FIFO-full events: stream mode overwrote the oldest packets
    uint32_t resyncs;       // FIFO flushes after an undecodable packet
    uint32_t latest_packet; // the packet count at the latest valid sample
    bool latest_valid;
    int16_t accel[3], gyro[3]; // the latest valid packet, raw counts in the sensor's axes
    int8_t temp;               // its FIFO temperature: C = temp / 2 + 25
    // Every valid sample updates both windows: the one a report covers, and the one
    // since the last snapshot, which becomes the report window once that report is sent.
    icm45686_window_t window[2];
} icm45686_stats_t;

// Motion thresholds for the profile and the still rate, after a configuration; counters
// and windows carry on.
void icm45686_stats_start(icm45686_stats_t *stats, const icm45686_profile_t *profile);
// Soft reset, identification, push-pull active-high INT1/INT2, the profile's ranges at
// the still rate, the FIFO in stream mode, low-noise power, both UI low-pass filters at
// ODR/4, a start-up flush, then the INT1 sources. True only when the configuration,
// filters included, reads back.
bool icm45686_configure(const icm45686_io_t *io, const icm45686_profile_t *profile);
// Reads INT1_STATUS0 and services the FIFO, retaining its newest packet per the
// streaming-mode erratum AN-000364; false on a transfer failure. Reconfigure
// after failure: a partial transfer can leave the FIFO between packet boundaries.
bool icm45686_service(const icm45686_io_t *io, icm45686_stats_t *stats);
// Decodes whole packets; returns the bytes consumed, which stops short of an
// undecodable header (the caller flushes the FIFO to realign).
size_t icm45686_parse(const uint8_t *fifo, size_t length, icm45686_stats_t *stats);
// After a service: true when the governor wants the other rate, given in *moving.
bool icm45686_rate_due(icm45686_stats_t *stats, int64_t now_ms, bool *moving);
// Both output rates in one write, read back; the ranges and filters are unchanged.
// Reconfigure on failure, as the write may have taken effect despite a failed read-back.
bool icm45686_set_rate(const icm45686_io_t *io, const icm45686_profile_t *profile, bool moving,
                       icm45686_stats_t *stats);
// A report was built from a snapshot: the next report covers what followed it.
void icm45686_window_snapshot(icm45686_stats_t *stats);
void icm45686_window_sent(icm45686_stats_t *stats);
#endif
