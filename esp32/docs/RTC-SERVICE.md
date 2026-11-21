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
sources, sample age, freshness, reported uncertainty (null when unknown), the
last wall-clock change, and the [backup's proof](#backup-proof) as `backup`. GNSS evidence expires after two seconds without fresh
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
Retention is shown only by a loss of main power that the clock counts through:
see [Backup proof](#backup-proof).

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

## Backup proof

The MCP79412 has no pin or register that senses its cell, and the boards give the ESP32 no
other view of it. What the part does keep is a record of each loss of main power. When VCC
falls below its trip voltage with the backup input enabled, it sets PWRFAIL and stamps the
minute, hour, date and month from its own calendar; it stamps them again when VCC returns
(DS20002266 section 5.7.1). The time between the two stamps is time the calendar counted with
only the backup input to run it.

The board task takes that record wherever it finds PWRFAIL set on a running calendar. It saves
the raw calendar and both stamps in NVS (`nvf_rtc/last_pfail`), and only then writes RTCWKDAY
back as it stands. That write clears the flag and the stamps, and until it is made the part
stamps no later loss. A record that cannot be saved stays on the part and is tried again.

A record proves the backup when all of this holds:

- it was found with PWRFAIL and VBATEN set, on a running calendar dated 2024 or later;
- its stamps are at least two minutes apart, which is more than 60 seconds counted;
- the clock was not read stopped while the record was on the part.

The part draws about a microamp from its backup input (925 nA typical at 3 V), so counting
for a minute takes some 30 µF of stored charge: hundreds of times what a decoupling capacitor
on that input holds. A board with no cell, a dead one or an open holder cannot make that
record. A shorter loss is logged and proves nothing either way: stamps one minute apart can
be a single second that crossed a minute.

The proof is that record, kept in `nvf_rtc/proof`, and it is always reported with the loss
that earned it:

```
rtc: power loss recorded, 2026-11-21 23:17 to 23:22 UTC (5 min): the clock counted through it, which proves the backup
rtc: running calendar retained (UTC epoch=1795303366); backup proven by the power loss of 2026-11-21 23:17 to 23:22 UTC (5 min)
rtc: power loss recorded, 2026-11-21 22:32 to 22:33 UTC (1 min): too short to prove the backup, which takes more than a minute
rtc: running calendar retained (UTC epoch=1795301907); backup enabled, not yet proven by a power loss
```

`rtc status` gives it as `backup`: `proven`, with `power_down_epoch` and `power_up_epoch`
(both null until there is a proof). The member is null on a part that keeps no such record.

A proof is withdrawn when the part shows that its backup has since failed: VBATEN reads
clear, which it does only after a power-on reset, when main power and the backup were lost
together; or the clock reads stopped with a power failure on record, so it went over to the
backup and did not keep counting. Either has to be read on two polls running. The record is
erased before the clock is set again, and if it cannot be erased the set waits.

To prove a board at the bench, fit the cell, let the clock be set from a fix, take the power
away for three minutes or more, and restore it. The first lines of the next boot say what the
part recorded.

What this does not show:

- It is a dated fact about one loss. It is not a measurement of the cell: there is no voltage
  and no remaining life in it. A cell that has died since is found out at the next loss, and
  a station that never loses power never tests its cell.
- The stamps hold no seconds and no year. A loss is known to a minute either way, the year is
  the calendar's when the record was found, and a loss of more than a year reads as what is
  left over.
- A clock that stopped part of the way through a long loss, and was running again before the
  first read after it, shows the minutes it did count and can pass. Its calendar is then
  behind, which shows as the step when the system clock is promoted from the RTC to GNSS
  (`rtc status`, journal event 11).
- The DS3231M and the MAX31328 keep no record of a power loss, so there is no proof for them:
  a calendar they lose shows as their oscillator-stop flag.

## Rate discipline

On a board whose RTC is an MCP79412, the board task keeps the crystal's rate trimmed to the
GNSS pulse. Nothing here touches the calendar: the rules above for setting the time stand.

The RTC's 1 Hz output and the receiver's pulse are timed on one counter (TIMING.md), so each
second shows where the RTC's edge falls within the GNSS second, and how far that moves from one
RTC second to the next is the RTC's rate error for that second. The part applies its digital
trim once a minute, as a step of two 32.768 kHz cycles for each TRIMVAL count (DS20002266
section 5.6); every other second shows the crystal alone. The median of 600 one-second
readings is therefore the crystal's own rate, whatever trim is in force, and their
interquartile range says whether the readings agree.

A window counts only readings taken while the receiver holds a position, both pulses are fresh
with valid periods, and the RTC gives 1 Hz from its crystal in fine-trim mode. A reading that
fails those conditions, a missed or irregular pulse on either input, or a gap between readings
starts the window again. A window whose spread is over 1 ppm, or whose rate is beyond
150 ppm, is logged and not used.

OSCTRIM is then held at the step nearest to cancelling the crystal: 1.017 ppm a step, at most
127 steps either way. It is rewritten only when the step in force leaves more than three
quarters of a step, so a crystal between two steps does not make the register flap. The write
is read back, and is never made with EXTOSC or CRSTRIM set. Each window logs one line:

```
rtc: rate: crystal +3.090 ppm (spread 0.075); trim changed from +127 to -3 steps (0xff to 0x03), leaving +0.038 ppm
rtc: rate: crystal +3.094 ppm (spread 0.071); trim -3 steps (0x03) leaves +0.042 ppm
```

OSCTRIM is in the RTC's battery-backed domain. With a cell fitted it survives a power loss and
the trim applies in holdover; without one it returns to its power-on value and is set again
ten minutes into the next fix. The data sheet gives that power-on value as zero, but the first
P3 read `0xff` on several power-ups: the full +129 ppm, eleven seconds a day. That is why the
register is owned here and not left as found. The timing record carries OSCTRIM every second
(`rtc_trim`), so the collector sees each change.

Not done: a crystal's rate follows temperature, and the trim is corrected only while there is
a fix; in holdover it stays where it was last set. The MAX31328 and DS3231M compensate for
temperature themselves and are not disciplined.

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
