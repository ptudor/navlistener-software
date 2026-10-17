# Observer installation and motion policy

Collector integrity configuration describes antenna use. It does not select
receiver dynamics, start a receiver survey, or command a timing/static mode.
Confirm those settings and the exact receiver/firmware readback separately: a
receiver holding a fixed position can suppress movement in its own PVT output.

Existing `mode = "fixed"` and `mode = "mobile"` entries keep their existing
bands and continuity behavior. Mobile mode still requires `max_speed_mps`.
Named `installation` entries infer the compatible mode and add these starting
envelopes; they are configurable analysis limits, not certified vehicle limits.

| Installation | Speed m/s | Acceleration m/s² | Lateral acceleration m/s² | Vertical speed m/s |
|---|---:|---:|---:|---:|
| `fixed_structure` | Existing stationary checks | — | — | — |
| `pedestrian` | 3 | 4 | — | — |
| `bicycle` | 20 | 6 | 6 | — |
| `road_vehicle` | 70 | 12 | 10 | — |
| `aircraft` | 350 | 30 | 30 | 80 |
| `portable_test` | 50 | 15 | — | — |

Named moving policies check reported speed as well as displacement. Velocity
differences check acceleration and the component normal to mean horizontal
velocity checks turning, avoiding heading wrap at north and a turn-rate
singularity at a stop. Limits are tested after subtracting three times the
reported uncertainty. If uncertainty cannot resolve a limit, that comparison
is unavailable; another clearly failed limit can still produce an alarm. The
checks share one position evidence domain and cannot create an independent
spoofing quorum by counting multiple kinematic failures.

```toml
[[integrity.station]]
observer = "test-observer"
installation = "portable_test"
max_speed_mps = 12
segment_id = "deployment-2"
[integrity.station.motion]
max_acceleration_mps2 = 3
gap = "45s"
reset_after_gap = true
max_speed_accuracy_mps = 1
max_position_accuracy_m = 10
```

Motion overrides also accept `reported_speed`, `max_lateral_acceleration_mps2`
and `max_vertical_speed_mps`. Zero disables an optional numerical limit. Named
policies default to maximum speed accuracy 2 m/s and position accuracy 25 m;
invalid/missing/nonfinite inputs remain unavailable. `mode = "mobile"` can also
opt into individual motion fields. An incompatible explicit mode is rejected.

## Gaps and declared relocation

By default pedestrian, bicycle, road and aircraft policies keep the legacy
displacement reference across missing data, up to the engine's ten-minute
reference age. Their velocity/acceleration comparison interval is at most ten
seconds (`motion.gap` can shorten this interval). A gap does not excuse continuous
implausible motion. Ordinary fixed/mobile entries are unchanged: a receiver-time
gap over ten seconds clears position-difference history while the mobile
reference can remain. Receiver restart/backward time resets retain their
existing behavior.

`portable_test` defaults to a new segment after a receiver-time gap over 30
seconds, including that long without a usable position. It resets the motion
reference, position/velocity, receiver-clock and pulse-phase comparison windows.
Those comparisons report unavailable while refilling. The assessment retains
the previous segment's results, including alarms, for up to eight transitions;
raw observations and persisted events retain their normal history. Portable
units cannot have a fixed reception site, so stationary sky history is not
reused while they travel.

For a declared relocation, change `segment_id` and restart the collector with
the reviewed configuration. The deployment ID is durable in configuration and
changes the assessment hash. The pure evaluator also exposes `Station.Relocate`
for callers that supply their own durable maintenance-event stream; this is not
an HTTP or receiver command. Its reason and previous results are retained in
the assessment. Preserve the relevant configuration with replay evidence.

## Resolving fixed displacement

A one-metre rule needs uncertainty small enough to resolve one metre. Add a
pinned reference and explicit quality gates instead of lowering the legacy
noise bands alone:

```toml
[[integrity.station]]
observer = "mast-observer"
installation = "fixed_structure"
position = [35.0, 140.0, 50.0]
[integrity.station.fixed]
displacement_m = 1
max_horizontal_accuracy_m = 0.2
min_sv = 8
require_gnss_only = true
[integrity.station.reference]
source = "external_survey"
accepted_by = "operator"
reason = "reviewed antenna survey"
site_id = "mast-site-2"
horizontal_uncertainty_m = 0.1
vertical_uncertainty_m = 0.2
```

`vertical_displacement_m` and `max_vertical_accuracy_m` provide a separate
vertical rule. At each epoch, three times the combined receiver/reference
uncertainty must fit inside the requested tolerance. A displacement alarm needs
the measured offset minus that uncertainty to exceed the tolerance. Ordinary
standalone noise near a wall stays unavailable. The configured reference never
follows suspect positions. Named tight rules replace the legacy instantaneous
and mean-offset test; only axes with an explicit displacement tolerance are
tested by that rule. Other position/velocity checks continue.
Reference provenance, quality gates and all effective limits are
included in the assessment configuration hash.

When a reception site exists, its position must agree and `power_model_epoch`
must equal `reference.site_id`. Use a new value at a new site, including a return
to the same coordinates, so a previous deployment's power history is not reused.

## Candidate surveys and explicit acceptance

An optional `[integrity.station.survey]` takes `duration = "10m"`,
`min_samples = 600`, `max_dispersion_m = 1`, `max_accuracy_m = 0.2`, and
`min_sv = 8`. It collects valid GNSS-only 3D fixes with resolved UTC. Missing or
poor fixes, excessive dispersion, time discontinuities or gaps over ten seconds
restart collection. Candidate duration follows receiver time, with UTC
progression checked against it. A ready candidate freezes. It does not change a
reference; the ordinary assessment exposes readiness without coordinates.

For offline review, build `go build ./cmd/navlistener-survey` from `go/`. Feed a
newline-delimited trace of `integrity.Solution` JSON on stdin. The JSON uses its
Go field names (`TOW`, `UTC`, `UTCValid`, `FixType`, `FixOK`, `NumSV`, `LatDeg`,
`LonDeg`, `HeightM`, `HAccM`, `VAccM`, `VelN`, `VelE`, `VelD`, `SAccMPS`, and
optional receipt/host-stamp fields); times use RFC3339. Preserve the original
solution evidence alongside the trace.

```sh
./navlistener-survey < solutions.jsonl > candidate.json
./navlistener-survey -accept-by operator -reason 'reviewed candidate' \
  -observer mast-observer -site-id mast-site-3 < solutions.jsonl > reference.toml
```

Review `candidate.json` before explicit acceptance, then review and install the
generated TOML. The accepted reference carries duration, sample count, 3D RMS
dispersion, maximum reported accuracy, minimum satellite count, fix quality,
UTC bounds and `source = "receiver_survey"`, `independent = false`. Its
uncertainty cannot be smaller than the reported accuracy or dispersion; sample
count does not justify dividing systematic error by its square root. Update the
reception site and power-model epoch together. Learning a position from this
receiver does not independently confirm its absolute location.
