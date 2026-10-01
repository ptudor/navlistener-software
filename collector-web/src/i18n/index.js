import { createI18n } from 'vue-i18n'
import { DEFAULT_LOCALE } from '../siteRoutes.js'
import { SUPPORTED_LOCALES } from './localeLabels.js'

const modules = import.meta.glob('./locales/*.json', { eager: true, import: 'default' })
export const catalogs = Object.fromEntries(
  SUPPORTED_LOCALES.map((locale) => [locale, modules[`./locales/${locale}.json`]]),
)

export function createSiteI18n(locale = DEFAULT_LOCALE) {
  return createI18n({
    legacy: false,
    locale: SUPPORTED_LOCALES.includes(locale) ? locale : DEFAULT_LOCALE,
    fallbackLocale: DEFAULT_LOCALE,
    messages: catalogs,
  })
}

export function localeFromPath(pathname = '/') {
  const candidate = pathname.slice('/in/'.length).split('/')[0]
  return candidate && SUPPORTED_LOCALES.includes(candidate) ? candidate : DEFAULT_LOCALE
}
