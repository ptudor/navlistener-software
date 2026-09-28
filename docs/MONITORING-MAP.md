# Collector monitoring map

Open `/gnss/map/` on the collector's read listener. The page and its NASA Earth
image are embedded in the daemon; building or hosting the page needs no JavaScript
dependencies. Enable `[serve].addr` as usual and route `/gnss/map/` and
`/gnss/api/v2/` through the same authenticated reverse proxy when using a private
collector. The standalone page has no external browser requests.

The world is a 2:1 equirectangular map with calculated daylight, observation gaps,
and satellite subpoints. Filters select GPS, Galileo, BeiDou, GLONASS, and QZSS;
the horizon is 5°, 10° (default), or 15°. The default target is one reporting
station; select two for redundancy planning. Click a location or enter its
coordinates to list the visible satellites, elevation angles, and station counts.
Missing satellites are sorted first, which helps compare prospective station sites.

## What the colors mean

For each 2° grid cell, the browser computes WGS-84 topocentric elevation and
examines every selected satellite above the horizon:

- **Red:** at least one has no navigation observation within 60 seconds.
- **Amber:** all have observations, but at least one is below the station target.
- **Untinted:** every visible reference satellite meets the target and the
  reference geometry is available. This is the map's meaning of “target met.”
- **Gray hatching:** the reference is unavailable, delayed, missing a selected
  constellation, or contains satellites without usable coordinates. Known red and
  amber gaps remain visible underneath. Cells with no known visible satellites
  are unknown rather than covered.

Station counts deduplicate sources across navigation signals for each satellite.
They measure recent structural navigation decoding, not bitwise agreement, full
ephemeris reception on every signal, physical separation, or authenticated RF.
RAWX measurements and reference downloads never count as navigation witnesses.
Satellite health is deliberately separate: an unhealthy satellite can still be
monitored, and a healthy satellite can be missing from this collector.

The area figure weights grid cells by their spherical surface area instead of
counting rectangular map pixels. It is an approximate instantaneous share of
Earth's surface meeting the selected criterion. It becomes **Unknown** when the
reference is incomplete; there is no claim of complete geographic coverage based
on a partial catalogue. Satellite markers show subpoints, not receiver locations
or the extent of a satellite's visibility footprint.

This view does not assess local interference, positioning accuracy, every GNSS
signal, terrain/antenna obstructions, or service availability. SBAS, NavIC and
experimental GLONASS slots 25–27 are outside its scope. A useful next station can
be thousands of kilometres from a missing satellite's subpoint: use the elevation
list at the proposed site. A snapshot does not establish coverage over a whole
orbital cycle.

## Why a separate orbit reference is necessary

The collector's normal `svs` and `almanac` feeds describe its received state.
They cannot reliably enumerate satellites the collector has never heard. Removing
an expired satellite from that inventory could make a coverage map look better
precisely when monitoring gets worse.

The map therefore downloads BKG's merged RINEX 3 broadcast-navigation file and
keeps its expected identities separately from local observations. These public
orbits are used only for map geometry. They never enter the ingest store, integrity
detectors, edge reception forecasts, receiver counts, or ordinary almanac feed.

With the read listener enabled, reference downloads run at startup and every
15 minutes. The previous UTC day's file also bootstraps the catalogue at startup
and day changes. Downloads are bounded to 8 MiB compressed and 64 MiB expanded,
with a 45-second timeout. Parsing is all-or-nothing; failed or malformed downloads
retain the last good catalogue. Reference download age above 35 minutes is
reported as delayed.

Propagation uses the existing GNSS library, including BeiDou GEO rotation and
GLONASS numerical integration. RINEX GPS/Galileo/QZSS weeks use the GPS epoch;
BeiDou uses its own epoch. GLONASS record times are UTC. `[state].leap_seconds`
also applies to reference time conversion. Coordinates expire outside these
conservative intervals relative to their absolute orbit epoch:

