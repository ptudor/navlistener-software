#include "rtc_max31328.h"

bool max31328_decode(const uint8_t r[7], int64_t *epoch)
{
    // Unused bits read zero on this part, and the century bit marks a calendar this
    // firmware never wrote as valid.
    if ((r[0] & 0x80) || (r[1] & 0x80) || (r[2] & 0x80) || (r[3] & 0xf8) || (r[4] & 0xc0) ||
        (r[5] & 0xe0)) return false;
    return rtc_decode(r, epoch);
}

bool max31328_encode(int64_t epoch, uint8_t r[7])
{
    if (!rtc_encode(epoch, r)) return false;
    r[3] &= 7; // day of week only; rtc_encode also sets the MCP79412's VBATEN
    return true;
}

bool max31328_running(const uint8_t r[7], uint8_t control, uint8_t status, int64_t *epoch)
{
    return !(control & MAX31328_EOSC) && !(status & MAX31328_OSF) && max31328_decode(r, epoch);
}

// Writing OSF, A2F or A1F as 1 leaves them unchanged; only 0 clears.
static uint8_t status_keeping_flags(uint8_t status) { return (status & ~0x70u) | MAX31328_A2F | MAX31328_A1F; }

static void invalidate(const rtc_io_t *io)
{
    uint8_t month;
    if (io->read(io->ctx, 5, &month, 1)) {
        month |= 0x80;
        (void)io->write(io->ctx, 5, &month, 1);
    }
}

static unsigned verified_read(const rtc_io_t *io, int64_t *epoch)
{
    uint8_t regs[7], state[2];
    if (!io->read(io->ctx, 0, regs, sizeof regs) || !io->read(io->ctx, MAX31328_CONTROL, state, 2))
        return MAX31328_SET_IO;
    if (state[1] & MAX31328_OSF) return MAX31328_SET_OSF_STUCK;
    return max31328_running(regs, state[0], state[1], epoch) ? MAX31328_SET_OK : MAX31328_SET_UNVERIFIED;
}

unsigned max31328_set_verified(const rtc_io_t *io, int64_t epoch)
{
    uint8_t regs[7], state[2];
    if (!max31328_encode(epoch, regs)) return MAX31328_SET_UNVERIFIED;
    if (!io->read(io->ctx, MAX31328_CONTROL, state, 2)) return MAX31328_SET_IO;
    unsigned result = MAX31328_SET_IO;
    // The oscillator must stay enabled on the backup cell.
    uint8_t control = state[0] & ~MAX31328_EOSC, status = status_keeping_flags(state[1]) & ~MAX31328_OSF;
    // One burst: writing the seconds restarts the countdown chain, and the rest must
    // follow within the second.
    if ((control != state[0] && !io->write(io->ctx, MAX31328_CONTROL, &control, 1)) ||
        !io->write(io->ctx, 0, regs, sizeof regs) ||
        !io->write(io->ctx, MAX31328_STATUS, &status, 1)) goto failed;
    int64_t first, later;
    if ((result = verified_read(io, &first)) != MAX31328_SET_OK) goto failed;
    if (first < epoch || first > epoch + 3) { result = MAX31328_SET_UNVERIFIED; goto failed; }
    io->delay(io->ctx, 1200);
    if ((result = verified_read(io, &later)) != MAX31328_SET_OK) goto failed;
    if (later > first && later <= first + 3) return MAX31328_SET_OK;
    result = MAX31328_SET_UNVERIFIED;
failed:
    invalidate(io);
    return result;
}

unsigned max31328_square_wave(const rtc_io_t *io, uint8_t *control, uint8_t *aging)
{
    uint8_t regs[7], state[3];
    int64_t epoch;
    if (!io->read(io->ctx, 0, regs, sizeof regs) || !io->read(io->ctx, MAX31328_CONTROL, state, 3)) return 4;
    *control = state[0]; *aging = state[2];
    if (!max31328_running(regs, state[0], state[1], &epoch)) return 2;
    if (state[0] & (MAX31328_A2IE | MAX31328_A1IE)) return 3;
    // No conversion request, no battery-backed output, 1 Hz, square wave rather than alarms.
    uint8_t desired = state[0] & ~(MAX31328_EOSC | MAX31328_BBSQW | MAX31328_CONV | MAX31328_RS | MAX31328_INTCN);
    uint8_t status = status_keeping_flags(state[1]) & ~MAX31328_EN32KHZ;
    if (desired != state[0] && !io->write(io->ctx, MAX31328_CONTROL, &desired, 1)) return 4;
    if ((state[1] & MAX31328_EN32KHZ) && !io->write(io->ctx, MAX31328_STATUS, &status, 1)) return 4;
    if (!io->read(io->ctx, MAX31328_CONTROL, state, 3)) return 4;
    *control = state[0]; *aging = state[2];
    return state[0] == desired && !(state[1] & MAX31328_EN32KHZ) ? 1 : 4;
}
