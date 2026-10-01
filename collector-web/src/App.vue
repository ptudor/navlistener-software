<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import CoverageMap from './components/CoverageMap.vue'
import { LOCALE_LABELS, SUPPORTED_LOCALES } from './i18n/localeLabels.js'
import {
  SYSTEMS,
  count,
  duration,
  groupSatellites,
  health,
  isNumber,
  stamp,
  systemFor,
  utc,
  validateEnvelope,
} from './model.js'
import { pagePath } from './siteRoutes.js'

const props = defineProps({
  locale: { type: String, default: 'en' },
  page: { type: String, default: 'overview' },
})

const { t } = useI18n()
const FEED_KEYS = ['global', 'svs', 'observers']
const FEED_LABEL_KEYS = { global: 'status.overview', svs: 'status.satellites', observers: 'status.receivers' }
const PAGE_SIZE = 12

const feeds = reactive(Object.fromEntries(
  FEED_KEYS.map((key) => [key, { data: null, error: false, time: 0, age: 0, received: 0 }]),
))
const opened = reactive(new Set())
const receiverOpened = reactive(new Set())
const receiverNumbers = new Map()
const constellation = ref('all')
const healthFilter = ref('all')
const query = ref('')
const page = ref(0)
const busy = ref(false)
const paused = ref(false)
const theme = ref('dark')
const tick = ref(0)
let refreshTimer
let ageTimer

const localeLinks = SUPPORTED_LOCALES.map((locale) => ({
  code: locale,
  label: LOCALE_LABELS[locale],
  href: pagePath(locale, props.page),
}))
const homeHref = computed(() => pagePath(props.locale))
const sectionHref = (section) => props.page === 'map' ? `${pagePath(props.locale)}#${section}` : `#${section}`
const currentLocaleLabel = computed(() => LOCALE_LABELS[props.locale] || LOCALE_LABELS.en)
const satellites = computed(() => groupSatellites(feeds.svs.data))
const presentSystems = computed(() => {
  const ids = new Set(satellites.value.map((satellite) => satellite.id))
  return SYSTEMS.filter((system) => ids.has(system.id))
})
const flaggedCount = computed(() => satellites.value.filter((satellite) => satellite.health.kind === 'flagged').length)
const activeSystems = computed(() => {
  const data = feeds.global.data
  if (!data) return []
  return SYSTEMS
    .filter((system) => isNumber(data[`${system.key}_svs`]) && data[`${system.key}_svs`] > 0)
    .map((system) => ({
      ...system,
      satellites: data[`${system.key}_svs`],
      signals: data[`${system.key}_sigs`],
    }))
})
const mixMaximum = computed(() => Math.max(1, ...activeSystems.value.map((system) => system.satellites)))
const mixNote = computed(() => {
  const data = feeds.global.data
  if (!data) return t('hero.bars_compare')
  const silent = SYSTEMS.filter((system) => data[`${system.key}_svs`] === 0).map((system) => system.name)
  return silent.length ? t('hero.no_snapshot_reports', { systems: silent.join(' · ') }) : t('hero.bars_compare')
})
const filteredSatellites = computed(() => {
  const search = query.value.trim().toLowerCase()
  return satellites.value.filter((satellite) => (
    (constellation.value === 'all' || satellite.id === Number(constellation.value))
    && (healthFilter.value === 'all' || satellite.health.kind === healthFilter.value)
    && `${satellite.name} ${systemFor(satellite.id).name}`.toLowerCase().includes(search)
  ))
})
const pageCount = computed(() => Math.max(1, Math.ceil(filteredSatellites.value.length / PAGE_SIZE)))
const pageStart = computed(() => page.value * PAGE_SIZE)
const visibleSatellites = computed(() => filteredSatellites.value.slice(pageStart.value, pageStart.value + PAGE_SIZE))
const resultCount = computed(() => {
  if (!filteredSatellites.value.length) return t('satellite.none_shown')
  return t('satellite.shown', {
    start: pageStart.value + 1,
    end: Math.min(pageStart.value + PAGE_SIZE, filteredSatellites.value.length),
    count: filteredSatellites.value.length,
  })
})
const satelliteEmpty = computed(() => {
  if (!feeds.svs.data) return feeds.svs.error ? t('satellite.unavailable') : t('satellite.loading')
  return satellites.value.length ? t('satellite.no_match') : t('satellite.no_observations')
})

