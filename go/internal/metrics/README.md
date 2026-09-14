# `internal/metrics` — the Prometheus collectors

**Headline:** every metric navlistener exposes, defined in one file as package-level `promauto`
collectors so any pipeline stage can record without threading a registry through. Labels stay
low-cardinality by design — and there is exactly one, carefully bounded, exception.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `metrics.go` | The whole package: every collector, with the reasoning for each in its doc comment. |
| `README.md` | This file. |

Exposed at `/metrics` by `internal/server`.

---

## Summary

Collectors are **package-level via `promauto`** — the same shape as the radiolistener sibling. The
alternative (plumbing a registry through ingest → state → detect → store) buys nothing here and
costs a parameter on every constructor.

**The reusable `github.com/ptudor/gnss` library never imports this package.** The math library is
I/O-free and dependency-free; the daemon records metrics from the typed results and errors the
library returns. That separation is what lets `gnss` be vendored on its own.

### Label discipline

Labels are: source name, numeric `gnssId` (0..7), signal id, and short fixed kinds. **Never a
per-SV identifier** — there are a few hundred satellite×signal combinations and they change over
time, which is exactly the shape that turns a metrics backend into a memory problem.

---

## The one bounded exception — read this before writing a query

**regression fix.** The regression fix defense-in-depth gate in `state.Apply` rejects a frame whose `gnssId` is
outside the valid domain and records it as:

```
navlistener_decode_errors_total{gnssid="out_of_range", kind="gnssid_range"}
```

The raw byte is deliberately **not** printed. An attacker-supplied id would otherwise mint an
unbounded label set — a metrics-cardinality denial of service via crafted frames, in a product
whose threat model explicitly includes crafted frames.

So the `gnssid` label's domain is *the numeric ids **plus** that one fixed sentinel*. Cardinality
stays bounded, but:

> **A query written against `gnssid=~"[0-7]"` silently drops the very series that flags id
> corruption.** Match the sentinel too.

`decode_errors_total` is the only collector whose `gnssid` label can carry a non-numeric value.

---

## The collectors

### Build and lifecycle

| Metric | Type | Labels |
|---|---|---|
| `navlistener_build_info` | gauge (always 1) | version, build_time |

### Ingest

| Metric | Type | Labels |
|---|---|---|
| `navlistener_frames_total` | counter | source, gnssid |
| `navlistener_ingest_errors_total` | counter | source, kind |
| `navlistener_source_up` | gauge | source, type |
| `navlistener_source_connects_total` | counter | source |
| `navlistener_source_last_frame_timestamp_seconds` | gauge | source |
| `navlistener_source_security_degraded` | gauge | source, reason |
| `navlistener_raw_observation_invalid_total` | counter | source, field |

**`source_last_frame_timestamp_seconds` ** is the one to alert on, paired with
`source_up == 1`: a receiver streaming bytes that never frame — an F9 reset to NMEA-only output, a
mis-pointed TCP port — keeps `source_up` at 1 and `frames_total` frozen. Byte silence trips the
idle timeout; *frame* silence trips the watchdog and shows here.

It is a **timestamp, not an age**, following the Prometheus idiom: an age computed at query time
cannot go stale between scrapes. Same shape as `serve_feed_refresh_timestamp_seconds`.

**Alerting caveat :** the regression fix connect-time seed means every watchdog-driven re-dial
resets this gauge, capping the observable age near `max_frame_silence + backoff`. So an age alert
only fires reliably for thresholds **below** `max_frame_silence`; the `frame_silence` error
counter is the unconditional signal for longer outages.

### Decode

| Metric | Type | Labels |
|---|---|---|
| `navlistener_nav_crc_fail_total` | counter | gnssid, sigid, source |
| `navlistener_decode_total` | counter | gnssid, msg_type |
| `navlistener_decode_errors_total` | counter | gnssid (+ sentinel), kind |
| `navlistener_decode_panics_total` | counter | gnssid |
| `navlistener_captured_only_frames_total` | counter | — |

