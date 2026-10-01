import { readFile, readdir } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { LOCALE_LABELS } from '../src/i18n/localeLabels.js'

const directory = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../src/i18n/locales')

function leafKeys(value, prefix = '') {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return [prefix]
  return Object.entries(value).flatMap(([key, child]) => leafKeys(child, prefix ? `${prefix}.${key}` : key))
}

const files = (await readdir(directory)).filter((file) => file.endsWith('.json')).sort()
const locales = files.map((file) => file.replace(/\.json$/, '')).sort()
if (!files.includes('en.json')) throw new Error('Missing source locale: en.json')
if (locales.join('\n') !== Object.keys(LOCALE_LABELS).sort().join('\n')) throw new Error('Locale files and language labels differ')

const sourceKeys = new Set(leafKeys(JSON.parse(await readFile(path.join(directory, 'en.json'), 'utf8'))))
for (const file of files.filter((file) => file !== 'en.json')) {
  const keys = new Set(leafKeys(JSON.parse(await readFile(path.join(directory, file), 'utf8'))))
  const missing = [...sourceKeys].filter((key) => !keys.has(key))
  const unexpected = [...keys].filter((key) => !sourceKeys.has(key))
  if (missing.length || unexpected.length) throw new Error(`${file}: locale key drift; missing=${missing.join(',')} unexpected=${unexpected.join(',')}`)
}
console.log(`locales OK — ${files.length} English placeholder catalog(s), ${sourceKeys.size} keys`)
