import { SYSTEMS, RAD, sunDirection, modelFromFeed, assess, worldGrid, subpoint, site, elevation } from './geometry.mjs';

const $ = id => document.getElementById(id);
const canvas = $('map'), ctx = canvas.getContext('2d');
const selected = new Set(SYSTEMS.map(s => s.id));
let envelope = null, model = null, grid = null, token = '', audience = '';
let revision = 0, request = null, timer = null, failure = '', location = null;
let satellite = '';
const markerButtons = new Map();
const earth = new Image();
earth.src = 'earth.jpg';
earth.onload = draw;
earth.onerror = () => { failure = 'Earth imagery unavailable; the monitoring overlay still works.'; update(); };

for (const system of SYSTEMS) {
  const label = document.createElement('label'); label.className = 'system';
  const input = document.createElement('input'); input.type = 'checkbox'; input.checked = true;
  input.addEventListener('change', () => {
    if (input.checked) selected.add(system.id); else selected.delete(system.id);
    if (!selected.size) { selected.add(system.id); input.checked = true; }
    update();
  });
  label.append(input, system.name); $('constellations').append(label);
}

function headers() {
  const h = { Accept: 'application/json' };
  if (token) h.Authorization = `Bearer ${token}`;
  if (audience) h['X-GNSS-Audience'] = audience;
  return h;
}

async function read(path, signal, h = headers()) {
  const response = await fetch(`../api/v2/${path}`, { headers: h, signal, cache: 'no-store', credentials: 'same-origin' });
  if (!response.ok) {
    const error = new Error(response.status === 401 || response.status === 403 ? 'Access unavailable. Reconnect with a permitted audience.' : `Collector request failed (${response.status}).`);
    error.status = response.status;
    throw error;
  }
  return response.json();
}

function clearData() {
  envelope = model = grid = null;
  satellite = '';
  $('satellite-picker').replaceChildren(new Option('Choose a satellite', ''));
  $('satellite-picker').disabled = true;
  markerButtons.clear(); $('satellite-markers').replaceChildren();
  for (const id of ['heard', 'missing', 'area', 'unknown']) $(id).textContent = '—';
  $('satellites').replaceChildren(); $('unknown-list').textContent = '';
  $('location-summary').textContent = 'Waiting for observations in this audience.';
  $('map-audience').textContent = audience || 'Collector default';
  $('reference-status').textContent = 'Reference status pending.';
  draw(); showSelection();
}

async function refresh() {
  clearTimeout(timer);
  request?.abort(); request = new AbortController();
  const mine = ++revision, signal = request.signal;
  const timeout = setTimeout(() => request?.abort(), 15000);
  try {
    const next = await read('coverage', signal);
    if (mine !== revision) return;
    modelFromFeed(next, selected); // validate before adopting a response
    envelope = next; failure = ''; update();
  } catch (error) {
    if (mine !== revision) return;
    // Clear private data on any failed refresh, including permission loss.
    // A disconnected tab must never present retained observations as current.
    clearData();
    failure = error.name === 'AbortError' ? 'Collector request timed out.' : error.message;
    $('notice').textContent = `${failure} Coverage is unknown until the next successful update.`;
    $('updated').textContent = 'Disconnected · retrying';
  } finally {
    clearTimeout(timeout);
    if (mine === revision) timer = setTimeout(refresh, document.hidden ? 60000 : 30000);
  }
}

async function discover() {
  const mine = revision;
  try {
    const result = await read('audiences', AbortSignal.timeout(15000));
    if (mine !== revision) return;
    $('audience').replaceChildren(new Option('Collector default', ''));
    for (const key of result.data.audiences) $('audience').append(new Option(key, key));
    $('audience').value = audience;
    $('access-status').textContent = result.data.principal ? 'Authenticated. Choose an audience.' : 'Using the collector’s default read view.';
  } catch (error) {
    if (mine === revision) $('access-status').textContent = error.message;
  }
}

function changeAccess() {
  ++revision; request?.abort(); clearData(); failure = '';
  refresh(); discover();
}
$('access').addEventListener('submit', event => {
  event.preventDefault(); token = $('token').value.trim(); $('token').value = '';
  audience = ''; $('audience').value = ''; changeAccess();
});
$('sign-out').addEventListener('click', () => {
  token = ''; audience = ''; $('token').value = ''; $('audience').replaceChildren(new Option('Collector default', '')); changeAccess();
});
$('audience').addEventListener('change', () => { audience = $('audience').value; changeAccess(); });
for (const id of ['elevation', 'target']) $(id).addEventListener('change', update);
for (const id of ['night', 'gaps']) $(id).addEventListener('change', draw);
$('markers').addEventListener('change', syncMarkers);
$('satellite-picker').addEventListener('change', () => selectSatellite($('satellite-picker').value));
$('clear-selection').addEventListener('click', clearSelection);

