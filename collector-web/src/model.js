export const SYSTEMS = [
  { id: 0, key: 'gps', name: 'GPS', primary: 'L1 C/A' },
  { id: 2, key: 'galileo', name: 'Galileo', primary: 'E1' },
  { id: 3, key: 'beidou', name: 'BeiDou', primary: 'B1I' },
  { id: 6, key: 'glonass', name: 'GLONASS', primary: 'L1OF' },
  { id: 5, key: 'qzss', name: 'QZSS', primary: 'L1 C/A' },
  { id: 7, key: 'navic', name: 'NavIC', primary: 'L5' },
  { id: 1, key: 'sbas', name: 'SBAS', primary: 'L1' },
]

const byID = new Map(SYSTEMS.map((system) => [system.id, system]))

export const isNumber = (value) => typeof value === 'number' && Number.isFinite(value)
export const count = (value) => (isNumber(value) && value >= 0 ? value.toLocaleString('en-US') : '—')
export const systemFor = (id) => byID.get(id) || { id, key: 'sbas', name: 'Other', primary: 'Primary signal' }
export const systemColor = (system) => `var(--${system.key})`

export function duration(seconds) {
  if (!isNumber(seconds) || seconds < 0) return 'Unknown'
  if (seconds < 60) return `${Math.floor(seconds)}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${Math.floor(seconds % 60)}s`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`
  return `${Math.floor(seconds / 86400)}d ${Math.floor((seconds % 86400) / 3600)}h`
}

export function utc(time) {
  return `${new Date(time).toLocaleTimeString('en-GB', { timeZone: 'UTC', hour12: false })} UTC`
}

export function stamp(time) {
  return new Date(time).toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC')
}

export function health(signals) {
  if (signals.some((signal) => signal.health_code === 3)) return { name: 'Do not use', tone: 'error', kind: 'flagged' }
  if (signals.some((signal) => signal.health_code === 2 || signal.health_issue_level === 2)) return { name: 'Not OK', tone: 'error', kind: 'flagged' }
  if (signals.some((signal) => signal.health_issue_level === 1 || signal.alert === true)) return { name: 'Warning', tone: 'warning', kind: 'flagged' }
  if (signals.every((signal) => signal.health_code === 1)) return { name: 'OK', tone: 'ok', kind: 'ok' }
  return { name: signals.some((signal) => signal.health_code === 1) ? 'Partly known' : 'Unknown', tone: '', kind: 'unknown' }
}

export function signalName(signal) {
  if (signal.gnssid === 2 && signal.sigid === 3) return 'E5a'
  if (signal.gnssid === 3 && signal.sigid === 8) return 'B2a'
  return signal.sigid === 0 ? systemFor(signal.gnssid).primary : `Signal ${signal.sigid}`
}

export function groupSatellites(feed) {
  const groups = new Map()
  for (const signal of Object.values(feed?.svs || {})) {
    if (!signal || !isNumber(signal.gnssid) || !isNumber(signal.svid) || !isNumber(signal.sigid)) continue
    const key = `${signal.gnssid}-${signal.svid}`
    if (!groups.has(key)) {
      groups.set(key, {
        key,
        name: signal.name || `${systemFor(signal.gnssid).name} ${signal.svid}`,
        id: signal.gnssid,
        svid: signal.svid,
        signals: [],
      })
    }
    groups.get(key).signals.push(signal)
  }

  const satellites = [...groups.values()].sort((left, right) => left.id - right.id || left.svid - right.svid)
  for (const satellite of satellites) {
    satellite.signals.sort((left, right) => left.sigid - right.sigid)
    satellite.health = health(satellite.signals)
    const ages = satellite.signals.map((signal) => signal.last_seen_s).filter((age) => isNumber(age) && age >= 0)
    satellite.lastSeen = ages.length ? Math.min(...ages) : null
  }
  return satellites
}

export function validateEnvelope(key, envelope) {
  const data = envelope?.data
  const time = Date.parse(envelope?.time)
  if (envelope?.ok !== true || !data || typeof data !== 'object' || !Number.isFinite(time) || !String(data.schema).startsWith('2.')) return null
  if (data.audience !== 'public') return null
  if (key === 'svs' && (!data.svs || typeof data.svs !== 'object' || Array.isArray(data.svs))) return null
  if (key === 'observers' && !Array.isArray(data.observers)) return null
  if (key === 'global' && !['total_live_svs', 'total_live_signals', 'total_live_receivers'].every((field) => isNumber(data[field]) && data[field] >= 0)) return null
  return { data, time }
}
