<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { mapPagePath } from '../siteRoutes.js'
import { SYSTEMS, assess, elevation, modelFromFeed, site, subpoint, sunDirection, worldGrid } from '../map/geometry.js'

const props = defineProps({
  full: { type: Boolean, default: false },
  locale: { type: String, default: 'en' },
})

const { t } = useI18n()
const canvas = ref(null)
const selectedSystems = ref(SYSTEMS.map((system) => system.id))
const elevationLimit = ref(10)
const stationTarget = ref(1)
const showNight = ref(true)
const showGaps = ref(true)
const showMarkers = ref(true)
const envelope = ref(null)
const model = ref(null)
const grid = ref(null)
const failure = ref('')
const loading = ref(true)
const selectedSatelliteName = ref('')
const groundLocation = ref(null)
const latitude = ref('0')
const longitude = ref('0')
const tokenInput = ref('')
const token = ref('')
const audience = ref('')
const audiences = ref([])
const accessStatus = ref('')
const earthReady = ref(false)
let earth
let refreshTimer
let ageTimer
let request
let revision = 0
let hitTargets = []

const fullMapHref = computed(() => mapPagePath(props.locale))
const selectedSet = computed(() => new Set(selectedSystems.value))
const satellites = computed(() => model.value?.satellites || [])
const selectedSatellite = computed(() => satellites.value.find((satellite) => satellite.name === selectedSatelliteName.value))
const heard = computed(() => satellites.value.filter((satellite) => satellite.witnesses > 0).length)
const missing = computed(() => satellites.value.filter((satellite) => satellite.reference && !satellite.witnesses).length)
const unknown = computed(() => model.value?.unknown.length ?? null)
const coveredArea = computed(() => {
  if (!model.value || !grid.value || model.value.uncertain) return '—'
  return `${grid.value.coveredPercent.toFixed(1)}%`
})
const snapshotText = computed(() => {
  if (!model.value) return t('map.connecting')
  return t('map.snapshot', { time: new Date(model.value.sampled).toISOString().slice(11, 19) })
})
const referenceTone = computed(() => model.value && !model.value.uncertain ? 'current' : 'uncertain')
const referenceLabel = computed(() => model.value && !model.value.uncertain
  ? t('map.reference_current')
  : t('map.reference_incomplete'))
const notice = computed(() => {
  if (failure.value) return failure.value
  if (!model.value) return t('map.loading')
  const messages = []
  if (model.value.stale) messages.push(t('map.stale'))
  if (model.value.reference.status !== 'current') messages.push(t('map.reference_delayed'))
  if (model.value.unknown.length) messages.push(t('map.unknown_orbits', { count: model.value.unknown.length }))
  if (model.value.absentSystems.length) {
    const names = SYSTEMS.filter((system) => model.value.absentSystems.includes(system.id)).map((system) => system.name).join(', ')
    messages.push(t('map.missing_systems', { systems: names }))
  }
  return messages.length ? messages.join(' ') : t('map.current_notice')
})
const referenceSummary = computed(() => {
  if (!model.value) return t('map.reference_pending')
  const fetched = model.value.reference.fetched_at
    ? t('map.reference_downloaded', { time: new Date(model.value.reference.fetched_at).toISOString().replace('T', ' ').slice(0, 19) })
    : t('map.reference_not_downloaded')
  return `${model.value.reference.source}: ${model.value.reference.status}. ${fetched}`
})
const groundResult = computed(() => {
  if (!model.value || !groundLocation.value) return null
  return assess(model.value, groundLocation.value.lat, groundLocation.value.lon, Number(elevationLimit.value), Number(stationTarget.value), true)
})
const visibleSatellites = computed(() => {
  if (!groundResult.value) return []
  return [...groundResult.value.visible].sort((left, right) => left.witnesses - right.witnesses || right.elevation - left.elevation)
})
const groundSummary = computed(() => {
  if (!groundLocation.value || !groundResult.value) return t('map.location_empty')
  const result = groundResult.value
  const share = result.expected ? ` (${Math.round(result.observedFraction * 100)}%)` : ''
  const uncertainty = result.uncertain ? ` ${t('map.location_uncertain')}` : ''
  return t('map.location_summary', {
    latitude: groundLocation.value.lat.toFixed(2),
    longitude: groundLocation.value.lon.toFixed(2),
    observed: result.observed,
    expected: result.expected,
    share,
    missing: result.missing,
    thin: result.thin,
    uncertainty,
  })
})
const selectionTitle = computed(() => {
  if (selectedSatellite.value) return `${selectedSatellite.value.name} · ${systemName(selectedSatellite.value)}`
  if (groundLocation.value) return t('map.ground_selection', {
    latitude: groundLocation.value.lat.toFixed(2),
    longitude: groundLocation.value.lon.toFixed(2),
  })
  return t('map.no_selection')
})
const selectionDetail = computed(() => {
  const satellite = selectedSatellite.value
  if (satellite) {
    const monitoring = satellite.witnesses
      ? t('map.reporting_stations', { count: satellite.witnesses })
      : t('map.no_recent_navigation')
    const point = satellite.position ? subpoint(satellite.position) : null
    const position = point
      ? t('map.satellite_position', { latitude: point.lat.toFixed(2), longitude: point.lon.toFixed(2) })
      : t('map.position_unavailable')
    const local = groundLocation.value && satellite.position
      ? t('map.local_elevation', {
          elevation: elevation(satellite.position, site(groundLocation.value.lat, groundLocation.value.lon)).toFixed(1),
        })
      : ''
    return [monitoring, position, local].filter(Boolean).join(' ')
  }
  if (groundResult.value) return groundSummary.value
  return props.full ? t('map.selection_help_full') : t('map.selection_help')
})

