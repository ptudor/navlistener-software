import assert from 'node:assert/strict'
import test from 'node:test'

import { duration, groupSatellites, health, signalHealth, signalName, validateEnvelope } from '../src/model.js'

test('satellites retain every signal health category and reception age', () => {
  const satellites = groupSatellites({ svs: {
    second: { gnssid: 2, svid: 12, sigid: 3, name: 'E12', health_code: 1, last_seen_s: 7 },
    first: { gnssid: 0, svid: 3, sigid: 0, name: 'G03', health_code: 1, last_seen_s: 4 },
    warning: { gnssid: 0, svid: 3, sigid: 2, name: 'G03', health_issue_level: 1, last_seen_s: 9 },
  } })
  assert.deepEqual(satellites.map(({ key }) => key), ['0-3', '2-12'])
  assert.deepEqual(satellites[0].healthKinds, ['ok', 'flagged'])
  assert.deepEqual(satellites[0].signals.map(({ last_seen_s }) => last_seen_s), [4, 9])
  assert.deepEqual(satellites[0].signals.map(({ sigid }) => sigid), [0, 2])
})

const l5Flag = Object.freeze({
  gnssid: 0, svid: 6, sigid: 6, name: 'G06',
  health_code: 3, health_subcode: 1, health_issue_level: 2, alert: false,
  acc_index: 0, last_seen_s: 48,
})

test('GPS L5 pre-operational context preserves the broadcast health', () => {
  for (const sigid of [6, 7]) {
    const signal = Object.freeze({ ...l5Flag, sigid })
    assert.equal(signalHealth(signal).name, 'Pre-operational')
    assert.equal(signalHealth(signal).kind, 'preoperational')
    assert.equal(health([signal]).name, 'Do not use')
    assert.equal(signal.health_code, 3)
    assert.equal(signal.health_subcode, 1)
  }
})

test('the L5 service policy is scoped to GPS L5 and can be retired', () => {
  for (const signal of [
    { ...l5Flag, sigid: 0 },
    { ...l5Flag, sigid: 3 },
    { ...l5Flag, sigid: 4 },
    { ...l5Flag, gnssid: 5, sigid: 8 },
    { ...l5Flag, gnssid: 5, sigid: 9 },
    { ...l5Flag, gnssid: 7, sigid: 0 },
  ]) {
    assert.equal(signalHealth(signal).kind, 'flagged')
    assert.equal(signalHealth(signal).preOperational, false)
  }
  const operational = signalHealth(l5Flag, { phase: 'operational' })
  assert.equal(operational.kind, 'flagged')
  assert.equal(operational.preOperational, false)
})

test('additional L5 alerts and missing evidence remain flagged', () => {
  for (const change of [
    { alert: true },
    { alert: undefined },
    { wn_mismatch: true },
    { acc_index: 15 },
    { acc_index: -16 },
    { health_subcode: undefined },
    { health_subcode: 2 },
    { health_issue_level: undefined },
    { health_code: 2 },
  ]) {
    assert.equal(signalHealth({ ...l5Flag, ...change }).kind, 'flagged', JSON.stringify(change))
  }
  assert.equal(signalHealth({ gnssid: 0, sigid: 6, health_code: 0 }).kind, 'unknown')
  assert.equal(signalHealth({ ...l5Flag, health_code: 1, health_subcode: 0, health_issue_level: 0 }).kind, 'ok')
})

test('mixed signal filters distinguish L5 context from other flags without hiding either', () => {
  const l1 = { ...l5Flag, sigid: 0, health_code: 1, health_subcode: 0, health_issue_level: 0, last_seen_s: 3600 }
  const feed = { svs: { l1, l5: l5Flag } }
  const before = JSON.stringify(feed)
  const [satellite] = groupSatellites(feed)
  assert.deepEqual(satellite.healthKinds, ['ok', 'preoperational'])
  assert.deepEqual(satellite.signals.map(({ last_seen_s }) => last_seen_s), [3600, 48])
  assert.equal(JSON.stringify(feed), before)

  const [flagged] = groupSatellites({ svs: { l1: { ...l1, health_code: 3 }, l5: l5Flag } })
  assert.deepEqual(flagged.healthKinds, ['flagged', 'preoperational'])
})

test('GPS and QZSS CNAV signals use carrier names with distinct components', () => {
  for (const [gnssid, sigid, expected] of [
    [0, 0, 'L1 C/A'], [0, 3, 'L2C M'], [0, 4, 'L2C L'], [0, 6, 'L5 I'], [0, 7, 'L5 Q'],
    [5, 4, 'L2C M'], [5, 5, 'L2C L'], [5, 8, 'L5 I'], [5, 9, 'L5 Q'],
    [2, 3, 'E5a'], [3, 8, 'B2a'], [0, 99, 'Signal 99'],
  ]) assert.equal(signalName({ gnssid, sigid }), expected)
})

test('public v2 envelope validation keeps malformed and private feeds out', () => {
  const envelope = { ok: true, time: '2026-09-30T12:00:00Z', data: { schema: '2.1', audience: 'public', total_live_svs: 1, total_live_signals: 2, total_live_receivers: 1 } }
  assert.ok(validateEnvelope('global', envelope))
  assert.equal(validateEnvelope('global', { ...envelope, data: { ...envelope.data, audience: 'private' } }), null)
  assert.equal(validateEnvelope('observers', envelope), null)
})

test('age and broadcast health formatting preserve dashboard semantics', () => {
  assert.equal(duration(3665), '1h 1m')
  assert.equal(health([{ health_code: 3 }]).name, 'Do not use')
  assert.equal(health([{ health_code: 1 }]).kind, 'ok')
})
