# Expected reception and edge alarms

The collector supplies a forecast; the observer compares it with fresh local
measurements and owns its alarm state. The collector independently evaluates the
reported observations. A local alarm does not wait for an operator or a server
verdict. A reception deficit is an abnormal-reception warning, not proof of
interference.

The portable C and Go contract and comparison rules are implemented. Transport,
forecast generation and persistent reporting are being integrated.

## Version 1 contract

All integers are big endian. GNF1 frame `0x0b` carries an expectation. Its 40-byte
header contains version 1, five slots, 60 seconds per slot, entry count, a 64-bit
expectation ID, 64-bit Unix issue time, signed latitude/longitude in 1e-7 degrees,
16-bit site radius in metres, 16-bit alarm/recovery dwell seconds, and byte-sized
minimum expected count, minimum missing count and missing percentage. Three
reserved bytes are zero. Each of at most 128 entries adds four bytes: GNSS ID,
satellite ID, canonical signal ID and a five-bit slot mask. Signal 255 means a
satellite-level check. A set slot means the entry is expected throughout that
minute, not just at its beginning.

The 76-byte assessment carries version, valid constellation mask, alarm mask, a
zero reserved byte, expectation ID, sample Unix time, monotonic uptime, persistent
boot/event IDs, 128 matched-entry bits, and eight expected and eight observed
counts. The collector recomputes counts from the entry bits. It keeps the edge
verdict separate from its own verdict.

Unknown or expired predictions, missing/stale telemetry and insufficient expected
coverage interrupt onset/recovery timers. They never clear an existing alarm.
Both transitions require consecutive fresh samples; a gap over 15 seconds breaks
the dwell. Replacing a forecast does not itself reset a continuous deficit.

## Local indication

An affected constellation alternates green and yellow on a **500 ms full cycle**:
250 ms green, then 250 ms yellow. Other constellation columns retain their normal
indication. The uplink column remains a connectivity indication. An unresolved
alarm remains visible when its prediction becomes stale; the diagnostic record
distinguishes stale coverage from a current measured deficit.

## Receiver evidence

NAV-SAT supplies satellite tracking quality, signal strength and navigation-use
flags. NAV-SIG supplies individual signal identities and quality. The comparison
must distinguish code-locked tracking from a satellite merely listed by the
receiver, and from whether it contributes to the navigation solution.
[u-blox M9 SPG 4.04 interface description, sections 3.15.13 and 3.15.15](https://content.u-blox.com/sites/default/files/u-blox-M9-SPG-4.04_InterfaceDescription_UBX-21022436.pdf).

Data/pilot components in the same supported signal group count once. Exact
receiver/firmware output still needs qualification; an absent NAV-SIG stream is
unknown signal coverage, not a measured loss of every expected signal.