function update() {
  if (!envelope) { draw(); return; }
  model = modelFromFeed(envelope, selected);
  grid = worldGrid(model, +$('elevation').value, +$('target').value);
  const heard = model.satellites.filter(s => s.witnesses > 0).length;
  $('heard').textContent = heard;
  $('missing').textContent = model.satellites.filter(s => s.reference && !s.witnesses).length;
  $('area').textContent = model.uncertain ? 'Unknown' : `${grid.coveredPercent.toFixed(1)}%`;
  $('unknown').textContent = model.unknown.length;
  $('map-audience').textContent = model.audience;
  $('updated').textContent = `Snapshot ${new Date(model.sampled).toISOString().slice(11, 19)} UTC`;
  $('reference-status').textContent = `${model.reference.source}: ${model.reference.status}. ${model.reference.fetched_at ? `Last download ${new Date(model.reference.fetched_at).toISOString().replace('T', ' ').slice(0, 19)} UTC.` : 'No reference downloaded yet.'}`;
  const messages = [];
  if (model.stale) messages.push('Collector snapshot is stale; observations and geometry are unknown.');
  if (model.reference.status !== 'current') messages.push('Independent orbit reference is unavailable or delayed.');
  if (model.unknown.length) messages.push(`${model.unknown.length} satellite orbit${model.unknown.length === 1 ? ' is' : 's are'} unavailable; hatching marks uncertain coverage.`);
  if (model.absentSystems.length) messages.push(`Reference missing for ${SYSTEMS.filter(s => model.absentSystems.includes(s.id)).map(s => s.name).join(', ')}.`);
  if (!messages.length) messages.push(`Reference geometry available for ${model.satellites.length} selected satellites. Red: below 50% observed. Amber: 50–80%, or below the station target. Clear: at least 80% meet the target.`);
  $('notice').textContent = failure || messages.join(' ');
  if (!model.satellites.some(s => s.name === satellite)) satellite = '';
  syncPicker(); syncMarkers(); draw(); inspect(); showSelection();
}

function draw() {
  const w = canvas.width, h = canvas.height;
  ctx.clearRect(0, 0, w, h); ctx.fillStyle = '#0d2030'; ctx.fillRect(0, 0, w, h);
  if (earth.complete && earth.naturalWidth) ctx.drawImage(earth, 0, 0, w, h);
  ctx.fillStyle = '#050b1426'; ctx.fillRect(0, 0, w, h);
  if ($('night').checked) {
    const shade = document.createElement('canvas'); shade.width = 720; shade.height = 360;
    const sc = shade.getContext('2d'), pixels = sc.createImageData(720, 360), sun = sunDirection(new Date());
    for (let y = 0; y < 360; y++) {
      const lat = (90 - (y + 0.5) / 2) * RAD, sl = Math.sin(lat), cl = Math.cos(lat);
      for (let x = 0; x < 720; x++) {
        const lon = (-180 + (x + 0.5) / 2) * RAD;
        const dot = cl * Math.cos(lon) * sun[0] + cl * Math.sin(lon) * sun[1] + sl * sun[2];
        const i = 4 * (y * 720 + x);
        pixels.data[i] = 4; pixels.data[i + 1] = 9; pixels.data[i + 2] = 24;
        pixels.data[i + 3] = 155 * Math.max(0, Math.min(1, -dot / Math.sin(6 * RAD)));
        if (Math.abs(dot) < 0.004) { pixels.data[i] = 240; pixels.data[i + 1] = 218; pixels.data[i + 2] = 153; pixels.data[i + 3] = 90; }
      }
    }
    sc.putImageData(pixels, 0, 0); ctx.drawImage(shade, 0, 0, w, h);
  }
  if ($('gaps').checked && grid) {
    const overlay = document.createElement('canvas'); overlay.width = grid.columns; overlay.height = grid.rows;
    const oc = overlay.getContext('2d'), pixels = oc.createImageData(grid.columns, grid.rows);
    grid.cells.forEach((c, i) => {
      const color = c.status === 'gap' ? [248, 81, 73, 65 + 70 * (1 - c.observedFraction)]
        : c.status === 'thin' ? [210, 153, 34, 25 + 85 * (1 - c.targetFraction)]
          : c.status === 'unknown' ? [130, 143, 165, 50] : [0, 0, 0, 0];
      pixels.data.set(color, i * 4);
    });
    oc.putImageData(pixels, 0, 0); ctx.save(); ctx.imageSmoothingEnabled = false; ctx.drawImage(overlay, 0, 0, w, h); ctx.restore();
  }
  if (!model || model.uncertain) {
    ctx.fillStyle = '#18233932'; ctx.fillRect(0, 0, w, h);
    ctx.strokeStyle = '#c8d2e633'; ctx.lineWidth = 1;
    ctx.beginPath(); for (let x = -h; x < w; x += 20) { ctx.moveTo(x, 0); ctx.lineTo(x + h, h); } ctx.stroke();
  }
  ctx.strokeStyle = '#e0e9fa1b'; ctx.lineWidth = 1; ctx.beginPath();
  for (let lon = -150; lon < 180; lon += 30) { const x = (lon + 180) / 360 * w; ctx.moveTo(x, 0); ctx.lineTo(x, h); }
  for (let lat = -60; lat <= 60; lat += 30) { const y = (90 - lat) / 180 * h; ctx.moveTo(0, y); ctx.lineTo(w, y); } ctx.stroke();
  if (location) {
    const x = (location.lon + 180) / 360 * w, y = (90 - location.lat) / 180 * h;
    ctx.strokeStyle = '#fff'; ctx.lineWidth = 2; ctx.beginPath(); ctx.arc(x, y, 9, 0, 2 * Math.PI); ctx.stroke();
    ctx.beginPath(); ctx.moveTo(x - 16, y); ctx.lineTo(x + 16, y); ctx.moveTo(x, y - 16); ctx.lineTo(x, y + 16); ctx.stroke();
  }
}

