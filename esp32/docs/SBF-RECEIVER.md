# Septentrio receiver on COM2 (P3 revision A)

How the universal image runs the mosaic-G5 P3 on the P3 observer (`INTSAT_MOSAIC_P3`
revision A): the UART, flow control and reset lines, the commands it sends, the SBF it reads,
how that becomes the observer's receiver status, and how the blocks reach the collector. The code is the `sbf` component
(`components/sbf`) and the SBF task in `main/receiver.c`. Field layouts, command syntax and
reply formats are from the mosaic-G5 firmware reference guide v1.0.1; section numbers below
refer to it, except where the hardware manual (v2.0.0) is named.

The first P3 has run this path on a bench, on receiver firmware 1.0.0: over seven hours on one
fix every block passed its CRC and no pulse was missed. The host tests build every block field
by field from the reference guide's tables; they are not captures. Section 9 lists the
bring-up checks; the ones that need a collector session, a scope or an induced fault have not
been run yet.

## 1. When it runs

A manifest that lists `GPS_MOSAIC_G5_P3` on a board row with COM2 flow-control pins selects
this path in place of the u-blox one; on the P3 those pins are GPIO17 (RTS) and GPIO16 (CTS),
and GPIO13 drives the receiver's reset. A manifest that lists the mosaic-G5 beside a u-blox
receiver, or a row without the flow-control pins, configures no receiver. Receiver bytes on
this path never reach the UBX parser, and the observer pushes to the collector on the `sbf`
feed instead of `ubx` (section 7).

## 2. Lines

| ESP32 | Net | Receiver | Use |
|---|---|---|---|
| GPIO4 (UART1 TX) | `GNSS_UART_TX` → U41 → `P3_RXD2` | RXD2 | commands |
| GPIO5 (UART1 RX) | `P3_TXD2` → U43 → `GNSS_UART_RX` | TXD2 | SBF and replies |
| GPIO17 (UART1 RTS) | `ESP_UART_RTS_N` → U41 → `P3_CTS_N` | nCTS2 | the ESP32 is ready for data |
| GPIO16 (UART1 CTS) | `P3_RTS_N` → U43 → `ESP_UART_CTS_N` | nRTS2 | the receiver is ready for data |
| GPIO13 | `P3_RESET_DRV` → Q1 → `P3_RESET_N` | nRST_IN | high holds the receiver in reset |

The UART runs at 115200 baud, 8N1, the receiver's default (3.2, `setCOMSettings`); at the
1 Hz output below that is under 40 % of the line. GPIO13 is driven low before the UART starts
and stays low except for a recovery pulse. R94 holds the receiver's CTS high, not ready, until
the UART drives GPIO17.

## 3. Flow control without a deadlock

The receiver starts with flow control off (hardware manual 4.3), and while it is off the
meaning of its RTS output is not specified. The ESP32 therefore drives its own RTS from the
start, so the receiver may send whenever the ESP32 has room, but honors CTS only after the
receiver has confirmed `RTS|CTS` in its reply to `setCOMSettings`. Before every new
configuration it stops honoring CTS again. Commands go through a 1 KiB transmit ring, so a
write never waits on a receiver that is not listening.

## 4. Configuration

Each command is one line ending in CR. The ESP32 waits for its reply, whose first line is
`$R: ` and an exact copy of the command, or `$R?` and an error, and for the prompt that ends
every reply (3.1, 3.1.3). A reply that does not echo the command in flight is not taken for
its reply.

