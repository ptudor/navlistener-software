# Collector monitoring map

The public Vue dashboard opens with a compact map at `/in/`; `/in/map/` provides
the complete location, redundancy, reference, and audience controls. Both are
static exports from `collector-web/` and read the Go collector's
`/gnss/api/v2/coverage` endpoint on the same origin. The Go service publishes the
API only. When using a private collector, serve the static export and route
`/gnss/api/v2/` through the same authenticated reverse proxy. Every feed is
requested with same-origin credentials, so the proxy must authenticate
`/gnss/api/v2/*` with a same-origin cookie or HTTP Basic credential; one
browser login then serves the overview feeds and the map alike. The page makes
no external browser requests.

The world is a 2:1 equirectangular map with calculated daylight, observation gaps,
and satellite subpoints. Filters select GPS, Galileo, BeiDou, GLONASS, and QZSS;
the horizon is 5°, 10° (default), or 15°. The default target is one reporting
station; select two for redundancy planning. Click a location or enter its
coordinates to list the visible satellites, elevation angles, and station counts.
Missing satellites are sorted first, which helps compare prospective station sites.

Clicking or tapping a satellite dot selects it and shows its reporting-station
count and map subpoint directly below the map. If a ground location is selected,
the details also show the satellite's elevation there. The satellite selector
supports keyboard access and overlapping dots. Satellite IDs in the
ground-location table open the same details.

Clicking empty map space selects a ground location instead: a crosshair marks it,
the summary gives its observed/missing counts, and “View visible satellites” links
to the detailed list. Selecting a satellite does not move the ground location.
Selection details update with observations and clear when access is lost.

The interface uses Integrity Satellite's compass mark, Inter and JetBrains Mono,
lavender accent (`#a78bfa`), dark backgrounds, and constellation colors. Fonts,
their licenses, and the Earth image ship with the static export.

## What the colors mean

For each 2° grid cell, the browser computes WGS-84 topocentric elevation and
examines every selected satellite above the horizon:

- **Red:** fewer than 50% of visible satellites with known positions have
  navigation observations within 60 seconds.
- **Amber:** at least 50% are observed, but fewer than 80% meet the station target.
  The tint gets lighter as more visible satellites meet the target.
- **Untinted:** at least 80% meet the station target and reference geometry is
  available. This is useful monitoring, not a claim of perfect reception.
- **Subtle gray tint and reference status:** the reference is unavailable,
  delayed, missing a selected constellation, or contains satellites without
  usable coordinates. Known red and amber gaps remain visible. Cells with no
  known visible satellites are unknown rather than covered.
- **Gray hatching:** no satellite with a known position is above the horizon,
  while some orbits are unknown or a selected constellation is absent from the
  reference. Nothing there can be assessed, so the cell must not look clearer
  than the known gaps around it. A stale snapshot hatches the whole map.

The 50% and 80% thresholds grade practical monitoring instead of demanding every
satellite. The default one-station target therefore clears at 80% observed. With
at least 50% observed, insufficient two-station redundancy shows amber rather than
claiming an absence of observations. The location details show the observed share
and every remaining gap, even above the 80% threshold. This maps observations of
shared satellites anywhere in the audience, not a reception radius around a station.

Station counts deduplicate sources across navigation signals for each satellite.
They measure recent structural navigation decoding, not bitwise agreement, full
ephemeris reception on every signal, physical separation, or authenticated RF.
RAWX measurements and reference downloads never count as navigation witnesses.
Satellite health is deliberately separate: an unhealthy satellite can still be
monitored, and a healthy satellite can be missing from this collector.

The area figure weights grid cells by their spherical surface area instead of
counting rectangular map pixels. It is an approximate instantaneous share of
Earth's surface where at least 80% meet the selected station target. It becomes **Unknown** when the
reference is incomplete; there is no claim of complete geographic coverage based
on a partial catalogue. Satellite markers show subpoints, not receiver locations
or the extent of a satellite's visibility footprint.

This view does not assess local interference, positioning accuracy, every GNSS
signal, terrain/antenna obstructions, or service availability. SBAS, NavIC and
GLONASS slots above 24 (test and commissioning slots such as R27 and R30) are
outside its scope. A useful next station can
be thousands of kilometres from a missing satellite's subpoint: use the elevation
list at the proposed site. A snapshot does not establish coverage over a whole
orbital cycle.

## Why a separate orbit reference is necessary