function systemName(s) { return SYSTEMS.find(sys => sys.id === s.gnssid)?.name || ''; }
function coordinates(point) { return `${point.lat.toFixed(2)}°, ${point.lon.toFixed(2)}°`; }

function syncPicker() {
  // Keep the native selector stable while someone is using it.
  const names = model.satellites.map(s => s.name);
  const previous = [...$('satellite-picker').options].slice(1).map(o => o.value);
  if (names.join(',') !== previous.join(',')) {
    $('satellite-picker').replaceChildren(new Option('Choose a satellite', ''),
      ...model.satellites.map(s => new Option(`${s.name} · ${systemName(s)}`, s.name)));
  }
  $('satellite-picker').disabled = false;
  $('satellite-picker').value = satellite;
}

function syncMarkers() {
  $('satellite-markers').hidden = !$('markers').checked;
  const placed = new Set();
  for (const s of model?.satellites || []) {
    if (!s.position) continue;
    placed.add(s.name);
    let button = markerButtons.get(s.name);
    if (!button) {
      button = document.createElement('button'); button.type = 'button';
      button.className = 'satellite-marker'; button.dataset.satellite = s.name;
      // The native satellite selector provides keyboard access without 100+ tab stops.
      button.tabIndex = -1;
      const dot = document.createElement('span'); dot.className = 'marker-dot';
      const label = document.createElement('span'); label.className = 'marker-label';
      label.textContent = `${s.name} · ${systemName(s)}`;
      button.append(dot, label);
      button.addEventListener('click', () => selectSatellite(s.name));
      markerButtons.set(s.name, button); $('satellite-markers').append(button);
    }
    const point = subpoint(s.position);
    button.style.left = `${(point.lon + 180) / 360 * 100}%`;
    button.style.top = `${(90 - point.lat) / 180 * 100}%`;
    button.style.setProperty('--satellite-color', SYSTEMS.find(sys => sys.id === s.gnssid)?.color || '#e6edf3');
    button.dataset.observed = String(s.witnesses > 0);
    button.dataset.edge = point.lon < -90 ? 'left' : point.lon > 90 ? 'right' : '';
    button.dataset.top = String(point.lat > 65);
    button.classList.toggle('selected', satellite === s.name);
    button.setAttribute('aria-label', `${s.name}, ${systemName(s)}, ${s.witnesses} reporting station${s.witnesses === 1 ? '' : 's'}. Show details.`);
    button.setAttribute('aria-pressed', String(satellite === s.name));
  }
  for (const [name, button] of markerButtons) if (!placed.has(name)) {
    button.remove(); markerButtons.delete(name);
  }
}

function selectSatellite(name) {
  satellite = model?.satellites.some(s => s.name === name) ? name : '';
  $('satellite-picker').value = satellite;
  syncMarkers(); showSelection();
}

function clearSelection() {
  satellite = ''; location = null;
  $('satellite-picker').value = '';
  $('satellites').replaceChildren(); $('unknown-list').textContent = '';
  $('location-summary').textContent = 'Select a ground location to list its visible satellites.';
  syncMarkers(); draw(); showSelection();
}

