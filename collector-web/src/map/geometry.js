// WGS-84 visibility and NOAA/Meeus solar coordinates for a 2:1 Plate Carrée map.
// Solar method: https://gml.noaa.gov/grad/solcalc/calcdetails.html
export const RAD = Math.PI / 180
export const COVERAGE = { partial: 0.5, good: 0.8 }
export const SYSTEMS = [
  { id: 0, key: 'gps', name: 'GPS', color: '#2dd4bf' },
  { id: 2, key: 'galileo', name: 'Galileo', color: '#58a6ff' },
  { id: 3, key: 'beidou', name: 'BeiDou', color: '#bc8cff' },
  { id: 6, key: 'glonass', name: 'GLONASS', color: '#f0883e' },
  { id: 5, key: 'qzss', name: 'QZSS', color: '#e3b341' },
]

export function sunDirection(date) {
  const jd = date.getTime() / 86400000 + 2440587.5
  const t = (jd - 2451545) / 36525
  const meanLongitude = 280.46646 + t * (36000.76983 + 0.0003032 * t)
  const meanAnomaly = (357.52911 + t * (35999.05029 - 0.0001537 * t)) * RAD
  const center = Math.sin(meanAnomaly) * (1.914602 - t * (0.004817 + 0.000014 * t))
    + Math.sin(2 * meanAnomaly) * (0.019993 - 0.000101 * t) + Math.sin(3 * meanAnomaly) * 0.000289
  const omega = (125.04 - 1934.136 * t) * RAD
  const lambda = (meanLongitude + center - 0.00569 - 0.00478 * Math.sin(omega)) * RAD
  const epsilon = (23 + (26 + (21.448 - t * (46.815 + t * (0.00059 - t * 0.001813))) / 60) / 60
    + 0.00256 * Math.cos(omega)) * RAD
  const declination = Math.asin(Math.sin(epsilon) * Math.sin(lambda))
  const ra = Math.atan2(Math.cos(epsilon) * Math.sin(lambda), Math.cos(lambda))
  const gmst = (280.46061837 + 360.98564736629 * (jd - 2451545) + 0.000387933 * t * t - t * t * t / 38710000) * RAD
  const lon = ra - gmst
  return [Math.cos(declination) * Math.cos(lon), Math.cos(declination) * Math.sin(lon), Math.sin(declination)]
}

export function site(latitude, longitude) {
  const lat = latitude * RAD
  const lon = longitude * RAD
  const up = [Math.cos(lat) * Math.cos(lon), Math.cos(lat) * Math.sin(lon), Math.sin(lat)]
  const e2 = 6.6943799901413165e-3
  const n = 6378137 / Math.sqrt(1 - e2 * up[2] * up[2])
  return { up, xyz: [n * up[0], n * up[1], n * (1 - e2) * up[2]] }
}

export function elevation(position, ground) {
  const d = position.map((value, index) => value - ground.xyz[index])
  return Math.asin(Math.max(-1, Math.min(1, d.reduce((sum, value, index) => sum + value * ground.up[index], 0) / Math.hypot(...d)))) / RAD
}

export function subpoint(position) {
  return {
    lat: Math.atan2(position[2], Math.hypot(position[0], position[1])) / RAD,
    lon: Math.atan2(position[1], position[0]) / RAD,
  }
}

function validPosition(position) {
  return Array.isArray(position) && position.length === 3 && position.every(Number.isFinite)
    && Math.hypot(...position) > 2e7 && Math.hypot(...position) < 5e7
}

export const REFERENCE_STATUSES = new Set(['current', 'delayed', 'unavailable'])

// How far the collector's clock, read from a same-origin response's Date
// header, is ahead of the browser's. Feed times are collector times, so the
// staleness and witness windows must be measured on that clock, not the
// browser's. null when the header is absent or unparsable.
export function serverOffset(dateHeader, local = Date.now()) {
  const server = Date.parse(dateHeader)
  return Number.isFinite(server) ? server - local : null
}

