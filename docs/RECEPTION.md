# Expected reception and edge alarms

The collector supplies an availability forecast and, to version 2 observers, a
received-power companion. The observer compares both with fresh local measurements
and owns its alarm state. The collector independently evaluates the reported
observations. A local alarm does not wait for an operator. A reception deficit or
C/N₀ departure is an abnormal-reception warning, not proof of interference.

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
power_model_epoch = "antenna-1" # change after antenna/cable/receiver/site changes
power_min_deviation_dbhz = 6
power_mad_multiplier = 4
power_min_support_days = 3
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
complete all-satellite catalogue or proof that the witnessed broadcasts agree bit
for bit. Missing orbits and
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

## Received-power histories

Received-power history is for a fixed receiver, antenna, cable path and site. The
receiver reports carrier-to-noise density, C/N₀ in dB-Hz; this is not calibrated
input power in dBm. Receiver firmware, AGC behavior, antenna gain, cable loss,
nearby obstructions and the local noise floor all affect it. Those dependencies
make a stable installation useful for comparison and make a mobile installation
or an unrecorded hardware change unsafe for reuse.

GPS satellites repeat nearly the same ground track on a sidereal-day cadence. The
collector keys each station, satellite and sidereal phase into five-minute cells,
retains up to eight distinct completed passes, and uses the median and median
absolute deviation (MAD) as its reference. Repeated reports during one pass are
averaged and count as one support day. The observer independently maintains
ten-minute GPS cells with up to four completed passes. A reference is unknown until
at least `power_min_support_days` distinct passes exist; the supported range is
2–4 and the default is 3.

With the defaults, an entry is anomalous when its absolute departure is at least
`max(6 dB-Hz, 4 × MAD)`. A mature reference rejects such a sample from future
training, limiting abrupt poisoning. The entry results use the availability
profile's `min_expected`, `min_missing` and `missing_percent` quorum to form a
constellation result. Missing observations, an immature cell and a stale forecast
remain unknown; they are never converted to zero power.

With the historian configured, the collector stores its compact station model in
`reception_power_models`, restores it at startup, checkpoints changed models every
five minutes, and makes a final checkpoint during orderly shutdown. The S3 stores
its local model as CRC-protected, alternating blobs in the diagnostic NVS partition.
It checkpoints an identity reset immediately and, within a boot, subsequent changes
no more often than once per six hours. These stores let the collector and observer
survive restarts independently; without the historian, the server model starts cold.

The server sends a stable site identity derived from the observer ID, surveyed
position, configured signal set, availability and power policies, and
`power_model_epoch`. The observer clears its local model when that identity changes.
Change `power_model_epoch` before using a different antenna, cable, receiver,
mounting location or any other change that invalidates the old RF path. The new epoch
also discards the station's stored AGC baseline ([DEFENSE-PNT.md](DEFENSE-PNT.md#2-jamming-detection-the-rf-front-end)). The
software cannot discover every physical change on its own.

Local and delivered references are evaluated separately. The received-power panel
alarm advances only when both references cover the same entries, both mark enough
of those entries anomalous, and the configured alarm dwell completes. Recovery also
requires joint agreement for the clear dwell. Local-only, remote-only and disagreement
states remain visible in live telemetry. Material validity, alarm and conflict
transitions are journaled when their model identities can be represented; a transient
gap before a new companion arrives holds the alarm and does not fabricate a joint
transition. Both histories ultimately come from the same antenna and receiver stream,
so their agreement is one RF physics gate, not two independent spoofing gates.

The shipped edge model intentionally learns GPS only. The collector can model all
configured satellite-level NAV-SAT observations, but only GPS can form the joint
edge alarm. Use `per_signal = false` for the end-to-end received-power gate; the
server does not currently build signal-level references from NAV-SIG. A transmitter
that reproduces the expected per-satellite levels, slow baseline poisoning, multipath,
snow or water on the antenna, and legitimate RF-path changes can evade or trigger
this check. It must remain one input to the broader PNT defense.

## Version 1 availability contract

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

## Version 2 received-power contract

The ESP32 advertises `"reception":2` in its authenticated HELLO. It receives the
unchanged 0x0b availability forecast plus GNF1 frame 0x0d, a power companion bound
to that exact expectation ID and entry order. A version 1 client continues to
receive availability forecasts and never receives 0x0d. Deploy the collector
before version 2 firmware because an older collector treats the new ObserverDetails
tags as unsupported telemetry.

The 0x0d payload is 40 bytes plus 16 bytes per availability entry, up to 2,088
bytes. Its payload version is 1; all multi-byte fields are big endian.