| Command | Why |
|---|---|
| `scs, COM2, baud115200, bits8, No, bit1, RTS\|CTS` | hardware flow control; on success the ESP32 starts honoring CTS |
| `sdio, COM2, CMD, SBF` | commands in, SBF out and nothing else on this port |
| `gnt` | the signals it tracks (3.2, `getSignalTracking`) |
| `snt, +GPSL1C` | one signal added to that list: sent once for each signal the receiver could track and does not (see Signal tracking below), so usually not at all |
| `scst, usec500` | the receiver time within 0.5 ms of GNSS time once FINETIME is set (2.3), which bounds the time accuracy in section 6 |
| `spps, , , , , , 100` | a 100 ms xPPS pulse in place of the default 5 ms (3.2, `setPPSParameters`), so the board's PPS lamp and the panel's show each pulse, as they do on the u-blox boards. Only the width is named: the interval, polarity, delay, time scale and sync age stay as the receiver has them, and the pulse's leading edge, which carries the time, does not move |
| `sso, Res1, COM2, PVTGeodetic+ReceiverTime+MeasEpoch+SatVisibility+ReceiverStatus+RFStatus, sec1` | the 1 Hz stream |
| `sso, Res2, COM2, ReceiverSetup, OnChange` | the receiver's identity, every 60 s and on change |
| `sso, Res3, COM2, GPSRawCA+GPSRawL2C+GPSRawL5+GLORawCA+GALRawFNAV+GALRawINAV+GEORawL1+BDSRaw+BDSRawB2a+QZSRawL1CA+QZSRawL2C+QZSRawL5, OnChange` | each navigation page as it is decoded (4.2.2), for the collector; a fixed interval would decimate them |
| `gst` | the satellites it may track, read as the constellation mask |

The settings are RAM-only, as on the u-blox path: nothing is saved to the receiver's boot
configuration. `Res1` to `Res3` are streams the receiver never saves and resets at boot
(3.2, `setSBFOutput`), so the observer's output cannot land in a saved configuration or take
over a stream someone set up for the receiver's own USB port. A command answered with an
error is logged and counted, and the configuration carries on; a refused `setCOMSettings`
leaves CTS ignored.

**Signal tracking.** A mosaic-G5 starts with 20 of the 30 signals `setSignalTracking` names.
GPS L1C, GLONASS L2P and L3, Galileo E5 AltBOC, SBAS L5, BeiDou B2b and QZSS L6, L1C, L1S and
L1C/B are off. An observer is there to hear everything its receiver can, wherever it stands,
so after `gnt` it adds each signal that is missing, by name and one command per signal: a `+`
before a name adds it to the list the receiver has (3.1).

`setSignalTracking` stops and restarts every tracking loop (3.2), so it is sent only for a
signal that is not on. The first configuration after the receiver starts adds them, while it
has no satellite to lose. A configuration repeated on a receiver that kept its settings, after
the ESP32 restarted or COM2 fell silent, finds every signal on and sends none: the receiver
keeps each satellite it has. On a P3 a restart of the ESP32 alone left the receiver's uptime
running and the same 22 satellites tracked.

One name per command, so that a name a receiver does not accept costs only that signal: a
refused command changes nothing. A signal that is refused, or accepted and still missing
from the reply's list, is not asked for again until the ESP32 restarts; the log names it once,
and the refusal is not a command error. The P3 accepted all ten. Within minutes GPS L1C,
GLONASS L2P and L3, SBAS L5 and BeiDou B2b each carried satellites. Galileo E5 AltBOC was
listed and carried none while five Galileo satellites were tracked on E5a: the permission
file read from a P3 lists AltBOC tracking as not permitted. No QZSS satellite was in view to
show its four.

Tracking a signal does not put it in the receiver's position solution: `setSignalUsage` stays
as the receiver has it. The satellites allowed (`setSatelliteTracking`) and the elevation mask
also stay as they are, and the receiver's default allows every satellite of every
constellation.

## 5. Recovery

The state machine is `sbf_control` (`components/sbf/include/sbf_control.h`), with host tests
for each path.

- **Start-up.** The first configuration waits 3 s for the receiver to come up, less if a
  prompt or a block arrives first.
- **No reply.** A command with no reply within 3 s is tried again after the escape sequence,
  ten `S` and a CR, which returns a port to command input (3.1, `setDataInOut`). After three
  tries the configuration has failed.
- **Silence.** Configured, but no CRC-valid block for 30 s: configured again.
- **Restart.** A ReceiverStatus uptime lower than the last one means the receiver restarted and
  lost the RAM settings: configured again at once.
