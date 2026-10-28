# Receiver firmware updates: bridge mode and our own updater

Status: design, not implemented. Nothing in the firmware today puts a u-blox receiver into
safeboot or writes its flash.

## Why

u-blox releases receiver firmware rarely, and its flashing protocol is not published. The
interface descriptions document only UBX-UPD-SOS (backup and restore) in the UBX-UPD class,
and the ZED-X20P integration manual (UBXDOC-963802114-12901, 3.19) says only that an update
runs over UART1, I2C, SPI or USB in safeboot mode. An update is therefore something we must
be able to do once in a while, on the bench or in the field. We should not depend on a
vendor desktop tool being able to reach the receiver.

The plan has three steps:

1. A bridge mode that lets u-blox's own tool update a receiver through the ESP32.
2. Captures of those sessions with an in-line logic analyser, from which we describe the
   protocol in this repository.
3. Our own updater in the firmware, built from that description and fed by the signed
   update path.

Firmware versions belong to the exact module ordering code. The ZED/X20 boards fit the
ZED-X20P-00B, which runs HPG 2.02. HPG 2.10 is listed for the ZED-X20P-01B, a different
ordering code, and is not an update for the -00B. Read MON-VER (`MOD=` and `FWVER=`) before
choosing an image. The updater must refuse an image whose stated product does not match.

## What the vendor documents

- **Entering safeboot.** Hold `SAFEBOOT_N` low at power-up. On the ZED-X20P, sending
  UBX-UPD-SAFE also works (integration manual 3.17). In safeboot GNSS operation stops, and
  the ZED-X20P answers on UART1, I2C and SPI, but not USB.
- **UART1 training sequence.** On UART1 the host first sends the raw bytes `0x55 0x55` at
  9600 baud. These are not a UBX message. The host then waits at least 2 ms before anything
  else. These are the trigger bytes to watch for.
- **SAFEBOOT_N.**
  - ZED-X20P and MAX-M10S: an internal pull-up and an internal 1 kΩ series resistor.
  - NEO-M9N: a pull-up of 30–130 kΩ.
  - MAX-M10S: the pin is tied inside the module to TIMEPULSE through that 1 kΩ, so nothing
    may hold TIMEPULSE low at power-up. On the MAX board it drives the PPS path.
- **RESET_N.** On every one of these receivers it has a 7–13 kΩ pull-up and must not carry a
  capacitor to ground. On the M9 and M10 a reset clears the battery-backed configuration
  and forces a cold start, so treat it as a recovery tool.
- **Power.** Every board can switch the receiver's supply with `GNSS_EN`, and a power cycle is
  a full reset. The safeboot loader is in ROM, so a receiver whose flash image is bad still
  answers in safeboot on UART1. A failed update can be retried.

## Bridge mode

This mode lets u-blox's update tool, on a computer attached to the console USB port, talk
straight to the receiver's UART1 through the ESP32. It is a session, entered and left on
purpose:

- Entered by a commissioning-console command, `receiver bridge <maintenance reason>`, beside
  the `rtc` commands (`CONFIG_NVF_COMMISSION_CONSOLE`). It has no remote command path.
- The reason and the receiver's MON-VER, read just before entry, go into the journal.
- Left only by a reset or a power cycle. Nothing returns the board to normal operation
  mid-session, so a half-finished update is never read back as receiver traffic.

On entry the firmware:

1. stops the receiver task and its UART1 driver, and stops reporting receiver telemetry. It
   records, in the journal and the next ObserverDetails, that the receiver is being serviced,
   so the gap does not read as an outage or as interference;
2. silences the console log, because log text would corrupt the byte stream;
3. copies bytes both ways without interpreting them, at the rate the host asks for.

Copying bytes is easy. Following the host's line rate is the hard part. The tool may move
between 9600 baud (training) and a faster rate (the transfer). The S3's USB Serial/JTAG
console does not tell firmware what line coding the host set. So bridge mode switches the
port to the S3's native USB (TinyUSB CDC-ACM). That stack reports SET_LINE_CODING, so the
bridge sets UART1 to each rate the host chooses. The console returns to USB Serial/JTAG after
the reset that ends the session. If a capture shows the tool keeps one rate, a fixed-rate
bridge on the existing console is enough. In that case this is a build option, not a USB
stack change.

