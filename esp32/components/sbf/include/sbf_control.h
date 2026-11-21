// sbf_control: configuring a mosaic receiver's COM2 for the observer, and recovering it.
// I/O-free: the reading task feeds it reply lines, prompts and blocks, polls it for the next
// action (a command to write, a change to the ESP32 UART's CTS handling, the reset line) and
// carries the action out.
//
// Configuration is RAM-only and repeated whenever needed, like the u-blox path's: it is lost
// when the receiver restarts. Each command waits for its reply and the prompt that ends it
// (reference guide 3.1, 3.1.3):
//   scs, COM2, baud115200, bits8, No, bit1, RTS|CTS   hardware flow control (3.2, setCOMSettings)
//   sdio, COM2, CMD, SBF                               commands in, SBF out, nothing else
//   scst, usec500                                      receiver time within 0.5 ms of GNSS time
//   spps, , , , , , 100                                a 100 ms pulse, long enough for the PPS lamps;
//                                                      every other xPPS setting stays as the receiver has it
//   sso, Res1, COM2, PVTGeodetic+ReceiverTime+MeasEpoch+SatVisibility+ReceiverStatus+RFStatus, sec1
//   sso, Res2, COM2, ReceiverSetup, OnChange           the receiver's identity, every 60 s
//   gst                                                the constellations it may track
// Res1 and Res2 are the streams the receiver never saves and resets at boot, so the observer's
// output never lands in a saved configuration or in a stream someone set up on another port.
//
// Signals. A mosaic-G5 starts with a third of its signals off (GPS L1C, GLONASS L2P and L3,
// Galileo E5 AltBOC, SBAS L5, BeiDou B2b, QZSS L6, L1C, L1S and L1C/B), and an observer is there
// to hear everything its receiver can. After sdio the observer asks which signals are tracked
// and adds each one that is missing, a command apiece:
//   gnt                                                the signals it tracks (3.2, getSignalTracking)
//   snt, +GPSL1C                                       one signal added to that list (3.1 for the "+")
// setSignalTracking stops and restarts every tracking loop, so it is sent only for a signal
// that is not on. A configuration repeated on a receiver that kept its settings (the ESP32
// restarted, or COM2 fell silent) finds them all on and sends none: the receiver keeps every
// satellite it has. One name per command, so that a name a receiver does not accept costs
// only that signal: a refused command changes nothing. A signal refused, or accepted and still
// not in the list, is not asked for again until the ESP32 restarts.
//
// The receiver starts with flow control off, and its RTS output is then of no meaning, so the
// ESP32 honors CTS only after the receiver has confirmed RTS|CTS, and stops before every new
// configuration. Its own RTS runs throughout. A command that gets no reply is retried after the
// escape sequence (ten S and a CR), which returns a port to command input. A configuration
// that never gets a reply, or SBF that stops for 30 s and cannot be configured again, ends in a
// reset pulse on the board's reset line: at most once per 10 minutes, 500 ms long, then a boot wait.
#ifndef NVF_SBF_CONTROL_H
#define NVF_SBF_CONTROL_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define SBF_CONTROL_REPLY_MS          3000
#define SBF_CONTROL_ESCAPE_MS         2000
#define SBF_CONTROL_TRIES             3
#define SBF_CONTROL_SILENCE_MS        30000
#define SBF_CONTROL_RETRY_MS          30000
#define SBF_CONTROL_STARTUP_MS        3000
#define SBF_CONTROL_RESET_HOLD_MS     500
#define SBF_CONTROL_BOOT_MS           10000
#define SBF_CONTROL_RESET_INTERVAL_MS (10 * 60 * 1000)

typedef enum {
    SBF_ACTION_NONE,
    SBF_ACTION_SEND,      // write text, a command line or the escape sequence
    SBF_ACTION_CTS_ON,    // the receiver confirmed RTS|CTS: honor CTS
    SBF_ACTION_CTS_OFF,   // stop honoring CTS until it confirms again
    SBF_ACTION_RESET_ON,  // hold the receiver in reset
    SBF_ACTION_RESET_OFF, // release it
} sbf_action_kind_t;
typedef struct {
    sbf_action_kind_t kind;
    const char *text; // SEND only, NUL-terminated
} sbf_action_t;

typedef enum {
    SBF_CONTROL_STARTING,    // waiting for the receiver to come up
    SBF_CONTROL_CONFIGURING,
    SBF_CONTROL_RUNNING,     // configured, SBF arriving
    SBF_CONTROL_WAITING,     // a configuration failed and no reset is due; retrying later
    SBF_CONTROL_RESETTING,   // reset held
} sbf_control_state_t;

typedef struct {
    sbf_control_state_t state;
    bool reset_available;
    unsigned step, tries;
    bool sent, escaping, escape_next, reply_ok, reply_error, prompt;
    bool cts, cts_off_due, cts_on_due;
    int64_t deadline;
    int64_t last_block_ms;
    bool reset_ever;
    int64_t last_reset_ms;
    bool uptime_known;
    uint32_t uptime_s;
    bool collecting;
    uint8_t collected;
    bool supported_known;
    uint8_t supported; // UBX gnssId bits the receiver may track (gst)
    // Signal tracking, as masks of signal numbers (sbf.h): what the receiver's last list named,
    // what the latest configuration added to it, and what was asked for since the ESP32
    // started and not given.
    bool signals_known;
    uint64_t signals, signals_added, signals_refused;
    uint64_t listed;  // the list being read from a reply
    bool adding;      // the command out is command[], one signal's snt
    uint8_t add;      // that signal's number
    char command[32];
    unsigned configurations, resets, command_errors;
} sbf_control_t;

void sbf_control_init(sbf_control_t *c, bool reset_available, int64_t now_ms);
void sbf_control_line(sbf_control_t *c, const char *line);
void sbf_control_prompt(sbf_control_t *c);
void sbf_control_block(sbf_control_t *c, const uint8_t *block, size_t length, int64_t now_ms);
sbf_action_t sbf_control_poll(sbf_control_t *c, int64_t now_ms);
// The command list, for logs and tests.
size_t sbf_control_commands(const char *const **list);
// Whether a block is one of those the commands request, which the observer forwards to the
// collector whole (GNF1 SBF record format 1). Anything else that reaches COM2 stays local.
bool sbf_control_forwarded(uint16_t number);
// gst's satellite list ("G01+...+R24+E36+...") as a UBX gnssId mask; 0 when it names none.
uint8_t sbf_control_constellations(const char *text);
// The signals a line of gnt's or snt's reply names ("SignalTracking, GPSL1CA+GPSL2PY+..."), as
// a mask of signal numbers; 0 when it names none.
uint64_t sbf_control_signals(const char *text);
// Every signal the observer wants tracked: all that setSignalTracking names on a mosaic-G5.
uint64_t sbf_control_wanted(void);
#endif
