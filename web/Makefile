# NavListen — static Vue 3 project site at navlisten.com
# Periodically run: npx npm-check-updates -u && npm install
#
# The site root is named for the PRODUCT SLUG (navlistener), not the domain
# (navlisten.com) and not the brand in the page. That is the design rule on
# junia wherever the two differ -- rcktfl.com serves /usr/local/www/rocketfuel
# -- and the Ground sibling already does it: radiolistener's Django tree is
# /usr/local/www/radiolistener/django. Do not shorten this to match the
# hostname; navlisten.com.conf's DocumentRoot is the other half of the pair.
#
# Keep the generated site in a web/ subdirectory so rsync --delete is scoped
# away from any future sibling trees under /usr/local/www/navlistener/.
SITE_DIR   = /usr/local/www/navlistener
DEPLOY_DIR = $(SITE_DIR)/web

default: build

build: test
	npm run build

install:
	npm install

test:
	npm test

dev:
	npm run dev

preview:
	npm run preview

digest:
	find dist -type f ! -name '*.sha256' -exec sh -c 'shasum -a 256 "$$1" > "$$1.sha256"' _ {} \;

# Run on the web server. Build and digest complete before rsync starts;
# --delay-updates keeps the published tree internally consistent while files land.
deploy: build digest
	rsync -a --delete --delay-updates dist/ $(DEPLOY_DIR)/

.PHONY: default build test install dev preview digest deploy
