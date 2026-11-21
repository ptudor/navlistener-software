#include "sbf_control.h"
#include "sbf_build.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// A simulated receiver COM2 in command input mode (reference guide 3.1.3): each command line
// gets "$R: " and the command, a settings line and the prompt; a failing command gets "$R?"
// and the prompt. A silent receiver answers nothing. It tracks `signals`, lists them for gnt,
// and adds one for "snt, +NAME" when it is among those it offers, restarting its tracking
// loops as the real one does; a name it does not offer is an invalid argument. One in
// `ignored` is taken and listed no differently. Its two elevation masks are what the observer
// wants unless it is `factory`, which starts both at 5 degrees; it gives one for
// "gem, Engine" and takes "sem, Engine, N" unless it is `stubborn`, which answers and keeps
// the mask it has.
typedef struct {
    bool silent, flow_control, mismatch;
    bool deaf_to_signals; // falls silent at the first snt
    const char *fail; // the command (first word) answered with an error
    unsigned sent, escapes, resets;
    uint64_t signals, offered, ignored;
    unsigned adds, restarts; // snt commands received, and those that restarted the tracking loops
    bool factory, stubborn, masks_up;
    int mask[SBF_MASKS];
    unsigned mask_sets; // sem commands received
    char last[256];
} rx_t;
static void signal_list(const rx_t *rx, char *out, size_t size)
{
    char names[400];
    sbf_signal_names(rx->signals, names, sizeof names);
    for (char *p = names; *p; p++) if (*p == ' ') *p = '+';
    snprintf(out, size, "  SignalTracking, %s", rx->signals ? names : "none");
}
static void deliver(sbf_control_t *c, rx_t *rx, const char *text)
{
    snprintf(rx->last, sizeof rx->last, "%s", text);
    rx->sent++;
    if (!strcmp(text, "SSSSSSSSSS\r")) rx->escapes++;
    if (rx->deaf_to_signals && !strncmp(text, "snt", 3)) rx->silent = true;
    if (rx->silent) return;
    char command[256];
    snprintf(command, sizeof command, "%s", text);
    command[strcspn(command, "\r")] = 0;
    char line[512];
    const size_t word = strcspn(command, ",");
    const int added = !strncmp(command, "snt, +", 6) ? sbf_signal_number(command + 6, strlen(command + 6)) : -1;
    if (!strncmp(command, "snt", 3)) rx->adds++;
    if (rx->fail && strlen(rx->fail) == word && !strncmp(command, rx->fail, word)) {
        snprintf(line, sizeof line, "$R? %s: Invalid command!", command);
        sbf_control_line(c, line);
    } else if (!strncmp(command, "snt", 3) && (added < 0 || !(rx->offered >> added & 1))) {
        sbf_control_line(c, "$R? setSignalTracking: Argument 'Signal' is invalid!");
    } else {
        snprintf(line, sizeof line, "$R: %s%s", command, rx->mismatch ? " extra" : "");
        sbf_control_line(c, line);
        if (!strncmp(command, "scs", 3)) { rx->flow_control = true; sbf_control_line(c, "  COMSettings, COM2, baud115200, bits8, No, bit1, RTS|CTS"); }
        else if (!strcmp(command, "gst")) {
            sbf_control_line(c, "  SatelliteTracking, G01+G02+G03+R01+R02+E01+E36");
            sbf_control_line(c, "  +S120+S158+C01+C63+J01+J10+I01+I14");
        } else if (!strcmp(command, "gnt") || added >= 0) {
            if (added >= 0) {
                rx->restarts++;
                if (!(rx->ignored >> added & 1)) rx->signals |= 1ull << added;
            }
            signal_list(rx, line, sizeof line);
            sbf_control_line(c, line);
        } else if (!strncmp(command, "gem, ", 5) || !strncmp(command, "sem, ", 5)) {
            if (!rx->masks_up) {
                rx->mask[SBF_MASK_TRACKING] = rx->factory ? 5 : SBF_CONTROL_TRACKING_MASK_DEG;
                rx->mask[SBF_MASK_PVT] = rx->factory ? 5 : SBF_CONTROL_PVT_MASK_DEG;
                rx->masks_up = true;
            }
            const unsigned i = !strncmp(command + 5, "PVT", 3) ? SBF_MASK_PVT : SBF_MASK_TRACKING;
            if (command[0] == 's') {
                rx->mask_sets++;
                if (!rx->stubborn) rx->mask[i] = atoi(strrchr(command, ',') + 1);
            }
            snprintf(line, sizeof line, "  ElevationMask, %s, %d", i == SBF_MASK_PVT ? "PVT" : "Tracking", rx->mask[i]);
            sbf_control_line(c, line);
        } else sbf_control_line(c, "  done");
    }
    sbf_control_prompt(c);
}
typedef struct { bool cts, reset; unsigned cts_on, cts_off, reset_pulses; } esp_t;
// Polls until now_ms, carrying out each action; returns the number of commands written.
static void run(sbf_control_t *c, rx_t *rx, esp_t *esp, int64_t *now, int64_t until, int64_t step)
{
    for (; *now <= until; *now += step) {
        // Several polls per instant, as the reading task polls more often than replies take.
        for (int polls = 0; polls < 16; polls++) {
            sbf_action_t a = sbf_control_poll(c, *now);
            if (a.kind == SBF_ACTION_NONE) continue;
            switch (a.kind) {
            case SBF_ACTION_SEND: assert(a.text && !esp->reset); deliver(c, rx, a.text); break;
            case SBF_ACTION_CTS_ON: esp->cts = true; esp->cts_on++; assert(rx->flow_control); break;
            case SBF_ACTION_CTS_OFF: esp->cts = false; esp->cts_off++; break;
            case SBF_ACTION_RESET_ON: assert(!esp->cts); esp->reset = true; esp->reset_pulses++; rx->resets++;
                                      rx->flow_control = false; break;
            case SBF_ACTION_RESET_OFF: esp->reset = false; break;
            default: break;
            }
        }
    }
}

