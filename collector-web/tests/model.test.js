import assert from 'node:assert/strict'
import test from 'node:test'

import { duration, groupSatellites, health, validateEnvelope } from '../src/model.js'

test('satellite signals are grouped, sorted, and assigned the most serious health', () => {
  const satellites = groupSatellites({ svs: {
    second: { gnssid: 2, svid: 12, sigid: 3, name: 'E12', health_code: 1, last_seen_s: 7 },
    first: { gnssid: 0, svid: 3, sigid: 0, name: 'G03', health_code: 1, last_seen_s: 4 },
    warning: { gnssid: 0, svid: 3, sigid: 2, name: 'G03', health_issue_level: 1, last_seen_s: 9 },
  } })
  assert.deepEqual(satellites.map(({ key }) => key), ['0-3', '2-12'])
  assert.equal(satellites[0].health.kind, 'flagged')
  assert.equal(satellites[0].lastSeen, 4)
  assert.deepEqual(satellites[0].signals.map(({ sigid }) => sigid), [0, 2])
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
