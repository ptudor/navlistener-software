# Expected reception and edge alarms

The collector supplies a forecast; the observer compares it with fresh local
measurements and owns its alarm state. The collector independently evaluates the
reported observations. A local alarm does not wait for an operator or a server
verdict. A reception deficit is an abnormal-reception warning, not proof of
interference.

This exchange is implemented for the ESP32 observer and the Go collector. The
C router feeder continues forwarding observations without local reception alarms.

## Configure a station

Add an explicit fixed-site profile to the collector's TOML configuration. Use
the authenticated observer ID, surveyed position and actually enabled signals;
do not infer those from whichever signals survived an incident.

```toml
[reception]
# Optional: SHA-256 hex digest of a separate operator bearer credential.
# Without this credential, forecasts still work and manual snapshots are disabled.
# operator_token_sha256 = "<64 hex characters>"

[[reception.station]]
observer = "roof-observer"
position = [35.0, 140.0, 50.0] # replace with latitude, longitude, ellipsoid height m
signals = ["0:0", "2:0", "3:0", "6:0"] # GPS L1, Galileo E1, BeiDou B1I, GLONASS L1
per_signal = false           # NAV-SAT satellite counts; true selects NAV-SIG groups
elevation_mask_degrees = 20
radius_m = 1000
alarm_seconds = 30
clear_seconds = 30
min_expected = 4
min_missing = 3
missing_percent = 50
```

With these defaults, 2 observed GPS satellites against 12 expected raises a GPS
alarm after 30 seconds of consecutive fresh deficit measurements. Both the
minimum missing count and percentage must be met. Recovery requires 30 seconds
of fresh measurements below that deficit threshold. Fewer than four expected
entries makes that constellation unknown, rather than certifying good reception.
These are configurable engineering thresholds, not integrity protection levels.

The collector refreshes a five-minute forecast every 30 seconds. It uses only
the station's organization view, or the public view for an unassigned station.
An entry needs healthy decoded navigation data, a fresh propagated position and
a navigation witness from another receiver in the preceding 60 seconds. Existing
ephemeris age and replay limits still apply. Every minute is sampled at ten-second
intervals with a two-degree margin above the configured elevation mask. The
assessed receiver cannot be its own independent witness.

This is a conservative expectation from available network evidence. It is not a
complete all-satellite catalogue, an antenna gain model, a learned C/N0 baseline,
or proof that the witnessed broadcasts agree bit for bit. Missing orbits and
signals outside the configured profile are omitted; the edge marks unsupported
constellations unknown. A
single-station network cannot supply an independent witness. Trees, buildings,
antenna placement and receiver configuration still affect the comparison.

A fresh local position outside the configured radius invalidates the forecast.
Loss of position does not invalidate the administratively surveyed fixed site.
After accepting a forecast, the edge anchors validity to monotonic elapsed time;
a wall-clock difference over five seconds makes coverage unknown. Expired models
stop supporting new decisions. Already active alarms remain latched, including
after reboot when the journal is available.
Forecast acceptance and remote snapshot expiration require usable system wall
time. This clock is not an authenticated timing reference.

## Version 1 contract

The feeder negotiates `"reception":1` in its authenticated GNF1 HELLO. Older
feeders receive no reception controls. All integers are big endian. GNF1 frame
`0x0b` carries an expectation:

| Offset | Bytes | Meaning |
|---:|---:|---|
| 0–3 | 4 | Version 1, five slots, 60 seconds per slot, entry count |
| 4 | 8 | Nonzero expectation ID |
| 12 | 8 | Unix issue time, seconds |
| 20, 24 | 4 each | Signed latitude, longitude in 1e-7 degrees |
| 28 | 2 | Site radius, metres |
| 30, 32 | 2 each | Alarm and recovery dwell, 5–120 seconds |
| 34–36 | 3 | Minimum expected count, minimum missing count, missing percentage |
| 37–39 | 3 | Reserved, zero |
| 40… | 4 each | Up to 128 entries |

Each entry contains GNSS ID,
satellite ID, canonical signal ID and a five-bit slot mask. Signal 255 means a
satellite-level check. A set slot means the entry is expected throughout that
minute, not just at its beginning.

The 76-byte assessment travels in ObserverDetails tag 17 for current status and
tag 18 for historical local transitions:

| Offset | Bytes | Meaning |
|---:|---:|---|
| 0–3 | 4 | Version 1, valid constellation mask, alarm mask, reserved zero |
| 4 | 8 | Expectation ID |
| 12 | 8 | Assessment Unix time, seconds; zero when unknown |
| 20 | 8 | Receiver measurement uptime, milliseconds |
| 28, 36 | 8 each | Boot and event identifiers of the last persisted transition |
| 44 | 16 | Matched-entry bitmap; entry i is bit i%8 in byte i/8 |
| 60, 68 | 8 each | Expected and observed counts indexed by GNSS ID |

