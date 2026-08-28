# NavListen — static Vue 3 project site at navlisten.com
# Periodically run: npx npm-check-updates -u && npm install
#
# Keep the generated site in a web/ subdirectory so rsync --delete is scoped
# away from any future sibling trees under /usr/local/www/navlisten/.
SITE_DIR   = /usr/local/www/navlisten
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