- **Reset.** A failed configuration pulses GPIO13 high for 500 ms, at most once per 10 minutes,
  then waits up to 10 s for the receiver to boot before configuring it. Between resets it retries
  every 30 s. The hardware manual (4.1) gives nRST_IN no minimum pulse width; it says the input
  is internally debounced and may be wired straight to a push-button, so the pulse is long
  enough to pass any push-button debounce. The receiver's GPIO pins stay in input mode for
  about 2 s after a reset (4.9), well inside the boot wait.

The ESP32 never resets the receiver at its own boot, and adds no signal that is already on
(section 4): an observer that restarts for an update keeps the receiver tracking.

## 6. From SBF to the receiver status

`sbf_status` fills the same `gnss_status_t` the u-blox path fills, so the panel, ObserverDetails
tag 6, the reception assessment and the RTC read one receiver-neutral state. All blocks are
CRC-checked, and every field is bounds-checked against the block's Length and SBLength
(4.1.12).

| Block | Number | Status it feeds |
|---|---:|---|
| MeasEpoch | 4027 | Tracking: a satellite counts when a signal on the main antenna has a pseudorange and a C/N0 of at least 1 dB-Hz; its C/N0 is the strongest such signal's. The satellites on each signal number are counted by the same rule for the log. An epoch scrambled for want of the Measurement Availability permission (CommonFlags bit 7) is not used |
| SatVisibility | 4012 | Elevations, rounded to whole degrees; unknown (127) when older than 5 s |
| PVTGeodetic | 4007 | A fix: a GNSS solution type (1, 2, 4-8, 10, not 3 "fixed location", which reports entered coordinates) with Error 0 |
| ReceiverTime | 5914 | The UTC calendar for the RTC, paired with the PVT of the same epoch |
| RFStatus | 4092 | Jamming and spoofing states and interference events |
| ReceiverStatus | 4014 | Restart detection; antenna and congestion changes are logged; each front end's AGC gain and the receiver's temperature are logged every 30 s |
| ReceiverSetup | 5902 | Product, engine, firmware and serial, logged when they change |

**Satellites.** SVIDs map to UBX gnssId and satellite numbers (4.1.9): GLONASS by slot,
SBAS by PRN, QZSS J01-J10 as 1-10, NavIC I01-I14, BeiDou C01-C63. L-band beams and a GLONASS
satellite whose slot is unknown are left out.

**Signals.** Each SBF signal number (4.1.10) with a counterpart in the reception space of
`common/reception.h` is listed under that ID, with data and pilot components merged as
`nr_signal` merges them:

| SBF | Signal | Reception ID |
|---:|---|---|
| 0 | GPS L1C/A | GPS 0 |
| 3 | GPS L2C | GPS 3 |
| 4 | GPS L5 | GPS 6 |
| 6 | QZSS L1C/A | QZSS 0 |
| 7 | QZSS L2C | QZSS 4 |
| 26 | QZSS L5 | QZSS 8 |
| 33 | QZSS L1S | QZSS 1 |
| 8 | GLONASS L1C/A | GLONASS 0 |
| 11 | GLONASS L2C/A | GLONASS 2 |
| 17 | Galileo E1 | Galileo 0 |
| 20 | Galileo E5a | Galileo 3 |
| 21 | Galileo E5b | Galileo 5 |
| 28 | BeiDou B1I | BeiDou 0 |
| 29 | BeiDou B2I | BeiDou 2 |
| 13 | BeiDou B1C | BeiDou 5 |
| 14 | BeiDou B2a | BeiDou 8 |
| 15 | NavIC L5 | NavIC 0 |
| 24 | SBAS L1C/A | SBAS 0 |

GPS P(Y), GPS and QZSS L1C, QZSS L1CB, L5S and L6, GLONASS P and L3, Galileo E6 and AltBOC,
BeiDou B3I and B2b, NavIC L1 and SBAS L5 have no counterpart there. They still count for their
satellite, and are not given an invented signal ID.

