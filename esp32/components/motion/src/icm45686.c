#include "icm45686.h"
#include <string.h>

enum {
    REG_PWR_MGMT0 = 0x10, REG_FIFO_COUNT = 0x12, REG_FIFO_DATA = 0x14,
    REG_INT1_CONFIG0 = 0x16, REG_INT1_CONFIG2 = 0x18, REG_INT1_STATUS0 = 0x19,
    REG_ACCEL_CONFIG0 = 0x1b, REG_GYRO_CONFIG0 = 0x1c, REG_FIFO_CONFIG0 = 0x1d,
    REG_FIFO_CONFIG1_0 = 0x1e, REG_FIFO_CONFIG1_1 = 0x1f, REG_FIFO_CONFIG2 = 0x20,
    REG_FIFO_CONFIG3 = 0x21, REG_FIFO_CONFIG4 = 0x22,
    REG_INT2_CONFIG0 = 0x56, REG_INT2_CONFIG2 = 0x58, REG_WHO_AM_I = 0x72, REG_MISC2 = 0x7f,

    MISC2_SOFT_RST = 0x02,
    // Push-pull, active high (DS-000577 17.23 and 17.76); INT1 latched until its status is read.
    INT1_PUSH_PULL_LATCHED_HIGH = 0x03, INT2_PUSH_PULL_PULSE_HIGH = 0x01,
    INT1_FIFO_THS_AND_FULL = 0x03, INT1_STATUS_FIFO_FULL = 0x01,
    ACCEL_8G_100HZ = 0x29, GYRO_1000DPS_100HZ = 0x29, // FS_SEL 010 / 0010, ODR 1001
    FIFO_BYPASS_2K = 0x07, FIFO_STREAM_2K = 0x47,
    FIFO_CONFIG2_RESERVED = 0x20, FIFO_WM_GE = 0x08, FIFO_FLUSH = 0x80,
    FIFO_ACCEL_GYRO_IF = 0x07,
    PWR_ACCEL_GYRO_LN = 0x0f,

    HEADER_EXT = 0x80, HEADER_ACCEL = 0x40, HEADER_GYRO = 0x20, HEADER_HIRES = 0x10,
    RESET_POLLS = 10, GYRO_STARTUP_MS = 50, // 35 ms gyroscope start-up, with margin
    READ_PACKETS = 16,                      // bytes per FIFO transaction: 256
};

static bool put(const icm45686_io_t *io, uint8_t reg, uint8_t value) { return io->write(io->ctx, reg, &value, 1); }
static bool get(const icm45686_io_t *io, uint8_t reg, uint8_t *value) { return io->read(io->ctx, reg, value, 1); }

bool icm45686_configure(const icm45686_io_t *io)
{
    uint8_t v;
    if (!get(io, REG_WHO_AM_I, &v) || v != ICM45686_WHO_AM_I || !put(io, REG_MISC2, MISC2_SOFT_RST)) return false;
    for (unsigned i = 0; ; i++) {
        io->delay_ms(io->ctx, 1);
        if (get(io, REG_MISC2, &v) && !(v & MISC2_SOFT_RST)) break;
        if (i == RESET_POLLS) return false;
    }
    // Interrupt drive and mode change only while every interrupt source is disabled.
    static const uint8_t setup[][2] = {
        {REG_INT1_CONFIG0, 0}, {REG_INT2_CONFIG0, 0},
        {REG_INT1_CONFIG2, INT1_PUSH_PULL_LATCHED_HIGH}, {REG_INT2_CONFIG2, INT2_PUSH_PULL_PULSE_HIGH},
        {REG_ACCEL_CONFIG0, ACCEL_8G_100HZ}, {REG_GYRO_CONFIG0, GYRO_1000DPS_100HZ},
        // Depth and the watermark condition change only in bypass mode; the threshold
        // takes effect when its high byte is written.
        {REG_FIFO_CONFIG3, 0}, {REG_FIFO_CONFIG0, FIFO_BYPASS_2K},
        {REG_FIFO_CONFIG2, FIFO_CONFIG2_RESERVED | FIFO_WM_GE},
        {REG_FIFO_CONFIG1_0, ICM45686_WATERMARK & 0xff}, {REG_FIFO_CONFIG1_1, ICM45686_WATERMARK >> 8},
        {REG_FIFO_CONFIG4, 0},
        // Enable the FIFO, then the sensor-register interface into it.
        {REG_FIFO_CONFIG0, FIFO_STREAM_2K}, {REG_FIFO_CONFIG3, FIFO_ACCEL_GYRO_IF},
        {REG_PWR_MGMT0, PWR_ACCEL_GYRO_LN},
    };
    for (size_t i = 0; i < sizeof setup / sizeof setup[0]; i++)
        if (!put(io, setup[i][0], setup[i][1])) return false;
    // Discard what was queued while the gyroscope started, then clear stale status.
    io->delay_ms(io->ctx, GYRO_STARTUP_MS);
    if (!put(io, REG_FIFO_CONFIG2, FIFO_CONFIG2_RESERVED | FIFO_WM_GE | FIFO_FLUSH) ||
        !get(io, REG_INT1_STATUS0, &v) || !put(io, REG_INT1_CONFIG0, INT1_FIFO_THS_AND_FULL))
        return false;
    uint8_t rates[2], fifo, interface, power, int1[3];
    return io->read(io->ctx, REG_ACCEL_CONFIG0, rates, 2) && rates[0] == ACCEL_8G_100HZ &&
           rates[1] == GYRO_1000DPS_100HZ && get(io, REG_FIFO_CONFIG0, &fifo) && fifo == FIFO_STREAM_2K &&
           get(io, REG_FIFO_CONFIG3, &interface) && interface == FIFO_ACCEL_GYRO_IF &&
           get(io, REG_PWR_MGMT0, &power) && power == PWR_ACCEL_GYRO_LN &&
           io->read(io->ctx, REG_INT1_CONFIG0, int1, 3) && int1[0] == INT1_FIFO_THS_AND_FULL &&
           int1[2] == INT1_PUSH_PULL_LATCHED_HIGH;
}