**`nav_crc_fail_total`** counts frames dropped for a failed parity/CRC check. When the historian
is enabled, raw bytes are enqueued *before* live-state decode, so they remain available for
forensics even when this check rejects the frame. The `source` label is what makes a noisy
push/federation link visible — including the elevated reject rate that GLONASS's
detection-only Hamming policy deliberately accepts.

**`decode_panics_total` ** is the alertable signal for a failure class
`decode_errors_total` — which counts *rejections* — structurally cannot see. A decoder that panics
on a specific broadcast bit pattern panics on **every** recurrence: the same SV re-transmits it
every few seconds, or crafted frames replay it deliberately. Any nonzero value here is worth
investigating immediately.

**`captured_only_frames_total`** counts valid raw frames persisted but intentionally not decoded
into live state — the `capture_only` sources (SBF, RTCM) pending central decoders.

### Push ingest

| Metric | Type | Labels |
|---|---|---|
| `navlistener_push_connects_total` | counter | |
| `navlistener_push_errors_total` | counter | |
| `navlistener_push_auth_failures_total` | counter | |
| `navlistener_push_auth_failures_by_reason_total` | counter | reason |
| `navlistener_push_observers_up` | gauge | |
| `navlistener_push_server_cert_not_after_seconds` | gauge | |

`push_server_cert_not_after_seconds` is the certificate-expiry gauge — alert on it well before it
matters, because an expired push certificate takes the entire fleet offline at once.

### State

| Metric | Type |
|---|---|
| `navlistener_live_svs` | gauge |
| `navlistener_svs_expired_total` | counter |
| `navlistener_spoof_gates_wired` | gauge |
| `navlistener_spoof_gate_quorum` | gauge |

The two spoof-gate metrics make the PNT-defense fusion rule observable: `spoof_gates_wired` is
how many independent physics gates are actually implemented (the maximum the fusion can count),
and `spoof_gate_quorum` is how many must agree before `spoofing_suspected` fires. **`wired <
quorum` means the detector is dormant** — it cannot fire at all. That is exactly the kind of
silently-disabled state a fusion rule needs to expose, and it's why both gauges exist rather than
just one.

### Store

| Metric | Type |
|---|---|
| `navlistener_store_rows_total` | counter |
| `navlistener_store_dropped_total` | counter |
| `navlistener_store_errors_total` | counter |
| `navlistener_store_quarantined_total` | counter |
| `navlistener_store_empty_raw_total` | counter |

`store_dropped_total` is the bounded-queue overflow policy made visible — the historian degrading
so live decode doesn't. A sustained rise means the database can't keep up.

### Serve and events

| Metric | Type | Labels |
|---|---|---|
| `navlistener_serve_feed_refresh_timestamp_seconds` | gauge | feed |
| `navlistener_serve_feed_marshal_errors_total` | counter | feed |
| `navlistener_sse_clients` | gauge | |
| `navlistener_sse_events_dropped_total` | counter | |
| `navlistener_sse_subscribe_rejected_total` | counter | |
| `navlistener_events_total` | counter | |
| `navlistener_event_write_errors_total` | counter | |

`sse_subscribe_rejected_total` rising means the `sseMaxClients` cap is being hit — either
a buggy reconnect loop or someone probing. `serve_feed_marshal_errors_total` rising with a frozen
`serve_feed_refresh_timestamp_seconds` means a feed is serving a stale body.

---

## Adding a collector

1. **Check the label cardinality first.** Source names, numeric constellation ids, signal ids, and
   short fixed kinds are fine. Anything derived from untrusted input needs a bounded sentinel, the
   way `gnssid` does.
2. **Prefer a timestamp gauge over an age gauge** for "when did X last happen."
3. **Write the reasoning in the doc comment**, not just the `Help` string — the `Help` text goes
   to Prometheus, the comment goes to the next person changing the code.
4. **Don't record from `gnss`.** The library returns typed results and errors; the daemon does the
   counting.

---

## See also

- `../server/README.md` — the listener that exposes `/metrics`.
- `../../../docs/DESIGN.md` — the observability design.
- `../../../docs/DEFENSE-PNT.md` — the spoof-gate fusion rule the two gauges expose.