While bridging, the firmware also watches the host-to-receiver stream:

- **Training sequence.** It notes `0x55 0x55` at 9600 baud, and UBX-UPD frames once
  checksum-valid UBX framing is recognised. It journals when an update starts, so an
  interrupted session is visible later.
- **Capture.** It writes a timestamped copy of both directions to PSRAM, in fixed-size
  records of direction, time, rate and bytes. A `receiver capture` command dumps that copy
  after the session. This is a software logic analyser that needs no probe clips, and it
  sees exactly what the receiver saw. It cannot see line timing finer than its own buffer
  latency, which is what the in-line analyser is for.

## Capturing and describing the protocol

Capture with an in-line logic analyser on UART1 TX and RX (and `SAFEBOOT_N`, `RESET_N` and the
receiver's supply, where reachable), with u-blox's tool driving an update through the bridge.
For each receiver family (ZED-X20P, MAX-M10S, NEO-M9N) record:

- how safeboot is entered (pin or UBX-UPD-SAFE), and how long the host waits for the ROM;
- the training sequence, and every rate change with its timing;
- the identification the tool asks for and the answers (chip, flash, current image);
- the transfer: block size, framing, per-block acknowledgement or retry, erase and write
  order, and pauses for flash erase;
- verification and completion: what proves the image was written, and how the receiver
  leaves safeboot;
- one deliberately interrupted session, and the recovery that followed.

Write the description as a protocol document beside this one, with test vectors taken from
the captures. Keep the captures themselves out of Git: they contain u-blox's firmware image.
The description we write is our own work; the images and the vendor tool remain under their
licences. Check those terms before publishing the description.

## Our own updater

Once the description exists, the firmware updates the receiver itself. Only an operator
starts it: a commissioning-console command on the bench, or a signed update request in the
field. A field update is not applied unattended until it has been proven on the bench.

1. **Stage.** The image arrives through the existing signed update path
   ([SOFTWARE-UPDATES.md](SOFTWARE-UPDATES.md)). Its SHA-256, size and target product (the
   `MOD=` and the ordering code it applies to) are in the signed metadata. It is staged
   in the inactive application slot, 4 MiB and free between ESP32 updates, so it survives a
   reset. It is hashed again before use.
2. **Check.** Read MON-VER and compare `MOD=` and the current `FWVER=` with the metadata.
   Refuse a different product, and anything that is not a newer image unless the request
   says otherwise.
3. **Enter safeboot.**
   - On a board whose revision wires `SAFEBOOT_N` to the ESP32 or to an I/O expander: hold
     it low, power-cycle through `GNSS_EN`, release.
   - Otherwise send UBX-UPD-SAFE where the receiver supports it. Failing that, an operator
     holds the board's SAFEBOOT button while the firmware power-cycles the receiver.
   - Then send the training sequence.
4. **Write.** Run the protocol from the staged copy, block by block, with the
   acknowledgement and retry rules the captures show. Journal progress, so an interrupted
   update resumes from the start of the image, never from an assumed position.
5. **Verify.** Power-cycle, read MON-VER, and require the new `FWVER=`. Then run the normal
   receiver configuration and report the result in the update status. Note that the receiver's
   RAM settings are lost at every power cycle, so the firmware applies them again in any case.
6. **Recover.** A receiver left in safeboot still answers on UART1, so the updater retries from
   the staged copy. Power-cycling through `GNSS_EN` is the last resort before an operator
   visit.

## Hardware that makes this easier

On revision A boards the ESP32 drives only the receiver's UART1 and supply.
`SAFEBOOT_N` and `RESET_N` reach test points at best. The hardware repository's revision B
notes add a SAFEBOOT and a RESET button to each u-blox board, so an operator can force
safeboot with no firmware involved. Where a pin is free they also give the ESP32, or the
ZED/X20's I/O expander, a drive-low connection to the same nets.
