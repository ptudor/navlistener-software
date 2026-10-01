# Collector dashboard

This Vue 3 application is the public status view at `https://in.intsat.net/in/`.
It is a static export: Apache serves the generated HTML, CSS, JavaScript, fonts,
and language catalogs directly, while the browser reads live public data from
the collector's existing `/gnss/api/v2/` endpoints.

The export writes real directories for the source English page and four English
regional placeholders. Apache retains ordinary 404 behavior because there is no
single-page fallback. Replace a placeholder catalog in `src/i18n/locales/` when
translated copy is ready; every build checks its keys against `en.json`.

```sh
npm ci
gmake build
gmake deploy-dry SSH_CONF=/path/to/config SSH_KEY=/path/to/key
gmake deploy SSH_CONF=/path/to/config SSH_KEY=/path/to/key
```

Deployment requires a clean `main` whose exact commit is present on `origin`.
The build is published under `/usr/local/www/navlistener/in/`; `/gnss/` remains
the separately proxied collector API.
