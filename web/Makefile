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
HOST       ?= junia-daybreak
SSH_CONF   ?=
SSH_KEY    ?=
REMOTE_REPO ?= /home/daybreak2026/Git/apps/navlistener-software
SSH         = ssh -F $(SSH_CONF) -i $(SSH_KEY) -o IdentitiesOnly=yes -o IdentityAgent=none -- $(HOST)

# Board preview images stay out of Git. Run publish-boards on the machine holding
# them; it pushes to the web host over rsync, unlike deploy, which runs there.
# The page shows JPEG copies flattened onto BOARDS_MATTE, the .hardware background
# in src/styles.css, so the transparent PNG corners disappear into the section.
BOARDS_DIR          = public/assets/boards
BOARDS_HOST         = junia
BOARDS_MATTE        = rgb(18,23,32)
BOARDS_JPEG_QUALITY = 80

default: build

build: test
	npm run build

install:
	npm ci

audit:
	npm audit

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
# Board preview images are published by publish-boards. Excluding them skips any
# local copies in dist/ and keeps --delete off the published files.
deploy-local: digest
	rsync -rlpt --delete --delay-updates \
		--exclude='/assets/boards/*.png' --exclude='/assets/boards/*.png.sha256' \
		--exclude='/assets/boards/*.jpg' --exclude='/assets/boards/*.jpg.sha256' \
		dist/ $(DEPLOY_DIR)/

deploy-source-check:
	@test -z "$$(git status --porcelain --untracked-files=no)" || { \
		echo "Refusing to deploy tracked changes that are not committed" >&2; exit 1; }
	@revision=$$(git rev-parse HEAD); \
		remote_head=$$(git ls-remote origin refs/heads/main | awk '{print $$1}'); \
		test "$$revision" = "$$remote_head" || { \
		echo "Refusing to deploy: HEAD $$revision is not pushed main $$remote_head" >&2; \
		exit 1; }

deploy-ssh-check:
	@test -n "$(SSH_CONF)" -a -f "$(SSH_CONF)" || { \
		echo "Set SSH_CONF to the dedicated deployment SSH config" >&2; exit 1; }
	@test -n "$(SSH_KEY)" -a -f "$(SSH_KEY)" || { \
		echo "Set SSH_KEY to the dedicated deployment private key" >&2; exit 1; }

deploy-dry: deploy-source-check deploy-ssh-check
	@revision=$$(git rev-parse HEAD); \
		$(SSH) "sudo -n $(REMOTE_REPO)/web/deploy/freebsd/deploy-from-git --check $$revision"

deploy: deploy-source-check deploy-ssh-check
	@revision=$$(git rev-parse HEAD); \
		$(SSH) "sudo -n $(REMOTE_REPO)/web/deploy/freebsd/deploy-from-git $$revision"

# oxipng is lossless and rewrites a file only when it gets smaller. Each JPEG is
# regenerated from its PNG so a new export never leaves a stale copy behind.
prepare-boards:
	oxipng -o 4 --strip safe $(BOARDS_DIR)/*-3d.png
	for png in $(BOARDS_DIR)/*-3d.png; do \
		convert "$$png" -background '$(BOARDS_MATTE)' -alpha remove -alpha off -strip \
			-interlace JPEG -sampling-factor 4:2:0 -quality $(BOARDS_JPEG_QUALITY) \
			"$${png%.png}.jpg" || exit 1; \
	done
publish-boards: prepare-boards deploy-ssh-check
	rsync -vrlpt --delay-updates \
		-e 'ssh -F $(SSH_CONF) -i $(SSH_KEY) -o IdentitiesOnly=yes -o IdentityAgent=none' \
		$(BOARDS_DIR)/*-3d.png $(BOARDS_DIR)/*-3d.jpg \
		$(BOARDS_HOST):$(DEPLOY_DIR)/assets/boards/

.PHONY: default build test install audit dev preview digest deploy-local deploy-source-check deploy-ssh-check deploy-dry deploy prepare-boards publish-boards
