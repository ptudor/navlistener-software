#include "icm45686.h"
#include <string.h>

enum {
    REG_PWR_MGMT0 = 0x10, REG_FIFO_COUNT = 0x12, REG_FIFO_DATA = 0x14,
    REG_INT1_CONFIG0 = 0x16, REG_INT1_CONFIG2 = 0x18, REG_INT1_STATUS0 = 0x19,
    REG_ACCEL_CONFIG0 = 0x1b, REG_GYRO_CONFIG0 = 0x1c, REG_FIFO_CONFIG0 = 0x1d,
    REG_FIFO_CONFIG1_0 = 0x1e, REG_FIFO_CONFIG1_1 = 0x1f, REG_FIFO_CONFIG2 = 0x20,
    REG_FIFO_CONFIG3 = 0x21, REG_FIFO_CONFIG4 = 0x22,
    REG_INT2_CONFIG0 = 0x56, REG_INT2_CONFIG2 = 0x58, REG_WHO_AM_I = 0x72,
    REG_IREG_ADDR_15_8 = 0x7c, REG_IREG_DATA = 0x7e, REG_MISC2 = 0x7f,

    MISC2_SOFT_RST = 0x02, MISC2_IREG_DONE = 0x01,
    // Indirect registers (section 14): IPREG_SYS1 at 0xA400, IPREG_SYS2 at 0xA500. The
    // UI low-pass bandwidth is bits 2:0 of each; 001 is ODR/4.
    IREG_GYRO_UI_LPF = 0xa4ac, IREG_ACCEL_UI_LPF = 0xa583, LPF_MASK = 0x07, LPF_ODR_4 = 0x01,
    IREG_POLLS = 10,
    // Push-pull, active high (DS-000577 17.23 and 17.76); INT1 latched until its status is read.
    INT1_PUSH_PULL_LATCHED_HIGH = 0x03, INT2_PUSH_PULL_PULSE_HIGH = 0x01,
    INT1_FIFO_THS_AND_FULL = 0x03, INT1_STATUS_FIFO_FULL = 0x01,
    FIFO_BYPASS_2K = 0x07, FIFO_STREAM_2K = 0x47,
    FIFO_CONFIG2_RESERVED = 0x20, FIFO_WM_GE = 0x08, FIFO_FLUSH = 0x80,
    FIFO_ACCEL_GYRO_IF = 0x07,
    PWR_ACCEL_GYRO_LN = 0x0f,

    HEADER_EXT = 0x80, HEADER_ACCEL = 0x40, HEADER_GYRO = 0x20, HEADER_HIRES = 0x10,
    HEADER_ACCEL_ODR = 0x02, // the first accelerometer packet at a new rate
    ODR_12_5_HZ = 0x0c, ODR_50_HZ = 0x0a, ODR_100_HZ = 0x09,
    RESET_POLLS = 10, GYRO_STARTUP_MS = 50, // 35 ms gyroscope start-up, with margin
    READ_PACKETS = 16,                      // bytes per FIFO transaction: 256
    REFERENCE_TAU_MS = 2000,                // the gravity reference's time constant
};

// Accelerometer FS_SEL 010 is 8 g and 001 is 16 g; gyroscope FS_SEL 0010 is 1000 dps and
// 0001 is 2000 dps; ODR 1100 is 12.5 Hz, 1010 is 50 Hz and 1001 is 100 Hz.
const icm45686_profile_t ICM45686_SURFACE = {.accel_fs_g = 8, .accel_fs_sel = 2, .gyro_fs_dps = 1000,
    .gyro_fs_sel = 2, .moving_decihz = 500, .moving_odr = ODR_50_HZ};
const icm45686_profile_t ICM45686_AERIAL = {.accel_fs_g = 16, .accel_fs_sel = 1, .gyro_fs_dps = 2000,
    .gyro_fs_sel = 1, .moving_decihz = 1000, .moving_odr = ODR_100_HZ};

uint16_t icm45686_period_ms(uint16_t decihz) { return decihz ? (uint16_t)(10000 / decihz) : 0; }

// ACCEL_CONFIG0 and GYRO_CONFIG0: FS_SEL << 4 | ODR.
static void config0(const icm45686_profile_t *p, bool moving, uint8_t out[2])
{
    uint8_t odr = moving ? p->moving_odr : ODR_12_5_HZ;
    out[0] = (uint8_t)(p->accel_fs_sel << 4 | odr);
    out[1] = (uint8_t)(p->gyro_fs_sel << 4 | odr);
}

