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
| `hardware_trust_test.go` | `[hardware_trust]`: key loading, registry verification, and every incomplete combination. |
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
| `[collector]` | always | stable `instance_id` for the deployment/CA/operator audience realm |
| `[logging]` | always | `level` (debug/info/warn/error), `format` (json/text) |
| `[metrics]` | `addr` set | Prometheus `/metrics` + `/healthz`, loopback-bound |
| `[state]` | always | shard count, propagate cadence, SV TTL, `leap_seconds` |
| `[store]` | `dsn` set | TimescaleDB historian, `raw_retention`, `compress_after` |
| `[authorization]` | `dsn` set | DB-backed observer/read grants, bounded cache, active-session recheck |
| `[serve]` | `addr` set | native v2 API, public default, authenticated audience selection, refresh cadences |
| `[[serve.principal]]` | no DB auth | standalone/bootstrap read token and explicit private audience grants |
| `[push]` | `addr` set | the authenticated GNF1 fleet listener; TLS mandatory |
| `[hardware_trust]` | `manufacturer_keys` set | pinned manufacturer keys for device evidence, optional signed registry and its keys |
| `[[federation.export_grant]]` | no transport | explicit directed export authorization, validated before peer transport exists |
| `[[push.observer]]` | — | credential/feed grant plus server-owned organization and publication context |
| `[[ingest]]` | per entry | dial connector plus the same server-owned organization/publication context |

---

## Details

### `[authorization]` — production control-plane resolution

Setting `dsn` replaces static credential rows; it never supplements or falls back to them.
The collector reads the stable `navlistener_observer_authorization_v2` and
`navlistener_read_authorization_v1` views documented in
`internal/authorization`, caches positive and negative decisions by token digest, and listens
for `NOTIFY navlistener_authorization_changed`. `cache_ttl` (default 30s, maximum 5m) is the
stale-authority ceiling when notifications are interrupted. `session_recheck_interval`
(default 10s, maximum 5m) closes active feeder/read sessions after a revoked or changed row is
observed. The worst case without NOTIFY is their sum.

The authorization DSN should use a read-only database role with access only to the versioned
views and notification channel. Because it contains credentials, normal config-permission
warnings include this DSN. `-check-config` validates its syntax but does not connect.

### `[serve]` — isolated and authenticated read audiences

`audience = "public"` is the fail-closed default. The collector builds this state only from
sources whose server-resolved policy grants public aggregate use; private observations never
enter it. `audience = "operator"` selects the all-source local operations view and must be
protected by an authenticated private front. When database authorization or validated
`[[serve.principal]]` bootstrap rows are present, `/gnss/api/v2/audiences` discovers the
principal's server-side grants and opaque authorization/policy revision, and `X-GNSS-Audience`
selects one. Selection never creates a view and never widens authority; the view must already
have been materialized from trusted frame ownership/membership.

Static read rows store only SHA-256 token digests and are mutually exclusive with
`[authorization].dsn`. Their `audiences` values must be explicit canonical private keys; public
is always credential-free and is rejected as a stored grant.

Events queries and SSE use the same resolved key as polling. Organization/collection detector
state and event-id sequences are separate; a private event in one audience cannot create a
cursor gap in another.

### `[collector]` — one stable deployment realm

`instance_id` defaults to `local` for development and must be set deliberately in production.
Every static observer context and federation grant must match it, the operator audience is
`operator:<instance_id>`, and database-authorized push sessions are rejected if their enrollment
belongs to another collector instance. This is what makes `airport-f-onsite` a distinct,
standalone licensing/CA jurisdiction rather than a label supplied by a feeder.

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
  `collections`, `aggregate_use`, `station_metadata`, `event_visibility`, `raw_export`,
  `federation_peers`, `publish_signals`, and `policy_revision`. They are resolved by
  this collector, stamped on every frame, and persisted with the raw receipt. Omitting them is
  deliberately safe: `local-unassigned`, `private`, and no station metadata. A receiver cannot
  send or override them. Database-authorized push sources resolve the same context
  through the versioned authorization views; configured sources provide it locally
  for standalone and bootstrap installations.
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
- **`event_visibility`** is independent from aggregate use and defaults to `private`.
  `public_redacted` contributes to a public detector without a station identity; `public`
  retains only attribution already allowed by `aggregate_use`. A private aggregate can never
  grant public events.