**Time.** ReceiverTime's UTC fields are written into the status's NAV-PVT-layout UTC bytes,
which the RTC policy reads, and only for the epoch of the PVT block just received. The date
and time are valid when the receiver gives them. They are fully resolved once WNSET, TOWSET and
FINETIME are all set and the leap-second count is known. The time accuracy is then 0.5 ms (the
threshold `scst` sets); with TOWSET alone it is 20 ms (2.3, 4.2.12). The RTC policy still
requires three advancing solutions over two seconds and an accuracy of 100 ms or better before
it trusts GNSS UTC.

**Interference.** The status keeps u-blox's scales, so the panel and tag 6 read the same way on
every board:

- **Jamming:** ok (1) with no interference detected. With interference detected (RFStatus band
  modes 2 or 8), it is warning (2) while the receiver holds a fix and critical (3) without one,
  as u-blox defines its states. A notch filter a user set (mode 1) is not interference.
- **Spoofing:** indicated (2) when either RFStatus flag is set (the receiver's authenticity
  tests, or a navigation message failing authentication) and not indicated (1) otherwise.
- **Events:** a change of either state is an interference event, as on the u-blox path.
- **Tag 6 fields:** the "MON-RF" and "NAV-STATUS" uptimes carry the time of the last RFStatus.

**Constellations.** The `gst` reply's satellite list sets the supported mask: G, S, E, C, J, R
and I are GPS, SBAS, Galileo, BeiDou, QZSS, GLONASS and NavIC. Until it arrives, the default
mask stands.

**Satellites and signals by name.** The 10 s log line counts the tracked satellites of each
constellation. Once a minute the observer also names them, constellation by constellation and
by number, each with its elevation in degrees and its C/N0 in dB-Hz, and counts the satellites
tracked on each signal:

```
satellites (elevation deg/C/N0 dB-Hz): G05 61/45, G12 23/38, R03 -/31, E07 12/44, C36 9/40,
J02 4/33, S131 42/41
satellites per signal: GPSL1CA 2, GPSL2C 2, GPSL5 1, GPSL1C 1, GLOL1CA 1, GALE1BC 1, GALE5a 1,
GEOL1 1, GEOL5 1, BDSB1I 1, BDSB1C 1, BDSB2b 1, QZSL1CA 1, QZSL1S 1
```

G, R, E, C, J, I and S are GPS, GLONASS, Galileo, BeiDou, QZSS, NavIC and SBAS; a GLONASS
satellite is named by its slot and an SBAS one by its PRN. An elevation the receiver has not
given reads `-`. The first line shows which satellite a count gained or lost, and a low
elevation marks one seen from far away. The second shows whether a signal that was switched on
is being received: a signal counting no satellite is left out. Signals are named as
`setSignalTracking` names them, and the counts include the signals section 6's table has no
reception ID for. Both lines are for the bench; the collector has the same facts in the
forwarded MeasEpoch and SatVisibility blocks.

**Front ends.** ReceiverStatus ends with one AGCState sub-block for each RF front end (4.2):
the front end's code and antenna, its AGC gain in dB, the normalized variance of its IF samples
(nominal 100) and the share of samples its pulse blanker removes. Every 30 s the observer logs
them, in the receiver's order and under the guide's names, with the receiver's own temperature
and CPU load:

```
front ends (gain dB/variance/blanked %): L5/E5a/B2a 37/98/1, E5b/B2b 46/104/1, GPSL2 51/107/1,
GLOL2 50/100/0, B3 51/98/0, E6 51/93/0, GPSL1/E1 40/96/0, GLOL1 44/96/0, B1 42/98/0; receiver
temperature 27 C, CPU 4%
```

