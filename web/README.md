# NavListen website

The Vue 3 static recruiting site for `https://navlisten.com/`.

## Stack

- Vue 3 with `<script setup>` and Vite 8.
- Server-rendered at build time: the shipped `dist/index.html` contains the full semantic page,
  while Vue hydrates it for the mobile navigation. JavaScript is an enhancement, not a reading
  requirement.
- Self-hosted Public Sans, Space Grotesk, Charis Italic, and IBM Plex Mono from the `ptudornet`
  family stack, plus local SVG/PNG artwork. There are no analytics, embeds, or third-party
  runtime requests; the page runs under a strict same-origin CSP.
- Product colors and constellation identities come from the Integrity Station client. The board
  illustration follows the observer's 127 × 50.8 mm Rev A layout and real component set while
  keeping receiver and ESP32 variant labels generic.

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

The join links currently use `ptudor@ptudor.net`, a known working project-owner address. Change
`emailHref()` in `src/App.vue` when a `@navlisten.com` mailbox is ready.
