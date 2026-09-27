# Deploying the NavListen website on Junia

The public site is <https://navlisten.com/>. It is a generated Vue 3 site
served directly by Apache httpd. Routine releases run from `web/` with
`make deploy`: Junia fetches the exact pushed commit into
`/home/daybreak2026/Git/apps/navlistener-software`, installs the lockfile with
`npm ci`, audits it, runs the full test and semantic static-export checks, and
copies the generated files locally into `/usr/local/www`.

Use `make deploy-dry` to review the commits and file summary first.

## Ownership and data boundary

```text
/home/daybreak2026/Git/apps/navlistener-software/  daybreak2026:daybreak2026
/usr/local/www/navlistener/                       daybreak2026:navlistener
└── web/                                          daybreak2026:navlistener
    └── assets/boards/                            daybreak2026:navlistener 2775
        ├── manifest.json                         generated from Git
        └── *.png, *.jpg                          production media, preserved
```

The website mirror excludes the published PNG and JPEG board previews and
their digest sidecars. The tracked manifest is updated with the site build.
The setgid directory lets the independent media publisher and the deployment
account share this narrow path without giving the media account ownership of
the generated website.

## First conversion

```sh
pw groupadd -n navlistener
pw useradd -n navlistener -g navlistener \
    -d /usr/local/www/navlistener -s /usr/sbin/nologin \
    -c "NavListen static media"
pw groupmod navlistener -m ptudor,daybreak2026

install -d -o daybreak2026 -g daybreak2026 -m 0755 \
    /home/daybreak2026/Git /home/daybreak2026/Git/apps
sudo -u daybreak2026 env HOME=/home/daybreak2026 \
    git -c safe.directory=/git/apps/navlistener-software clone \
    /git/apps/navlistener-software \
    /home/daybreak2026/Git/apps/navlistener-software

chown -R daybreak2026:navlistener /usr/local/www/navlistener
chown -R navlistener:navlistener \
    /usr/local/www/navlistener/web/assets/boards
chown daybreak2026:navlistener \
    /usr/local/www/navlistener/web/assets/boards
chmod 2775 /usr/local/www/navlistener/web/assets/boards
```

Before the first deployment, archive the current document root and record the
board-media file count. The deployment script verifies the count again after
every release.