A front end whose PLL is not locked reads `unlocked` (its gain field is the Do-Not-Use value),
one on an auxiliary antenna is prefixed `aux1` or `aux2`, and a temperature of zero in the
block reads `unknown`. The gain is the receiver's measure of the power at its input: it turns
its gain down as that power rises. The hardware manual (4.2.1) gives the amplification ahead
of the module as 65 dB less this gain and allows 15 to 50 dB of it on a single-antenna module,
so a gain between 15 and 50 dB is in range and one at 15 dB is at the limit. The line above is
an L1 and L5 antenna in a quiet place: the L2, B3 and E6 front ends, which that antenna does
not pass, show the least power. On the same unit a noisy bench lowered every gain by about
8 dB, and no satellite was tracked there. A gain that falls with no change of antenna is
therefore noise or interference arriving, and one that rises is an antenna, its amplifier or
its cable giving less. ReceiverStatus's ACTIVEANTENNA bit, which the status line reports, stayed
clear on firmware 1.0.0 with an antenna drawing current, so the gains are the evidence that an
antenna is connected. The observer only logs these values. The collector decodes them from the
forwarded blocks and learns each front end's normal gain (section 7).

## 7. To the collector

The observer pushes on the GNF1 `sbf` feed with SBF record format 1
([DESIGN.md §2](../../docs/DESIGN.md#2-the-gnf1-wire-feeder--collector)): its HELLO says
`"feed":"sbf","sbf":1`, and a WELCOME that does not repeat `"sbf":1` is treated as a refusal,
so nothing is sent to a collector that would misread it. The observer's enrollment must grant
the `sbf` feed, and the collector must be one that knows the format: an older collector, or a
grant of `ubx` alone, refuses the session, and the P3 then sends nothing, its ObserverDetails
included. Upgrade the collector and grant `sbf` before this firmware reaches a P3.

Every CRC-checked block of the 19 the commands in section 4 request (`sbf_control_forwarded`)
goes to the spool whole, sync to padding, as one record stamped with the observer's receive
time, through the same spool and pusher as u-blox records. A record over the inline DATA size
is written as the frame header and then the record, never cut short, and the pusher copies a
backlog out of the spool at most 32 KiB at a time. ObserverDetails and the board's other
telemetry go out on the same session as on every board, and the collector's reception
forecasts run on `sbf` sessions as on `ubx` ones.

The collector checks each block's sync, Length and CRC, and stores it as it stores a dial-mode
SBF capture: the block number, the eight original header bytes and the body, which rebuild the
block byte for byte, with the session and sequence that dedup a replay. It decodes one of them,
ReceiverStatus, and stores the rest undecoded (section 8).

**Front-end gains at the collector.** From each ReceiverStatus the collector takes the front
ends' gains and learns a baseline for each one: the median of its quiet minutes over six
hours, served after ten minutes. A gain 4 dB below its baseline makes the station's RF
`station_rf_degraded`, or `jamming_detected` when something corroborates it, and 10 dB below
is `jamming_detected` on its own. A gain 4 dB above its baseline, which is what an antenna,
amplifier or cable giving less looks like, is `station_rf_degraded` and never a jamming
claim. The station's row in the collector's `observers` feed lists every front end with its
gain and departure, and the blocks themselves are kept with the station's other RF evidence
([DEFENSE-PNT.md §2](../../docs/DEFENSE-PNT.md#2-jamming-detection-the-rf-front-end)). Changing
the antenna, its amplifier or its cable moves every gain. The collector reports the station
as degraded while a gain sits away from its baseline and, by default, learns the new level
once it has held for a day; give the station a new `power_model_epoch` at the collector to
have the baselines learned again at once. An antenna unplugged and put back within the day
needs neither: the gains come back to the baseline they left.

**Line budget.** Estimated from the block sizes in 4.2 for about 45 tracked satellites: the
1 Hz stream is about 2.7 KB/s, most of it MeasEpoch, and the navigation pages about 1.7 KB/s,
more where BeiDou's geostationary satellites send a page every 0.6 s. That is under half of
COM2's 11.5 KB/s at 115200 baud; ReceiverStatus's output-congestion flag, which the observer
logs, says when it is not. Each added signal (section 4) is 12 more bytes of MeasEpoch for
every satellite that sends it: on a P3 tracking 22 satellites indoors the line carried about
2.4 KB/s with every signal on, against about 2.2 KB/s for as many satellites before. The
PSRAM spool's 4 MiB holds about 15 minutes of this output
through a collector outage, and the historian keeps every block, about 0.4 GB a day per P3 at
this rate, under its raw retention.

## 8. What it does not do yet

- **Collector decoding.** The collector stores the blocks and decodes only ReceiverStatus
  (section 7): the P3's
  navigation pages do not update live constellation state, and its PVT, measurement,
  visibility and RF status blocks do not yet feed the station integrity checks, reception
  records or interference telemetry the u-blox records feed. Those mappings must keep fields
  SBF does not provide unavailable rather than invent u-blox equivalents (see the [P3
  integration proposal](../../docs/proposals/OBSERVER-TIME-MOTION-AND-SEPTENTRIO.md#custom-mosaic-g5-p3-observer)).
  The P3's ObserverDetails, including tags 6, 17 and 20 built from this status, go out as on
  every board.
- **Front-end telemetry in ObserverDetails.** The AGC gains, sample variances, blanking and
  receiver temperature of section 6 are logged, and reach the collector inside the forwarded
  ReceiverStatus blocks, where it keeps their series and a baseline of each gain. They are
  not in ObserverDetails, whose RF fields carry u-blox MON-RF's scales, so the panel and the
  board report do not show them.
- **Navigation pages of the added signals.** GPS L1C, SBAS L5, BeiDou B2b and QZSS L6, L1C and
  L1S are tracked and measured (section 4), and their measurements are in the forwarded
  MeasEpoch. Their navigation page blocks are not in `Res3`'s list, so their messages are not
  forwarded yet.
- **PPS.** The PPS metadata u-blox gives in TIM-TP has no counterpart here, so tag 8's TIM-TP
  validity stays clear.
- **Higher baud rates.** 115200 baud carries the streams above. A faster stream needs a higher
  rate, set first on the receiver and then on the ESP32.

## 9. Bring-up checks

1. **Identity.** After boot the log names the receiver: product, engine, firmware, serial. A
   receiver that is not a mosaic-G5 is named as such.
2. **Configuration.** "receiver COM2 configured" appears with no command errors. "COM2 RTS/CTS
   confirmed" appears, and on a scope `ESP_UART_RTS_N` is low while the ESP32 has room.
3. **Data.** The 10 s log line shows the SBF count rising, no CRC errors, no scrambled epochs,
   and tracked counts that agree with the receiver's own web or USB view. The forwarded count
   rises with it, and the bytes per 10 s show the actual line load against section 7's
   estimate; the receiver status line reports no output congestion.
4. **Collector.** The pusher log shows `feed=sbf`, the collector's `nav_frames` gains rows with
   an `sbf_header` for this observer, and each rebuilds with `SBFWire`. Navigation page blocks
   (4017 to 4219) arrive within a minute of a fix.
5. **Time.** With an antenna and a fix, the RTC is set from GNSS (RTC-SERVICE.md), and its
   second agrees with the receiver's ReceiverTime. The PPS lamp flashes for a tenth of a
   second each second, and the timing record's GNSS pulse width is near 100 ms.
6. **Restart.** Resetting the receiver with SW4 is followed by a new configuration within
   about 30 s and no GPIO13 pulse.
7. **Recovery.** Lift the receiver's TXD2 test point to stop its data. After 30 s the observer
   configures again; after the failed configuration it pulses GPIO13 once and logs it.
8. **Flow control.** An ESP32 busy long enough to fill its FIFO deasserts RTS, and the receiver
   holds its output; no CRC errors follow.
9. **Front ends.** Every 30 s the log lists each front end's gain, variance and blanking and
   the receiver's temperature (section 6). With the antenna attached every gain is between 15
   and 50 dB and every variance near 100; moving the antenna away from interference raises the
   gains. The receiver's temperature reads a few degrees above the board's sensors: 27 C
   against 22 C on an open bench.
10. **Signals.** After the first configuration the log lists the signals tracked, those the
    observer enabled and any the receiver does not offer (section 4). Reset the ESP32 alone,
    with SW1: the list comes back with nothing enabled, the receiver status line shows an
    uptime that kept running, and the next tracked counts are the ones before the reset. Once
    a minute the log names the satellites and counts them per signal (section 6); with sky in
    view the added signals count satellites within a few minutes.