| Orbit | Earliest | Latest |
|---|---:|---:|
| GPS, Galileo, BeiDou, QZSS | −2 hours | +4 hours |
| GLONASS | −15 minutes | +30 minutes |

Expired coordinates are omitted, but the satellite identity remains. Missing
coordinates prevent a trustworthy visibility test, so uncertainty affects the
whole selected view; turning off the affected constellation can still give a
useful assessment of the others. A fresh local orbit may supply geometry while
the reference orbit is stale. The public catalogue remains independent of all
private audience data.

BKG is an independent reference, not a guaranteed complete operational fleet
inventory. Satellites absent from both the reference and this audience cannot be
assessed. “Target met” always means the selected reference catalogue, at the
displayed time. Retained identities do not age out automatically: an upstream
omission must not silently improve the map. A genuinely retired satellite can
therefore remain unknown. After confirming retirement, rebuild the optional
reference cache during a collector restart; a fresh download restores the current
reference roster. This maintenance also resets the local observation-only roster.

## Configuration and persistence

```toml
[serve]
addr = "127.0.0.1:8080"
map_reference = true
map_reference_cache = "/var/lib/navlistener/map-orbits.json"
```

`map_reference` defaults to true when serving. Set it to false to disable outgoing
reference requests; local observations still appear, with coverage marked unknown.
The optional cache contains only public reference orbits and identities. Create
its parent directory with write permission for the daemon; updates use an atomic
rename. Without a cache, startup reconstructs the reference roster from downloads.
No external network connection is made when the read listener is disabled.

Within an audience, identities learned only from local navigation remain as
unknown entries after normal state expiry. They are removed by an audience reset
or process restart, alongside the rest of that audience's state. Privacy withdrawal
takes precedence over retaining an expected identity.

## Read API and access

`GET /gnss/api/v2/coverage` uses the standard envelope, read authorizer, audience
selection and policy-delivery fence. Its `data` contains:

- `schema` and `audience`, as in other v2 feeds;
- `fresh_seconds`: 60;
- `reference`: source label, download status/time, and expected satellites with
  `name`, `gnssid`, absolute `orbit_epoch`, and optional `ecef_m` in metres;
- `observations`: audience-scoped satellite identities, optional fresh `ecef_m`,
  and `witness_times` (Unix seconds), one timestamp per distinct source across
  all signals, without source identifiers.

This endpoint is rendered on request rather than stored in the historian's
fixed feed set. It does not expand that set or change existing feeds. The browser
polls every 30 seconds, ages witness timestamps locally, and marks snapshots older
than 90 seconds unknown. It clears observations and selected-location rows after
a failed request or an audience change. Late responses from earlier requests are
discarded. Read tokens remain only in tab memory and are cleared when leaving.

## Verification and sources

```sh
make -C go test-map
cd go
go test ./internal/orbitref ./internal/state ./internal/serve
go test -race ./internal/orbitref ./internal/state ./internal/serve
```

The parser/propagator tests compare five constellations against the existing
independent ESA SP3 fixtures. Tests cover malformed and truncated RINEX, epoch
expiry, shrinking/failed downloads, cache restoration, witness deduplication,
audience resets and access control. JavaScript tests cover missing satellites,
redundancy, stale observations, uncertain references, dateline/pole geometry, and
equinox/solstice daylight. `test-map` needs Node.js 18 or newer.

- [BKG GNSS Data Center](https://igs.bkg.bund.de/) and
  [merged navigation archive](https://igs.bkg.bund.de/root_ftp/IGS/BRDC/).
- [RINEX 3.05](../reference/icd/RINEX-3.05.pdf), navigation layouts in Appendix A.
- [NASA Blue Marble](https://science.nasa.gov/earth/earth-observatory/blue-marble-next-generation/);
  image provenance and hashes are in [credits.txt](../go/internal/serve/map/credits.txt).
- [NOAA solar calculation method](https://gml.noaa.gov/grad/solcalc/calcdetails.html),
  based on Meeus. The map uses the geometric solar horizon with a twilight wash.
