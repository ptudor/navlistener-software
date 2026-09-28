// WGS-84 visibility and NOAA/Meeus solar coordinates for a 2:1 Plate Carree map.
// Solar method: https://gml.noaa.gov/grad/solcalc/calcdetails.html
export const RAD = Math.PI / 180;
export const COVERAGE = { partial: 0.5, good: 0.8 };
export const SYSTEMS = [
  { id: 0, name: 'GPS', color: '#2dd4bf' },
  { id: 2, name: 'Galileo', color: '#58a6ff' },
  { id: 3, name: 'BeiDou', color: '#bc8cff' },
  { id: 6, name: 'GLONASS', color: '#f0883e' },
  { id: 5, name: 'QZSS', color: '#e3b341' },
];

export function sunDirection(date) {
  const jd = date.getTime() / 86400000 + 2440587.5;
  const t = (jd - 2451545) / 36525;
  const meanLongitude = 280.46646 + t * (36000.76983 + 0.0003032 * t);
  const meanAnomaly = (357.52911 + t * (35999.05029 - 0.0001537 * t)) * RAD;
  const center = Math.sin(meanAnomaly) * (1.914602 - t * (0.004817 + 0.000014 * t))
    + Math.sin(2 * meanAnomaly) * (0.019993 - 0.000101 * t) + Math.sin(3 * meanAnomaly) * 0.000289;
  const omega = (125.04 - 1934.136 * t) * RAD;
  const lambda = (meanLongitude + center - 0.00569 - 0.00478 * Math.sin(omega)) * RAD;
  const epsilon = (23 + (26 + (21.448 - t * (46.815 + t * (0.00059 - t * 0.001813))) / 60) / 60
    + 0.00256 * Math.cos(omega)) * RAD;
  const declination = Math.asin(Math.sin(epsilon) * Math.sin(lambda));
  const ra = Math.atan2(Math.cos(epsilon) * Math.sin(lambda), Math.cos(lambda));
  const gmst = (280.46061837 + 360.98564736629 * (jd - 2451545) + 0.000387933 * t * t - t * t * t / 38710000) * RAD;
  const lon = ra - gmst;
  return [Math.cos(declination) * Math.cos(lon), Math.cos(declination) * Math.sin(lon), Math.sin(declination)];
}

export function site(latitude, longitude) {
  const lat = latitude * RAD, lon = longitude * RAD;
  const up = [Math.cos(lat) * Math.cos(lon), Math.cos(lat) * Math.sin(lon), Math.sin(lat)];
  const e2 = 6.6943799901413165e-3;
  const n = 6378137 / Math.sqrt(1 - e2 * up[2] * up[2]);
  return { up, xyz: [n * up[0], n * up[1], n * (1 - e2) * up[2]] };
}

export function elevation(position, ground) {
  const d = position.map((v, i) => v - ground.xyz[i]);
  return Math.asin(Math.max(-1, Math.min(1, d.reduce((sum, v, i) => sum + v * ground.up[i], 0) / Math.hypot(...d)))) / RAD;
}

function validPosition(p) {
  return Array.isArray(p) && p.length === 3 && p.every(Number.isFinite) && Math.hypot(...p) > 2e7 && Math.hypot(...p) < 5e7;
}

export function modelFromFeed(envelope, selected, now = Date.now()) {
  if (!envelope?.ok || !Array.isArray(envelope.data?.reference?.satellites) || !Array.isArray(envelope.data?.observations)) throw new Error('Invalid coverage response');
  const data = envelope.data, sampled = Date.parse(envelope.time);
  if (!Number.isFinite(sampled)) throw new Error('Invalid coverage time');
  const freshSeconds = 60;
  const stale = now - sampled > 90000 || sampled - now > 10000;
  const observations = new Map(data.observations.map(s => [s.name, s]));
  const all = new Map(data.reference.satellites.map(s => [s.name, { ...s, reference: true }]));
  for (const s of observations.values()) if (!all.has(s.name)) all.set(s.name, { ...s, reference: false });
  const satellites = [...all.values()].filter(s => selected.has(s.gnssid)).map(s => {
    const seen = observations.get(s.name);
    const witnesses = !stale && Array.isArray(seen?.witness_times)
      ? seen.witness_times.filter(t => Number.isFinite(t) && t * 1000 <= now + 1000 && now - t * 1000 <= freshSeconds * 1000).length : 0;
    const position = !stale && validPosition(seen?.ecef_m) ? seen.ecef_m : !stale && validPosition(s.ecef_m) ? s.ecef_m : null;
    return { ...s, position, witnesses };
  }).sort((a, b) => a.name.localeCompare(b.name));
  const unknown = satellites.filter(s => !s.position);
  const absentSystems = [...selected].filter(id => !data.reference.satellites.some(s => s.gnssid === id));
  return { satellites, unknown, stale, sampled, audience: data.audience, reference: data.reference,
    uncertain: stale || data.reference.status !== 'current' || unknown.length > 0 || absentSystems.length > 0,
    absentSystems };
}

// Grade the observed share of the known sky, rather than requiring perfection.
// A separate station target can flag a need for redundancy. Unknown orbits cannot
// establish which cells they affect, so uncertainty applies to the whole view.
export function assess(model, latitude, longitude, minElevation, target, details = false) {
  const ground = site(latitude, longitude);
  let expected = 0, missing = 0, thin = 0;
  const visible = [];
  for (const s of model.satellites) {
    if (!s.position) continue;
    const el = elevation(s.position, ground);
    if (el < minElevation) continue;
    expected++;
    if (!s.witnesses) missing++;
    else if (s.witnesses < target) thin++;
    if (details) visible.push({ ...s, elevation: el });
  }
  const observed = expected - missing;
  const observedFraction = expected ? observed / expected : 0;
  const targetFraction = expected ? (observed - thin) / expected : 0;
  const uncertain = model.uncertain || expected === 0;
  const status = expected === 0 ? 'unknown' : observedFraction < COVERAGE.partial ? 'gap'
    : targetFraction < COVERAGE.good ? 'thin' : uncertain ? 'unknown' : 'covered';
  return { expected, observed, missing, thin, observedFraction, targetFraction, uncertain, status, visible };
}

export function worldGrid(model, minElevation, target, step = 2) {
  const cells = [], weights = { gap: 0, thin: 0, unknown: 0, covered: 0 };
  for (let lat = 90 - step / 2; lat > -90; lat -= step) {
    for (let lon = -180 + step / 2; lon < 180; lon += step) {
      const result = assess(model, lat, lon, minElevation, target);
      cells.push(result);
      weights[result.status] += Math.cos(lat * RAD);
    }
  }
  const total = Object.values(weights).reduce((a, b) => a + b, 0);
  return { cells, columns: 360 / step, rows: 180 / step,
    coveredPercent: total ? 100 * weights.covered / total : 0 };
}
