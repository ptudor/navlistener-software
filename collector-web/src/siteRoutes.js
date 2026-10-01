export const SITE_ORIGIN = 'https://in.intsat.net'
export const BASE_PATH = '/in/'
export const DEFAULT_LOCALE = 'en'

export function pagePath(locale, page = 'overview') {
  const localePath = locale === DEFAULT_LOCALE ? BASE_PATH : `${BASE_PATH}${locale}/`
  return page === 'map' ? `${localePath}map/` : localePath
}

export function mapPagePath(locale) {
  return pagePath(locale, 'map')
}

export function pageUrl(locale, page = 'overview') {
  return new URL(pagePath(locale, page), SITE_ORIGIN).href
}

export function catalogPath(locale) {
  return `${BASE_PATH}catalogs/${locale}.json`
}

export function catalogUrl(locale) {
  return new URL(catalogPath(locale), SITE_ORIGIN).href
}

export function pageFromPath(pathname = '/') {
  return pathname.endsWith('/map/') || pathname.endsWith('/map') ? 'map' : 'overview'
}