watch([filteredSatellites, pageCount], () => {
  page.value = Math.max(0, Math.min(page.value, pageCount.value - 1))
})
watch([constellation, healthFilter, query], () => { page.value = 0 })
watch(presentSystems, (systems) => {
  if (constellation.value !== 'all' && !systems.some((system) => system.id === Number(constellation.value))) {
    constellation.value = 'all'
  }
})

function elapsed(feed) {
  void tick.value
  return feed.data ? feed.age + (Date.now() - feed.received) / 1000 : 0
}

function ageValue(seconds, feedName) {
  return isNumber(seconds) && seconds >= 0 ? seconds + elapsed(feeds[feedName]) : null
}

function ageText(seconds, feedName) {
  const value = ageValue(seconds, feedName)
  return value === null ? t('satellite.unknown') : duration(value)
}

function ageClass(seconds, feedName) {
  const value = ageValue(seconds, feedName)
  return { 'is-old': value !== null && value > 300 }
}

function ageTitle(seconds, feedName) {
  const value = ageValue(seconds, feedName)
  return value !== null && value > 300 ? t('satellite.old_title') : t('satellite.age_title')
}

function toneClass(system) {
  return `tone-${system.key}`
}

function signalAccuracy(signal) {
  return signal.sisa_valid === true && isNumber(signal.sisa_m)
    ? `${signal.sisa_m.toFixed(2)} m`
    : t('satellite.not_reported')
}

function displaySignalName(signal) {
  if (signal.gnssid === 2 && signal.sigid === 3) return 'E5a'
  if (signal.gnssid === 3 && signal.sigid === 8) return 'B2a'
  return signal.sigid === 0 ? systemFor(signal.gnssid).primary : t('satellite.signal_name', { id: signal.sigid })
}

function healthText(info) {
  const keys = {
    'Do not use': 'health.do_not_use',
    'Not OK': 'health.not_ok',
    Warning: 'health.warning',
    OK: 'health.ok',
    'Partly known': 'health.partly_known',
    Unknown: 'health.unknown',
  }
  return t(keys[info.name] || 'health.unknown')
}

function orbitReference(signal) {
  if (!isNumber(signal.eph_age_m)) return t('satellite.not_reported')
  const key = signal.eph_age_m < 0 ? 'satellite.minutes_ahead' : 'satellite.minutes_ago'
  return t(key, { minutes: Math.abs(signal.eph_age_m).toFixed(1) })
}

const observers = computed(() => (feeds.observers.data?.observers || [])
  .filter((observer) => observer && typeof observer.id === 'string')
  .map((observer) => {
    if (!receiverNumbers.has(observer.id)) receiverNumbers.set(observer.id, receiverNumbers.size + 1)
    const number = String(receiverNumbers.get(observer.id)).padStart(2, '0')
    const capabilities = Array.isArray(observer.capabilities)
      ? observer.capabilities.filter((capability) => capability && isNumber(capability.gnss) && isNumber(capability.sig))
      : []
    const times = [observer.last_seen, ...capabilities.map((capability) => capability.last_seen)]
      .filter((time) => isNumber(time) && time > 0)
    const firstSeen = capabilities.map((capability) => capability.first_seen)
      .filter((time) => isNumber(time) && time > 0)
    const frames = capabilities.length && capabilities.every((capability) => isNumber(capability.count))
      ? capabilities.reduce((sum, capability) => sum + capability.count, 0)
      : null
    return {
      ...observer,
      number,
      capabilities,
      lastSeen: times.length ? Math.max(...times) : null,
      firstSeen: firstSeen.length ? Math.min(...firstSeen) : null,
      frames,
      systems: [...new Set(capabilities.map((capability) => capability.gnss))].map(systemFor),
    }
  }))

const observerTotal = computed(() => (
  feeds.observers.data ? t('receiver.listed', { count: observers.value.length }) : t('receiver.unavailable')
))

function observerStatus(observer) {
  const age = observer.lastSeen && feeds.observers.data
    ? Math.max(0, feeds.observers.time / 1000 - observer.lastSeen) + elapsed(feeds.observers)
    : null
  if (observer.disabled === true) return { tone: '', text: t('receiver.disabled') }
  if (age === null) return { tone: '', text: t('receiver.time_unavailable') }
  return age <= 300
    ? { tone: 'ok', text: t('receiver.receiving', { duration: duration(age) }) }
    : { tone: 'warning', text: t('receiver.last_heard', { duration: duration(age) }) }
}

