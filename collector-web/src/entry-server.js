import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from './App.vue'
import { createSiteI18n } from './i18n/index.js'

export async function render(locale = 'en') {
  const app = createSSRApp(App, { locale })
  app.use(createSiteI18n(locale))
  return renderToString(app)
}