Mask bits use UBX GNSS IDs; IMES bit 4 is reserved. The collector recomputes counts
from the entry bits and the exact expectation. It keeps the edge verdict separate
from its own verdict, rejects stale live assessments and does not advance dwell
on a repeated measurement uptime. This independent calculation still depends on
the reported observations; it does not authenticate the RF environment.

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

The panel uses its own refresh task, so slow environmental conversions do not
stretch the blink cycle. Existing brightness controls still apply. Each alarm or
recovery also requests a bounded receiver/environment snapshot.

## Repoll and submit

`POST /gnss/api/v2/station-snapshot` accepts a separate operator bearer credential
whose hash is configured above. Read credentials do not authorize commands. This
endpoint requires HTTPS at the collector's API front and rejects browser Origin
requests. Example JSON:

```json
{"observer_id":"roof-observer","request_id":"202609260001","scopes":7}
```

The positive decimal string preserves a 64-bit request ID. Use a new ID for each
operation. Scope bits are environment=1, receiver=2 and RF=4. GNF1 frame `0x0c`
contains version 1, scopes, two reserved zero bytes, the 8-byte request ID and an
8-byte Unix expiration (20 bytes total). There is one outstanding request per
station, it expires after 30 seconds, and delivery is retried every five seconds
on the current authenticated session. Repeating the most recent ID with the same
scopes is idempotent. A completed edge request resends its result without polling
again. Pending requests are held in collector RAM and do not survive its restart.

The UART task polls NAV-SAT, NAV-SIG, MON-RF, NAV-STATUS and NAV-PVT; the board task
attempts fresh environmental conversions with a five-second minimum interval.
The report includes ObserverDetails tag 19: version, status, completed scope bits,
reserved zero, and request ID (12 bytes). Status is complete=1, partial=2,
expired=3 or unsupported=4. Environment completion means a conversion was
attempted: individual sensor validity remains authoritative. Receiver completion
requires a fresh NAV-SAT after the request, and RF completion a fresh MON-RF.
`GET /gnss/api/v2/station-snapshot?observer_id=roof-observer`, with the same
operator credential, reports pending, reported or expired state and the result.

## Reporting and retention

The edge emits a current assessment each second and saves alarm/recovery and
coverage changes to the separate 128-record reception journal. On reconnect it
replays that retained history, oldest first, while current reports continue.
Uploading never deletes the local records. The navigation/telemetry transport
spool remains bounded RAM; journal records survive reboot and can be uploaded
again on the next connection. Delivery uses the existing GNF1 ACK rules, including
their finite-buffer and skipped-sequence limits.

Private observer feeds expose `board.reception`, `reception_stale` (15 seconds),
`reception_events` (up to 32 deduplicated local transitions), and `snapshot`.
The reception sample has the edge assessment in `details.reception` and the
independent result in `collector_reception`, including a disagreement mask and
whether counts represent signal groups. Integrity Station shows both verdicts
and includes either active alarm in its station warning state. Coverage validity
and freshness are separate from a latched alarm. Public feeds omit board data.

When configured, the historian persists the original ObserverDetails bodies and
edge reports under the existing private `observer_samples` rules. Historical
tag 18 records do not advance the live alarm machine. Forecasts and the collector's
own machine are in memory; a collector restart begins its confirmation again.
The journal's stored counts remain readable, but interpreting an old match bitmap
requires its forecast, which this version does not archive.

## Receiver evidence

NAV-SAT supplies satellite tracking quality, signal strength and navigation-use
flags. NAV-SIG supplies individual signal identities and quality. The comparison
must distinguish code-locked tracking from a satellite merely listed by the
receiver, and from whether it contributes to the navigation solution.
[u-blox M9 SPG 4.04 interface description, sections 3.15.13 and 3.15.15](https://content.u-blox.com/sites/default/files/u-blox-M9-SPG-4.04_InterfaceDescription_UBX-21022436.pdf).

Data/pilot components in the same supported signal group count once. Exact
receiver/firmware output still needs qualification; an absent NAV-SIG stream is
unknown signal coverage, not a measured loss of every expected signal.
Profiles use the canonical IDs: GPS 4→3 and 7→6; Galileo 1→0, 4→3 and 6→5;
BeiDou 1→0, 3→2, 6→5 and 7→8; QZSS 5→4 and 9→8. Other IDs are unchanged.

The NAV-SIG UART1 key is `0x20910346` in M9 SPG 4.04, M10 SPG 5.10 and
[X20 HPG 2.11](https://content.u-blox.com/sites/default/files/documents/u-blox-X20-HPG-2.11_InterfaceDescription_UBXDOC-304424225-21617.pdf).
The firmware enables it in RAM. Host tests cover the C/Go wire contract, geometry,
freshness, alarm/recovery, reboot restoration, TLS delivery, snapshot retries and
LED phase boundaries. Firmware builds do not substitute for an antenna-connected
bench test of each receiver profile.