function capabilityAge(capability) {
  if (!isNumber(capability.last_seen) || capability.last_seen <= 0) return null
  return Math.max(0, feeds.observers.time / 1000 - capability.last_seen)
}

function feedNote(key) {
  const feed = feeds[key]
  const parts = []
  if (feed.error) parts.push(t('satellite.update_unavailable'))
  if (feed.data) parts.push(t('satellite.snapshot', { time: stamp(feed.time) }))
  parts.push(t(key === 'svs' ? 'satellite.feed_note' : 'receiver.feed_note'))
  return parts.join(' ')
}

function feedNoteTone(key) {
  const feed = feeds[key]
  return feed.error || (feed.data && elapsed(feed) > 120) ? 'warning' : ''
}

const statusView = computed(() => {
  void tick.value
  const failed = FEED_KEYS.filter((key) => feeds[key].error)
  const stale = FEED_KEYS.filter((key) => feeds[key].data && elapsed(feeds[key]) > 120)
  const seen = feeds.global.data?.last_seen
  const age = isNumber(seen) && seen > 0
    ? Math.max(0, feeds.global.time / 1000 - seen) + elapsed(feeds.global)
    : null
  let text = t('status.waiting')
  let tone = ''
  if (feeds.global.data && age !== null) {
    text = age <= 300 ? t('status.receiving') : t('status.no_recent')
    tone = age <= 300 ? 'ok' : 'warning'
  }
  if (paused.value) {
    text = t('status.paused')
    tone = ''
  } else if (failed.length) {
    text = failed.length === FEED_KEYS.length ? t('status.unavailable') : t('status.partial')
    tone = 'warning'
  } else if (stale.length) {
    text = t('status.stale')
    tone = 'warning'
  }
  const messages = []
  if (failed.length) messages.push(t('status.failed', { feeds: failed.map((key) => t(FEED_LABEL_KEYS[key])).join(', ') }))
  if (stale.length) messages.push(t('status.stale_feeds', { feeds: stale.map((key) => t(FEED_LABEL_KEYS[key])).join(', ') }))
  if (messages.length) messages.push(paused.value ? t('status.resume_retry') : t('status.automatic_retry'))
  return { text, tone, age, seen, notice: messages.join(' ') }
})

const updatedText = computed(() => {
  if (!feeds.global.data) return t('status.automatic_updates')
  return t(paused.value ? 'status.snapshot_paused' : 'status.snapshot_refresh', { time: utc(feeds.global.time) })
})
const lastObservation = computed(() => statusView.value.age === null ? '—' : duration(statusView.value.age))
const lastObservationNote = computed(() => statusView.value.age === null
  ? t('metrics.none_reported')
  : t('metrics.receipt_time', { time: utc(statusView.value.seen * 1000) }))

function toggleSatellite(key) {
  opened.has(key) ? opened.delete(key) : opened.add(key)
}

function setReceiverOpen(number, event) {
  event.target.open ? receiverOpened.add(number) : receiverOpened.delete(number)
}

async function selectFlagged() {
  constellation.value = 'all'
  query.value = ''
  healthFilter.value = 'flagged'
  await nextTick()
  document.getElementById('health-filter')?.focus({ preventScroll: true })
  document.getElementById('satellites')?.scrollIntoView({ block: 'start' })
}

function applyTheme(nextTheme) {
  theme.value = nextTheme === 'light' ? 'light' : 'dark'
  document.documentElement.dataset.theme = theme.value
  document.querySelector('meta[name="theme-color"]').content = theme.value === 'light' ? '#e8eaed' : '#0a0a1a'
}

function toggleTheme() {
  applyTheme(theme.value === 'light' ? 'dark' : 'light')
  try { localStorage.setItem('navlistener-theme', theme.value) } catch {}
}

const themeAction = computed(() => t(theme.value === 'light' ? 'theme.dark' : 'theme.light'))

async function fetchFeed(key) {
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), 10000)
  try {
    const response = await fetch(`/gnss/api/v2/${key}`, {
      signal: controller.signal,
      cache: 'no-cache',
      credentials: 'omit',
      headers: { Accept: 'application/json' },
    })
    if (!response.ok) throw new Error('Feed unavailable')
    const validated = validateEnvelope(key, await response.json())
    if (!validated) throw new Error('Invalid feed')
    const serverTime = Date.parse(response.headers.get('Date'))
    const now = Number.isFinite(serverTime) ? serverTime : Date.now()
    Object.assign(feeds[key], {
      ...validated,
      age: Math.max(0, (now - validated.time) / 1000),
      received: Date.now(),
      error: false,
    })
  } catch {
    feeds[key].error = true
  } finally {
    clearTimeout(timeout)
  }
}