static int16_t le16(const uint8_t *p) { return (int16_t)(p[0] | p[1] << 8); }
static uint32_t square(const int16_t v[3])
{
    return (uint32_t)((int32_t)v[0] * v[0]) + (uint32_t)((int32_t)v[1] * v[1]) + (uint32_t)((int32_t)v[2] * v[2]);
}

void icm45686_window_snapshot(icm45686_stats_t *s)
{
    s->window[ICM45686_WINDOW_SNAPSHOT] = (icm45686_window_t){0};
}

void icm45686_window_sent(icm45686_stats_t *s)
{
    s->window[ICM45686_WINDOW_REPORT] = s->window[ICM45686_WINDOW_SNAPSHOT];
}

static void extend(icm45686_window_t *w, uint32_t a, uint32_t g)
{
    if (!w->valid) {
        *w = (icm45686_window_t){.valid = true, .accel_min_sq = a, .accel_max_sq = a, .gyro_max_sq = g};
        return;
    }
    if (a < w->accel_min_sq) w->accel_min_sq = a;
    if (a > w->accel_max_sq) w->accel_max_sq = a;
    if (g > w->gyro_max_sq) w->gyro_max_sq = g;
}

size_t icm45686_parse(const uint8_t *fifo, size_t length, icm45686_stats_t *s)
{
    size_t offset = 0;
    while (length - offset >= ICM45686_PACKET) {
        const uint8_t *p = fifo + offset;
        // Only the configured format: accelerometer and gyroscope, 16-bit, no extension.
        if ((p[0] & (HEADER_EXT | HEADER_ACCEL | HEADER_GYRO | HEADER_HIRES)) != (HEADER_ACCEL | HEADER_GYRO)) break;
        offset += ICM45686_PACKET;
        s->packets++;
        int16_t accel[3], gyro[3];
        bool valid = true;
        for (unsigned axis = 0; axis < 3; axis++) {
            accel[axis] = le16(p + 1 + 2 * axis);
            gyro[axis] = le16(p + 7 + 2 * axis);
            // -32768 marks an axis without data, such as a sensor still starting.
            valid &= accel[axis] != INT16_MIN && gyro[axis] != INT16_MIN;
        }
        if (!valid) continue;
        memcpy(s->accel, accel, sizeof accel);
        memcpy(s->gyro, gyro, sizeof gyro);
        s->temp = (int8_t)p[13];
        s->latest_valid = true;
        s->latest_packet = s->packets;
        uint32_t a = square(accel), g = square(gyro);
        extend(&s->window[ICM45686_WINDOW_REPORT], a, g);
        extend(&s->window[ICM45686_WINDOW_SNAPSHOT], a, g);
    }
    return offset;
}

bool icm45686_service(const icm45686_io_t *io, icm45686_stats_t *s)
{
    uint8_t status, count[2];
    if (!get(io, REG_INT1_STATUS0, &status) || !io->read(io->ctx, REG_FIFO_COUNT, count, 2)) return false;
    if (status & INT1_STATUS_FIFO_FULL) s->overflows++;
    unsigned packets = (unsigned)(count[0] | count[1] << 8);
    if (packets > ICM45686_FIFO_PACKETS) packets = ICM45686_FIFO_PACKETS;
    uint8_t buffer[READ_PACKETS * ICM45686_PACKET];
    while (packets) {
        unsigned n = packets < READ_PACKETS ? packets : READ_PACKETS;
        size_t bytes = (size_t)n * ICM45686_PACKET;
        if (!io->read(io->ctx, REG_FIFO_DATA, buffer, bytes)) return false;
        if (icm45686_parse(buffer, bytes, s) != bytes) {
            // Out of step with the packet boundaries: start again from an empty FIFO.
            s->resyncs++;
            return put(io, REG_FIFO_CONFIG2, FIFO_CONFIG2_RESERVED | FIFO_WM_GE | FIFO_FLUSH);
        }
        packets -= n;
    }
    return true;
}
