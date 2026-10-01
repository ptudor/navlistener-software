export const SITE_ORIGIN = 'https://in.intsat.net'
export const BASE_PATH = '/in/'
export const DEFAULT_LOCALE = 'en'

export function pagePath(locale) {
  return locale === DEFAULT_LOCALE ? BASE_PATH : `${BASE_PATH}${locale}/`
}

export function pageUrl(locale) {
  return new URL(pagePath(locale), SITE_ORIGIN).href
}

export function catalogPath(locale) {
  return `${BASE_PATH}catalogs/${locale}.json`
}

export function catalogUrl(locale) {
  return new URL(catalogPath(locale), SITE_ORIGIN).href
}
