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
- Product colors and constellation identities come from the Integrity Station client.
- Midnight and graphite surfaces with violet accents connect the site to the data service.
  The four-color compass mark is shared with Integrity Satellite. Constellation colors
  retain their own identities; pale neutral sections give the longer page a change of pace.

The vector brand sources are `public/assets/mark.svg` and `public/assets/og-image.svg`.
Use `rsvg-convert` to render the mark at 64, 180, and 512 pixels for `favicon.png`,
`apple-touch-icon.png`, and `app-icon-512.png`, and the social card at 1200 × 630
for `og-image.png`. Update their URL version in the page and web manifest when
replacing artwork so cached icons refresh.

## Develop

```sh
npm install
npm run dev       # http://localhost:5177/
npm test
npm run build     # semantic static site -> dist/
npm run preview
make deploy-dry   # review the exact pushed commit Junia would deploy
make deploy       # build + audit + publish through Junia's local checkout
make prepare-boards  # optimize rendered PNGs and make JPEG copies
make publish-boards  # publish the untracked board renders using the dedicated SSH connection
```

The build runs the copy guards, creates the client bundle, server-renders `App.vue` into the
generated page, removes its temporary SSR bundle, and checks the result for semantic content,
metadata, CSP compatibility, and required brand assets.

The development server uses a per-start nonce for Vite’s injected styles, so CSS updates work
under the page’s CSP. The production build uses external stylesheets and retains the strict
policy in `index.html`.

## Deploy

`make deploy` requires a clean, pushed `main`. Junia fetches that exact commit into
`/home/daybreak2026/Git/apps/navlistener-software`, runs `npm ci`, audits the lockfile,
runs the tests and production build, writes a `.sha256` file beside every generated file,
and publishes `dist/` locally at `/usr/local/www/navlistener/web/`.

Use the lower-level host target only for installation or recovery:

```sh
make deploy-local
make deploy-local DEPLOY_DIR=/another/document/root
```

The first conversion and ownership contract are in `deploy/freebsd/QUICKSTART.md`.
Pass the dedicated connection files through `SSH_CONF` and `SSH_KEY` when they
are not already set in your shell; their machine-specific paths stay outside
the public repository.

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

## Board renders

The Hardware section shows top and bottom 3D renders of NEO, MAX, and ZED/X20.
They use the saved board placements and bound STEP component models, with green
solder mask and no silkscreen artwork. Pads, mounting holes, board thickness,
component markings, and connector overhangs come from the design sources.
Copper routing, vias, solder joints, and cables are omitted. These are illustrations,
not assembly photographs or fabrication proofs.

The website receives six images and a manifest. Design geometry stays in the
hardware checkout and the local scene directory. The normal website build needs
neither the hardware repository nor any rendering dependencies.

### Generate

Use Python with FreeCAD's `FreeCAD` and `Part` modules installed. Create the
rendering environments separately from the website dependencies:

```sh
python3 -m venv --system-site-packages .render-python
.render-python/bin/pip install pycryptodome==3.23.0
npm install --prefix .render-tools three@0.183.2 playwright@1.63.0
```

Choose both source checkouts explicitly. The model library supplies the colour
palette used by its STEP exporter. The scripts read saved projects without
launching or changing the editor, verify the component transforms and STEP
digests against the model receipts, and stop on missing models or unsupported
geometry. Test points and explicitly unfitted parts remain bare pads.

```sh
.render-python/bin/python scripts/export-board-scenes.py \
  --hardware /path/to/navlistener-hardware \
  --materials /path/to/easyeda-tudor/libraries/models3d/tudor_step.py \
  --output board-scenes
node scripts/render-boards.mjs \
  --scenes board-scenes --output public/assets/boards \
  --tools .render-tools --browser /path/to/chromium
make prepare-boards
npm run build
```

The renderer serves the scenes only on localhost during capture. It writes
`neo-top-3d.png`, `neo-bottom-3d.png`, `max-top-3d.png`, `max-bottom-3d.png`,
`zed-top-3d.png`, `zed-bottom-3d.png`, and `manifest.json`. Images are 1600 × 1200.
Review every view before publishing, including bottom-side orientation and holes.

The manifest records dimensions, component counts, source and model-set digests,
and image digests without local paths. Only the manifest is tracked. Scene files,
rendering dependencies, PNGs, and JPEGs are Git-ignored.

### Publish

`make prepare-boards` requires `oxipng` and ImageMagick's `convert`. It optimizes
the PNGs losslessly and makes progressive JPEGs at quality 80. Their transparent
corners are filled with `BOARDS_MATTE`, matching the hardware section background.
The manifest's PNG hashes identify the original renders before optimization and
provide cache versions for the JPEG URLs.

Publish the new images before deploying the page that references them:

```sh
make publish-boards SSH_CONF=/path/to/dedicated/config SSH_KEY=/path/to/dedicated/key
make deploy HOST=registered-host SSH_CONF=/path/to/dedicated/config SSH_KEY=/path/to/dedicated/key
```

`BOARDS_HOST` defaults to `junia`; use only a host registered in the dedicated
configuration. Transfers use that configuration and key with the SSH agent disabled.
The `-3d` asset names let the new images be published while the previous page still
shows its existing images. `make deploy` preserves independently published board
PNGs and JPEGs. The static check verifies all six references, image dimensions,
cache versions, and descriptions against the manifest.
