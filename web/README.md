# NavListen website

The Vue 3 static project site for `https://navlisten.com/`, welcoming volunteers,
station hosts, researchers, and prospective customers.

## Editorial direction

Keep the voice welcoming, practical, and collaborative. Explain what people can do with
NavListen and how to take part. Write for both volunteers and customers; give each a clear
way to start a conversation. The observer exists: describe the hardware in present tense.
Use short, active sentences and concrete headings. Name what people can observe, study,
or build.

Use plain language before technical detail. Illustrations should explain the system and
be labeled as illustrations. Avoid invented telemetry, precision claims, live status, or
deployment locations. Keep the constellation names and product colors meaningful. Lead
with curiosity and useful applications; avoid adversarial language, reader accusations,
and comparisons that put other projects down.

## Stack

- Vue 3 with `<script setup>` and Vite 8.
- Server-rendered at build time: the shipped `dist/index.html` contains the full semantic page,
  while Vue hydrates it for the mobile navigation. Getting-started answers use native
  disclosure elements, and the full navigation remains available without JavaScript.
- Self-hosted Public Sans, Space Grotesk, Charis Italic, and IBM Plex Mono from the `ptudornet`
  family stack, plus local SVG/PNG artwork. There are no analytics, embeds, or third-party
  runtime requests; the page runs under a strict same-origin CSP.
- Product colors and constellation identities come from the Integrity Station client. The board
  illustration follows the observer's 127 × 50.8 mm Rev A layout and real component set while
  keeping receiver and ESP32 variant labels generic.
- Midnight and graphite surfaces with violet accents connect the site to the data service.
  The turquoise brand mark and distinct constellation colors retain their own identities;
  pale neutral sections give the longer page a change of pace.

## Develop

```sh
npm install
npm run dev       # http://localhost:5177/
npm test
npm run build     # semantic static site -> dist/
npm run preview
make deploy       # build + SHA-256 digests + rsync to /usr/local/www/navlistener/web/
```

The build runs the copy guards, creates the client bundle, server-renders `App.vue` into the
generated page, removes its temporary SSR bundle, and checks the result for semantic content,
metadata, CSP compatibility, and required brand assets.

The development server uses a per-start nonce for Vite’s injected styles, so CSS updates work
under the page’s CSP. The production build uses external stylesheets and retains the strict
policy in `index.html`.

## Deploy

`make deploy` follows the sibling-site convention: it runs the tests and production build,
writes a `.sha256` file beside every generated file, then uses `rsync --delete --delay-updates`
to publish `dist/` at `/usr/local/www/navlistener/web/`. Run it on the web server, or override
`DEPLOY_DIR` for another host:

```sh
make deploy
make deploy DEPLOY_DIR=/another/document/root
```

For Apache httpd, the essential shape is:

```apache
ServerName navlisten.com
DocumentRoot "/usr/local/www/navlistener/web"

<Directory "/usr/local/www/navlistener/web">
    DirectoryIndex index.html
    Require all granted
</Directory>
```

There is one real page and no client-side router, so no fallback rewrite is needed. Send
`frame-ancestors 'none'` as an HTTP response header; browsers ignore that directive in a meta
CSP. Point the domain's A/AAAA records at the web host, issue its TLS certificate, and copy or
rsync `dist/` into the document root.

The contact links use `ptudor@ptudor.net`, a known working project-owner address, with subjects
for hosting, contributions, hardware, and customer projects. When a `@navlisten.com` mailbox
is ready, update `emailHref()` and the visible address in `src/App.vue` together.
