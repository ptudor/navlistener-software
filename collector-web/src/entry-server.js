import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from './App.vue'
import { createSiteI18n } from './i18n/index.js'

export async function render(locale = 'en', page = 'overview') {
  const app = createSSRApp(App, { locale, page })
  app.use(createSiteI18n(locale))
  return renderToString(app)
}