void icm45686_stats_start(icm45686_stats_t *s, const icm45686_profile_t *p)
{
    // 1 g is 32768/range counts; 5 dps is 5 x 32768/range counts.
    double g = 32768.0 / p->accel_fs_g, rate = 5.0 * 32768.0 / p->gyro_fs_dps;
    s->accel_low_sq = (uint32_t)(0.9 * g * 0.9 * g);
    s->accel_high_sq = (uint32_t)(1.1 * g * 1.1 * g);
    s->gyro_motion_sq = (uint32_t)(rate * rate);
    s->reference_motion_sq = (int64_t)(0.1 * g * 256 * 0.1 * g * 256);
    s->reference_valid = false;
    s->moving = false;
    s->motion_seen = false;
    s->period_ms = icm45686_period_ms(ICM45686_STILL_DECIHZ);
    s->pending_period_ms = 0;
    s->latest_valid = false;
}

static bool put(const icm45686_io_t *io, uint8_t reg, uint8_t value) { return io->write(io->ctx, reg, &value, 1); }
static bool get(const icm45686_io_t *io, uint8_t reg, uint8_t *value) { return io->read(io->ctx, reg, value, 1); }

static bool flush_fifo(const icm45686_io_t *io)
{
    if (!put(io, REG_FIFO_CONFIG2, FIFO_CONFIG2_RESERVED | FIFO_WM_GE | FIFO_FLUSH)) return false;
    // Flush is asynchronous. TDK's inv_imu_flush_fifo waits for the bit to clear;
    // a stuck flush must not leave the task accepting data from an unknown boundary.
    for (unsigned i = 0; i < 100; i++) {
        uint8_t value;
        io->delay_ms(io->ctx, 1);
        if (!get(io, REG_FIFO_CONFIG2, &value)) return false;
        if (!(value & FIFO_FLUSH)) return true;
    }
    return false;
}

// An indirect access may start only once the previous one has finished; the delay also
// covers the 4 us minimum gap between accesses.
static bool ireg_idle(const icm45686_io_t *io)
{
    for (unsigned i = 0; i < IREG_POLLS; i++) {
        uint8_t v;
        if (!get(io, REG_MISC2, &v)) return false;
        if (v & MISC2_IREG_DONE) return true;
        io->delay_ms(io->ctx, 1);
    }
    return false;
}
// The address and the data in one burst, so no read pre-fetch is triggered between them.
static bool ireg_write(const icm45686_io_t *io, uint16_t address, uint8_t value)
{
    const uint8_t burst[3] = {address >> 8, address & 0xff, value};
    if (!ireg_idle(io) || !io->write(io->ctx, REG_IREG_ADDR_15_8, burst, sizeof burst)) return false;
    io->delay_ms(io->ctx, 1);
    return true;
}
// Writing the address starts a pre-fetch into IREG_DATA.
static bool ireg_read(const icm45686_io_t *io, uint16_t address, uint8_t *value)
{
    const uint8_t pointer[2] = {address >> 8, address & 0xff};
    if (!ireg_idle(io) || !io->write(io->ctx, REG_IREG_ADDR_15_8, pointer, sizeof pointer)) return false;
    io->delay_ms(io->ctx, 1);
    return ireg_idle(io) && get(io, REG_IREG_DATA, value);
}
// Sets a UI low-pass selection, keeping the register's other bits, and reads it back.
static bool set_lowpass(const icm45686_io_t *io, uint16_t address)
{
    uint8_t v;
    if (!ireg_read(io, address, &v)) return false;
    uint8_t wanted = (uint8_t)((v & ~LPF_MASK) | LPF_ODR_4);
    return ireg_write(io, address, wanted) && ireg_read(io, address, &v) && v == wanted;
}