export function modelFromFeed(envelope, selected, now = Date.now()) {
  if (!envelope?.ok || !Array.isArray(envelope.data?.reference?.satellites) || !Array.isArray(envelope.data?.observations)) {
    throw new Error('Invalid coverage response')
  }
  const data = envelope.data
  const sampled = Date.parse(envelope.time)
  if (!Number.isFinite(sampled)) throw new Error('Invalid coverage time')
  const stale = now - sampled > 90000 || sampled - now > 10000
  // Parsed once here so the template formats a number or shows "pending";
  // an unknown status is never mistaken for a current reference.
  const fetchedAt = Date.parse(data.reference.fetched_at)
  const reference = {
    ...data.reference,
    status: REFERENCE_STATUSES.has(data.reference.status) ? data.reference.status : 'unavailable',
    fetchedAt: Number.isFinite(fetchedAt) ? fetchedAt : null,
  }
  const observations = new Map(data.observations.map((satellite) => [satellite.name, satellite]))
  const all = new Map(data.reference.satellites.map((satellite) => [satellite.name, { ...satellite, reference: true }]))
  for (const satellite of observations.values()) {
    if (!all.has(satellite.name)) all.set(satellite.name, { ...satellite, reference: false })
  }
  const satellites = [...all.values()].filter((satellite) => selected.has(satellite.gnssid)).map((satellite) => {
    const seen = observations.get(satellite.name)
    const witnesses = !stale && Array.isArray(seen?.witness_times)
      ? seen.witness_times.filter((time) => Number.isFinite(time) && time * 1000 <= now + 1000 && now - time * 1000 <= 60000).length
      : 0
    const local = !stale && validPosition(seen?.ecef_m)
    const position = local
      ? seen.ecef_m
      : !stale && validPosition(satellite.ecef_m) ? satellite.ecef_m : null
    // Only a reference position can be extrapolated past its orbit's fit window.
    const extrapolated = !local && position !== null && satellite.extrapolated === true
    // A collector position comes from a fresh ephemeris or a decoded almanac.
    const positionSource = local ? (seen.position_source === 'almanac' ? 'almanac' : 'ephemeris')
      : position !== null ? 'reference' : null
    return { ...satellite, position, positionSource, extrapolated, witnesses }
  }).sort((a, b) => a.name.localeCompare(b.name))
  const unknown = satellites.filter((satellite) => !satellite.position)
  const absentSystems = [...selected].filter((id) => !data.reference.satellites.some((satellite) => satellite.gnssid === id))
  return {
    satellites,
    unknown,
    stale,
    sampled,
    audience: data.audience,
    reference,
    uncertain: stale || reference.status !== 'current' || unknown.length > 0 || absentSystems.length > 0,
    absentSystems,
  }
}

export function assess(model, latitude, longitude, minElevation, target, details = false) {
  const ground = site(latitude, longitude)
  let expected = 0
  let missing = 0
  let thin = 0
  const visible = []
  for (const satellite of model.satellites) {
    if (!satellite.position) continue
    const angle = elevation(satellite.position, ground)
    if (angle < minElevation) continue
    expected += 1
    if (!satellite.witnesses) missing += 1
    else if (satellite.witnesses < target) thin += 1
    if (details) visible.push({ ...satellite, elevation: angle })
  }
  const observed = expected - missing
  const observedFraction = expected ? observed / expected : 0
  const targetFraction = expected ? (observed - thin) / expected : 0
  const uncertain = model.uncertain || expected === 0
  const status = expected === 0 ? 'unknown' : observedFraction < COVERAGE.partial ? 'gap'
    : targetFraction < COVERAGE.good ? 'thin' : uncertain ? 'unknown' : 'covered'
  // No located satellite is overhead while some orbits are unknown or a
  // selected constellation is absent: nothing here can be assessed, so the
  // cell must not read as clear sky beside known gaps.
  const unmapped = expected === 0 && (model.unknown.length > 0 || model.absentSystems.length > 0)
  return { expected, observed, missing, thin, observedFraction, targetFraction, uncertain, status, unmapped, visible }
}

export function worldGrid(model, minElevation, target, step = 2) {
  const cells = []
  const weights = { gap: 0, thin: 0, unknown: 0, covered: 0 }
  for (let latitude = 90 - step / 2; latitude > -90; latitude -= step) {
    for (let longitude = -180 + step / 2; longitude < 180; longitude += step) {
      const result = assess(model, latitude, longitude, minElevation, target)
      cells.push(result)
      weights[result.status] += Math.cos(latitude * RAD)
    }
  }
  const total = Object.values(weights).reduce((left, right) => left + right, 0)
  return {
    cells,
    columns: 360 / step,
    rows: 180 / step,
    coveredPercent: total ? 100 * weights.covered / total : 0,
  }
}
