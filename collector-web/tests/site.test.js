import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { SUPPORTED_LOCALES } from '../src/i18n/localeLabels.js'
import { pagePath } from '../src/siteRoutes.js'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

test('the collector publishes five real English locale paths under /in/', () => {
  assert.deepEqual(SUPPORTED_LOCALES, ['en', 'en-US', 'en-GB', 'en-CA', 'en-AU'])
  assert.deepEqual(SUPPORTED_LOCALES.map(pagePath), ['/in/', '/in/en-US/', '/in/en-GB/', '/in/en-CA/', '/in/en-AU/'])
})

test('branding and legal destinations stay in the Vue template', async () => {
  const app = await readFile(path.join(root, 'src/App.vue'), 'utf8')
  assert.match(app, />NavListen</)
  assert.match(app, /brand\.station_network/)
  assert.match(app, /https:\/\/intsat\.space\/intsat\/terms\//)
  assert.match(app, /https:\/\/intsat\.space\/intsat\/privacy\//)
  assert.doesNotMatch(app, /\s:style=/)
})
