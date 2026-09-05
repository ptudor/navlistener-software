import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { createServer, resolveConfig } from 'vite'

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

test('development styles carry a nonce allowed by the page CSP', async () => {
  const server = await createServer({
    root: webRoot,
    logLevel: 'silent',
    server: { middlewareMode: true, watch: null, ws: false },
  })

  try {
    const source = await readFile(path.join(webRoot, 'index.html'), 'utf8')
    const html = await server.transformIndexHtml('/index.html', source)
    const nonce = server.config.html.cspNonce

    assert.ok(nonce, 'development has a nonce for Vite’s injected styles')
    assert.ok(html.includes(`style-src 'self' 'nonce-${nonce}'`), 'CSP authorizes the same nonce')
    assert.ok(html.includes(`<meta property="csp-nonce" nonce="${nonce}">`), 'Vite can discover the nonce')
    assert.ok(!html.includes("'unsafe-inline'"), 'development does not need unrestricted inline styles')
  } finally {
    await server.close()
  }
})

test('production keeps the original external-stylesheet policy', async () => {
  const config = await resolveConfig({ root: webRoot, logLevel: 'silent' }, 'build')
  assert.equal(config.html.cspNonce, undefined)
  assert.ok(!config.plugins.some(plugin => plugin.name === 'navlisten-development-csp'))
})