bool icm45686_configure(const icm45686_io_t *io, const icm45686_profile_t *profile)
{
    if (!profile) return false;
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
    uint8_t still[2];
    config0(profile, false, still);
    if (!io->write(io->ctx, REG_ACCEL_CONFIG0, still, 2)) return false;
    for (size_t i = 0; i < sizeof setup / sizeof setup[0]; i++)
        if (!put(io, setup[i][0], setup[i][1])) return false;
    // The filters are in the main-clock domain, which runs once the sensors are on.
    if (!set_lowpass(io, IREG_GYRO_UI_LPF) || !set_lowpass(io, IREG_ACCEL_UI_LPF)) return false;
    // Discard what was queued while the gyroscope started and the filters changed, then
    // clear stale status.
    io->delay_ms(io->ctx, GYRO_STARTUP_MS);
    if (!flush_fifo(io) ||
        !get(io, REG_INT1_STATUS0, &v) || !put(io, REG_INT1_CONFIG0, INT1_FIFO_THS_AND_FULL))
        return false;
    uint8_t rates[2], fifo, interface, power, int1[3];
    return io->read(io->ctx, REG_ACCEL_CONFIG0, rates, 2) && rates[0] == still[0] &&
           rates[1] == still[1] && get(io, REG_FIFO_CONFIG0, &fifo) && fifo == FIFO_STREAM_2K &&
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

static void extend(icm45686_window_t *w, const int16_t accel[3], const int16_t gyro[3], uint32_t a, uint32_t g,
                   uint16_t period_ms)
{
    if (!w->valid) {
        *w = (icm45686_window_t){.valid = true, .accel_min_sq = a, .accel_max_sq = a, .gyro_max_sq = g};
    } else {
        if (a < w->accel_min_sq) w->accel_min_sq = a;
        if (a > w->accel_max_sq) w->accel_max_sq = a;
        if (g > w->gyro_max_sq) w->gyro_max_sq = g;
    }
    w->samples++;
    w->span_ms += period_ms;
    for (unsigned axis = 0; axis < 3; axis++) {
        w->accel_sum[axis] += (int64_t)accel[axis] * period_ms;
        w->gyro_sum[axis] += (int64_t)gyro[axis] * period_ms;
    }
}

// Compares a sample with the tracked gravity vector, then moves the vector toward it by
// the sample's share of the time constant. False when the sample is 0.1 g or more away.
static bool follow_gravity(icm45686_stats_t *s, const int16_t accel[3])
{
    if (!s->reference_valid) {
        for (unsigned axis = 0; axis < 3; axis++) s->reference[axis] = accel[axis] * 256;
        s->reference_valid = true;
        return true;
    }
    int64_t distance = 0;
    for (unsigned axis = 0; axis < 3; axis++) {
        int64_t d = (int64_t)accel[axis] * 256 - s->reference[axis];
        distance += d * d;
        s->reference[axis] += (int32_t)(d * s->period_ms / REFERENCE_TAU_MS);
    }
    return distance <= s->reference_motion_sq;
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
        if ((p[0] & HEADER_ACCEL_ODR) && s->pending_period_ms) {
            s->period_ms = s->pending_period_ms;
            s->pending_period_ms = 0;
        }
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
        if (a < s->accel_low_sq || a > s->accel_high_sq || g > s->gyro_motion_sq ||
            !follow_gravity(s, accel))
            s->motion_seen = true;
        extend(&s->window[ICM45686_WINDOW_REPORT], accel, gyro, a, g, s->period_ms);
        extend(&s->window[ICM45686_WINDOW_SNAPSHOT], accel, gyro, a, g, s->period_ms);
    }
    return offset;
}

bool icm45686_service(const icm45686_io_t *io, icm45686_stats_t *s)
{
    s->motion_seen = false;
    uint8_t status, count[2];
    if (!get(io, REG_INT1_STATUS0, &status) ||
        !io->read(io->ctx, REG_FIFO_COUNT, count, 2) || !io->read(io->ctx, REG_FIFO_COUNT, count, 2)) return false;
    if (status & INT1_STATUS_FIFO_FULL) s->overflows++;
    unsigned packets = (unsigned)(count[0] | count[1] << 8);
    // AN-000364, as implemented in TDK's reference driver: use the second count
    // read, and leave the newest frame in the FIFO when using stream mode.
    if (packets > ICM45686_FIFO_PACKETS) return false;
    if (packets) packets--;
    uint8_t buffer[READ_PACKETS * ICM45686_PACKET];
    while (packets) {
        unsigned n = packets < READ_PACKETS ? packets : READ_PACKETS;
        size_t bytes = (size_t)n * ICM45686_PACKET;
        if (!io->read(io->ctx, REG_FIFO_DATA, buffer, bytes)) return false;
        if (icm45686_parse(buffer, bytes, s) != bytes) {
            // Out of step with the packet boundaries: start again from an empty FIFO. Any
            // rate change is in force for everything queued after the flush.
            s->resyncs++;
            if (s->pending_period_ms) { s->period_ms = s->pending_period_ms; s->pending_period_ms = 0; }
            return flush_fifo(io);
        }
        packets -= n;
    }
    return true;
}

bool icm45686_rate_due(icm45686_stats_t *s, int64_t now_ms, bool *moving)
{
    if (s->motion_seen) s->last_motion_ms = now_ms;
    if (s->motion_seen && !s->moving) { *moving = true; return true; }
    if (s->moving && !s->motion_seen && now_ms - s->last_motion_ms >= ICM45686_QUIET_MS) { *moving = false; return true; }
    return false;
}

bool icm45686_set_rate(const icm45686_io_t *io, const icm45686_profile_t *profile, bool moving, icm45686_stats_t *s)
{
    // One write, so the two sensors never run at different rates.
    uint8_t wanted[2], actual[2];
    config0(profile, moving, wanted);
    if (!io->write(io->ctx, REG_ACCEL_CONFIG0, wanted, 2) || !io->read(io->ctx, REG_ACCEL_CONFIG0, actual, 2) ||
        actual[0] != wanted[0] || actual[1] != wanted[1])
        return false;
    s->moving = moving;
    s->rate_changes++;
    s->pending_period_ms = icm45686_period_ms(moving ? profile->moving_decihz : ICM45686_STILL_DECIHZ);
    return true;
}
