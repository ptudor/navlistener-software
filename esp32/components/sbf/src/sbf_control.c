#include "sbf_control.h"
#include "sbf.h"
#include <stdio.h>
#include <string.h>

static const char *const commands[] = {
    "scs, COM2, baud115200, bits8, No, bit1, RTS|CTS\r",
    "sdio, COM2, CMD, SBF\r",
    "gnt\r",
    "scst, usec500\r",
    "spps, , , , , , 100\r",
    "sso, Res1, COM2, PVTGeodetic+ReceiverTime+MeasEpoch+SatVisibility+ReceiverStatus+RFStatus, sec1\r",
    "sso, Res2, COM2, ReceiverSetup, OnChange\r",
    "sso, Res3, COM2, GPSRawCA+GPSRawL2C+GPSRawL5+GLORawCA+GALRawFNAV+GALRawINAV+GEORawL1+BDSRaw+BDSRawB2a+QZSRawL1CA+QZSRawL2C+QZSRawL5, OnChange\r",
    "gst\r",
};
enum { STEP_FLOW_CONTROL = 0, STEP_SIGNALS = 2, STEP_TRACKING = 8, STEPS = sizeof commands / sizeof commands[0] };

// The signals setSignalTracking names on a mosaic-G5 (reference guide 3.2), by signal number
// (4.1.10), in the command's order. A missing one is asked for in this order.
static const uint8_t wanted[] = {
    0, 2, 3, 4, 5,                 // GPS L1C/A, L2P(Y), L2C, L5, L1C
    8, 10, 11, 12,                 // GLONASS L1C/A, L2P, L2C/A, L3
    17, 19, 20, 21, 22,            // Galileo E1, E6, E5a, E5b, E5 AltBOC
    24, 25,                        // SBAS L1, L5
    28, 29, 30, 13, 14, 34,        // BeiDou B1I, B2I, B3I, B1C, B2a, B2b
    6, 7, 26, 27, 32, 33, 38,      // QZSS L1C/A, L2C, L5, L6, L1C, L1S, L1C/B
    15,                            // NavIC L5
};

bool sbf_control_forwarded(uint16_t number)
{
    switch (number) {
    case SBF_PVT_GEODETIC: case SBF_RECEIVER_TIME: case SBF_MEAS_EPOCH: case SBF_SAT_VISIBILITY:
    case SBF_RECEIVER_STATUS: case SBF_RF_STATUS: case SBF_RECEIVER_SETUP:
    case SBF_GPS_RAW_CA: case SBF_GPS_RAW_L2C: case SBF_GPS_RAW_L5: case SBF_GLO_RAW_CA:
    case SBF_GAL_RAW_FNAV: case SBF_GAL_RAW_INAV: case SBF_GEO_RAW_L1: case SBF_BDS_RAW:
    case SBF_BDS_RAW_B2A: case SBF_QZS_RAW_L1CA: case SBF_QZS_RAW_L2C: case SBF_QZS_RAW_L5:
        return true;
    default:
        return false;
    }
}
static const char escape[] = "SSSSSSSSSS\r";

size_t sbf_control_commands(const char *const **list) { *list = commands; return STEPS; }

uint8_t sbf_control_constellations(const char *text)
{
    // Gnn, Rnn, Enn, Snnn, Cnn, Jnn and Inn name GPS, GLONASS, Galileo, SBAS, BeiDou, QZSS and
    // NavIC satellites (setSatelliteTracking).
    static const char letters[] = "GSEC.JRI"; // indexed by UBX gnssId; IMES (4) has none
    uint8_t mask = 0;
    for (const char *p = text; *p; p++) {
        const char *hit = p[1] >= '0' && p[1] <= '9' && *p != '.' ? strchr(letters, *p) : NULL;
        if (!hit || (p != text && ((p[-1] >= 'A' && p[-1] <= 'Z') || (p[-1] >= 'a' && p[-1] <= 'z')))) continue;
        mask |= (uint8_t)(1u << (hit - letters));
    }
    return mask;
}