function systemName(satellite) {
  return SYSTEMS.find((system) => system.id === satellite.gnssid)?.name || ''
}

function statusText(satellite) {
  if (!satellite.witnesses) return t('map.status_missing')
  if (satellite.witnesses < Number(stationTarget.value)) return t('map.status_redundancy')
  return t('map.status_met')
}

function toggleSystem(id) {
  const next = new Set(selectedSystems.value)
  if (next.has(id)) {
    if (next.size === 1) return
    next.delete(id)
  } else next.add(id)
  selectedSystems.value = SYSTEMS.filter((system) => next.has(system.id)).map((system) => system.id)
}

function headers() {
  const result = { Accept: 'application/json' }
  if (token.value) result.Authorization = `Bearer ${token.value}`
  if (audience.value) result['X-GNSS-Audience'] = audience.value
  return result
}

async function read(path, signal) {
  const response = await fetch(`/gnss/api/v2/${path}`, {
    headers: headers(),
    signal,
    cache: 'no-store',
    credentials: 'same-origin',
  })
  if (!response.ok) {
    throw new Error(response.status === 401 || response.status === 403
      ? t('map.access_unavailable')
      : t('map.request_failed', { status: response.status }))
  }
  return response.json()
}

function clearPrivateData() {
  envelope.value = null
  model.value = null
  grid.value = null
  selectedSatelliteName.value = ''
  groundLocation.value = null
  hitTargets = []
  draw()
}

function recalculate() {
  if (!envelope.value) {
    draw()
    return
  }
  try {
    const next = modelFromFeed(envelope.value, selectedSet.value)
    model.value = next
    grid.value = worldGrid(next, Number(elevationLimit.value), Number(stationTarget.value))
    if (!next.satellites.some((satellite) => satellite.name === selectedSatelliteName.value)) selectedSatelliteName.value = ''
    draw()
  } catch {
    failure.value = t('map.invalid_response')
    clearPrivateData()
  }
}

async function refresh() {
  clearTimeout(refreshTimer)
  request?.abort()
  request = new AbortController()
  const mine = ++revision
  const timeout = setTimeout(() => request?.abort(), 15000)
  try {
    const next = await read('coverage', request.signal)
    if (mine !== revision) return
    modelFromFeed(next, selectedSet.value)
    envelope.value = next
    failure.value = ''
    loading.value = false
    recalculate()
  } catch (error) {
    if (mine !== revision) return
    clearPrivateData()
    loading.value = false
    failure.value = error.name === 'AbortError' ? t('map.timeout') : error.message
  } finally {
    clearTimeout(timeout)
    if (mine === revision) refreshTimer = setTimeout(refresh, document.hidden ? 60000 : 30000)
  }
}

async function discoverAudiences() {
  if (!props.full) return
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), 15000)
  try {
    const result = await read('audiences', controller.signal)
    audiences.value = Array.isArray(result.data?.audiences) ? result.data.audiences : []
    accessStatus.value = result.data?.principal ? t('map.authenticated') : t('map.public_view')
  } catch (error) {
    accessStatus.value = error.message
  } finally {
    clearTimeout(timeout)
  }
}

function changeAccess() {
  revision += 1
  request?.abort()
  clearPrivateData()
  failure.value = ''
  loading.value = true
  refresh()
  discoverAudiences()
}