| Offset | Bytes | Meaning |
|---:|---:|---|
| 0–3 | 4 | Payload version 1, five slots, 60 seconds per slot, entry count |
| 4 | 8 | Nonzero availability expectation ID |
| 12 | 8 | Nonzero server model ID |
| 20 | 8 | Unix issue time; equal to the availability forecast |
| 28 | 8 | Nonzero stable site/configuration identity |
| 36 | 1 | Minimum absolute deviation, dB-Hz |
| 37 | 1 | MAD multiplier |
| 38 | 1 | Minimum distinct-pass support, 2–4 |
| 39 | 1 | Reserved, zero |
| 40… | 16 each | Power entries in the exact 0x0b entry order |

Each power entry contains a five-bit valid-slot mask, five expected C/N₀ bytes,
five MAD bytes and five support-day bytes. The expected, MAD and support arrays
are indexed by the same minute slot. A value is usable only when its valid bit is
set; an invalid cell is unknown.

ObserverDetails tag 20 carries the current 256-byte report:

| Offset | Bytes | Meaning |
|---:|---:|---|
| 0–7 | 8 | Payload version, entry count, model flags, local valid/alarm masks, remote valid/alarm masks, reserved zero |
| 8, 16, 24 | 8 each | Expectation ID, server model ID, local model ID |
| 32, 40 | 8 each | Assessment Unix time and receiver measurement uptime in milliseconds |
| 48 | 16 | Observed-entry-valid bitmap |
| 64 | 128 | C/N₀ bytes indexed by expectation entry |
| 192, 208 | 16 each | Local-model valid and anomalous entry bitmaps |
| 224, 240 | 16 each | Delivered-model valid and anomalous entry bitmaps |

Model flag bit 0 means the local model participated and bit 1 means the delivered
model participated. Its corresponding model ID must be nonzero. Entry bits past
the reported count are zero. Model-valid entries must also have an observed C/N₀,
and an anomalous bit cannot exist outside its model's valid bitmap. The aggregate
masks use UBX constellation bit positions, keep IMES bit 4 clear, and use the same
quorum as availability.

The collector locates the exact model it delivered, recomputes the remote entry
comparison from the observed C/N₀ array, and independently advances remote and
joint dwell machines. `collector_reception_power.report_disagreement_mask` marks
a constellation where the edge's delivered-model result differs from that
recomputation. `model_conflict_mask` marks constellations with at least one jointly
covered entry where the recomputed remote model and reported local model disagree.
A stale report, unknown expectation, model mismatch or repeated measurement
uptime cannot advance a dwell.

ObserverDetails tag 21 carries a retained 68-byte power transition. Bytes 0–7
hold version, model flags, local valid/alarm, remote valid/alarm and joint
valid/alarm masks. The next seven 8-byte fields are expectation ID, remote model
ID, local model ID, assessment Unix time, measurement uptime, boot ID and event
ID. Bytes 64–67 are local abnormal, remote abnormal, joint abnormal and model
conflict constellation masks. The event is written for material availability,
alarm or conflict changes; routine forecast and model-ID refreshes alone do not
write flash. Abnormal masks are subsets of their corresponding valid masks; joint
validity is a subset of local and remote validity, and conflict is a subset of joint
validity. Joint fields require both model flags.

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

The edge emits current availability and power assessments each second. It saves
availability alarm/recovery/coverage changes and material power state changes in
one 128-record reception journal lane. On reconnect it replays each retained event
type oldest first while current reports continue. Uploading never deletes local
records. The navigation/telemetry transport spool remains bounded RAM; journal
records survive reboot and can be uploaded again on the next connection. Delivery
uses the existing GNF1 ACK rules, including their finite-buffer and skipped-sequence
limits.

Private observer feeds expose `board.reception`, `board.reception_stale` (15 seconds),
`board.reception_events` (up to 32 deduplicated availability transitions),
`board.reception_power`, `board.reception_power_stale` (15 seconds),
`board.reception_power_events` (up to 32 deduplicated power transitions), and
`board.snapshot`.
Live power reports place the edge result in `details.reception_power` and the
collector recomputation in `collector_reception_power`; replayed tag 21 records
use `details.reception_power_event`. Coverage validity and freshness are separate
from a latched alarm. Public feeds omit board data.

When configured, the historian persists original ObserverDetails bodies under the
existing private `observer_samples` rules and persists raw receiver RF telemetry
in `rf_samples` in the same replay-ledger transaction. Historical tags 18 and 21
do not advance live alarm machines. Forecasts and the collector's dwell machines
are in memory; a collector restart begins confirmation again. The latest server
power model is durable, but individual five-minute forecasts are not archived.
The journal's stored counts remain readable; interpreting old entry bitmaps
requires the exact forecast entry list.

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
The firmware enables it in RAM. Host tests cover the C/Go wire contracts, geometry,
freshness, distinct-pass model maturity, outlier rejection, CRC restore, local and
remote dwell agreement, reboot restoration, TLS delivery, snapshot retries and LED
phase boundaries. Firmware builds do not substitute for an antenna-connected bench
test of each receiver profile or a multi-day stationary collection.