static void configuration_test(void)
{
    sbf_control_t c;
    rx_t rx = {0};
    esp_t esp = {0};
    int64_t now = 0;
    sbf_control_init(&c, true, now);
    // The receiver gets its start-up time; CTS is off first of all.
    run(&c, &rx, &esp, &now, 2900, 100);
    assert(c.state == SBF_CONTROL_STARTING && !esp.cts_off && !rx.sent);
    run(&c, &rx, &esp, &now, 3500, 100);
    const char *const *list;
    const size_t steps = sbf_control_commands(&list);
    assert(c.state == SBF_CONTROL_RUNNING && rx.sent == steps && c.configurations == 1 && !c.command_errors);
    assert(!strcmp(rx.last, "gst\r") && esp.cts && esp.cts_on == 1);
    assert(c.supported_known && c.supported == 0xef); // GPS SBAS Galileo BeiDou QZSS GLONASS NavIC
    // The xPPS pulse is widened once per configuration, and only its width is named: the
    // interval, polarity, delay, time scale and sync age stay as the receiver has them.
    unsigned pulse_width = 0;
    for (size_t i = 0; i < steps; i++) pulse_width += !strcmp(list[i], "spps, , , , , , 100\r");
    assert(pulse_width == 1);
    // SBF keeps it running; 30 s of silence configures again, CTS off first.
    uint8_t b[64];
    size_t n = status_block(b, 100, 0, 0);
    for (int i = 0; i < 40; i++) { now += 1000; sbf_control_block(&c, b, n, now); run(&c, &rx, &esp, &now, now, 1); }
    assert(c.state == SBF_CONTROL_RUNNING && rx.sent == steps);
    run(&c, &rx, &esp, &now, now + SBF_CONTROL_SILENCE_MS + 1000, 100);
    assert(c.configurations == 2 && rx.sent == 2 * steps && esp.cts_off == 2 && esp.cts_on == 2);
    // A receiver whose uptime went back restarted: configured again at once.
    sbf_control_block(&c, b, status_block(b, 5000, 0, 0), now);
    sbf_control_block(&c, b, status_block(b, 3, 0, 0), now + 1000);
    assert(c.state == SBF_CONTROL_CONFIGURING);
    run(&c, &rx, &esp, &now, now + 1000, 100);
    assert(c.configurations == 3 && esp.cts_off == 3 && !esp.reset_pulses);
    // A prompt or block during start-up skips the rest of the wait.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){0}; now = 0;
    run(&c, &rx, &esp, &now, 100, 100);
    assert(c.state == SBF_CONTROL_RUNNING && rx.sent == steps);
}

