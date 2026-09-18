# NavListen software

NavListen collects broadcast navigation messages from GNSS receivers and uses them
to study satellite orbits, clocks, signal health, and the radio environment around
each station. The collector daemon is named `navlistener`.

Receivers and edge feeders forward raw frames to the collector, where they are
decoded, checked, and made available through a versioned JSON API and event stream.
An optional TimescaleDB historian stores raw frames, snapshots, and events for
later analysis and replay.

The repository includes a reusable Go GNSS library, the collector, C and ESP32
feeders, and an Apple station-monitoring app. ESP32 firmware supports the custom
ESP32-S3 observer with 16 MiB flash and 8 MiB PSRAM; the earlier ESP32-C6
development board is unsupported. Observer board projects and
fabrication exports live in the separate `navlistener-hardware` repository,
hosted on ptudor.net.

## Current capabilities

The collector, GNSS library, C and ESP32 feeders, and Apple companion app are
implemented and available to build and use. The collector is ready to configure
and deploy with the supported inputs below. Development continues; support depends
on the signal and input format, and planned extensions are marked in the docs.

| Area | Current support |
|---|---|
| GPS and QZSS | LNAV and CNAV decoding, orbit propagation, and clock corrections |
| Galileo | E1-B I/NAV and E5a F/NAV decoding and propagation; E5b live decoding is deferred |
| BeiDou | B1I D1 and B2a B-CNAV2 decoding and propagation |
| GLONASS | L1OF/L2OF navigation strings, numerical ephemeris propagation, and almanacs |
| SBAS | L1 message headers and the message-type-0 “do not use” indication; correction payloads are not decoded |
| NavIC | Decoder deferred |
| Receiver inputs | UBX navigation and receiver telemetry; SBF and RTCM capture with raw persistence, pending central decoders; NTRIP transport |
| Monitoring | Orbit and clock discontinuities, station liveness, signal capabilities, and receiver RF telemetry |
| Hardware trust | Collector-verified `trusted`, `open`, `test` or `none` per session, from a manufacturer-signed commissioning record and a session proof; implemented in the collector and firmware, with on-device key generation awaiting bench validation |
| Serving | Native `/gnss/api/v2/*` JSON feeds and `/gnss/events` server-sent events, with public and authorized private audiences |

See the [GNSS library](gnss/README.md), [signal coverage](docs/CONSTELLATIONS.md),
[observer hardware contract](docs/HARDWARE-OBSERVER.md), and
[commissioning and hardware trust](docs/COMMISSIONING.md) for details. Federation
transport and cryptographic OSNMA verification remain planned work. The current
[RF monitor](docs/DEFENSE-PNT.md) reports jamming and receiver anomalies; confirmed
spoofing alerts await additional independent detection inputs. ESP32 feeders use
a [RAM-only spool](esp32/README.md#durability-envelope-read-before-deploying-one-as-a-primary-observer).

## Build and run locally

Start from the repository root with Go 1.25 or newer and Make installed. Keep
`go/`, `gnss/`, and `go.work` together: the collector uses the local GNSS module.

```sh
make -C go build
./go/navlistener -version
cp go/navlistener.toml.example go/navlistener.toml
./go/navlistener -config go/navlistener.toml -check-config
./go/navlistener -config go/navlistener.toml
```

The example starts an idle collector with metrics and health on loopback. It has
no enabled receiver sources, database, push listener, or read API. From another
terminal, check its health:

```sh
curl http://127.0.0.1:9100/healthz
```

Stop it with Ctrl-C. Edit `go/navlistener.toml` to configure your receiver and
enable the services you need, then validate the configuration again before
starting it. This local configuration file is Git-ignored.

For live data, configure a `[[ingest]]` source pointing to a receiver's raw TCP
stream, or connect an authenticated edge feeder through `[push]`. The
[configuration example](go/navlistener.toml.example) documents both paths,
including publication policy. Sources are private unless explicitly granted
public use. Direct serial connections use an edge feeder.

Persistence requires TimescaleDB when `[store].dsn` is set. Read feeds are enabled
through `[serve].addr`. See the [collector guide](go/README.md),
[authorization guide](go/internal/authorization/README.md), and
[deployment notes](go/deploy/README.md) for installation and access configuration.

## Tests

Run the Go tests for both modules:

```sh
make -C go test
```

The broader check also runs formatting and vet checks, verifies bundled reference
document hashes, builds ESP32 host tests, and exercises the C feeder against the
Go collector:

```sh
make -C go check
```

That check additionally needs a C compiler and OpenSSL and zstd development
libraries. The [feeder Makefile](feeder/Makefile) provides build overrides.
Live database tests are skipped unless their test database settings are supplied;
see [the integration test setup](go/internal/store/integration_test.go).

The GNSS library includes independent broadcast-ephemeris and precise-orbit
fixtures, alongside analytic, regression, and fuzz tests. Their provenance and
tolerances are documented in [the fixture guide](gnss/testdata/README.md).

## Repository map

| Path | Purpose |
|---|---|
| [gnss/](gnss/README.md) | GNSS decoding and math library, with no third-party dependencies |
| [go/](go/README.md) | Collector, historian, API, and replay tools |
| [feeder/](feeder/deploy/README.md) | C feeder for serial or TCP receivers |
| [esp32/](esp32/README.md) | ESP32 feeder firmware and host tests |
| [swift/](swift/) | Integrity Station companion app for iOS and macOS |
| [web/](web/README.md) | Project website |
| [docs/](docs/DESIGN.md) | Architecture, math, integrity monitoring, and API contracts |
| [reference/](reference/REFERENCES.md) | Interface Control Document catalog and source records |

## Background and license

NavListen draws inspiration from the GNSS monitoring community, including
[galmon](https://galmon.eu/) and its approach to collecting raw navigation
messages for central analysis.

The GNSS math and decoders are developed from published Interface Control
Documents, with citations in the source and [math reference](docs/MATH.md).
Comparisons with independent implementations provide an additional validation
method.

The project code is licensed under [Apache License 2.0](LICENSE). Bundled reference
documents and other third-party assets retain their own terms; see the
[reference catalog](reference/REFERENCES.md) and accompanying notices.

## Release and build identity

[VERSION](VERSION) is the human-facing release (`0.1.0` initially).
[BUILD_NUMBER](BUILD_NUMBER) is a positive, manually advanced build number (`1`
initially). Increment it when publishing a new numbered build; ordinary local
compilation does not change it. Both the Go Makefile and ESP-IDF build consume these
files. Keep them with the source when building from an exported archive.

The daemon reports release, build number, source revision and build time separately.
Firmware uses the compact `release+build.revision` form in its app descriptor and
journal; OTA identity also retains the ELF hash. Rebuilding a numbered release from
modified source adds `-dirty` to its revision. The revision/hash remains the exact
code reference even when a build number is reused during development. This naming
does not change image verification, rollback or anti-rollback policy.
