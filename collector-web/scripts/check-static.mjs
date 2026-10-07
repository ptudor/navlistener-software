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

assert.equal(htmlFiles.length, SUPPORTED_LOCALES.length * 2, 'overview and map page per locale')
assert.equal(catalogs.length, SUPPORTED_LOCALES.length, 'one public catalog per locale')

const OVERVIEW_DESCRIPTION = "A live view of NavListen's public satellite observations, broadcast health, and receiver activity."
const MAP_DESCRIPTION = 'Live worldwide GNSS monitoring coverage from the NavListen station network.'

for (const locale of SUPPORTED_LOCALES) {
  const relative = locale === DEFAULT_LOCALE ? 'index.html' : `${locale}/index.html`
  assert.ok(htmlFiles.includes(relative), `${locale}: exported page`)
  const html = await readFile(path.join(dist, relative), 'utf8')
  const catalog = JSON.parse(await readFile(path.join(root, 'src/i18n/locales', `${locale}.json`), 'utf8'))
  assert.match(html, new RegExp(`<html lang="${locale}"`), `${locale}: document language`)
  assert.match(html, /<main id="overview"/, `${locale}: semantic main`)
  assert.match(html, /NavListen/, `${locale}: brand`)
  assert.match(html, /STATION NETWORK/, `${locale}: stacked lockup`)
  assert.match(html, /Ground Monitoring for Space Integrity\./, `${locale}: footer tagline`)
  // The year lives in the catalogs; the page must carry exactly what they say, and it must still be a year.
  assert.match(html, /© \d{4} Integrity Satellite/, `${locale}: copyright`)
  assert.ok(html.includes(catalog.footer.copyright), `${locale}: footer copyright from the catalog`)
  assert.ok(html.includes(OVERVIEW_DESCRIPTION), `${locale}: overview description`)
  assert.match(html, /<link rel="canonical" href="https:\/\/in\.intsat\.net\/in\//, `${locale}: canonical`)
  assert.match(html, /<script type="module" crossorigin src="\/in\/assets\//, `${locale}: local Vue bundle`)
  assert.doesNotMatch(html, /\sstyle=/i, `${locale}: no inline style under CSP`)

  const mapRelative = locale === DEFAULT_LOCALE ? 'map/index.html' : `${locale}/map/index.html`
  assert.ok(htmlFiles.includes(mapRelative), `${locale}: exported map page`)
  const mapHtml = await readFile(path.join(dist, mapRelative), 'utf8')
  assert.match(mapHtml, new RegExp(`<html lang="${locale}"`), `${locale}: map document language`)
  assert.match(mapHtml, /<main[^>]+id="monitoring-map-page"/, `${locale}: map semantic main`)
  assert.match(mapHtml, /GNSS monitoring map · NavListen/, `${locale}: map title`)
  assert.ok(mapHtml.includes(MAP_DESCRIPTION), `${locale}: map description`)
  assert.ok(!mapHtml.includes(OVERVIEW_DESCRIPTION), `${locale}: map page keeps no overview description`)
  assert.match(mapHtml, /<canvas[^>]+width="1800"[^>]+height="900"/, `${locale}: rendered map canvas`)
  assert.match(mapHtml, new RegExp(`<link rel="canonical" href="https:\\/\\/in\\.intsat\\.net${locale === DEFAULT_LOCALE ? '\\/in\\/map\\/' : `\\/in\\/${locale}\\/map\\/`}`), `${locale}: map canonical`)
  assert.doesNotMatch(mapHtml, /\sstyle=/i, `${locale}: map has no inline style under CSP`)
}

for (const relative of catalogs) JSON.parse(await readFile(path.join(dist, relative), 'utf8'))
const sitemap = await readFile(path.join(dist, 'sitemap.xml'), 'utf8')
assert.equal(sitemap.match(/<loc>/g)?.length, SUPPORTED_LOCALES.length * 2, 'sitemap URL count')
console.log(`static export OK — ${htmlFiles.length} HTML page(s), ${catalogs.length} catalog(s)`)