static void reply_test(void)
{
    sbf_control_t c;
    rx_t rx = {.fail = "scst"};
    esp_t esp = {0};
    int64_t now = 0;
    const char *const *list;
    const size_t steps = sbf_control_commands(&list);
    // An error reply is counted and the configuration carries on.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    run(&c, &rx, &esp, &now, 100, 100);
    assert(c.state == SBF_CONTROL_RUNNING && c.command_errors == 1 && esp.cts);
    // Refused flow control: CTS stays off.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.fail = "scs"}; esp = (esp_t){0}; now = 0;
    run(&c, &rx, &esp, &now, 100, 100);
    assert(c.state == SBF_CONTROL_RUNNING && c.command_errors == 1 && !esp.cts && !esp.cts_on);
    // A reply that is not an echo of the command is not taken for one.
    sbf_control_init(&c, false, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.mismatch = true}; esp = (esp_t){0}; now = 0;
    run(&c, &rx, &esp, &now, SBF_CONTROL_REPLY_MS - 100, 100);
    assert(c.state == SBF_CONTROL_CONFIGURING && rx.sent == 1);
    // gst naming no satellite leaves the default constellations in place.
    assert(sbf_control_constellations("SatelliteTracking, none") == 0);
    assert(sbf_control_constellations("+G01+R24") == 0x41 && sbf_control_constellations("COM2") == 0);
    assert(sbf_control_constellations("SatelliteTracking, C05") == 0x08);
    (void)steps;
}

static void recovery_test(void)
{
    sbf_control_t c;
    rx_t rx = {.silent = true};
    esp_t esp = {0};
    int64_t now = 0;
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    // No reply: the command, the escape sequence, two more tries, then a reset pulse.
    run(&c, &rx, &esp, &now, 2 * SBF_CONTROL_REPLY_MS + SBF_CONTROL_ESCAPE_MS + SBF_CONTROL_REPLY_MS + 200, 50);
    assert(rx.sent == 4 && rx.escapes == 1 && esp.reset_pulses == 1 && esp.reset);
    const int64_t pulse = now;
    run(&c, &rx, &esp, &now, pulse + SBF_CONTROL_RESET_HOLD_MS + 100, 50);
    assert(!esp.reset && c.resets == 1 && c.state == SBF_CONTROL_STARTING);
    // Still silent after the boot wait: no second reset within 10 minutes, retries every 30 s.
    run(&c, &rx, &esp, &now, now + SBF_CONTROL_BOOT_MS + 12000, 50);
    assert(c.state == SBF_CONTROL_WAITING && esp.reset_pulses == 1);
    run(&c, &rx, &esp, &now, now + 9 * 60 * 1000, 500);
    assert(esp.reset_pulses == 1 && rx.escapes > 2);
    run(&c, &rx, &esp, &now, now + 2 * 60 * 1000, 500);
    assert(esp.reset_pulses == 2);
    // The receiver comes back: configured, running, CTS on.
    rx.silent = false;
    run(&c, &rx, &esp, &now, now + 20 * 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && esp.cts && c.configurations == 1);
    // A board without a reset line only retries.
    sbf_control_init(&c, false, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.silent = true}; esp = (esp_t){0}; now = 0;
    run(&c, &rx, &esp, &now, 20 * 60 * 1000, 500);
    assert(!esp.reset_pulses && c.state != SBF_CONTROL_RUNNING);
    // An escape answered with an error moves on at once rather than waiting out its time.
    sbf_control_init(&c, false, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.silent = true}; esp = (esp_t){0}; now = 0;
    run(&c, &rx, &esp, &now, SBF_CONTROL_REPLY_MS + 100, 100);
    assert(rx.escapes == 1);
    sbf_control_line(&c, "$R? SSSSSSSSSS: Invalid command!");
    rx.silent = false;
    run(&c, &rx, &esp, &now, now + 100, 100);
    assert(c.state == SBF_CONTROL_RUNNING);
}

// A mosaic-G5 as shipped tracks 20 of the 30 signals setSignalTracking names (firmware 1.0.0).
#define B(n) (1ull << (n))
static const uint64_t factory = B(0) | B(2) | B(3) | B(4) | B(8) | B(11) | B(17) | B(19) | B(20) | B(21) | B(24) |
                                B(28) | B(29) | B(30) | B(13) | B(14) | B(6) | B(7) | B(26) | B(15);
