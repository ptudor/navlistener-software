import assert from 'node:assert/strict'
import { readFile, readdir } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { SUPPORTED_LOCALES } from '../src/i18n/localeLabels.js'
import { DEFAULT_LOCALE } from '../src/siteRoutes.js'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const dist = path.join(root, 'dist')
const entries = await readdir(dist, { recursive: true })
const htmlFiles = entries.filter((entry) => entry.endsWith('index.html')).sort()
const catalogs = entries.filter((entry) => /^catalogs[/\\].+\.json$/.test(entry)).sort()

assert.equal(htmlFiles.length, SUPPORTED_LOCALES.length, 'one static page per locale')
assert.equal(catalogs.length, SUPPORTED_LOCALES.length, 'one public catalog per locale')

for (const locale of SUPPORTED_LOCALES) {
  const relative = locale === DEFAULT_LOCALE ? 'index.html' : `${locale}/index.html`
  assert.ok(htmlFiles.includes(relative), `${locale}: exported page`)
  const html = await readFile(path.join(dist, relative), 'utf8')
  assert.match(html, new RegExp(`<html lang="${locale}"`), `${locale}: document language`)
  assert.match(html, /<main id="overview"/, `${locale}: semantic main`)
  assert.match(html, /NavListen/, `${locale}: brand`)
  assert.match(html, /STATION NETWORK/, `${locale}: stacked lockup`)
  assert.match(html, /Open tools for studying satellite navigation\./, `${locale}: footer tagline`)
  assert.match(html, /© 2026 Integrity Satellite/, `${locale}: copyright`)
  assert.match(html, /<link rel="canonical" href="https:\/\/in\.intsat\.net\/in\//, `${locale}: canonical`)
  assert.match(html, /<script type="module" crossorigin src="\/in\/assets\//, `${locale}: local Vue bundle`)
  assert.doesNotMatch(html, /\sstyle=/i, `${locale}: no inline style under CSP`)
}

for (const relative of catalogs) JSON.parse(await readFile(path.join(dist, relative), 'utf8'))
const sitemap = await readFile(path.join(dist, 'sitemap.xml'), 'utf8')
assert.equal(sitemap.match(/<loc>/g)?.length, SUPPORTED_LOCALES.length, 'sitemap URL count')
console.log(`static export OK — ${htmlFiles.length} HTML page(s), ${catalogs.length} catalog(s)`)
