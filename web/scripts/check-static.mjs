import assert from 'node:assert/strict'
import { access, readFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const dist = path.join(webRoot, 'dist')
const html = await readFile(path.join(dist, 'index.html'), 'utf8')

assert.match(html, /<html lang="en" class="no-js">/, 'document language and no-JS mode')
assert.match(html, /<main id="main-content"/, 'semantic main is pre-rendered')
assert.match(html, /<h1>Satellites broadcast\.<br\s*\/?><em>We listen\.<\/em><\/h1>/, 'hero copy is pre-rendered')
assert.match(html, /Come help us listen\./, 'join section is pre-rendered')
assert.match(html, /mailto:ptudor@ptudor\.net/, 'contact path is present')
assert.match(html, /rel="canonical" href="https:\/\/navlisten\.com\/"/, 'canonical URL')
assert.match(html, /<script type="module" crossorigin src="\/assets\/.+\.js"><\/script>/, 'local client bundle')
assert.match(html, /<link rel="stylesheet" crossorigin href="\/assets\/.+\.css">/, 'local stylesheet')
assert.doesNotMatch(html, /<!--app-html-->/, 'static marker was replaced')
assert.doesNotMatch(html, /<div id="app"><\/div>/, 'app is not an empty JavaScript shell')
assert.doesNotMatch(html, /\sstyle=/i, 'inline style would violate the CSP')
assert.match(html, /style-src 'self';/, 'production keeps the strict stylesheet policy')
assert.doesNotMatch(html, /nonce-|unsafe-inline/, 'development style authorization stays out of production')
assert.doesNotMatch(html, /(?:src|href)="https?:\/\/(?!navlisten\.com)/, 'no third-party runtime assets')

await Promise.all([
  'assets/mark.svg',
  'assets/observer-board.svg',
  'assets/og-image.png',
  'assets/favicon.png',
  'assets/apple-touch-icon.png',
  'assets/app-icon-512.png',
  'fonts/PublicSans-VariableFont_wght.woff2',
  'fonts/SpaceGrotesk-VariableFont_wght.woff2',
  'fonts/IBMPlexMono-Regular.woff2',
  'fonts/IBMPlexMono-Medium.woff2',
  'fonts/Charis-Italic.woff2',
  'robots.txt',
  'sitemap.xml',
  'site.webmanifest',
].map((file) => access(path.join(dist, file))))

console.log('static export OK — semantic HTML, metadata, CSP, and brand assets present')
