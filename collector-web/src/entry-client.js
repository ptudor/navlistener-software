import { createApp, createSSRApp } from 'vue'
import App from './App.vue'
import { createSiteI18n, localeFromPath } from './i18n/index.js'
import { pageFromPath } from './siteRoutes.js'
import './styles.css'

document.documentElement.classList.replace('no-js', 'js')
const properties = {
  locale: localeFromPath(window.location.pathname),
  page: pageFromPath(window.location.pathname),
}
const app = import.meta.env.DEV ? createApp(App, properties) : createSSRApp(App, properties)
app.use(createSiteI18n(properties.locale))
app.mount('#app')
