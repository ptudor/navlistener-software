#ifndef RTK_STAT_H
#define RTK_STAT_H
#include <stdbool.h>
#include <stdint.h>
// The u-blox RTK_STAT pin (ZED-X20P integration manual UBXDOC-963802114-12901, section 2.4.7,
// Table 24): low once RTK fixed mode is achieved, alternating while a valid stream of
// corrections is received and used without a fix, high while no carrier-phase solution is
// available. The panel's RTK lamp follows the same samples; this names what they show.
#define RTK_STAT_WINDOW_MS 10000
typedef enum { RTK_STAT_UNKNOWN, RTK_STAT_HIGH, RTK_STAT_LOW, RTK_STAT_TOGGLING } rtk_stat_state_t;
typedef struct {
    bool started, level;     // level: the latest sample, true for high
    int64_t window_ms;       // when the current window began
    uint32_t changes;        // level changes in the current window
    rtk_stat_state_t state;  // the last closed window's classification
    uint32_t window_changes; // that window's changes
} rtk_stat_t;
// One sample of the pin, true for high. A window closes on the first sample at least
// RTK_STAT_WINDOW_MS after it began and is classified: two or more changes are toggling,
// otherwise the level it ends at. True when that differs from the previous window's
// classification, the first window's included.
bool rtk_stat_sample(rtk_stat_t *s, bool high, int64_t now_ms);
// "high", "low", "toggling" or "unknown".
const char *rtk_stat_name(rtk_stat_state_t state);
// What the receiver means by it, from Table 24.
const char *rtk_stat_meaning(rtk_stat_state_t state);
#endif