The collector's normal `svs` and `almanac` feeds describe its received state.
They cannot reliably enumerate satellites the collector has never heard. Removing
an expired satellite from that inventory could make a coverage map look better
precisely when monitoring gets worse.

Broadcast almanacs narrow that gap without closing it. Every satellite
broadcasts its whole constellation's almanac, so one station hearing any GPS,
Galileo, QZSS, GLONASS or BeiDou satellite learns where all of that
constellation's satellites are, including those no station hears. The collector
decodes these almanacs and the map uses them for any satellite without a fresh
ephemeris position. GPS, Galileo and QZSS almanacs are accurate to a few
kilometres (GPS and Galileo served within 3.5 days of the almanac reference
time, QZSS within 72 hours). BeiDou's B2a midi almanacs cover the IGSO and MEO
satellites to about 20 km, and its B1I D1 almanac pages add the GEOs and the
satellites the midi almanacs omit; both are served for 7 days. D1 almanacs are
anchored by the week/toa reference in subframe 5 page 8. Ordinary pages cover
PRNs 1–30 regardless of AmEpID; expanded pages are used only when the
transmitter reports AmEpID "11".
An almanac still describes only what the constellation broadcasts, and a
constellation no station hears contributes none.

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

While the reference is delayed, an orbit past its fit window keeps placing its
satellite for up to 72 hours after its epoch (48 hours for GLONASS, the GNSS
library's integration limit), marked `extrapolated`. An upstream outage then
keeps known gaps on the map instead of erasing every unheard satellite at once.
In one sample, broadcast orbits from 2026-10-05 propagated against fresh orbits
up to three days later stayed within 21 km after two days and 36 km after
three for every system, well inside a 2° cell. A current reference never
extrapolates: an orbit that a fresh download no longer refreshes belongs to a
satellite whose geometry is genuinely unknown. Satellite details show the age
of an extrapolated orbit.

Expired coordinates are omitted, but the satellite identity remains. Missing
coordinates prevent a trustworthy visibility test, so uncertainty affects the
whole selected view; turning off the affected constellation can still give a
useful assessment of the others. A fresh local ephemeris or a decoded almanac
may supply geometry while the reference orbit is stale. The public catalogue remains independent of all
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
  `name`, `gnssid`, absolute `orbit_epoch`, optional `ecef_m` in metres, and
  `extrapolated: true` when that position is propagated past the fit window;
- `observations`: audience-scoped satellite identities, optional fresh `ecef_m`
  with `position_source` (`ephemeris` from a fresh broadcast ephemeris, or
  `almanac` from a decoded almanac, including satellites no station hears), and
  `witness_times` (Unix seconds), one timestamp per distinct source across all
  signals, without source identifiers.

This endpoint is rendered on request rather than stored in the historian's
fixed feed set. It does not expand that set or change existing feeds. The browser
polls every 30 seconds, ages witness timestamps locally, and marks snapshots older
than 90 seconds unknown. It clears observations and selected-location rows after
a failed request or an audience change. Late responses from earlier requests are
discarded. Read tokens remain only in tab memory and are cleared when leaving.

## Verification and sources

```sh
gmake -C collector-web test
gmake -C collector-web build
cd go
go test ./internal/orbitref ./internal/state ./internal/serve
go test -race ./internal/orbitref ./internal/state ./internal/serve
```

The parser/propagator tests compare five constellations against the existing
independent ESA SP3 fixtures. Tests cover malformed and truncated RINEX, epoch
expiry, shrinking/failed downloads, cache restoration, witness deduplication,
audience resets and access control. JavaScript tests cover missing satellites,
redundancy, stale observations, uncertain references, dateline/pole geometry, and
equinox/solstice daylight. The collector web build needs Node.js 22 or newer.

- [BKG GNSS Data Center](https://igs.bkg.bund.de/) and
  [merged navigation archive](https://igs.bkg.bund.de/root_ftp/IGS/BRDC/).
- [RINEX 3.05](../reference/icd/RINEX-3.05.pdf), navigation layouts in Appendix A.
- [NASA Blue Marble](https://science.nasa.gov/earth/earth-observatory/blue-marble-next-generation/);
  image provenance and hashes are in [credits.txt](../collector-web/public/map/credits.txt).
- [NOAA solar calculation method](https://gml.noaa.gov/grad/solcalc/calcdetails.html),
  based on Meeus. The map uses the geometric solar horizon with a twilight wash.
