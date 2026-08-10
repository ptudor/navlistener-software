# `internal/config` — TOML configuration

**Headline:** one file, one format, one place where every operational knob is defined and
validated. TOML at `/usr/local/etc/navlistener/navlistener.toml`, passed with `-config`. Never a
`.env`, never an environment-variable soup.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `config.go` | The whole package: every config struct, `Load`, validation, defaults, and the two identity validators. |
| `config_test.go` | Load/validate cases, defaults, and the rejection paths. |
| `README.md` | This file. |

See `../../navlistener.toml.example` for a commented reference config.

---

## Summary

```go
func Load(path string) (*Config, error)
var DefaultPaths = []string{
    "/usr/local/etc/navlistener/navlistener.toml",
    "/etc/navlistener/navlistener.toml",
    "./navlistener.toml",
}
```

`Load` reads an explicit path, or the first default path that exists. It parses, applies
defaults, validates, and populates `Config.Warnings`.

**Optional stages stay dormant when their enabling value is empty** (`docs/DESIGN.md §5`). No
`[store].dsn` means no historian. No `[serve].addr` means no read API. No `[push].addr` means no
fleet ingest. The daemon runs happily as a collector-only process.

---

## The sections

| Section | Enables | Key fields |
|---|---|---|
| `[logging]` | always | `level` (debug/info/warn/error), `format` (json/text) |
| `[metrics]` | `addr` set | Prometheus `/metrics` + `/healthz`, loopback-bound |
| `[state]` | always | shard count, propagate cadence, SV TTL, `leap_seconds` |
| `[store]` | `dsn` set | TimescaleDB historian, `raw_retention`, `compress_after` |
| `[serve]` | `addr` set | the native v2 read API, `audience` (`public` default or explicit `operator`), and refresh cadences |
| `[push]` | `addr` set | the authenticated GNF1 fleet listener; TLS mandatory |
| `[[push.observer]]` | — | credential/feed grant plus server-owned organization and publication context |
| `[[ingest]]` | per entry | dial connector plus the same server-owned organization/publication context |

---

## Details

### `[serve]` — one isolated read audience

`audience = "public"` is the fail-closed default. The collector builds this state only from
sources whose server-resolved policy grants public aggregate use; private observations never
enter it. `audience = "operator"` selects the all-source local operations view and must be
protected by an authenticated private front. Free-form organization or collection audiences
are rejected until the read-auth layer can resolve them from a principal's server-side grants.

The public events endpoints stay unavailable rather than borrowing unscoped operator event
history. Feed snapshots are persisted with their audience key.

### `[[ingest]]` — dial sources

```toml
[[ingest]]
name = "observer16"
type = "ubx"          # ubx | sbf | rtcm | ntrip
addr = "10.0.0.16:2947"
remark = "roof, ZED-F9T"
capabilities = ["0:0", "2:0", "2:3", "3:0", "6:0"]
```

- **`remark` is the only field that reaches the public observers feed**. `addr` is the
  internal LAN dial target; publishing it would disclose network topology and the exact
  `host:port` of an unauthenticated raw receiver TCP stream.
- **`capabilities`** is the node's declared *tudorgps* fingerprint — the `"gnss:sig"` signals its
  silicon can produce. The integrity layer compares declared against observed: a declared signal
  that goes silent, or an observed signal the silicon can't produce, is a threat
  (`docs/INTEGRITY.md §6`).
- **`capture_only`** persists raw frames without decoding into live state. Required for the byte
  sources (SBF, RTCM) until their central decoders land.
- **`disabled`** keeps an entry in the file but doesn't start it — pause without deleting.
- **Identity and policy fields** are `organization`, `enrollment`, `collector_instance`,
  `collections`, `aggregate_use`, `station_metadata`, and `policy_revision`. They are resolved by
  this collector, stamped on every frame, and persisted with the raw receipt. Omitting them is
  deliberately safe: `local-unassigned`, `private`, and no station metadata. A receiver cannot
  send or override them. Production will source the same context from shared AAA rows; config is
  the bootstrap provider for local and standalone installations.
