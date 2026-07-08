# Deploying `navfeeder`

`navfeeder` is the dumb edge feeder: it reads raw UBX-RXM-SFRBX off a local u-blox receiver
and pushes each frame, undecoded, to the navlistener collector over an authenticated,
zstd-negotiated, spooled TLS link (GNF1). All decode + orbit math is central — the edge just
frames and forwards, and its spool means a collector restart or a reboot loses nothing
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
scp navfeeder root@box:/usr/bin/navfeeder
scp deploy/openwrt/navfeeder.init root@box:/etc/init.d/navfeeder
```

Write `/etc/navfeeder/feeder.conf` (`SERVER/SOURCE/BAUD/STATION/EXTRA`), the token to
`/etc/navfeeder/feeder.token` (`0600`), the CA to `/etc/navfeeder/ingest-ca.pem`, then
`chmod +x /etc/init.d/navfeeder && /etc/init.d/navfeeder enable && /etc/init.d/navfeeder start`.

## Notes

- **Source modes.** Only `--feed ubx` is implemented today (the fleet is u-blox). SBF and
  RTCM source modes are deferred — the collector's push path wires `ubx`.
- **Never `ssh` from CI/agents** to a box — the commands above are for the operator to run.
- **Spool.** `--spool` sizes the RAM ring (frames); `--spool-file` adds a reboot-surviving
  disk tier capped by `--spool-disk-mb`. Both are lossless up to their caps; overflow drops
  the oldest and is counted in the disconnect log.
