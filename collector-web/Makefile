# NavListen collector dashboard — localized static Vue 3 export at /in/
SITE_DIR   = /usr/local/www/navlistener
DEPLOY_DIR = $(SITE_DIR)/in
HOST       ?= klax1-navlistener
SSH_CONF   ?=
SSH_KEY    ?=
SSH        = ssh -F $(SSH_CONF) -i $(SSH_KEY) -o IdentitiesOnly=yes -o IdentityAgent=none -- $(HOST)
RSYNC_SSH  = ssh -F $(SSH_CONF) -i $(SSH_KEY) -o IdentitiesOnly=yes -o IdentityAgent=none

default: build

install:
	npm ci

test:
	npm test

build: test
	npm run build

digest: build
	printf '%s\n' "$$(git rev-parse HEAD)" > dist/REVISION
	find dist -type f -name '*.sha256' -delete
	find dist -type f ! -name '*.sha256' -exec sh -c 'shasum -a 256 "$$1" > "$$1.sha256"' _ {} \;

deploy-source-check:
	@test -z "$$(git status --porcelain --untracked-files=no)" || { echo "Refusing to deploy tracked changes that are not committed" >&2; exit 1; }
	@revision=$$(git rev-parse HEAD); remote_head=$$(git ls-remote origin refs/heads/main | awk '{print $$1}'); test "$$revision" = "$$remote_head" || { echo "Refusing to deploy: HEAD $$revision is not pushed main $$remote_head" >&2; exit 1; }

deploy-ssh-check:
	@test -n "$(SSH_CONF)" -a -f "$(SSH_CONF)" || { echo "Set SSH_CONF to the dedicated deployment SSH config" >&2; exit 1; }
	@test -n "$(SSH_KEY)" -a -f "$(SSH_KEY)" || { echo "Set SSH_KEY to the dedicated deployment private key" >&2; exit 1; }

deploy-dry: digest deploy-source-check deploy-ssh-check
	$(SSH) install -d -o root -g wheel -m 0755 $(DEPLOY_DIR)
	rsync -nrlpt --delete --delay-updates -e '$(RSYNC_SSH)' dist/ $(HOST):$(DEPLOY_DIR)/

deploy: digest deploy-source-check deploy-ssh-check
	$(SSH) install -d -o root -g wheel -m 0755 $(DEPLOY_DIR)
	rsync -rlpt --delete --delay-updates -e '$(RSYNC_SSH)' dist/ $(HOST):$(DEPLOY_DIR)/

.PHONY: default install test build digest deploy-source-check deploy-ssh-check deploy-dry deploy
