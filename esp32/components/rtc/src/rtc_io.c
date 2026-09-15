#include "rtc_io.h"
unsigned rtc_square_wave(const rtc_io_t *io, uint8_t *control, uint8_t *trim)
{
    uint8_t calendar[7], settings[2]; int64_t epoch;
    if (!io->read(io->ctx,0,calendar,sizeof calendar) || !io->read(io->ctx,7,settings,2)) return 4;
    *control=settings[0]; *trim=settings[1];
    if (!rtc_running(calendar,&epoch)) return 2;
    // Do not take over an active alarm or change coarse calibration behavior.
    if (*control & 0x34) return 3;
    uint8_t desired=(*control & ~3u) | 0x40;
    if (desired != *control && !io->write(io->ctx,7,&desired,1)) return 4;
    if (!io->read(io->ctx,7,settings,2)) return 4;
    *control=settings[0]; *trim=settings[1];
    return *control == desired ? 1 : 4;
}
static bool preserve_power_failure(const rtc_io_t *io, const uint8_t calendar[7])
{
    if (!(calendar[3] & 0x10)) return true;
    uint8_t stamps[8];
    return io->save_power_failure && io->read(io->ctx, 0x18, stamps, sizeof stamps) &&
           io->save_power_failure(io->ctx, calendar, stamps);
}
static bool wait_oscillator(const rtc_io_t *io, bool running)
{
    for (unsigned i = 0; i < 100; i++) {
        uint8_t weekday;
        if (!io->read(io->ctx, 3, &weekday, 1)) return false;
        if (!!(weekday & 0x20) == running) return true;
        io->delay(io->ctx, 20);
    }
    return false;
}
bool rtc_enable_backup(const rtc_io_t *io)
{
    uint8_t calendar[7];
    if (!io->read(io->ctx, 0, calendar, sizeof calendar)) return false;
    uint8_t weekday = calendar[3];
    if (weekday & 8) return true;
    if (!preserve_power_failure(io, calendar)) return false;
    weekday |= 8; // any RTCWKDAY write clears PWRFAIL, regardless of written bit
    if (!io->write(io->ctx, 3, &weekday, 1) || !io->read(io->ctx, 3, &weekday, 1)) return false;
    return (weekday & 8) != 0;
}
bool rtc_set_verified(const rtc_io_t *io, int64_t epoch)
{
    uint8_t old[8], regs[7];
    if (!io->read(io->ctx, 0, old, sizeof old) || !rtc_encode(epoch, regs) ||
        !preserve_power_failure(io, old)) return false;
    // MCP79412 has a crystal on this board. Stop either possible clock source,
    // then wait for OSCRUN to clear before changing calendar registers.
    uint8_t stopped = old[0] & 0x7f, control = old[7] & ~8;
    if (!io->write(io->ctx, 0, &stopped, 1) ||
        !io->write(io->ctx, 7, &control, 1) || !wait_oscillator(io, false)) goto failed;
    if (!io->write(io->ctx, 0, regs, sizeof regs)) goto failed;
    regs[0] |= 0x80;
    if (!io->write(io->ctx, 0, regs, 1) || !wait_oscillator(io, true)) goto failed;
    // Read all seven buffered registers together; also prove that seconds advance.
    uint8_t readback[7]; int64_t first, later;
    if (!io->read(io->ctx, 0, readback, sizeof readback) ||
        !rtc_running(readback, &first) || !(readback[3] & 8) || first < epoch || first > epoch + 3) goto failed;
    io->delay(io->ctx, 1200);
    if (io->read(io->ctx, 0, readback, sizeof readback) && rtc_running(readback, &later) &&
        (readback[3] & 8) && later > first && later <= first + 3) return true;
failed:
    // Best effort: a partially written or unverified calendar must not look
    // like a retained running clock on the next boot. A dead bus can prevent
    // even this cleanup; the caller must continue to report it as unconfirmed.
    (void)io->write(io->ctx, 0, &stopped, 1);
    return false;
}
