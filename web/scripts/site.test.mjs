import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

test('the recruiting page has a complete, honest project story', async () => {
  const app = await readFile(path.join(webRoot, 'src', 'App.vue'), 'utf8')

  for (const phrase of [
    'Navigation is a broadcast.',
    'From antenna to evidence.',
    'Meet the observer.',
    'Ask what the sky said',
    'Come help us listen.',
    'Apache License 2.0',
  ]) {
    assert.ok(app.includes(phrase), `missing core copy: ${phrase}`)
  }

  assert.ok(app.includes('fabrication-ready'), 'hardware is described at its real maturity')
  assert.ok(!app.includes('buy now'), 'the project site must not pretend the board is a retail product')
  assert.ok(!app.includes('military-grade'), 'the site avoids empty security language')
})

test('every constellation has a distinct Integrity Station identity', async () => {
  const app = await readFile(path.join(webRoot, 'src', 'App.vue'), 'utf8')
  const names = ['GPS', 'SBAS', 'Galileo', 'BeiDou', 'QZSS', 'GLONASS', 'NavIC']
  for (const name of names) assert.ok(app.includes(`name: '${name}'`), `missing ${name}`)
})

test('the system story represents both supported receiver families', async () => {
  const app = await readFile(path.join(webRoot, 'src', 'App.vue'), 'utf8')
  assert.match(app, /u-blox \+ Septentrio/)
  assert.match(app, /purpose-built ESP32 observer/)
  assert.match(app, /hardware-backed ECC identity/)
  assert.doesNotMatch(app, /\bNEO\b/)
  assert.doesNotMatch(app, /ESP32-S3/)
  assert.doesNotMatch(app, /existing station/i)
})

test('public copy avoids adversarial contrast and deficit framing', async () => {
  const app = await readFile(path.join(webRoot, 'src', 'App.vue'), 'utf8')
  const discouraged = [
    [/—not\b/i, 'em-dash false contrast'],
    [/,\s+not\s+\w+/i, 'comma false contrast'],
    [/no mystery boxes/i, 'combative shortcut'],
    [/rarely checks/i, 'reader accusation'],
    [/missing perspective/i, 'deficit framing'],
  ]

  for (const [pattern, label] of discouraged) {
    assert.doesNotMatch(app, pattern, label)
  }
})

test('the site uses family typography and IntSat health semantics', async () => {
  const styles = await readFile(path.join(webRoot, 'src', 'styles.css'), 'utf8')

  for (const family of ['Public Sans', 'Space Grotesk', 'IBM Plex Mono', 'Charis']) {
    assert.match(styles, new RegExp(`font-family: "${family}"`), `missing ${family}`)
  }

  assert.match(styles, /--signal: #67f5c7;/, 'NavListen turquoise identity')
  assert.match(styles, /--status-ok: #58a6ff;/, 'IntSat healthy\/info blue')
  assert.match(styles, /\.station-pulse[\s\S]*?background: var\(--status-ok\);/)
  assert.match(styles, /\.console-summary strong i[^\n]*background: var\(--status-ok\);/)
  assert.match(styles, /\.spark-line[^\n]*stroke: var\(--signal\);/, 'signal data remains turquoise')
})