function showSelection() {
  const s = model?.satellites.find(s => s.name === satellite);
  $('location-link').hidden = true;
  $('clear-selection').hidden = !satellite && !location;
  if (!model) {
    $('selection-title').textContent = 'No current data';
    $('selection-detail').textContent = 'Waiting for collector observations.';
  } else if (s) {
    $('selection-title').textContent = `${s.name} · ${systemName(s)}`;
    const monitoring = model.stale ? 'Snapshot stale; observations unavailable.' : s.witnesses
      ? `${s.witnesses} reporting station${s.witnesses === 1 ? '' : 's'} in the last 60 seconds.`
      : 'No navigation observations in the last 60 seconds.';
    const position = s.position ? `Map subpoint: ${coordinates(subpoint(s.position))}.` : 'Current position unavailable.';
    const local = location && s.position ? `Elevation at ${coordinates(location)}: ${elevation(s.position, site(location.lat, location.lon)).toFixed(1)}°.` : '';
    $('selection-detail').textContent = [monitoring, position, local].filter(Boolean).join(' ');
  } else if (location) {
    const result = assess(model, location.lat, location.lon, +$('elevation').value, +$('target').value);
    $('selection-title').textContent = `Ground location · ${coordinates(location)}`;
    $('selection-detail').textContent = `${result.observed} of ${result.expected} known visible satellites observed${result.expected ? ` (${Math.round(result.observedFraction * 100)}%)` : ''}. ${result.missing} missing; ${result.thin} below station target.${result.uncertain ? ' Reference incomplete.' : ''}`;
    $('location-link').hidden = false;
  } else {
    $('selection-title').textContent = 'No selection';
    $('selection-detail').textContent = 'Select a satellite dot for its details, or a ground location for its coverage.';
  }
}

function inspect() {
  if (!model || !location) return;
  const result = assess(model, location.lat, location.lon, +$('elevation').value, +$('target').value, true);
  $('location-summary').textContent = `${location.lat.toFixed(2)}°, ${location.lon.toFixed(2)}° · ${result.observed} of ${result.expected} known visible satellites observed${result.expected ? ` (${Math.round(result.observedFraction * 100)}%)` : ''} · ${result.missing} missing · ${result.thin} below station target.${result.uncertain ? ' Reference incomplete; additional satellites may be visible.' : ''}`;
  $('satellites').replaceChildren();
  result.visible.sort((a, b) => a.witnesses - b.witnesses || b.elevation - a.elevation).forEach(s => {
    const row = document.createElement('tr');
    const status = !s.witnesses ? 'gap' : s.witnesses < +$('target').value ? 'thin' : 'covered';
    const nameCell = document.createElement('td'), link = document.createElement('button');
    link.type = 'button'; link.className = 'satellite-link'; link.textContent = s.name;
    link.addEventListener('click', () => {
      selectSatellite(s.name);
      $('map-selection').scrollIntoView({ block: 'nearest' });
    });
    nameCell.append(link); row.append(nameCell);
    for (const value of [`${s.elevation.toFixed(0)}°`, s.witnesses, { gap: 'Missing', thin: 'Needs redundancy', covered: 'Target met' }[status]]) {
      const cell = document.createElement('td'); cell.textContent = value; row.append(cell);
    }
    row.lastChild.className = `status-${status}`; $('satellites').append(row);
  });
  $('unknown-list').textContent = model.unknown.length ? `Cannot locate: ${model.unknown.map(s => s.name).join(', ')}. These satellites remain expected; visibility at this location is unknown.` : '';
}

canvas.addEventListener('click', event => {
  const rect = canvas.getBoundingClientRect();
  satellite = ''; $('satellite-picker').value = '';
  location = { lat: 90 - (event.clientY - rect.top) / rect.height * 180, lon: (event.clientX - rect.left) / rect.width * 360 - 180 };
  $('latitude').value = location.lat.toFixed(3); $('longitude').value = location.lon.toFixed(3);
  syncMarkers(); draw(); inspect(); showSelection();
});
$('location').addEventListener('submit', event => {
  event.preventDefault(); satellite = ''; $('satellite-picker').value = '';
  location = { lat: +$('latitude').value, lon: +$('longitude').value };
  syncMarkers(); draw(); inspect(); showSelection();
});
document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
setInterval(() => {
  $('clock').textContent = `${new Date().toISOString().slice(0, 19).replace('T', ' ')} UTC`;
}, 1000);
// Age witnesses even when a proxy repeats the same response. Repaint daylight
// independently of receiver activity, including an entirely idle collector.
setInterval(() => { if (!document.hidden) update(); }, 10000);
window.addEventListener('pagehide', () => { ++revision; request?.abort(); clearTimeout(timer); token = ''; clearData(); });
window.addEventListener('pageshow', event => { if (event.persisted) { audience = ''; changeAccess(); } });
draw(); refresh(); discover();