- **Raw/federation fields** default to `deny` and an empty peer set. `named_peers` requires at
  least one explicit collector-instance id. `public` means the receipt policy does not narrow
  by destination, but an enabled destination `ExportGrant` is still mandatory—there is no
  wildcard transmission. `publish_signals` narrows both public state and export by
  `"gnss:sig"`; empty means all supported signals.

### `[hardware_trust]` — what the push endpoint verifies about hardware

Normative in [`docs/COMMISSIONING.md` §10](../../../docs/COMMISSIONING.md). The section holds
public keys only, so it never makes the config file secret-bearing.

| Field | Meaning |
|---|---|
| `manufacturer_keys` | PEM public keys, or certificates carrying them, that may sign commissioning records. Enables the section. |
| `registry` | The signed registry file. It can only withdraw trust. |
| `registry_keys` | The operations keys that may sign the registry; a separate set, required with `registry`. |
| `registry_reload` | How often the registry file is checked for a change. Default `30s`. |
| `registry_state` | Absolute path of the file that records the newest registry sequence adopted, so a restart cannot accept an older registry. Written by the daemon, mode 0600. |
| `require_registry_entry` | Withhold trust from a board the registry does not list. Default `false`, for registry copies that lag behind newly commissioned boards. |

Validation is strict in both directions. Every key file must load as a P-256 key, and a
configured registry must verify against `registry_keys` at load, so `-check-config` fails on a
registry the daemon could not start with. A dependent setting without its prerequisite —
`registry` without `registry_keys`, or `registry_keys`, `registry_reload` or
`require_registry_entry` without `registry`, or any of them without `manufacturer_keys` — is
an error rather than a silent no-op: an operator who set `require_registry_entry` with no
registry would otherwise believe unlisted boards were refused when nothing was. The section
also requires `[push].addr`, since evidence arrives only on a GNF1 session.

A registry only ever moves forward. Within a process that is enforced in memory; across a
restart it needs `registry_state`. `-check-config` reads that file without writing it and
applies the recorded sequence as a floor, so it refuses exactly the registry the daemon would
refuse at startup. A state file that is group- or world-writable, oversized or unparsable is an
error, because whoever can rewrite it can lower the floor. A `registry` with no
`registry_state` is a `WARNING`, not an error: it is a legitimate mode for a collector whose
registry file is itself protected, but never a silent one.

`HardwareTrust.NewVerifier` pins the keys; the daemon then starts the registry watch. Nothing
here can grant trust: a static `[[push.observer]]` row has no field for it, and
`hardware_trust` on a receipt is always the result of a verified record and, for `trusted`, a
session proof.

### `[[federation.export_grant]]` — directed egress authorization

These rows do not start a peer connection. They are parsed into the transport-independent
`internal/federation.ExportGrant` gate now so a later transport cannot exist without the policy
edge already being testable. Every grant names one source collector and destination peer, at
least one organization/collection/observer selector, allowed signals/data classes, maximum
attribution and retention, one or more purposes, a required validity window, approver,
revision, and enabled state. A selector-free row is rejected rather than interpreted as a
wildcard.

The grant is only half the decision: each send must also pass the immutable receipt policy and
the current owner policy. A peer HELLO/subscription may narrow these values but never widen them.

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
interval allowlist, and the warning-vs-error split. `hardware_trust_test.go` covers the
`[hardware_trust]` section with real key files and signed registries.

```sh
go test ./internal/config/
```

---

## See also

- `../../navlistener.toml.example` — the commented reference config.
- `../../deploy/freebsd/navlistener` — how rc.d invokes `-check-config` before starting.
- `../../../docs/DESIGN.md §5` — the optional-stage design.
- `../../../docs/COMMISSIONING.md` — commissioning records, the session proof and the registry.
