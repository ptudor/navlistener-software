#ifndef NVF_MOTION_H
#define NVF_MOTION_H
// The MAX board's ICM-45686 on the shared I2C bus: a task drains its FIFO on INT1, or
// after twice the watermark's time at the current rate, steps the rate with motion
// (icm45686.h), and the board task takes a summary for each ObserverDetails report
// (docs/OBSERVER-TELEMETRY.md, tag 13).
#include <stdbool.h>
#include <stdint.h>
#include "driver/i2c_master.h"
#include "esp_err.h"
#include "icm45686.h"

typedef struct {
    bool ready;              // configured and being serviced
    icm45686_stats_t stats;  // counters since boot, governor, latest sample, report window
    uint64_t latest_ms;      // uptime of the service that decoded the latest valid sample
} motion_status_t;

// Adds the IMU at 400 kHz, configures its interrupt inputs (push-pull outputs, so the
// inputs have only a weak pull-down) and starts the service task with the unit's
// profile. The IMU not answering is not an error; the task keeps configuring it.
esp_err_t motion_start(i2c_master_bus_handle_t bus, int int1_gpio, int int2_gpio,
                       const icm45686_profile_t *profile);
// A summary for a report. Samples after it form the next report's window once that
// report is queued; until then the current window stays open.
void motion_snapshot(motion_status_t *out);
void motion_report_sent(void);
#endif
