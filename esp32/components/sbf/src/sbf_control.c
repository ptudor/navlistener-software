#include "sbf_control.h"
#include "sbf.h"
#include <string.h>

static const char *const commands[] = {
    "scs, COM2, baud115200, bits8, No, bit1, RTS|CTS\r",
    "sdio, COM2, CMD, SBF\r",
    "scst, usec500\r",
    "sso, Res1, COM2, PVTGeodetic+ReceiverTime+MeasEpoch+SatVisibility+ReceiverStatus+RFStatus, sec1\r",
    "sso, Res2, COM2, ReceiverSetup, OnChange\r",
    "gst\r",
};
enum { STEP_FLOW_CONTROL = 0, STEP_TRACKING = 5, STEPS = sizeof commands / sizeof commands[0] };
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

static void configure(sbf_control_t *c, int64_t now)
{
    c->state = SBF_CONTROL_CONFIGURING;
    c->step = c->tries = 0;
    c->sent = c->escaping = c->escape_next = c->collecting = false;
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
        const char *echo = line + 3, *command = commands[c->step];
        while (*echo == ' ') echo++;
        const size_t length = strlen(command) - 1; // without the CR
        if (strlen(echo) != length || strncmp(echo, command, length)) return;
        c->reply_ok = true;
        c->collecting = c->step == STEP_TRACKING;
        c->collected = 0;
    } else if (!strncmp(line, "$R?", 3)) {
        c->reply_error = true;
        c->collecting = false;
    } else if (c->collecting) {
        c->collected |= sbf_control_constellations(line);
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
    sbf_rx_status_t status;
    if (sbf_number(block) != SBF_RECEIVER_STATUS || !sbf_receiver_status(block, length, &status)) return;
    // An uptime that went back means the receiver restarted and lost the RAM configuration.
    const bool restarted = c->uptime_known && status.uptime_s < c->uptime_s;
    c->uptime_known = true;
    c->uptime_s = status.uptime_s;
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
        return act(SBF_ACTION_SEND, commands[c->step]);
    }
    if ((c->reply_ok || c->reply_error) && c->prompt) {
        if (c->reply_error) c->command_errors++;
        if (c->step == STEP_FLOW_CONTROL && c->reply_ok) c->cts_on_due = true;
        if (c->step == STEP_TRACKING && c->reply_ok && c->collected) {
            c->supported = c->collected;
            c->supported_known = true;
        }
        c->sent = false;
        c->tries = 0;
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
