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

# Board preview PNGs stay out of Git. Run publish-boards on the machine holding
# them; it pushes to the web host over rsync, unlike deploy, which runs there.
BOARDS_DIR  = public/assets/boards
BOARDS_HOST = junia

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

digest: build
	find dist -type f -name '*.sha256' -delete
	find dist -type f ! -name '*.sha256' -exec sh -c 'shasum -a 256 "$$1" > "$$1.sha256"' _ {} \;

# Run on the web server. build -> digest -> deploy also orders parallel make;
# --delay-updates keeps the published tree internally consistent while files land.
# Board preview PNGs are optimized and published by publish-boards. Excluding
# them skips any local copies in dist/ and keeps --delete off the published files.
deploy: digest
	rsync -a --delete --delay-updates \
		--exclude='/assets/boards/*.png' --exclude='/assets/boards/*.png.sha256' \
		dist/ $(DEPLOY_DIR)/

# oxipng is lossless and rewrites a file only when it gets smaller.
publish-boards:
	oxipng -o max --strip safe $(BOARDS_DIR)/*.png
	rsync -a --delay-updates $(BOARDS_DIR)/*.png $(BOARDS_HOST):$(DEPLOY_DIR)/assets/boards/

.PHONY: default build test install dev preview digest deploy publish-boards