static bool word(char ch) { return (ch >= '0' && ch <= '9') || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z'); }
uint64_t sbf_control_signals(const char *text)
{
    // Whole words only: "SignalTracking" and "none" name no signal, and neither does part of a name.
    uint64_t mask = 0;
    for (const char *p = text; *p;) {
        size_t n = 0;
        while (word(p[n])) n++;
        if (!n) { p++; continue; }
        const int number = sbf_signal_number(p, n);
        if (number >= 0) mask |= 1ull << number;
        p += n;
    }
    return mask;
}

uint64_t sbf_control_wanted(void)
{
    uint64_t mask = 0;
    for (size_t i = 0; i < sizeof wanted; i++) mask |= 1ull << wanted[i];
    return mask;
}

// The next wanted signal that is neither tracked nor already asked for in vain, as the command
// that adds it. False when there is none.
static bool next_signal(sbf_control_t *c)
{
    for (size_t i = 0; i < sizeof wanted; i++) {
        if ((c->signals | c->signals_refused) >> wanted[i] & 1) continue;
        c->add = wanted[i];
        snprintf(c->command, sizeof c->command, "snt, +%s\r", sbf_signal_name(wanted[i]));
        return true;
    }
    return false;
}
static const char *command(const sbf_control_t *c) { return c->adding ? c->command : commands[c->step]; }

static void configure(sbf_control_t *c, int64_t now)
{
    c->state = SBF_CONTROL_CONFIGURING;
    c->step = c->tries = 0;
    c->sent = c->escaping = c->escape_next = c->collecting = c->adding = false;
    c->signals_added = 0;
    c->cts_off_due = true;
    c->cts_on_due = false;
    c->deadline = now;
}
void sbf_control_init(sbf_control_t *c, bool reset_available, int64_t now)
{
    memset(c, 0, sizeof *c);
    c->reset_available = reset_available;
    c->state = SBF_CONTROL_STARTING;
    c->deadline = now + SBF_CONTROL_STARTUP_MS; // the UART starts with CTS ignored
}

void sbf_control_line(sbf_control_t *c, const char *line)
{
    if (c->escaping) { c->prompt = true; return; } // any answer to the escape will do
    if (!c->sent) return;
    if (!strncmp(line, "$R:", 3)) {
        // The first line of a reply is an exact copy of the command (3.1.3); another is not ours.
        const char *echo = line + 3, *sent = command(c);
        while (*echo == ' ') echo++;
        const size_t length = strlen(sent) - 1; // without the CR
        if (strlen(echo) != length || strncmp(echo, sent, length)) return;
        c->reply_ok = true;
        c->collecting = c->adding || c->step == STEP_SIGNALS || c->step == STEP_TRACKING;
        c->collected = 0;
        c->listed = 0;
    } else if (!strncmp(line, "$R?", 3)) {
        c->reply_error = true;
        c->collecting = false;
    } else if (c->collecting) {
        if (c->adding || c->step == STEP_SIGNALS) c->listed |= sbf_control_signals(line);
        else c->collected |= sbf_control_constellations(line);
    }
}
void sbf_control_prompt(sbf_control_t *c)
{
    c->prompt = true;
    c->collecting = false;
    if (c->state == SBF_CONTROL_STARTING) c->deadline = 0; // the receiver is up
}
void sbf_control_block(sbf_control_t *c, const uint8_t *block, size_t length, int64_t now)
{
    c->last_block_ms = now;
    if (c->state == SBF_CONTROL_STARTING) c->deadline = 0;
    uint32_t uptime_s;
    if (sbf_number(block) != SBF_RECEIVER_STATUS || !sbf_receiver_uptime(block, length, &uptime_s)) return;
    // An uptime that went back means the receiver restarted and lost the RAM configuration.
    const bool restarted = c->uptime_known && uptime_s < c->uptime_s;
    c->uptime_known = true;
    c->uptime_s = uptime_s;
    if (restarted && c->state == SBF_CONTROL_RUNNING) configure(c, now);
}

static sbf_action_t act(sbf_action_kind_t kind, const char *text) { return (sbf_action_t){kind, text}; }
// A configuration that got no reply: reset if allowed, otherwise try again later.
static void failed(sbf_control_t *c, int64_t now)
{
    if (c->reset_available && (!c->reset_ever || now - c->last_reset_ms >= SBF_CONTROL_RESET_INTERVAL_MS)) {
        c->state = SBF_CONTROL_RESETTING;
        c->deadline = 0; // the reset starts at the next poll
    } else {
        c->state = SBF_CONTROL_WAITING;
        c->deadline = now + SBF_CONTROL_RETRY_MS;
    }
    c->sent = c->escaping = false;
}
sbf_action_t sbf_control_poll(sbf_control_t *c, int64_t now)
{
    // Writes must never wait on a CTS line the receiver may not be driving.
    if (c->cts_off_due) { c->cts_off_due = false; c->cts = false; return act(SBF_ACTION_CTS_OFF, NULL); }
    if (c->cts_on_due) { c->cts_on_due = false; c->cts = true; return act(SBF_ACTION_CTS_ON, NULL); }
    switch (c->state) {
    case SBF_CONTROL_STARTING:
        if (now >= c->deadline) configure(c, now);
        return act(SBF_ACTION_NONE, NULL);
    case SBF_CONTROL_WAITING:
        if (now >= c->deadline) configure(c, now);
        return act(SBF_ACTION_NONE, NULL);
    case SBF_CONTROL_RESETTING:
        if (!c->deadline) {
            c->deadline = now + SBF_CONTROL_RESET_HOLD_MS;
            c->reset_ever = true;
            c->last_reset_ms = now;
            c->resets++;
            c->uptime_known = false;
            return act(SBF_ACTION_RESET_ON, NULL);
        }
        if (now < c->deadline) return act(SBF_ACTION_NONE, NULL);
        c->state = SBF_CONTROL_STARTING;
        c->deadline = now + SBF_CONTROL_BOOT_MS;
        c->cts_off_due = true;
        return act(SBF_ACTION_RESET_OFF, NULL);
    case SBF_CONTROL_RUNNING:
        if (now - c->last_block_ms > SBF_CONTROL_SILENCE_MS) configure(c, now);
        return act(SBF_ACTION_NONE, NULL);
    case SBF_CONTROL_CONFIGURING:
        break;
    }
    if (c->escaping) {
        if (!c->prompt && now < c->deadline) return act(SBF_ACTION_NONE, NULL);
        c->escaping = false;
    }
    if (!c->sent) {
        if (c->escape_next) {
            c->escape_next = false;
            c->escaping = true;
            c->prompt = false;
            c->deadline = now + SBF_CONTROL_ESCAPE_MS;
            return act(SBF_ACTION_SEND, escape);
        }
        c->sent = true;
        c->reply_ok = c->reply_error = c->prompt = c->collecting = false;
        c->deadline = now + SBF_CONTROL_REPLY_MS;
        return act(SBF_ACTION_SEND, command(c));
    }
    if ((c->reply_ok || c->reply_error) && c->prompt) {
        c->sent = false;
        c->tries = 0;
        bool more = false;
        if (c->adding) {
            // The reply to a signal added lists what is tracked now. A signal refused, or taken
            // and still not listed, is not offered by this receiver.
            const uint64_t bit = 1ull << c->add;
            if (c->reply_ok && c->listed) c->signals = c->listed;
            if (c->signals & bit) c->signals_added |= bit;
            else c->signals_refused |= bit;
            more = next_signal(c);
        } else {
            if (c->reply_error) c->command_errors++;
            if (c->step == STEP_FLOW_CONTROL && c->reply_ok) c->cts_on_due = true;
            if (c->step == STEP_SIGNALS && c->reply_ok && c->listed) {
                c->signals = c->listed;
                c->signals_known = true;
                more = next_signal(c);
            }
            if (c->step == STEP_TRACKING && c->reply_ok && c->collected) {
                c->supported = c->collected;
                c->supported_known = true;
            }
        }
        // Each missing signal has its turn before the step after gnt.
        c->adding = more;
        if (more) return act(SBF_ACTION_NONE, NULL);
        if (++c->step == STEPS) {
            c->state = SBF_CONTROL_RUNNING;
            c->configurations++;
            c->last_block_ms = now; // SBF has 30 s from here to start
        }
        if (!c->cts_on_due) return act(SBF_ACTION_NONE, NULL);
        c->cts_on_due = false;
        c->cts = true;
        return act(SBF_ACTION_CTS_ON, NULL);
    }
    if (now < c->deadline) return act(SBF_ACTION_NONE, NULL);
    // No complete reply in time: the escape before the second try, then give up after the third.
    c->sent = false;
    if (++c->tries >= SBF_CONTROL_TRIES) { failed(c, now); return act(SBF_ACTION_NONE, NULL); }
    if (c->tries == 1) c->escape_next = true;
    return act(SBF_ACTION_NONE, NULL);
}
