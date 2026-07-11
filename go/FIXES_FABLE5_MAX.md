**regression fix** — `-check-config` parity gaps (push.addr, push TLS files, client_ca, store DSN, ntrip ca_file).
Changed: `finalizePush` now runs `validateAddr("push.addr")`, `tls.LoadX509KeyPair(tls_cert, tls_key)`,
and (when set) validates `client_ca` PEM; `finalize` parses `store.dsn` via `pgxpool.ParseConfig` and
validates each ntrip `ca_file` PEM (new `validatePEMFile` helper). All fatal at load, so a config edit
fails `-check-config` instead of a 5 s supervisor restart loop / endlessly-retried flaky receiver.
Files: `go/internal/config/config.go`; tests updated to supply real temp keypairs (`testKeypair` helper).
Verification: `TestCheckConfigParity` (push.addr=5580, tls_cert=/nonexistent, store.dsn="not a dsn",
ntrip ca_file=/nonexistent each fail with a named-field error) — PASS; existing push tests updated to
real keypairs still PASS.

**regression fix** — Explicitly-configured non-positive durations/sizes silently coerced to defaults; capture_only on ubx ignored.
Changed: added `parseDurPositive` — empty string → default, non-empty but ≤0 → hard error; applied to
sv_ttl/propagate_interval/batch_interval/refresh_interval/almanac_refresh_interval/ack_interval
(snapshot_interval keeps its "0s disables" special case). Negative batch_size/max_conns now error.
A `ubx` source with `capture_only = true` is rejected at load (capture_only is implied only for byte
sources).
Files: `go/internal/config/config.go`; test `config_test.go`.
Verification: `TestConfigRejectsExplicitNonPositiveAndUbxCaptureOnly` (sv_ttl=0s, batch_interval=-5s,
ubx+capture_only each rejected) — PASS; `TestSnapshotInterval` (0s still disables) still PASS.

**regression fix** — rc.d service: log rotation impossible without killing the collector; stop can hang; user never provisioned.
Changed: added `-H` to the daemon(8) invocation (reopens logfile on SIGHUP); shipped
`deploy/freebsd/newsyslog.conf.d/navlistener.conf` signalling the SUPERVISOR pidfile; `signal.Ignore(SIGHUP)`
in main.go so a mis-aimed HUP can't kill the collector. Rewrote `navlistener_stop` to verify the
supervisor pid via `pgrep -F` before TERM, fall back to TERMing the child if the supervisor is gone, and
keep `wait_for_pids` on the child. Documented `pw useradd navlistener` provisioning in the header.
Files: `go/deploy/freebsd/navlistener`, `go/deploy/freebsd/newsyslog.conf.d/navlistener.conf`,
`go/cmd/navlistener/main.go`.
Verification: `go build` compiles the SIGHUP-ignore; `sh -n` accepts the rc.d script. Full ops
verification (newsyslog rotate, kill -9 supervisor + service stop, fresh-host install) requires the
FreeBSD `collector-host` host and is not runnable in this environment.

