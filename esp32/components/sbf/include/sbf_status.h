// sbf_status: a mosaic receiver's SBF blocks into the observer's receiver status
// (gnss_status_t), the same structure the u-blox path fills, so the panel, reports, reception
// assessment and RTC read one receiver-neutral state. I/O-free.
//
// - MeasEpoch: a satellite is tracked when one of its signals on the main antenna has a
//   pseudorange and a C/N0 of at least 1 dB-Hz; its C/N0 is the strongest such signal's. Each
//   signal with a counterpart in the reception space is listed under that ID. Elevations come
//   from the latest SatVisibility, unknown (127) after 5 s. An epoch scrambled for want of the
//   Measurement Availability permission is not used.
// - PVTGeodetic: a GNSS position solution (sbf_pvt_t.position) is a fix.
// - ReceiverTime: the UTC calendar, written into pvt_utc in the NAV-PVT byte layout the RTC
//   policy reads, and only for the epoch of the PVT block just received. The date and time are
//   valid when given, and fully resolved once the receiver time is synchronized (WNSET, TOWSET,
//   FINETIME) and the leap-second count known. With FINETIME the receiver time is within the
//   clock-sync threshold of GNSS time, which the observer sets to 0.5 ms (setClockSyncThreshold),
//   so the time accuracy is 0.5 ms; with TOWSET alone, 20 ms (reference guide 2.3, 4.2.12).
// - RFStatus: interference and authenticity in the u-blox scales the status keeps. Jamming is
//   ok (1) with no interference detected; with interference detected, warning (2) while the
//   receiver holds a fix and critical (3) without one, as u-blox defines its states. Spoofing is
//   indicated (2) when either authenticity flag is set and not indicated (1) otherwise. A change
//   counts as an interference event, as on the u-blox path.
#ifndef NVF_SBF_STATUS_H
#define NVF_SBF_STATUS_H

#include "gnss_status.h"
#include "sbf.h"

#define SBF_STATUS_VISIBLE_MAX 128u
#define SBF_STATUS_ELEVATION_MS 5000
// The time accuracy of a FINETIME receiver time, in ns: the clock-sync threshold the observer sets.
#define SBF_STATUS_FINETIME_NS 500000u
#define SBF_STATUS_TOWSET_NS   20000000u

typedef struct { uint8_t gnss, sv; int8_t elevation; } sbf_visible_t;
typedef struct {
    sbf_visible_t visible[SBF_STATUS_VISIBLE_MAX];
    unsigned visible_count;
    int64_t visible_ms;
    bool visible_valid;
    bool pvt_seen, time_seen;
    uint32_t pvt_tow;
    sbf_time_t time;
    uint32_t scrambled_epochs; // MeasEpochs not used for want of the measurement permission
} sbf_status_t;

void sbf_status_init(sbf_status_t *a);
// Feeds one CRC-checked block received at now_ms. True when it changed the status.
bool sbf_status_feed(sbf_status_t *a, gnss_status_t *s, const uint8_t *block, size_t length, int64_t now_ms);
#endif
