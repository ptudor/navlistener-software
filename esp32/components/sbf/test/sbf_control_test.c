#include "sbf_control.h"
#include "sbf_build.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

// A simulated receiver COM2 in command input mode (reference guide 3.1.3): each command line
// gets "$R: " and the command, a settings line and the prompt; a failing command gets "$R?"
// and the prompt. A silent receiver answers nothing.
typedef struct {
    bool silent, flow_control, mismatch;
    const char *fail; // the command (first word) answered with an error
    unsigned sent, escapes, resets;
    char last[256];
} rx_t;
static void deliver(sbf_control_t *c, rx_t *rx, const char *text)
{
    snprintf(rx->last, sizeof rx->last, "%s", text);
    rx->sent++;
    if (!strcmp(text, "SSSSSSSSSS\r")) rx->escapes++;
    if (rx->silent) return;
    char command[256];
    snprintf(command, sizeof command, "%s", text);
    command[strcspn(command, "\r")] = 0;
    char line[300];
    const size_t word = strcspn(command, ",");
    if (rx->fail && strlen(rx->fail) == word && !strncmp(command, rx->fail, word)) {
        snprintf(line, sizeof line, "$R? %s: Invalid command!", command);
        sbf_control_line(c, line);
    } else {
        snprintf(line, sizeof line, "$R: %s%s", command, rx->mismatch ? " extra" : "");
        sbf_control_line(c, line);
        if (!strncmp(command, "scs", 3)) { rx->flow_control = true; sbf_control_line(c, "  COMSettings, COM2, baud115200, bits8, No, bit1, RTS|CTS"); }
        else if (!strcmp(command, "gst")) {
            sbf_control_line(c, "  SatelliteTracking, G01+G02+G03+R01+R02+E01+E36");
            sbf_control_line(c, "  +S120+S158+C01+C63+J01+J10+I01+I14");
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

int main(void)
{
    configuration_test();
    reply_test();
    recovery_test();
    puts("SBF control: configuration sequence, CTS only after RTS|CTS is confirmed, constellations, error and "
         "foreign replies, silence and restart recovery, escape, reset pulses and their 10-minute spacing passed");
    return 0;
}