static void signals_test(void)
{
    const char *const *list;
    const size_t steps = sbf_control_commands(&list);
    const uint64_t all = sbf_control_wanted(), e5 = B(22);
    assert(__builtin_popcountll(all) == 30 && (all & factory) == factory && __builtin_popcountll(factory) == 20);
    assert(!(all & (B(1) | B(9) | B(23) | B(37) | B(39)))); // no name in setSignalTracking on a mosaic-G5

    // A P3 as shipped, its permissions without Galileo E5 AltBOC: the ten missing signals are
    // asked for one by one, nine arrive, and the refusal is no command error.
    sbf_control_t c;
    rx_t rx = {.signals = factory, .offered = all & ~e5};
    esp_t esp = {0};
    int64_t now = 0;
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && c.configurations == 1 && !c.command_errors && esp.cts);
    assert(rx.sent == steps + 10 && rx.adds == 10 && rx.restarts == 9 && !strcmp(rx.last, "gst\r"));
    assert(c.signals_known && c.signals == (all & ~e5) && rx.signals == c.signals);
    assert(c.signals_added == (all & ~e5 & ~factory) && c.signals_refused == e5 && !c.adding);

    // Configured again with its settings kept (COM2 fell silent): it is asked what it tracks and
    // nothing else, so no tracking loop restarts.
    uint8_t b[64];
    size_t n = status_block(b, 100, 0, 0);
    sbf_control_block(&c, b, n, now);
    run(&c, &rx, &esp, &now, now + SBF_CONTROL_SILENCE_MS + 2000, 100);
    assert(c.configurations == 2 && rx.sent == 2 * steps + 10 && rx.adds == 10 && rx.restarts == 9);
    assert(!c.signals_added && c.signals == (all & ~e5)); // nothing added by this configuration

    // The receiver restarts and comes back as shipped: the nine are added again, and the one
    // it refused is not asked for.
    rx.signals = factory;
    sbf_control_block(&c, b, status_block(b, 5000, 0, 0), now);
    sbf_control_block(&c, b, status_block(b, 3, 0, 0), now + 1000);
    assert(c.state == SBF_CONTROL_CONFIGURING);
    run(&c, &rx, &esp, &now, now + 2000, 100);
    assert(c.configurations == 3 && rx.sent == 3 * steps + 19 && rx.adds == 19 && rx.restarts == 18);
    assert(c.signals == (all & ~e5) && c.signals_refused == e5 && !c.command_errors);
    assert(c.signals_added == (all & ~e5 & ~factory));

    // The ESP32 restarts beside a receiver that kept its settings: the refused signal is asked
    // for once more and refused again, which restarts nothing, and nothing counts as added.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx.sent = rx.adds = 0; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && rx.sent == steps + 1 && rx.adds == 1 && rx.restarts == 18);
    assert(c.signals == (all & ~e5) && c.signals_refused == e5 && !c.signals_added && !c.command_errors);

    // A receiver with every signal on is asked what it tracks and left alone.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.signals = all, .offered = all}; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && rx.sent == steps && !rx.adds && c.signals == all && !c.signals_refused);

    // One that takes a name and does not track it is asked once, not again in this boot.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.signals = all & ~e5, .offered = all, .ignored = e5}; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && rx.adds == 1 && c.signals_refused == e5 && !c.signals_added);
    sbf_control_block(&c, b, n, now);
    run(&c, &rx, &esp, &now, now + SBF_CONTROL_SILENCE_MS + 2000, 100);
    assert(c.configurations == 2 && rx.adds == 1);

    // gnt refused, or naming no signal: nothing is asked for, and only the refusal is an error.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.signals = factory, .offered = all, .fail = "gnt"}; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && c.command_errors == 1 && !c.signals_known && !rx.adds && rx.sent == steps);
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.offered = all}; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && !c.command_errors && !c.signals_known && !rx.adds);

    // An added signal that gets no answer is retried like any command: the escape, then the
    // same signal again, and the rest once the receiver answers.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.signals = factory, .offered = all, .deaf_to_signals = true}; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, SBF_CONTROL_REPLY_MS + 100, 100);
    assert(c.adding && c.state == SBF_CONTROL_CONFIGURING && rx.escapes == 1 && !rx.adds && rx.signals == factory);
    rx.deaf_to_signals = rx.silent = false;
    run(&c, &rx, &esp, &now, now + SBF_CONTROL_ESCAPE_MS + 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && rx.adds == 10 && c.signals == all && !c.signals_refused && !esp.reset_pulses);

    // The list is read by whole names.
    assert(sbf_control_signals("  SignalTracking, GPSL1CA+GPSL2PY+GALE5a") == (B(0) | B(2) | B(20)));
    assert(sbf_control_signals("+BDSB2b+QZSL1CB") == (B(34) | B(38)) && sbf_control_signals("SignalTracking, none") == 0);
    assert(sbf_control_signals("XGPSL1CA GPSL1CAX GALE5A gpsl1ca") == 0 && sbf_control_signals("") == 0);
}

