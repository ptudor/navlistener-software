# Septentrio receiver on COM2 (P3 revision A)

How the universal image runs the mosaic-G5 P3 on the P3 observer (`INTSAT_MOSAIC_P3`
revision A): the UART, flow control and reset lines, the commands it sends, the SBF it reads
and how that becomes the observer's receiver status. The code is the `sbf` component
(`components/sbf`) and the SBF task in `main/receiver.c`. Field layouts, command syntax and
reply formats are from the mosaic-G5 firmware reference guide v1.0.1; section numbers below
refer to it, except where the hardware manual (v2.0.0) is named.

Nothing here has run against a receiver yet. The host tests build every block field by field
from the reference guide's tables; they are not captures. Section 8 lists what the first
P3 must confirm.

## 1. When it runs

A manifest that lists `GPS_MOSAIC_G5_P3` on a board row with COM2 flow-control pins selects
this path in place of the u-blox one; on the P3 those pins are GPIO17 (RTS) and GPIO16 (CTS),
and GPIO13 drives the receiver's reset. A manifest that lists the mosaic-G5 beside a u-blox
receiver, or a row without the flow-control pins, configures no receiver. Receiver bytes on
this path never reach the UBX parser.

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
| `scst, usec500` | the receiver time within 0.5 ms of GNSS time once FINETIME is set (2.3), which bounds the time accuracy in section 6 |
| `sso, Res1, COM2, PVTGeodetic+ReceiverTime+MeasEpoch+SatVisibility+ReceiverStatus+RFStatus, sec1` | the 1 Hz stream |
| `sso, Res2, COM2, ReceiverSetup, OnChange` | the receiver's identity, every 60 s and on change |
| `gst` | the satellites it may track, read as the constellation mask |

The settings are RAM-only, as on the u-blox path: nothing is saved to the receiver's boot
configuration. `Res1` and `Res2` are the streams the receiver never saves and resets at boot
(3.2, `setSBFOutput`), so the observer's output cannot land in a saved configuration or take
over a stream someone set up for the receiver's own USB port. A command answered with an
error is logged and counted, and the configuration carries on; a refused `setCOMSettings`
leaves CTS ignored.

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

The ESP32 never resets the receiver at its own boot: an observer that restarts for an update
keeps the receiver tracking.

## 6. From SBF to the receiver status

`sbf_status` fills the same `gnss_status_t` the u-blox path fills, so the panel, ObserverDetails
tag 6, the reception assessment and the RTC read one receiver-neutral state. All blocks are
CRC-checked, and every field is bounds-checked against the block's Length and SBLength
(4.1.12).

| Block | Number | Status it feeds |
|---|---:|---|
| MeasEpoch | 4027 | Tracking: a satellite counts when a signal on the main antenna has a pseudorange and a C/N0 of at least 1 dB-Hz; its C/N0 is the strongest such signal's. An epoch scrambled for want of the Measurement Availability permission (CommonFlags bit 7) is not used |
| SatVisibility | 4012 | Elevations, rounded to whole degrees; unknown (127) when older than 5 s |
| PVTGeodetic | 4007 | A fix: a GNSS solution type (1, 2, 4-8, 10, not 3 "fixed location", which reports entered coordinates) with Error 0 |
| ReceiverTime | 5914 | The UTC calendar for the RTC, paired with the PVT of the same epoch |
| RFStatus | 4092 | Jamming and spoofing states and interference events |
| ReceiverStatus | 4014 | Restart detection; antenna and congestion changes are logged |
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

## 7. What it does not do yet

- **Collector data.** This path sends the collector no receiver data of its own. GNF1's raw-nav
  records and its reception, solution and jamming telemetry are u-blox-shaped. An SBF
  representation preserving each block's header needs format negotiation in the feeder,
  collector, grants, spool and replay together (see the [P3 integration
  proposal](../../docs/proposals/OBSERVER-TIME-MOTION-AND-SEPTENTRIO.md#custom-mosaic-g5-p3-observer)).
  The P3's ObserverDetails, including tags 6, 17 and 20 built from this status, go out as on
  every board.
- **Raw navigation and PPS.** Raw navigation bits (the `RawNavBits` group) are not requested.
  The PPS metadata u-blox gives in TIM-TP has no counterpart here, so tag 8's TIM-TP validity
  stays clear.
- **Higher baud rates.** 115200 baud carries the 1 Hz stream. A faster stream needs a higher
  rate, set first on the receiver and then on the ESP32.

## 8. Bring-up checks

1. **Identity.** After boot the log names the receiver: product, engine, firmware, serial. A
   receiver that is not a mosaic-G5 is named as such.
2. **Configuration.** "receiver COM2 configured" appears with no command errors. "COM2 RTS/CTS
   confirmed" appears, and on a scope `ESP_UART_RTS_N` is low while the ESP32 has room.
3. **Data.** The 10 s log line shows the SBF count rising, no CRC errors, no scrambled epochs,
   and tracked counts that agree with the receiver's own web or USB view.
4. **Time.** With an antenna and a fix, the RTC is set from GNSS (RTC-SERVICE.md), and its
   second agrees with the receiver's ReceiverTime.
5. **Restart.** Resetting the receiver with SW4 is followed by a new configuration within
   about 30 s and no GPIO13 pulse.
6. **Recovery.** Lift the receiver's TXD2 test point to stop its data. After 30 s the observer
   configures again; after the failed configuration it pulses GPIO13 once and logs it.
7. **Flow control.** An ESP32 busy long enough to fill its FIFO deasserts RTS, and the receiver
   holds its output; no CRC errors follow.