- **`max_frame_silence`** (default 5m, regression fix) tears down and re-dials a source that keeps
  delivering *bytes* but no decodable *frames*. The idle timeout covers byte silence (a half-open
  peer); this covers the chatter-but-no-frames variant — an F9 reset to factory NMEA output, a
  mis-pointed TCP port, a caster streaming an HTML error page — which otherwise reports
  `source_up = 1` with `frames_total` frozen, indefinitely. The slowest legitimate cadence on any
  dial source is RTCM ephemeris tens of seconds apart, so 5 minutes is roughly a 10× margin and a
  false trip costs one logged reconnect.
- **NTRIP** (`type = "ntrip"`) adds `mountpoint`, `username`, `password`, `ca_file`,
  `server_name`, and two explicit opt-outs: `allow_insecure_plaintext` and
  `allow_plaintext_credentials`. Real credentials live only in the deployed, git-ignored config —
  **never in a committed example.**

### `[push]` and `[[push.observer]]`

The production fleet ingest path. Feeders connect **outbound** to us over TLS, so observers are
configured separately from dial sources — they have credentials and feed grants rather than dial
addresses.

- **TLS is mandatory** when `addr` is set.
- **`client_ca`** enables mTLS. The bearer token remains the bootstrap tier and is *always*
  checked, so mTLS is defense in depth rather than a replacement.
- **`token_sha256`** stores the SHA-256 of the bearer token. The token itself is shown once at
  enrollment and never committed.
- **`max_conns`** (default 512) bounds concurrent in-flight feeder connections.
  Production should gate admission at the TLS layer via `client_ca`; when it isn't set, this is
  the only thing between the internet and unbounded goroutine and file-descriptor growth.
- **`ack_interval`** (default 1s) is the GNF1 ACK cadence.
- **Organization and publication fields** have the same meanings and fail-closed defaults as on
  `[[ingest]]`. They are authorization output, not feeder assertions. Config-backed observers
  can prove `token` or `software_mtls`; config alone can never claim hardware attestation.

### The two identity validators

```go
func ValidObserverID(s string) bool
```

Whether a station name can serve as a canonical observer identity bindable to an mTLS
certificate. The push handshake compares the certificate's single DNS SAN **byte-for-byte**
against this name, so it must be nonempty, at most 253 bytes (the DNS name bound), and contain
only ASCII letters, digits, `.`, and `-` — **no case folding, no Unicode aliases.** Enforced at
config load whenever `push.client_ca` is set, so an unbindable station name fails
`-check-config` instead of locking the observer out at connect time.

The sibling `wire.ValidSession` uses a deliberately similar charset, so a session identity can
never smuggle JSON or SQL metacharacters into logs or the historian ledger.

```go
var IntervalRe = regexp.MustCompile(`^[1-9][0-9]* (minute|hour|day|week)s?$`)
```

The allowlist for `[store]`'s `raw_retention` and `compress_after` (`"7 days"`, `"1 hour"`). This
is the **sole definition** — `../store/store.go` references this one rather than keeping a copy
 — and it doubles as the **injection guard** for that string's later interpolation into
Timescale policy DDL. It must never be relaxed.

### Warnings vs errors

`Config.Warnings` carries non-fatal findings surfaced at startup and by `-check-config`
:

- a group- or world-readable config file holding credentials,
- a non-loopback bind of an unauthenticated surface.

These are warnings rather than errors because each has a legitimate deliberate mode — a
secret-less dev config, remote Prometheus scraping behind a firewall. But **never a silent one**.
A world-readable `tls_key`, by contrast, is rejected outright.

---

## Tests

`config_test.go` covers loading from explicit and default paths, defaults applied to each
duration field, the identity validators (including the case-fold and Unicode rejections), the
interval allowlist, and the warning-vs-error split.

```sh
go test ./internal/config/
```

---

## See also

- `../../navlistener.toml.example` — the commented reference config.
- `../../deploy/freebsd/navlistener` — how rc.d invokes `-check-config` before starting.
- `../../../docs/DESIGN.md §5` — the optional-stage design.