static void masks_test(void)
{
    const char *const *list;
    const size_t steps = sbf_control_commands(&list);
    const unsigned both = 1u << SBF_MASK_TRACKING | 1u << SBF_MASK_PVT;
    // A receiver as shipped, both masks at 5 degrees: each is read, then set, tracking to the
    // horizon and PVT to 10 degrees.
    sbf_control_t c;
    rx_t rx = {.factory = true};
    esp_t esp = {0};
    int64_t now = 0;
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && c.configurations == 1 && !c.command_errors);
    assert(rx.sent == steps + 2 && rx.mask_sets == 2 && rx.mask[SBF_MASK_TRACKING] == 0 && rx.mask[SBF_MASK_PVT] == 10);
    assert(c.masks_known == both && c.masks_set == both && !c.masks_refused && !c.masking);
    assert(c.mask[SBF_MASK_TRACKING] == 0 && c.mask[SBF_MASK_PVT] == 10);
    assert(c.mask_before[SBF_MASK_TRACKING] == 5 && c.mask_before[SBF_MASK_PVT] == 5);

    // Configured again with its settings kept: both masks are read and neither is sent.
    uint8_t b[64];
    size_t n = status_block(b, 100, 0, 0);
    sbf_control_block(&c, b, n, now);
    run(&c, &rx, &esp, &now, now + SBF_CONTROL_SILENCE_MS + 2000, 100);
    assert(c.configurations == 2 && rx.sent == 2 * steps + 2 && rx.mask_sets == 2 && !c.masks_set);

    // The receiver restarts with its own masks again: both are set again.
    rx.masks_up = false;
    sbf_control_block(&c, b, status_block(b, 5000, 0, 0), now);
    sbf_control_block(&c, b, status_block(b, 3, 0, 0), now + 1000);
    run(&c, &rx, &esp, &now, now + 2000, 100);
    assert(c.configurations == 3 && rx.mask_sets == 4 && c.masks_set == both && rx.mask[SBF_MASK_TRACKING] == 0);

    // Only the mask that differs is set: a receiver whose PVT mask is already 10 degrees.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.masks_up = true, .mask = {5, 10}}; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && rx.sent == steps + 1 && rx.mask_sets == 1);
    assert(c.masks_set == 1u << SBF_MASK_TRACKING && c.mask[SBF_MASK_TRACKING] == 0 && c.mask[SBF_MASK_PVT] == 10);

    // A refused set is a command error, and is not sent again while the ESP32 runs.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.factory = true, .fail = "sem"}; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && c.command_errors == 2 && c.masks_refused == both && !c.masks_set);
    assert(rx.sent == steps + 2 && c.mask[SBF_MASK_TRACKING] == 5 && c.mask[SBF_MASK_PVT] == 5);
    sbf_control_block(&c, b, n, now);
    run(&c, &rx, &esp, &now, now + SBF_CONTROL_SILENCE_MS + 2000, 100);
    assert(c.configurations == 2 && c.command_errors == 2 && rx.sent == 2 * steps + 2);

    // One that answers and keeps its mask is asked once, with no error.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.factory = true, .stubborn = true}; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && !c.command_errors && c.masks_refused == both && rx.mask_sets == 2);
    sbf_control_block(&c, b, n, now);
    run(&c, &rx, &esp, &now, now + SBF_CONTROL_SILENCE_MS + 2000, 100);
    assert(c.configurations == 2 && rx.mask_sets == 2);

    // A mask that cannot be read is not set blind: the refusal is the only error.
    sbf_control_init(&c, true, 0);
    sbf_control_prompt(&c);
    rx = (rx_t){.factory = true, .fail = "gem"}; now = 0; esp = (esp_t){0};
    run(&c, &rx, &esp, &now, 1000, 100);
    assert(c.state == SBF_CONTROL_RUNNING && c.command_errors == 2 && !c.masks_known && !rx.mask_sets && rx.sent == steps);

    // The reply's line: an engine and a whole number of degrees, negative for below the horizon.
    unsigned index = 9;
    int degrees = 99;
    assert(sbf_control_mask("  ElevationMask, Tracking, 5", &index, &degrees) && index == SBF_MASK_TRACKING && degrees == 5);
    assert(sbf_control_mask("ElevationMask, PVT, 10", &index, &degrees) && index == SBF_MASK_PVT && degrees == 10);
    assert(sbf_control_mask("  ElevationMask, Tracking, -2", &index, &degrees) && degrees == -2);
    assert(sbf_control_mask("ElevationMask,PVT,0", &index, &degrees) && index == SBF_MASK_PVT && degrees == 0);
    assert(sbf_control_mask("  ElevationMask, Tracking, 90", &index, &degrees) && degrees == 90);
    assert(!sbf_control_mask("  ElevationMask, Tracking, 91", &index, &degrees));
    assert(!sbf_control_mask("  ElevationMask, Tracking, 5.5", &index, &degrees));
    assert(!sbf_control_mask("  ElevationMask, Tracking, ", &index, &degrees));
    assert(!sbf_control_mask("  ElevationMask, Aux, 5", &index, &degrees));
    assert(!sbf_control_mask("  SignalTracking, GPSL1CA", &index, &degrees) && !sbf_control_mask("", &index, &degrees));
    assert(!sbf_control_mask("  ElevationMask, TrackingX, 5", &index, &degrees));
}

