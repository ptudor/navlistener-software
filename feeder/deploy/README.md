# Deploying `navfeeder`

`navfeeder` is the dumb edge feeder: it reads raw UBX-RXM-SFRBX off a local u-blox receiver
and pushes each frame, undecoded, to the navlistener collector over an authenticated,
zstd-negotiated, spooled TLS link (GNF1). All decode + orbit math is central — the edge just
frames and forwards, and its spool means a collector restart loses nothing (and an orderly
reboot loses nothing too **when the spool is on persistent storage** — see the Spool note
below; on OpenWrt's tmpfs it survives a collector outage but not a reboot, regression fix)
(`docs/DESIGN.md §1/§2`). The fleet target is the OpenWrt/SBC observers next to each receiver.

## Build

```sh
make                      # host build (dev/test; MacPorts OpenSSL on macOS)
make openwrt STAGING_DIR=<openwrt>/staging_dir TARGET=mipsel_24kc_musl GCC_VER=15.2.0
```

The binary links OpenSSL (TLS 1.2, pinned — see the regression fix note in the source) + libzstd +
pthread, all of which OpenWrt already ships.

## Enrollment (the shared AAA control plane)

A GNSS observer is just a `Device` in the control plane navlistener shares with radiolistener
(one CA, one `devices` table — `docs/DESIGN.md §3`). Enroll the station and grant it the
`ubx` feed type, which mints a **bearer token shown once**:

1. Register the station (its id becomes `--station` and the mTLS cert CN) with `feed_types`
   including `ubx`.
2. Copy the one-time token to the box as `/etc/navfeeder/<station>.token` (`0640`,
   `root:navfeeder`), never into the unit's argv (the unit uses `--token-file`).
3. Pin the collector's CA to `/etc/navfeeder/ingest-ca.pem`.

The token is the bootstrap tier; upgrade a station to a software or ATECC mTLS cert
(`--cert`/`--key`) later without touching the feeder — same credential ladder as
radiolistener. Revocation is `enabled=false` in the DB.

## Install (systemd, one instance per receiver)

```sh
install -m 0755 navfeeder /usr/local/bin/navfeeder
install -m 0644 deploy/navfeeder@.service /etc/systemd/system/
useradd --system --no-create-home --shell /usr/sbin/nologin navfeeder
install -d -o navfeeder -g navfeeder /var/spool/navfeeder
```

`/etc/navfeeder/<instance>.env`:

```sh
SERVER=collector.host.invalid:5580     # the collector's [push] endpoint
SOURCE=/dev/ttyACM0                    # serial receiver, OR host:port for a TCP bridge
BAUD=460800                            # serial line rate (ignored for a TCP source)
STATION=observer16
EXTRA=--zstd                           # optional: request DATA-stream compression
```

Then:

```sh
systemctl enable --now navfeeder@observer16
journalctl -u navfeeder@observer16 -f
```

The unit runs as an unprivileged user in `dialout` (for serial device access), with only
`/var/spool/navfeeder` writable. A TCP source (`SOURCE=host:port`, e.g. a `ser2net` bridge or
the receiver's raw TCP port) needs no device group.

## Install (OpenWrt, one receiver per box)

```sh
addgroup -S navfeeder
adduser -S -D -H -s /bin/false -G navfeeder navfeeder
scp navfeeder root@box:/usr/bin/navfeeder
scp deploy/openwrt/navfeeder.init root@box:/etc/init.d/navfeeder
```

Write `/etc/navfeeder/feeder.conf` (`SERVER/SOURCE/BAUD/STATION/EXTRA`), the token to
`/etc/navfeeder/feeder.token`, and the CA to `/etc/navfeeder/ingest-ca.pem`. Make the two TLS
files group-readable by the service account, then start the unit:

```sh
chown root:navfeeder /etc/navfeeder/feeder.token /etc/navfeeder/ingest-ca.pem
chmod 0640 /etc/navfeeder/feeder.token /etc/navfeeder/ingest-ca.pem
chmod +x /etc/init.d/navfeeder
/etc/init.d/navfeeder enable
/etc/init.d/navfeeder start
```

The procd instance runs as `navfeeder:navfeeder`, and `start_service` makes its tmpfs spool
directory owned by that account. For a `/dev/...` source it also grants the `navfeeder` group
read access to the configured device at service start. Boxes whose receiver node is recreated
after boot should install the equivalent OpenWrt hotplug rule (`chgrp navfeeder` and `chmod g+r`
for that stable device path); TCP bridge sources need no device permission.

## Notes

- **Source modes.** Only `--feed ubx` is implemented today (the fleet is u-blox). SBF and
  RTCM source modes are deferred — the collector's push path wires `ubx`.
- **Never `ssh` from CI/agents** to a box — the commands above are for the operator to run.
- **Spool.** `--spool` sizes the RAM ring (frames); `--spool-file` adds a disk overflow tier
  capped by `--spool-disk-mb`. On an orderly stop/reboot the ring is flushed to the disk tier
  and fsync'd, so an orderly reboot is lossless **only when `--spool-file` points at
  persistent storage** — on OpenWrt `/var` is tmpfs (RAM), so the spool covers a collector
  outage while the box stays up but does NOT survive a reboot there (regression fix; the init caps the
  tmpfs spool at 16 MiB). Both tiers are lossless up to their caps; overflow drops the oldest
  and is counted in the disconnect log.
