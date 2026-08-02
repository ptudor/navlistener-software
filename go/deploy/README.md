# `go/deploy` — service integration files

**Headline:** the files that make navlistener a real service on the deploy host. Today that's
FreeBSD on `collector-host`: an rc.d script that supervises the daemon and refuses to start it on a bad
config, plus the newsyslog rule that rotates its log without interrupting the historian.

---

## Index — what's in this folder

| Path | Installs as | What it is |
|---|---|---|
| `freebsd/navlistener` | `/usr/local/etc/rc.d/navlistener` (mode 755) | The rc.d service script. |
| `freebsd/newsyslog.conf.d/navlistener.conf` | `/usr/local/etc/newsyslog.conf.d/navlistener.conf` | Log rotation. |
| `README.md` | — | This file. |

Only FreeBSD is present, because `collector-host` is the deploy target. The Makefile can cross-compile for
linux/amd64, linux/arm64, and darwin, but there is no systemd unit here yet — the SBC field boxes
run `navfeeder`, not the collector.

---

## Summary

### One-time provisioning

The daemon runs as its own unprivileged user, and the rundir must be owned by it. `service start`
on a fresh host **fails without this** :

```sh
pw useradd navlistener -d /nonexistent -s /usr/sbin/nologin -c "navlistener collector"
install -d -o navlistener -g navlistener -m 0755 /usr/local/etc/navlistener
```

### Install and enable

```sh
install -m 755 deploy/freebsd/navlistener /usr/local/etc/rc.d/navlistener
install -m 644 deploy/freebsd/newsyslog.conf.d/navlistener.conf \
        /usr/local/etc/newsyslog.conf.d/navlistener.conf
sysrc navlistener_enable=YES
service navlistener start
```

### rc.d variables

| Variable | Default |
|---|---|
| `navlistener_enable` | `NO` |
| `navlistener_config` | `/usr/local/etc/navlistener/navlistener.toml` |
| `navlistener_user` | `navlistener` |
| `navlistener_logfile` | `/var/log/navlistener.log` |
| `navlistener_rundir` | `/var/run/navlistener` |

---

## Details

### The preflight gate — why `service start` validates first

`daemon(8)` forks and returns 0 immediately. So without a gate, a bad config produces
`Starting navlistener.`, exit status 0, a green operator-visible surface — and a **5-second
supervisor crash-loop** visible only in the logfile. regression fix closes that: `start` runs
`navlistener -check-config` and refuses to start on failure.

**The check runs as `${navlistener_user}`, not as root,** and that detail is the whole
point. Root bypasses file permissions, so a config, TLS key, client CA, or NTRIP CA readable by
root but *not* by the daemon user would pass a root-run gate and then fail in the child — exactly
the crash-loop the gate exists to prevent, with the added insult of a green `service start`.
Running the preflight under the daemon's own identity closes the privilege skew rather than
documenting it. The failure message names the user, because a permission-denied here is a
provisioning fact about that account.

**Provisioning consequence:** `${navlistener_user}` must be able to **read** the config and every
file it references — `tls_cert`, `tls_key`, `client_ca`, NTRIP `ca_file`. Chown them to the
daemon user and keep `tls_key` at 0600; a world-readable key is rejected outright.

`-check-config` has **full startup parity** : it loads the `[push]` TLS keypair and client
CA, parses the store DSN, and validates NTRIP CA files. **On a bare host without certs it fails by
design** — provision certificates before first start.

Non-fatal `WARNING:` lines from the preflight (a group/world-readable secrets file, a
non-loopback bind of an unauthenticated surface) are echoed at start **without** blocking it.

### The two pidfiles

`daemon(8)` runs as a supervisor with `-r` (restart on unexpected exit) and `-R 5`, dropping the
child to `${navlistener_user}`. That produces two processes and therefore two pidfiles, both in a
daemon-owned rundir — because `daemon(8)` writes the child pidfile *after* the privilege drop:

| Pidfile | Names |
|---|---|
| `${rundir}/supervisor.pid` | the `daemon(8)` supervisor |
| `${rundir}/navlistener.pid` | the collector itself |

`status` targets the **child** (both `pidfile` and `procname` name the collector), so it works
even though `daemon(8)` renames the supervisor process.

`stop` prefers signalling the **supervisor**, so its restart loop ends before the child is torn
down — otherwise `-r` would helpfully restart the daemon you just stopped. But it **verifies the
supervisor pidfile actually names a live `daemon(8)`** first: a stale pidfile could otherwise
`TERM` a reused, unrelated PID. If the supervisor is already gone but the child survives, it
`TERM`s the child directly, so a dead supervisor can't leave `service stop` hanging on
`wait_for_pids` forever. Then it waits on the child.

### Log rotation — signal the supervisor, never the collector

`daemon(8)` is invoked with **`-H`**, so on `SIGHUP` it reopens its `-o` output file onto the
freshly-rotated path. Requires `daemon(8)` 13.x or later.

```
/var/log/navlistener.log  navlistener:navlistener  640  7  10240  *  JC  /var/run/navlistener/supervisor.pid  SIGHUP
```

The signal goes to the **supervisor pidfile**, never the child's. The collector is never
signalled, so its ordered drain and the historian's in-flight batch are never interrupted.
A `SIGHUP` delivered to the collector itself is ignored in `main.go` as belt and braces — but
rotation must target the supervisor regardless.

Rotation: 7 generations, at 10 MB, mode 640, owned by the daemon user, with `J` (bzip2) and `C`
(create if missing).

### Deploying a new binary

```sh
make build-freebsd                     # on the dev box → build/navlistener-freebsd-amd64
# copy to collector-host:/usr/local/bin/navlistener
service navlistener restart
```

Restarting loses in-RAM live state by design — there is no cross-restart persistence anywhere in
the daemon. Positions return as each SV re-broadcasts a full ephemeris set, and discos
return once a second post-restart ephemeris arrives. The `nav_frames` hypertable is the durable
record; offline replay is the recovery path. Check `navlistener_build_info` afterwards to confirm
the fleet is on the version you think it is.

---

## See also

- `../README.md` — build targets and the deployment overview.
- `../internal/config/README.md` — what `-check-config` validates.
- `../cmd/navlistener/README.md` — the health model behind rc.d's restart behavior, and why
  `degraded` is HTTP 200.
- `../../docs/DESIGN.md` — the deployment story.