// The blocks forwarded to the collector are exactly those the sso commands request: every name
// in them maps to a forwarded number (4.2), and no other number is forwarded.
static void forwarded_test(void)
{
    static const struct { const char *name; uint16_t number; } blocks[] = {
        {"PVTGeodetic", 4007}, {"ReceiverTime", 5914}, {"MeasEpoch", 4027}, {"SatVisibility", 4012},
        {"ReceiverStatus", 4014}, {"RFStatus", 4092}, {"ReceiverSetup", 5902}, {"GPSRawCA", 4017},
        {"GPSRawL2C", 4018}, {"GPSRawL5", 4019}, {"GLORawCA", 4026}, {"GALRawFNAV", 4022},
        {"GALRawINAV", 4023}, {"GEORawL1", 4020}, {"BDSRaw", 4047}, {"BDSRawB2a", 4219},
        {"QZSRawL1CA", 4066}, {"QZSRawL2C", 4067}, {"QZSRawL5", 4068},
    };
    const char *const *list;
    const size_t steps = sbf_control_commands(&list);
    unsigned named = 0;
    for (size_t i = 0; i < steps; i++) {
        if (strncmp(list[i], "sso, ", 5)) continue;
        char messages[512];
        const char *start = strchr(strchr(list[i] + 5, ',') + 1, ',') + 2, *end = strrchr(list[i], ',');
        assert(end > start && (size_t)(end - start) < sizeof messages);
        memcpy(messages, start, (size_t)(end - start));
        messages[end - start] = 0;
        for (char *name = strtok(messages, "+"); name; name = strtok(NULL, "+")) {
            size_t k = 0;
            while (k < sizeof blocks / sizeof blocks[0] && strcmp(blocks[k].name, name)) k++;
            assert(k < sizeof blocks / sizeof blocks[0] && sbf_control_forwarded(blocks[k].number));
            named++;
        }
    }
    unsigned forwarded = 0;
    for (unsigned n = 0; n < 8192; n++) forwarded += sbf_control_forwarded((uint16_t)n);
    assert(named == sizeof blocks / sizeof blocks[0] && forwarded == named);
}

int main(void)
{
    configuration_test();
    reply_test();
    recovery_test();
    signals_test();
    masks_test();
    forwarded_test();
    puts("SBF control: configuration sequence, CTS only after RTS|CTS is confirmed, constellations, error and "
         "foreign replies, silence and restart recovery, escape, reset pulses and their 10-minute spacing, signals "
         "added once and never twice, elevation masks set only when they differ, and the forwarded blocks passed");
    return 0;
}
