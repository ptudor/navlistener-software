# RTC service and system time

The board task qualifies GNSS UTC and reads the fitted RTC. The time service can
bootstrap an unusable system wall clock from either source before TLS,
WireGuard, HTTPS downloads and reception forecasts need it. Three advancing
GNSS reports must span at least two seconds, have a valid fix and resolved UTC,
and report at most 100 ms time uncertainty. A running RTC is continuity evidence;
its absolute uncertainty is unknown. Neither source authenticates GNSS time.

An RTC bootstrap may be promoted once to qualified GNSS. SNTP can subsequently
set system time and is labeled `network`; it does not become independent RTC or
GNSS evidence. A valid retained RTC is never automatically overwritten merely
because another source disagrees. Correct it explicitly with a service command.
Monotonic timers and earlier capture stamps are preserved across wall-clock steps.
Records captured with unknown UTC keep that status after time becomes available.

## USB operations

These commands share the USB Serial/JTAG commissioning console and require
`CONFIG_NVF_COMMISSION_CONSOLE`. They have no remote command path and do not
change board identity, manufacturer enrollment or credentials.

```text
rtc status
rtc verify <maintenance reason>
rtc battery <maintenance reason>
rtc gnss <maintenance reason>
rtc factory <unix_ms> <uncertainty_ms> <reference_id> <maintenance reason>
rtc last
rtc record <sequence>
```

`rtc status` emits `NVF-RTC-STATUS` JSON with separate system and independent
sources, sample age, freshness, reported uncertainty (null when unknown), and the
last wall-clock change. GNSS evidence expires after two seconds without fresh
qualified input; a read RTC expires after 30 seconds. Network status is fresh for
one hour. System time continues to run after a source becomes stale; freshness
does not claim holdover accuracy. Reported GNSS uncertainty excludes unmeasured
receiver-output and task scheduling latency.

`rtc gnss` requires an already qualified fresh receiver sequence. It can set a
valid running calendar as well as recover a stopped one. `rtc factory` is the
defined reference-device interface: the USB host supplies UTC milliseconds at
the instant the complete command line is accepted, an uncertainty of 0..100 ms,
and a reference identifier of at most 63 bytes. The host must include reference,
transport and scheduling error in its stated uncertainty. A host's ordinary
wall clock is not automatically a qualified factory reference. Firmware checks
format, range (2000..2099) and request age; it cannot independently verify the
external reference's accuracy. A queued reference older than two seconds is
refused. System bootstrap additionally needs a calendar in 2024..2099, matching
the date checks used by the time-dependent network paths.

`rtc verify` reads and verifies advancing continuity without writing RTC
registers or changing the system clock. `rtc battery` does the same and records
an operator-declared battery service; it needs neither GNSS nor network time.
Neither operation establishes absolute accuracy or physical battery retention.

Reasons have 1..63 bytes. Prefix a time-setting reason with `battery:` to declare operator
battery maintenance, for example `rtc gnss battery: powered cell replacement`.
This records maintenance, not battery presence, voltage, replacement detection
or successful retention. A powered swap can leave no oscillator event. Check
the board's wiring and maintain the receiver rail too when its backup is shared.

Before a service write the firmware durably saves the old raw calendar,
control/status and decoded validity. It records source, reference, uncertainty,
reason and uptime, then verifies calendar readback and advancing seconds. The
MCP79412's power-failure timestamps are separately preserved before any weekday
write clears them. An explicit service action retries a previously unusable
part; repeated automatic attempts do not bypass an uncleared oscillator-stop
flag. A rejected reference leaves a retained clock alone.

`NVF-RTC-SERVICE` JSON reports these numeric results:

| Result | Meaning |
|---:|---|
| 0 | Pending attempt; completion was not durably recorded |
| 1 | Requested write/continuity verification passed and final audit saved |
| 2 | Invalid, stale or unqualified reference |
| 3 | Old calendar/status could not be read |
| 4 | Audit storage failed; inspect the retained pending attempt |
| 5 | Write or readback failed |
| 6 | Oscillator-stop flag did not clear |
| 7 | No fitted/accessible RTC |
| 8 | Continuity verification failed |

The latest 32 attempts live in the existing diagnostic `journal` NVS partition
under `rtc_service/service00` through
`service31`. Each slot is one versioned blob containing its sequence. Startup
and `rtc last` find the greatest stored sequence by scanning slots. A power loss
between pre-write and completion leaves a pending attempt. A failed final audit
never reports a completed durable success. A timeout says the board task has not
answered yet; inspect `rtc last` before issuing another operation.

A verified explicit service action also submits the qualified reference to the
system time service. This closes the RTC/system-clock gap when correcting a
retained calendar without network time. Check `rtc status` separately: failure
to queue/apply a system update does not change the RTC's verified result.

## Provenance and qualification

The journal and updater retain their existing qualified GNSS/RTC evidence path.
SNTP never becomes a journal UTC anchor. Journal event 11 records system-clock
changes separately, including source, old/new UTC nanoseconds, reference uptime
and reported uncertainty. For an SNTP change the old UTC is projected from the
previous anchor; it is an estimate, not a reading before SNTP's setter ran.
Event 12 records the RTC service outcome; detailed maintenance evidence stays in
the 32-slot service history. No old capture date is recomputed.

ESP32 receiver-solution telemetry uses body version 2, which appends a timestamp
source to the version 1 blocks. Deploy the matching collector before this
firmware: older collectors accept only version 1 solution bodies. Other record
types and the C feeder's version 1 bodies are unchanged. The collector retains
the original body through persistence/replay. For backlog, receiver-derived or
RTC-continuity stamps cannot vote as an independent absolute time reference.
Live records can still use the collector's own receipt clock. Expired network
or factory evidence is stamped with explicitly unknown provenance for that
comparison, while the capture timestamp itself remains available.

Host tests exercise missing/stale sources, GNSS promotion, failed writes and
audits, oscillator-stop retry, and replay provenance. Physical qualification is
still required: a fresh board without a cell; powered cell replacement; removal
of both supplies; power-off retention; GNSS absent; network time absent; and a
retained but wrong calendar. For each, check `rtc status`, journal anchors,
updater metadata time, TLS/WireGuard startup and forecast acceptance separately.
Time setting and retention testing precede the first network acceptance test;
commissioning record installation can happen before or after time setting.
