# Collector dashboard

This Vue 3 application is the public status view at `https://in.intsat.net/in/`.
It is a static export: Apache serves the generated HTML, CSS, JavaScript, fonts,
and language catalogs directly, while the browser reads live public data from
the collector's existing `/gnss/api/v2/` endpoints.

The overview opens with the live coverage map. A complete analysis view is
exported at `/in/map/`, with a localized map page beside every locale overview.
The collector supplies coverage data through its API and does not serve page
content.

The satellite table shows health and reception age for each signal. Health
filters match any signal on a satellite, so a satellite can appear in more than
one category. GPS L5's expected pre-operational health flag has its own category;
the original broadcast health remains in the details and the API is unchanged.
Additional accuracy alerts, broadcast-week mismatches, and no-accuracy-prediction
indices keep that signal in the other-flags category.

`GPS_L5_SERVICE_STATUS` in `src/model.js` records the presentation policy, source,
and review date. Review the [GPS.gov CNAV service status](https://www.gps.gov/civil-navigation-cnav-message)
and update the policy when L5 becomes operational. This annotation does not infer
navigation accuracy or the cause of an individual unhealthy broadcast.

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