function schedule() {
  clearTimeout(refreshTimer)
  if (!paused.value && !document.hidden) refreshTimer = setTimeout(refresh, 30000)
}

async function refresh() {
  if (busy.value) return
  busy.value = true
  clearTimeout(refreshTimer)
  try {
    await Promise.allSettled(FEED_KEYS.map(fetchFeed))
  } finally {
    busy.value = false
    schedule()
  }
}

function togglePause() {
  paused.value = !paused.value
  if (paused.value) clearTimeout(refreshTimer)
  else refresh()
}

function handleVisibility() {
  if (document.hidden) clearTimeout(refreshTimer)
  else if (!paused.value) refresh()
}

onMounted(() => {
  theme.value = document.documentElement.dataset.theme === 'light' ? 'light' : 'dark'
  document.addEventListener('visibilitychange', handleVisibility)
  if (props.page === 'overview') {
    ageTimer = setInterval(() => { tick.value += 1 }, 5000)
    refresh()
  }
})

onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', handleVisibility)
  clearTimeout(refreshTimer)
  clearInterval(ageTimer)
})
</script>

<template>
  <a class="skip" :href="props.page === 'map' ? '#monitoring-map' : '#overview'">{{ t(props.page === 'map' ? 'accessibility.skip_map' : 'accessibility.skip') }}</a>
  <header class="masthead">
    <div class="wrap">
      <a class="brand" :href="homeHref" :aria-label="t('nav.home')">
        <img src="/logo.svg?v=f190b788f8e9" width="43" height="43" alt="">
        <span><strong>NavListen</strong><small>{{ t('brand.station_network') }}</small></span>
      </a>
      <nav :aria-label="t('nav.menu')">
        <a :href="sectionHref('satellites')">{{ t('nav.satellites') }}</a>
        <a :href="sectionHref('observers')">{{ t('nav.receivers') }}</a>
        <a href="#monitoring-map">{{ t('nav.coverage_map') }}</a>
        <details class="locale-menu">
          <summary>{{ t('locale.label') }}: <span :lang="props.locale">{{ currentLocaleLabel }}</span></summary>
          <div class="locale-list">
            <a v-for="item in localeLinks" :key="item.code" :href="item.href" :lang="item.code" :hreflang="item.code" :aria-current="item.code === props.locale ? 'page' : undefined">{{ item.label }}</a>
          </div>
        </details>
        <button class="theme-toggle" type="button" :aria-label="themeAction" :title="themeAction" @click="toggleTheme">
          <svg class="icon-sun" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M2 12h2m16 0h2M4.93 4.93l1.42 1.42m11.3 11.3 1.42 1.42M4.93 19.07l1.42-1.42m11.3-11.3 1.42-1.42"/></svg>
          <svg class="icon-moon" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20.9 13.1A9 9 0 0 1 10.9 3.1a9 9 0 1 0 10 10Z"/></svg>
        </button>
      </nav>
    </div>
  </header>

  <main v-if="props.page === 'map'" id="monitoring-map-page" class="wrap" tabindex="-1">
    <CoverageMap :locale="props.locale" full />
  </main>

  <main v-else id="overview" class="wrap" tabindex="-1">
    <CoverageMap :locale="props.locale" />

    <section class="hero" aria-labelledby="page-title">
      <div>
        <p class="eyebrow">{{ t('hero.eyebrow') }}</p>
        <h2 id="page-title" class="hero-title">{{ t('hero.title') }}</h2>
        <p class="intro">{{ t('hero.intro') }}</p>
        <div class="hero-links"><a href="#satellites">{{ t('hero.explore') }} <span aria-hidden="true">↓</span></a><a href="#about">{{ t('hero.about') }}</a></div>
      </div>
      <div class="mix" aria-labelledby="mix-heading">
        <h2 id="mix-heading" class="section-label">{{ t('hero.constellation_activity') }}</h2>
        <div class="mix-body">
          <div class="mix-list">
            <div v-if="activeSystems.length" class="mix-row mix-key"><span>{{ t('hero.system') }}</span><span>{{ t('hero.satellites') }}</span><span>SVs</span><span>{{ t('hero.signals') }}</span></div>
            <div v-for="system in activeSystems" :key="system.id" class="mix-row">
              <span class="mix-name"><span class="dot" :class="toneClass(system)" aria-hidden="true"></span>{{ system.name }}</span>
              <progress class="bar-track" :class="toneClass(system)" :max="mixMaximum" :value="system.satellites" :aria-label="`${system.name}: ${system.satellites}`"></progress>
              <span class="mono">{{ count(system.satellites) }}</span><span class="mono muted">{{ count(system.signals) }}</span>
            </div>
            <p v-if="feeds.global.data && !activeSystems.length" class="muted">{{ t('hero.no_reports') }}</p>
            <p v-if="!feeds.global.data" class="muted">{{ t('hero.waiting_snapshot') }}</p>
          </div>
        </div>
        <p class="mix-note">{{ mixNote }}</p>
      </div>
    </section>

    <div class="status-bar">
      <div class="status-left">
        <span class="status" :data-tone="statusView.tone" role="status"><span class="dot" aria-hidden="true"></span><span>{{ statusView.text }}</span></span>
        <span class="update-time">{{ updatedText }}</span>
      </div>
      <div class="controls">
        <button class="button" type="button" :aria-pressed="paused" @click="togglePause">{{ paused ? t('status.resume') : t('status.pause') }}</button>
        <button class="button button-primary" type="button" :disabled="busy" @click="refresh">{{ t('status.refresh') }} <span aria-hidden="true">↻</span></button>
      </div>
    </div>
    <p v-if="statusView.notice" class="notice" role="status">{{ statusView.notice }}</p>
    <noscript><p>{{ t('status.javascript') }}</p></noscript>

    <dl class="metrics" :aria-label="t('metrics.label')">
      <div class="metric"><dt>{{ t('metrics.navigation_satellites') }}</dt><dd><strong>{{ count(feeds.global.data?.total_live_svs) }}</strong><small>{{ t('metrics.current_feed') }}</small></dd></div>
      <div class="metric"><dt>{{ t('metrics.satellite_signals') }}</dt><dd><strong>{{ count(feeds.global.data?.total_live_signals) }}</strong><small>{{ t('metrics.several_signals') }}</small></dd></div>
      <div class="metric"><dt>{{ t('metrics.reporting_receivers') }}</dt><dd><strong>{{ count(feeds.global.data?.total_live_receivers) }}</strong><small>{{ t('metrics.five_minutes') }}</small></dd></div>
      <div class="metric"><dt>{{ t('metrics.last_observation') }}</dt><dd><strong>{{ lastObservation }}</strong><small>{{ lastObservationNote }}</small></dd></div>
    </dl>

    <section id="satellites" class="content-section" aria-labelledby="satellite-heading">
      <div class="section-heading">
        <div><h2 id="satellite-heading"><span class="section-index" aria-hidden="true">01</span>{{ t('satellite.heading') }}</h2><p>{{ t('satellite.description') }}</p></div>
        <button v-if="flaggedCount" class="flag-link" type="button" @click="selectFlagged">{{ t(flaggedCount === 1 ? 'satellite.flagged_count' : 'satellite.flagged_count_plural', { count: flaggedCount }) }}</button>
      </div>
      <div class="panel">
        <div class="toolbar">
          <div class="filters" role="group" :aria-label="t('satellite.filter_label')">
            <button class="filter" type="button" :aria-pressed="constellation === 'all'" @click="constellation = 'all'">{{ t('satellite.all') }}</button>
            <button v-for="system in presentSystems" :key="system.id" class="filter" type="button" :aria-pressed="constellation === String(system.id)" @click="constellation = String(system.id)"><span class="dot" :class="toneClass(system)" aria-hidden="true"></span>{{ system.name }}</button>
          </div>
          <div class="search">
            <label for="search" class="sr-only">{{ t('satellite.search') }}</label>
            <input id="search" v-model="query" type="search" :placeholder="t('satellite.search_placeholder')" autocomplete="off" maxlength="100">
            <label for="health-filter" class="sr-only">{{ t('satellite.health_filter') }}</label>
            <select id="health-filter" v-model="healthFilter"><option value="all">{{ t('satellite.all_health') }}</option><option value="flagged">{{ t('satellite.broadcast_flags') }}</option><option value="ok">{{ t('satellite.reported_ok') }}</option><option value="unknown">{{ t('satellite.unknown_health') }}</option></select>
          </div>
        </div>
        <div class="table-wrap">
          <table>
            <caption class="sr-only">{{ t('satellite.caption') }}</caption>
            <thead><tr><th scope="col">{{ t('satellite.satellite') }}</th><th scope="col" class="col-signals">{{ t('satellite.signals') }}</th><th scope="col">{{ t('satellite.broadcast_health') }}</th><th scope="col">{{ t('satellite.last_received') }}</th><th scope="col"><span class="sr-only">{{ t('satellite.details') }}</span></th></tr></thead>
            <tbody>
              <template v-for="satellite in visibleSatellites" :key="satellite.key">
                <tr class="sat-row">
                  <td><span class="sat-name"><span class="sat-symbol" :class="toneClass(systemFor(satellite.id))" aria-hidden="true">✦</span><span><span class="sat-id">{{ satellite.name }}</span><span class="sat-system">{{ systemFor(satellite.id).name }}</span></span></span></td>
                  <td class="col-signals mono muted">{{ satellite.signals.length }}</td>
                  <td><span class="badge" :data-tone="satellite.health.tone"><span class="dot" aria-hidden="true"></span><span>{{ healthText(satellite.health) }}</span></span></td>
                  <td><span class="age" :class="ageClass(satellite.lastSeen, 'svs')" :title="ageTitle(satellite.lastSeen, 'svs')">{{ ageText(satellite.lastSeen, 'svs') }}</span></td>
                  <td class="expand-cell"><button class="expand" type="button" :aria-label="t('satellite.detail_label', { name: satellite.name })" :aria-expanded="opened.has(satellite.key)" :aria-controls="`detail-${satellite.key}`" @click="toggleSatellite(satellite.key)">{{ opened.has(satellite.key) ? '−' : '+' }}</button></td>
                </tr>
                <tr :id="`detail-${satellite.key}`" :hidden="!opened.has(satellite.key)">
                  <td class="detail-cell" colspan="5">
                    <p class="detail-intro">{{ t('satellite.detail_intro') }}</p>
                    <div class="signal-list">
                      <article v-for="signal in satellite.signals" :key="signal.sigid" class="signal-card">
                        <h3><span>{{ displaySignalName(signal) }}</span><span class="badge" :data-tone="health([signal]).tone"><span class="dot" aria-hidden="true"></span><span>{{ healthText(health([signal])) }}</span></span></h3>
                        <dl class="facts">
                          <dt>{{ t('satellite.signal_id') }}</dt><dd>{{ count(signal.sigid) }}</dd>
                          <dt>{{ t('satellite.last_received') }}</dt><dd><span class="age" :class="ageClass(signal.last_seen_s, 'svs')" :title="ageTitle(signal.last_seen_s, 'svs')">{{ ageText(signal.last_seen_s, 'svs') }}</span></dd>
                          <dt>{{ t('satellite.broadcast_accuracy') }}</dt><dd>{{ signalAccuracy(signal) }}</dd>
                          <dt>{{ t('satellite.orbit_reference') }}</dt><dd>{{ orbitReference(signal) }}</dd>
                          <template v-if="isNumber(signal.iod)"><dt>{{ t('satellite.issue_of_data') }}</dt><dd>{{ count(signal.iod) }}</dd></template>
                          <template v-if="isNumber(signal.freq_ch)"><dt>{{ t('satellite.frequency_channel') }}</dt><dd>{{ signal.freq_ch }}</dd></template>
                          <template v-if="isNumber(signal.health_subcode) && signal.health_subcode !== 0"><dt>{{ t('satellite.health_bits') }}</dt><dd>0x{{ signal.health_subcode.toString(16).toUpperCase() }}</dd></template>
                        </dl>
                      </article>
                    </div>
                  </td>
                </tr>
              </template>
              <tr v-if="!visibleSatellites.length"><td class="empty" colspan="5">{{ satelliteEmpty }}</td></tr>
            </tbody>
          </table>
        </div>
        <div class="pagination"><p role="status">{{ resultCount }}</p><div class="controls"><button class="button" type="button" :disabled="page === 0" @click="page--">{{ t('satellite.previous') }}</button><button class="button" type="button" :disabled="pageStart + PAGE_SIZE >= filteredSatellites.length" @click="page++">{{ t('satellite.next') }}</button></div></div>
      </div>
      <p class="feed-note" :data-tone="feedNoteTone('svs')">{{ feedNote('svs') }}</p>
    </section>

    <section id="observers" class="content-section" aria-labelledby="receiver-heading">
      <div class="section-heading"><div><h2 id="receiver-heading"><span class="section-index" aria-hidden="true">02</span>{{ t('receiver.heading') }}</h2><p>{{ t('receiver.description') }}</p></div><span class="section-label">{{ observerTotal }}</span></div>
      <div class="panel">
        <article v-for="observer in observers" :key="observer.id" class="receiver">
          <div class="receiver-identity"><h3>{{ t('receiver.name', { number: observer.number }) }}</h3><span class="status" :data-tone="observerStatus(observer).tone"><span class="dot" aria-hidden="true"></span><span>{{ observerStatus(observer).text }}</span></span></div>
          <div><span class="receiver-stat">{{ t('receiver.frames') }}</span><strong class="receiver-value">{{ count(observer.frames) }}</strong></div>
          <div><span class="receiver-stat">{{ t('receiver.signals') }}</span><strong class="receiver-value">{{ observer.capabilities.length ? count(observer.capabilities.length) : '—' }}</strong></div>
          <div class="receiver-systems"><span class="receiver-stat">{{ t('receiver.constellations') }}</span><br><span v-for="system in observer.systems" :key="system.id" class="receiver-system" :class="toneClass(system)">{{ system.name }}</span><p v-if="!observer.systems.length">{{ t('satellite.not_reported') }}</p></div>
          <details v-if="observer.capabilities.length" :open="receiverOpened.has(observer.number)" @toggle="setReceiverOpen(observer.number, $event)">
            <summary>{{ t('receiver.view_details') }}</summary>
            <p v-if="observer.firstSeen" class="feed-note">{{ t('receiver.first_observed', { time: stamp(observer.firstSeen * 1000) }) }}</p>
            <div class="table-wrap"><table class="capabilities"><caption class="sr-only">{{ t('receiver.details_caption', { number: observer.number }) }}</caption><thead><tr><th scope="col">{{ t('receiver.constellation') }}</th><th scope="col">{{ t('receiver.signal_id') }}</th><th scope="col">{{ t('receiver.frames_short') }}</th><th scope="col">{{ t('receiver.last_received') }}</th></tr></thead><tbody><tr v-for="capability in observer.capabilities" :key="`${capability.gnss}-${capability.sig}`"><td>{{ systemFor(capability.gnss).name }}</td><td class="mono">{{ count(capability.sig) }}</td><td class="mono">{{ count(capability.count) }}</td><td><span class="age" :class="ageClass(capabilityAge(capability), 'observers')" :title="ageTitle(capabilityAge(capability), 'observers')">{{ ageText(capabilityAge(capability), 'observers') }}</span></td></tr></tbody></table></div>
          </details>
        </article>
        <p v-if="!observers.length" class="empty">{{ feeds.observers.data ? t('receiver.none') : feeds.observers.error ? t('receiver.temporarily_unavailable') : t('receiver.loading') }}</p>
      </div>
      <p class="feed-note" :data-tone="feedNoteTone('observers')">{{ feedNote('observers') }}</p>
    </section>

    <section id="about" class="explainer" :aria-label="t('about.label')">
      <div><h3>{{ t('about.context_heading') }}</h3><p>{{ t('about.context_copy') }}</p></div>
      <div><h3>{{ t('about.health_heading') }}</h3><p>{{ t('about.health_copy') }} <a href="https://intsat.space/">{{ t('about.explore') }} <span aria-hidden="true">↗</span></a></p></div>
    </section>
  </main>

  <footer>
    <div class="wrap footer-grid">
      <div class="footer-intro"><a class="brand footer-brand" :href="homeHref"><img src="/logo.svg?v=f190b788f8e9" width="43" height="43" alt=""><span><strong>NavListen</strong><small>{{ t('brand.station_network') }}</small></span></a><p>{{ t('footer.tagline') }}</p></div>
      <div class="footer-meta"><nav aria-label="Legal"><a href="https://intsat.space/intsat/terms/">{{ t('footer.terms') }}</a><a href="https://intsat.space/intsat/privacy/">{{ t('footer.privacy') }}</a></nav><span>{{ t('footer.copyright') }}</span></div>
    </div>
  </footer>
</template>
