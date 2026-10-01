import { mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { SUPPORTED_LOCALES } from '../src/i18n/localeLabels.js'
import { DEFAULT_LOCALE, catalogUrl, pagePath, pageUrl } from '../src/siteRoutes.js'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const dist = path.join(root, 'dist')
const documentPath = path.join(dist, 'index.html')
const serverBundle = path.join(root, '.static-ssr', 'entry-server.js')
const catalogs = Object.fromEntries(await Promise.all(SUPPORTED_LOCALES.map(async (locale) => [
  locale,
  JSON.parse(await readFile(path.join(root, 'src/i18n/locales', `${locale}.json`), 'utf8')),
])))

function localeHead(locale) {
  const alternates = SUPPORTED_LOCALES.map((alternate) =>
    `<link rel="alternate" hreflang="${alternate}" href="${pageUrl(alternate)}" />`,
  )
  return [
    `<link rel="canonical" href="${pageUrl(locale)}" />`,
    ...alternates,
    `<link rel="alternate" hreflang="x-default" href="${pageUrl(DEFAULT_LOCALE)}" />`,
    `<link rel="alternate" type="application/json" hreflang="${locale}" title="Language catalog" href="${catalogUrl(locale)}" />`,
  ].join('\n    ')
}

try {
  const [{ render }, shell] = await Promise.all([
    import(pathToFileURL(serverBundle)),
    readFile(documentPath, 'utf8'),
  ])
  if (!shell.includes('<!--app-html-->') || !shell.includes('<!--locale-links-->')) {
    throw new Error('Static export markers are missing from Vite output')
  }

  for (const locale of SUPPORTED_LOCALES) {
    const html = shell
      .replace('<html lang="en"', `<html lang="${locale}"`)
      .replace('<!--locale-links-->', localeHead(locale))
      .replace('<!--app-html-->', await render(locale))
    const directory = locale === DEFAULT_LOCALE ? dist : path.join(dist, locale)
    await mkdir(directory, { recursive: true })
    await writeFile(path.join(directory, 'index.html'), html, 'utf8')
  }

  const catalogDirectory = path.join(dist, 'catalogs')
  await mkdir(catalogDirectory, { recursive: true })
  for (const locale of SUPPORTED_LOCALES) {
    await writeFile(path.join(catalogDirectory, `${locale}.json`), `${JSON.stringify(catalogs[locale], null, 2)}\n`, 'utf8')
  }

  const sitemap = `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${SUPPORTED_LOCALES.map((locale) => `  <url><loc>${pageUrl(locale)}</loc></url>`).join('\n')}\n</urlset>\n`
  await writeFile(path.join(dist, 'sitemap.xml'), sitemap, 'utf8')
  console.log(`static site exported — ${SUPPORTED_LOCALES.length} page(s), ${SUPPORTED_LOCALES.length} locale catalog(s)`)
} finally {
  await rm(path.join(root, '.static-ssr'), { recursive: true, force: true })
}
