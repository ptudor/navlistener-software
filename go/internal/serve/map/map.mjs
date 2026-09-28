import { SYSTEMS, RAD, sunDirection, modelFromFeed, assess, worldGrid } from './geometry.mjs';

const $ = id => document.getElementById(id);
const canvas = $('map'), ctx = canvas.getContext('2d');
const selected = new Set(SYSTEMS.map(s => s.id));
let envelope = null, model = null, grid = null, token = '', audience = '';
let revision = 0, request = null, timer = null, failure = '', location = null;
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
  for (const id of ['heard', 'missing', 'area', 'unknown']) $(id).textContent = '—';
  $('satellites').replaceChildren(); $('unknown-list').textContent = '';
  $('location-summary').textContent = 'Waiting for observations in this audience.';
  $('map-audience').textContent = audience || 'Collector default';
  $('reference-status').textContent = 'Reference status pending.';
  draw();
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
for (const id of ['night', 'gaps', 'markers']) $(id).addEventListener('change', draw);

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
  if (!messages.length) messages.push(`Reference geometry available for ${model.satellites.length} selected satellites. Red shows missing observations; amber shows fewer than ${$('target').value} reporting stations.`);
  $('notice').textContent = failure || messages.join(' ');
  draw(); inspect();
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
      const color = c.status === 'gap' ? [237, 56, 76, 75 + 60 * c.missing / c.expected]
        : c.status === 'thin' ? [245, 181, 68, 90] : c.status === 'unknown' ? [130, 143, 165, 50] : [0, 0, 0, 0];
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
  if (model && $('markers').checked) for (const s of model.satellites) {
    if (!s.position) continue;
    const p = s.position, x = (Math.atan2(p[1], p[0]) / RAD + 180) / 360 * w;
    const y = (90 - Math.atan2(p[2], Math.hypot(p[0], p[1])) / RAD) / 180 * h;
    ctx.fillStyle = SYSTEMS.find(sys => sys.id === s.gnssid)?.color || '#fff';
    ctx.strokeStyle = '#07101e'; ctx.lineWidth = 2;
    ctx.beginPath(); ctx.arc(x, y, 4.5, 0, 2 * Math.PI);
    if (s.witnesses) { ctx.fill(); ctx.stroke(); } else { ctx.strokeStyle = '#f4c5cd'; ctx.stroke(); }
  }
  if (location) {
    const x = (location.lon + 180) / 360 * w, y = (90 - location.lat) / 180 * h;
    ctx.strokeStyle = '#fff'; ctx.lineWidth = 2; ctx.beginPath(); ctx.arc(x, y, 9, 0, 2 * Math.PI); ctx.stroke();
    ctx.beginPath(); ctx.moveTo(x - 16, y); ctx.lineTo(x + 16, y); ctx.moveTo(x, y - 16); ctx.lineTo(x, y + 16); ctx.stroke();
  }
}

function inspect() {
  if (!model || !location) return;
  const result = assess(model, location.lat, location.lon, +$('elevation').value, +$('target').value, true);
  $('location-summary').textContent = `${location.lat.toFixed(2)}°, ${location.lon.toFixed(2)}° · ${result.expected} above horizon · ${result.missing} missing · ${result.thin} below station target.${result.uncertain ? ' Reference incomplete; additional satellites may be visible.' : ''}`;
  $('satellites').replaceChildren();
  result.visible.sort((a, b) => a.witnesses - b.witnesses || b.elevation - a.elevation).forEach(s => {
    const row = document.createElement('tr');
    const status = !s.witnesses ? 'gap' : s.witnesses < +$('target').value ? 'thin' : 'covered';
    for (const value of [s.name, `${s.elevation.toFixed(0)}°`, s.witnesses, { gap: 'Missing', thin: 'Needs redundancy', covered: 'Target met' }[status]]) {
      const cell = document.createElement('td'); cell.textContent = value; row.append(cell);
    }
    row.lastChild.className = `status-${status}`; $('satellites').append(row);
  });
  $('unknown-list').textContent = model.unknown.length ? `Cannot locate: ${model.unknown.map(s => s.name).join(', ')}. These satellites remain expected; visibility at this location is unknown.` : '';
}

canvas.addEventListener('click', event => {
  const rect = canvas.getBoundingClientRect();
  location = { lat: 90 - (event.clientY - rect.top) / rect.height * 180, lon: (event.clientX - rect.left) / rect.width * 360 - 180 };
  $('latitude').value = location.lat.toFixed(3); $('longitude').value = location.lon.toFixed(3); draw(); inspect();
});
$('location').addEventListener('submit', event => {
  event.preventDefault(); location = { lat: +$('latitude').value, lon: +$('longitude').value }; draw(); inspect();
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