function connect() {
  token.value = tokenInput.value.trim()
  tokenInput.value = ''
  audience.value = ''
  changeAccess()
}

function clearAccess() {
  token.value = ''
  tokenInput.value = ''
  audience.value = ''
  audiences.value = []
  changeAccess()
}

function selectAudience() {
  changeAccess()
}

function inspectCoordinates() {
  const lat = Number(latitude.value)
  const lon = Number(longitude.value)
  if (!Number.isFinite(lat) || !Number.isFinite(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180) return
  selectedSatelliteName.value = ''
  groundLocation.value = { lat, lon }
  draw()
}

function selectSatellite(name) {
  selectedSatelliteName.value = satellites.value.some((satellite) => satellite.name === name) ? name : ''
  draw()
}

function clearSelection() {
  selectedSatelliteName.value = ''
  groundLocation.value = null
  draw()
}

function handleMapClick(event) {
  const element = canvas.value
  if (!element) return
  const rect = element.getBoundingClientRect()
  const x = (event.clientX - rect.left) / rect.width * element.width
  const y = (event.clientY - rect.top) / rect.height * element.height
  const hit = hitTargets.find((target) => Math.hypot(target.x - x, target.y - y) <= 18)
  if (hit) {
    groundLocation.value = null
    selectSatellite(hit.name)
    return
  }
  if (!props.full) return
  selectedSatelliteName.value = ''
  groundLocation.value = {
    lat: 90 - y / element.height * 180,
    lon: x / element.width * 360 - 180,
  }
  latitude.value = groundLocation.value.lat.toFixed(3)
  longitude.value = groundLocation.value.lon.toFixed(3)
  draw()
}

function drawNightOverlay(context, width, height) {
  const shade = document.createElement('canvas')
  shade.width = 720
  shade.height = 360
  const shadeContext = shade.getContext('2d')
  const pixels = shadeContext.createImageData(shade.width, shade.height)
  const sun = sunDirection(new Date())
  for (let y = 0; y < shade.height; y += 1) {
    const lat = (90 - (y + 0.5) / 2) * Math.PI / 180
    const sinLat = Math.sin(lat)
    const cosLat = Math.cos(lat)
    for (let x = 0; x < shade.width; x += 1) {
      const lon = (-180 + (x + 0.5) / 2) * Math.PI / 180
      const dot = cosLat * Math.cos(lon) * sun[0] + cosLat * Math.sin(lon) * sun[1] + sinLat * sun[2]
      const index = 4 * (y * shade.width + x)
      pixels.data[index] = 3
      pixels.data[index + 1] = 8
      pixels.data[index + 2] = 24
      pixels.data[index + 3] = 155 * Math.max(0, Math.min(1, -dot / Math.sin(6 * Math.PI / 180)))
      if (Math.abs(dot) < 0.008) {
        pixels.data[index] = 240
        pixels.data[index + 1] = 218
        pixels.data[index + 2] = 153
        pixels.data[index + 3] = 72
      }
    }
  }
  shadeContext.putImageData(pixels, 0, 0)
  context.drawImage(shade, 0, 0, width, height)
}

function drawCoverage(context, width, height) {
  if (!showGaps.value || !grid.value) return
  const overlay = document.createElement('canvas')
  overlay.width = grid.value.columns
  overlay.height = grid.value.rows
  const overlayContext = overlay.getContext('2d')
  const pixels = overlayContext.createImageData(overlay.width, overlay.height)
  grid.value.cells.forEach((cell, index) => {
    const color = cell.status === 'gap'
      ? [248, 81, 73, 52 + 64 * (1 - cell.observedFraction)]
      : cell.status === 'thin'
        ? [210, 153, 34, 24 + 72 * (1 - cell.targetFraction)]
        : cell.status === 'unknown' ? [130, 143, 165, 18] : [0, 0, 0, 0]
    pixels.data.set(color, index * 4)
  })
  overlayContext.putImageData(pixels, 0, 0)
  context.save()
  context.imageSmoothingEnabled = false
  context.drawImage(overlay, 0, 0, width, height)
  context.restore()
}

function drawMarkers(context, width, height) {
  hitTargets = []
  if (!showMarkers.value || !model.value) return
  for (const satellite of model.value.satellites) {
    if (!satellite.position) continue
    const point = subpoint(satellite.position)
    const x = (point.lon + 180) / 360 * width
    const y = (90 - point.lat) / 180 * height
    const color = SYSTEMS.find((system) => system.id === satellite.gnssid)?.color || '#e6edf3'
    hitTargets.push({ name: satellite.name, x, y })
    context.save()
    context.beginPath()
    context.arc(x, y, 9, 0, 2 * Math.PI)
    context.fillStyle = satellite.witnesses ? color : '#0d1117'
    context.fill()
    context.lineWidth = 3
    context.strokeStyle = '#0d1117'
    context.stroke()
    context.beginPath()
    context.arc(x, y, 7, 0, 2 * Math.PI)
    context.lineWidth = 2
    context.strokeStyle = color
    context.stroke()
    if (satellite.name === selectedSatelliteName.value) {
      context.beginPath()
      context.arc(x, y, 14, 0, 2 * Math.PI)
      context.lineWidth = 3
      context.strokeStyle = '#f4f7fb'
      context.stroke()
      context.font = '600 18px Inter, sans-serif'
      const labelWidth = context.measureText(satellite.name).width + 20
      const labelX = Math.max(8, Math.min(width - labelWidth - 8, x - labelWidth / 2))
      const labelY = y < 50 ? y + 20 : y - 42
      context.fillStyle = 'rgba(8, 12, 21, .9)'
      context.fillRect(labelX, labelY, labelWidth, 30)
      context.fillStyle = '#f4f7fb'
      context.fillText(satellite.name, labelX + 10, labelY + 21)
    }
    context.restore()
  }
}

function draw() {
  const element = canvas.value
  if (!element) return
  const context = element.getContext('2d')
  const width = element.width
  const height = element.height
  context.clearRect(0, 0, width, height)
  const ocean = context.createLinearGradient(0, 0, 0, height)
  ocean.addColorStop(0, '#11283b')
  ocean.addColorStop(1, '#07131f')
  context.fillStyle = ocean
  context.fillRect(0, 0, width, height)
  if (earthReady.value) context.drawImage(earth, 0, 0, width, height)
  context.fillStyle = 'rgba(5, 11, 20, .16)'
  context.fillRect(0, 0, width, height)
  if (showNight.value) drawNightOverlay(context, width, height)
  drawCoverage(context, width, height)

  context.strokeStyle = 'rgba(224, 233, 250, .12)'
  context.lineWidth = 1
  context.beginPath()
  for (let lon = -150; lon < 180; lon += 30) {
    const x = (lon + 180) / 360 * width
    context.moveTo(x, 0)
    context.lineTo(x, height)
  }
  for (let lat = -60; lat <= 60; lat += 30) {
    const y = (90 - lat) / 180 * height
    context.moveTo(0, y)
    context.lineTo(width, y)
  }
  context.stroke()
  drawMarkers(context, width, height)

  if (groundLocation.value) {
    const x = (groundLocation.value.lon + 180) / 360 * width
    const y = (90 - groundLocation.value.lat) / 180 * height
    context.strokeStyle = '#fff'
    context.lineWidth = 3
    context.beginPath()
    context.arc(x, y, 11, 0, 2 * Math.PI)
    context.stroke()
    context.beginPath()
    context.moveTo(x - 19, y)
    context.lineTo(x + 19, y)
    context.moveTo(x, y - 19)
    context.lineTo(x, y + 19)
    context.stroke()
  }
}

function handleVisibility() {
  if (!document.hidden) refresh()
}

watch([selectedSystems, elevationLimit, stationTarget], recalculate, { deep: true })
watch([showNight, showGaps, showMarkers, selectedSatelliteName, groundLocation], draw, { deep: true })
watch(audience, selectAudience)

onMounted(() => {
  earth = new Image()
  earth.src = '/in/map/earth.jpg'
  earth.addEventListener('load', () => {
    earthReady.value = true
    draw()
  })
  earth.addEventListener('error', () => {
    if (!failure.value) failure.value = t('map.image_unavailable')
    draw()
  })
  document.addEventListener('visibilitychange', handleVisibility)
  draw()
  refresh()
  discoverAudiences()
  ageTimer = setInterval(recalculate, 10000)
})

onBeforeUnmount(() => {
  revision += 1
  request?.abort()
  clearTimeout(refreshTimer)
  clearInterval(ageTimer)
  document.removeEventListener('visibilitychange', handleVisibility)
  token.value = ''
  clearPrivateData()
})
</script>

<template>
  <section id="monitoring-map" class="coverage-map" :class="{ 'coverage-map-full': props.full }" aria-labelledby="coverage-heading">
    <div class="coverage-heading">
      <div>
        <p class="coverage-eyebrow">{{ t('map.eyebrow') }}</p>
        <h1 id="coverage-heading">{{ t(props.full ? 'map.full_title' : 'map.title') }}</h1>
        <p>{{ t(props.full ? 'map.full_intro' : 'map.intro') }}</p>
      </div>
      <a v-if="!props.full" class="coverage-link" :href="fullMapHref">{{ t('map.open_full') }} <span aria-hidden="true">↗</span></a>
    </div>

    <div class="coverage-panel">
      <div class="coverage-toolbar">
        <fieldset class="system-filters">
          <legend>{{ t('map.constellations') }}</legend>
          <button
            v-for="system in SYSTEMS"
            :key="system.id"
            type="button"
            class="map-system"
            :class="`tone-${system.key}`"
            :aria-pressed="selectedSystems.includes(system.id)"
            @click="toggleSystem(system.id)"
          ><span class="map-check" aria-hidden="true">✓</span>{{ system.name }}</button>
        </fieldset>
        <div v-if="props.full" class="map-settings">
          <label>{{ t('map.horizon') }} <select v-model="elevationLimit"><option :value="5">5°</option><option :value="10">10°</option><option :value="15">15°</option></select></label>
          <label>{{ t('map.target') }} <select v-model="stationTarget"><option :value="1">{{ t('map.one_station') }}</option><option :value="2">{{ t('map.two_stations') }}</option></select></label>
          <label><input v-model="showNight" type="checkbox"> {{ t('map.day_night') }}</label>
          <label><input v-model="showGaps" type="checkbox"> {{ t('map.gaps') }}</label>
          <label><input v-model="showMarkers" type="checkbox"> {{ t('map.satellites') }}</label>
        </div>
      </div>

      <div class="map-frame">
        <canvas
          ref="canvas"
          width="1800"
          height="900"
          role="img"
          :aria-label="t(props.full ? 'map.canvas_full' : 'map.canvas')"
          @click="handleMapClick"
        ></canvas>
        <div class="map-caption" aria-hidden="true"><span>{{ t('map.coverage_label') }}</span><span>{{ model?.audience || t('map.public_audience') }}</span></div>
      </div>

      <div class="map-foot">
        <div class="map-legend" :aria-label="t('map.legend')">
          <span><i class="map-swatch gap"></i>{{ t('map.legend_gap') }}</span>
          <span><i class="map-swatch thin"></i>{{ t('map.legend_thin') }}</span>
          <span><i class="map-swatch covered"></i>{{ t('map.legend_covered') }}</span>
          <span><i class="map-swatch unknown"></i>{{ t('map.legend_unknown') }}</span>
        </div>
        <div class="map-freshness"><span class="reference-pill" :data-tone="referenceTone">{{ referenceLabel }}</span><span>{{ snapshotText }} UTC</span></div>
      </div>

      <div v-if="props.full" class="map-picker">
        <p>{{ t('map.map_help') }}</p>
        <label>{{ t('map.satellite') }} <select v-model="selectedSatelliteName" @change="draw"><option value="">{{ t('map.choose_satellite') }}</option><option v-for="satellite in satellites" :key="satellite.name" :value="satellite.name">{{ satellite.name }} · {{ systemName(satellite) }}</option></select></label>
      </div>

      <div v-if="props.full || selectedSatellite || groundLocation" class="map-selection" aria-live="polite">
        <div><strong>{{ selectionTitle }}</strong><p>{{ selectionDetail }}</p></div>
        <button v-if="selectedSatellite || groundLocation" type="button" class="map-clear" @click="clearSelection">{{ t('map.clear') }}</button>
      </div>
    </div>

    <p class="map-notice" :data-tone="failure ? 'error' : referenceTone" role="status">{{ notice }}</p>

    <dl class="map-metrics" :aria-label="t('map.summary')">
      <div><dt>{{ t('map.heard') }}</dt><dd>{{ model ? heard : '—' }}</dd><small>{{ t('map.heard_note') }}</small></div>
      <div><dt>{{ t('map.missing') }}</dt><dd>{{ model ? missing : '—' }}</dd><small>{{ t('map.missing_note') }}</small></div>
      <div><dt>{{ t('map.area') }}</dt><dd>{{ coveredArea }}</dd><small>{{ t('map.area_note') }}</small></div>
      <div><dt>{{ t('map.orbits_unknown') }}</dt><dd>{{ unknown ?? '—' }}</dd><small>{{ t('map.orbits_note') }}</small></div>
    </dl>

    <template v-if="props.full">
      <div class="map-analysis">
        <section id="ground-location" class="map-detail" aria-labelledby="location-heading">
          <div class="map-detail-heading"><div><h2 id="location-heading">{{ t('map.location_heading') }}</h2><p>{{ t('map.location_intro') }}</p></div></div>
          <form class="location-form" @submit.prevent="inspectCoordinates">
            <label>{{ t('map.latitude') }} <input v-model="latitude" type="number" min="-90" max="90" step="any" required></label>
            <label>{{ t('map.longitude') }} <input v-model="longitude" type="number" min="-180" max="180" step="any" required></label>
            <button type="submit" class="map-action">{{ t('map.inspect') }}</button>
          </form>
          <p class="location-summary" aria-live="polite">{{ groundSummary }}</p>
          <div class="map-table-wrap">
            <table>
              <caption class="sr-only">{{ t('map.location_caption') }}</caption>
              <thead><tr><th>{{ t('map.satellite') }}</th><th>{{ t('map.elevation') }}</th><th>{{ t('map.stations') }}</th><th>{{ t('map.monitoring') }}</th></tr></thead>
              <tbody>
                <tr v-for="satellite in visibleSatellites" :key="satellite.name">
                  <td><button type="button" class="satellite-link" @click="selectSatellite(satellite.name)">{{ satellite.name }}</button></td>
                  <td>{{ satellite.elevation.toFixed(0) }}°</td><td>{{ satellite.witnesses }}</td><td :data-status="statusText(satellite)">{{ statusText(satellite) }}</td>
                </tr>
                <tr v-if="groundLocation && !visibleSatellites.length"><td colspan="4" class="empty">{{ t('map.none_visible') }}</td></tr>
              </tbody>
            </table>
          </div>
          <p v-if="model?.unknown.length" class="unknown-list">{{ t('map.cannot_locate', { satellites: model.unknown.map((satellite) => satellite.name).join(', ') }) }}</p>
        </section>

        <aside class="map-detail map-explanation">
          <h2>{{ t('map.gap_heading') }}</h2>
          <p>{{ t('map.gap_copy_one') }}</p>
          <p>{{ t('map.gap_copy_two') }}</p>
          <p>{{ t('map.gap_copy_three') }}</p>
          <details>
            <summary>{{ t('map.reference_heading') }}</summary>
            <p>{{ t('map.reference_copy_one') }}</p>
            <p>{{ t('map.reference_copy_two') }}</p>
            <p>{{ referenceSummary }}</p>
            <p><a href="https://igs.bkg.bund.de/">{{ t('map.reference_link') }}</a> · <a href="/in/map/credits.txt">{{ t('map.image_provenance') }}</a></p>
          </details>
          <details>
            <summary>{{ t('map.access_heading') }}</summary>
            <p>{{ t('map.access_copy') }}</p>
            <form class="access-form" @submit.prevent="connect">
              <label>{{ t('map.read_token') }} <input v-model="tokenInput" type="password" autocomplete="off" :placeholder="t('map.optional')"></label>
              <div><button type="submit" class="map-action">{{ t('map.connect') }}</button><button type="button" class="map-secondary" @click="clearAccess">{{ t('map.clear') }}</button></div>
            </form>
            <label class="audience-select">{{ t('map.audience') }} <select v-model="audience"><option value="">{{ t('map.collector_default') }}</option><option v-for="item in audiences" :key="item" :value="item">{{ item }}</option></select></label>
            <p class="access-status" role="status">{{ accessStatus }}</p>
          </details>
        </aside>
      </div>
      <p class="map-credit">{{ t('map.credit') }}</p>
    </template>
  </section>
</template>

<style scoped>
.coverage-map { padding: 38px 0 28px; scroll-margin-top: 20px; }
.coverage-map-full { padding-bottom: 40px; }
.coverage-heading { display: flex; justify-content: space-between; align-items: end; gap: 28px; margin-bottom: 18px; }
.coverage-heading h1 { margin: 7px 0 5px; line-height: 1.08; letter-spacing: -.04em; }
.coverage-heading h1 { font-size: clamp(34px, 4vw, 50px); }
.coverage-heading p:not(.coverage-eyebrow) { color: var(--muted); font-size: 14px; }
.coverage-eyebrow { color: var(--accent); font: 10px/1.5 var(--mono); letter-spacing: .15em; text-transform: uppercase; }
.coverage-link { flex: none; font-size: 13px; margin-bottom: 5px; }
.coverage-panel { overflow: hidden; border: 1px solid var(--line); border-radius: 12px; background: var(--deep); box-shadow: 0 24px 70px rgba(0, 0, 0, .22); }
.coverage-toolbar { display: flex; justify-content: space-between; gap: 15px; padding: 13px 15px; flex-wrap: wrap; align-items: center; }
.system-filters { display: flex; gap: 7px; padding: 0; margin: 0; border: 0; flex-wrap: wrap; }
.system-filters legend { position: absolute; width: 1px; height: 1px; overflow: hidden; clip-path: inset(50%); }
.map-system { min-height: 34px; padding: 6px 10px; border: 1px solid var(--line); border-radius: 6px; background: var(--surface); color: var(--muted); font-size: 12px; }
.map-system.tone-gps { --system-color: var(--gps); }
.map-system.tone-galileo { --system-color: var(--galileo); }
.map-system.tone-beidou { --system-color: var(--beidou); }
.map-system.tone-glonass { --system-color: var(--glonass); }
.map-system.tone-qzss { --system-color: var(--qzss); }
.map-system[aria-pressed="true"] { color: var(--system-color); background: var(--hover); border-color: var(--system-color); }
.map-system[aria-pressed="false"] .map-check { visibility: hidden; }
.map-check { margin-right: 6px; }
.map-settings { display: flex; align-items: center; gap: 13px; flex-wrap: wrap; }
.map-settings label, .location-form label, .access-form label, .audience-select { display: inline-flex; align-items: center; gap: 6px; color: var(--text); font-size: 11px; }
.map-settings input { accent-color: var(--accent); }
.map-settings select, .audience-select select { min-height: 32px; padding: 4px 7px; }
.map-frame { position: relative; aspect-ratio: 2 / 1; overflow: hidden; background: #07131f; border-block: 1px solid var(--line); }
canvas { display: block; width: 100%; height: 100%; cursor: crosshair; }
.map-caption { position: absolute; z-index: 1; inset: 14px 16px auto; display: flex; justify-content: space-between; gap: 12px; pointer-events: none; color: #eef4ff; font: 10px/1 var(--mono); letter-spacing: .13em; text-transform: uppercase; text-shadow: 0 2px 6px #000; }
.map-caption span { padding: 8px 9px; border: 1px solid rgba(255, 255, 255, .12); border-radius: 5px; background: rgba(5, 10, 20, .76); }
.map-foot { display: flex; justify-content: space-between; align-items: center; gap: 18px; padding: 12px 15px; }
.map-legend, .map-freshness { display: flex; align-items: center; flex-wrap: wrap; gap: 10px 16px; color: var(--muted); font-size: 10px; }
.map-legend > span { display: inline-flex; align-items: center; gap: 6px; }
.map-swatch { display: inline-block; width: 13px; height: 9px; border: 1px solid rgba(255, 255, 255, .2); border-radius: 2px; }
.map-swatch.gap { background: rgba(248, 81, 73, .72); }
.map-swatch.thin { background: rgba(210, 153, 34, .66); }
.map-swatch.covered { background: rgba(45, 212, 191, .24); }
.map-swatch.unknown { background: rgba(130, 143, 165, .24); }
.map-freshness { justify-content: flex-end; font-family: var(--mono); }
.reference-pill { padding: 4px 7px; border: 1px solid var(--line); border-radius: 999px; }
.reference-pill[data-tone="current"] { color: var(--gps); border-color: color-mix(in srgb, var(--gps) 55%, transparent); }
.reference-pill[data-tone="uncertain"] { color: var(--warning); border-color: color-mix(in srgb, var(--warning) 55%, transparent); }
.map-picker { display: flex; justify-content: space-between; align-items: center; gap: 18px; padding: 0 15px 13px; }
.map-picker p { color: var(--muted); font-size: 11px; }
.map-picker label { display: inline-flex; align-items: center; gap: 7px; flex: none; color: var(--text); font-size: 11px; }
.map-picker select { max-width: 220px; min-height: 34px; padding: 5px 8px; }
.map-selection { display: flex; justify-content: space-between; align-items: start; gap: 18px; padding: 14px 16px; border-top: 1px solid var(--soft-line); background: var(--surface); }
.map-selection strong { font-size: 13px; }
.map-selection p { margin-top: 4px; color: var(--muted); font-size: 12px; }
.map-clear, .map-secondary { flex: none; min-height: 32px; padding: 5px 10px; border: 1px solid var(--line); border-radius: 5px; background: transparent; color: var(--accent); font-size: 11px; }
.map-notice { min-height: 42px; margin: 11px 0 0; padding: 10px 13px; border-left: 2px solid var(--accent); background: var(--deep); color: var(--muted); font-size: 12px; }
.map-notice[data-tone="uncertain"] { border-color: var(--warning); }
.map-notice[data-tone="error"] { border-color: var(--error); color: var(--error); }
.map-metrics { display: grid; grid-template-columns: repeat(4, 1fr); margin: 0; padding: 24px 0 8px; }
.map-metrics > div { padding: 0 20px; border-left: 1px solid var(--soft-line); }
.map-metrics > div:first-child { padding-left: 0; border: 0; }
.map-metrics dt { color: var(--muted); font: 10px/1.5 var(--mono); letter-spacing: .08em; text-transform: uppercase; }
.map-metrics dd { margin: 4px 0 1px; font: 28px/1.35 var(--mono); letter-spacing: -.05em; }
.map-metrics small { color: var(--muted); font-size: 10px; }
.map-analysis { display: grid; grid-template-columns: 1.2fr 1fr; gap: 20px; margin-top: 28px; }
.map-detail { padding: 22px; border: 1px solid var(--line); border-radius: 10px; background: var(--deep); }
.map-detail h2 { margin: 0 0 4px; font-size: 18px; }
.map-detail p { color: var(--muted); font-size: 12px; }
.location-form, .access-form { display: flex; align-items: end; gap: 11px; flex-wrap: wrap; margin-top: 17px; }
.location-form label, .access-form label { flex-direction: column; align-items: start; }
.location-form input { width: 100px; }
.location-form input, .access-form input { min-height: 35px; padding: 6px 8px; border: 1px solid var(--line); border-radius: 5px; background: var(--surface); color: var(--text); }
.map-action { min-height: 35px; padding: 6px 13px; border: 1px solid var(--accent); border-radius: 5px; background: var(--accent); color: var(--bg); font-size: 12px; font-weight: 600; }
.location-summary { min-height: 40px; margin: 16px 0 8px; }
.map-table-wrap { max-height: 360px; overflow: auto; }
table { width: 100%; border-collapse: collapse; text-align: left; font: 11px/1.5 var(--mono); }
th, td { padding: 9px 8px; border-bottom: 1px solid var(--soft-line); }
th { position: sticky; top: 0; background: var(--deep); color: var(--muted); font-size: 9px; text-transform: uppercase; letter-spacing: .06em; }
.satellite-link { min-height: 0; padding: 0; border: 0; background: transparent; color: var(--accent); font: inherit; text-decoration: underline; text-underline-offset: 3px; }
td[data-status="Missing"] { color: var(--error); }
td[data-status="Needs redundancy"] { color: var(--warning); }
td[data-status="Target met"] { color: var(--gps); }
.unknown-list, .access-status { margin-top: 12px; }
.map-explanation > p + p { margin-top: 10px; }
details { margin-top: 17px; }
summary { width: fit-content; cursor: pointer; color: var(--accent); font-size: 12px; }
details p { margin-top: 10px; }
.access-form > div { display: flex; gap: 7px; }
.audience-select { margin-top: 13px; }
.map-credit { margin-top: 14px; text-align: right; color: var(--muted); font-size: 10px; }
.empty { padding: 26px 10px; text-align: center; color: var(--muted); }
@media (max-width: 900px) {
  .coverage-heading { align-items: start; }
  .map-foot { align-items: start; flex-direction: column; }
  .map-freshness { justify-content: start; }
  .map-picker { align-items: start; flex-direction: column; }
  .map-analysis { grid-template-columns: 1fr; }
  .map-metrics > div { padding-inline: 12px; }
}
@media (max-width: 620px) {
  .coverage-map { padding-top: 27px; }
  .coverage-heading { align-items: start; flex-direction: column; gap: 9px; }
  .coverage-heading h1 { font-size: 32px; }
  .coverage-toolbar { padding: 10px; }
  .map-system { padding-inline: 8px; font-size: 10px; }
  .map-frame { aspect-ratio: 1.55 / 1; }
  .map-caption { inset: 7px 7px auto; font-size: 8px; }
  .map-caption span { padding: 6px; }
  .map-foot { padding: 10px; }
  .map-legend { gap: 7px 10px; }
  .map-metrics { grid-template-columns: 1fr 1fr; gap: 18px 0; }
  .map-metrics > div:nth-child(3) { padding-left: 0; border: 0; }
  .map-metrics dd { font-size: 24px; }
  .map-selection { flex-wrap: wrap; }
  .map-detail { padding: 16px; }
}
</style>
