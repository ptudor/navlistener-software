import { randomBytes } from 'node:crypto'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig(({ command }) => {
  // Vite injects styles during development. Authorize those tags with a nonce;
  // production keeps its external stylesheets and the original strict CSP.
  const devNonce = command === 'serve' ? randomBytes(16).toString('base64') : undefined

  return {
    plugins: [
      vue(),
      {
        name: 'navlisten-development-csp',
        apply: 'serve',
        transformIndexHtml(html, context) {
          if (!context.server) return html
          return html.replace("style-src 'self'", `style-src 'self' 'nonce-${devNonce}'`)
        },
      },
    ],
    html: { cspNonce: devNonce },
    base: '/',
    server: {
      port: 5177,
    },
  }
})
