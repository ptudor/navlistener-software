#ifndef NVF_ICM45686_H
#define NVF_ICM45686_H
// ICM-45686 six-axis IMU (the MAX board's U37 at I2C 0x69), TDK DS-000577 Rev 1.0.
// Accelerometer and gyroscope run in low-noise mode at a fixed rate into the FIFO in
// stream mode; INT1 (GPIO16) signals the watermark or a full FIFO, INT2 (GPIO17) is
// driven but unused. Data stays in the part's default little-endian format.
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
    ICM45686_ODR_HZ = 100,
    ICM45686_ACCEL_FS_G = 8, ICM45686_ACCEL_LSB_PER_G = 4096,
    ICM45686_GYRO_FS_DPS = 1000, // 32.8 LSB per degree per second
    ICM45686_PACKET = 16,        // header, accel, gyro, 1-byte temperature, timestamp
    ICM45686_FIFO_PACKETS = 128, // 2 KB FIFO
    ICM45686_WATERMARK = 32,     // packets, 320 ms
};
#define ICM45686_GYRO_LSB_PER_DPS 32.8

// Extremes of the valid samples in a window, as squared magnitudes in counts.
typedef struct {
    bool valid;
    uint32_t accel_min_sq, accel_max_sq, gyro_max_sq;
} icm45686_window_t;
enum { ICM45686_WINDOW_REPORT, ICM45686_WINDOW_SNAPSHOT };
typedef struct {
    uint32_t packets;   // accelerometer-and-gyroscope packets decoded since boot
    uint32_t overflows; // FIFO-full events: stream mode overwrote the oldest packets
    uint32_t resyncs;   // FIFO flushes after an undecodable packet
    uint32_t latest_packet; // the packet count at the latest valid sample
    bool latest_valid;
    int16_t accel[3], gyro[3]; // the latest valid packet, raw counts in the sensor's axes
    int8_t temp;               // its FIFO temperature: C = temp / 2 + 25
    // Every valid sample updates both windows: the one a report covers, and the one
    // since the last snapshot, which becomes the report window once that report is sent.
    icm45686_window_t window[2];
} icm45686_stats_t;

// Soft reset, identification, push-pull active-high INT1/INT2, ranges and rates, the
// FIFO in stream mode, low-noise power, a start-up flush, then the INT1 sources. True
// only when the configuration reads back.
bool icm45686_configure(const icm45686_io_t *io);
// Reads INT1_STATUS0 and drains the FIFO; false on a transfer failure.
bool icm45686_service(const icm45686_io_t *io, icm45686_stats_t *stats);
// Decodes whole packets; returns the bytes consumed, which stops short of an
// undecodable header (the caller flushes the FIFO to realign).
size_t icm45686_parse(const uint8_t *fifo, size_t length, icm45686_stats_t *stats);
// A report was built from a snapshot: the next report covers what followed it.
void icm45686_window_snapshot(icm45686_stats_t *stats);
void icm45686_window_sent(icm45686_stats_t *stats);
#endif
