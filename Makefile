# Release state and signer configuration are explicitly external to this tree.
RELEASE_PYTHON ?= python3
RELEASE_CONFIG ?=
NOTES ?= RELEASE-NOTES.md
PERCENT ?= 1
.PHONY: release release-dry-run release-resume release-promote release-withdraw release-check release-refresh-online release-test-setup release-test release-tests
release:
	$(RELEASE_PYTHON) tools/releases/release.py --config '$(RELEASE_CONFIG)' release --notes '$(NOTES)'
release-dry-run:
	$(RELEASE_PYTHON) tools/releases/release.py --config '$(RELEASE_CONFIG)' dry-run
release-resume:
	$(RELEASE_PYTHON) tools/releases/release.py --config '$(RELEASE_CONFIG)' resume --release '$(RELEASE)'
release-promote:
	$(RELEASE_PYTHON) tools/releases/release.py --config '$(RELEASE_CONFIG)' promote --release '$(RELEASE)' --channel '$(CHANNEL)' --percent '$(PERCENT)'
release-withdraw:
	$(RELEASE_PYTHON) tools/releases/release.py --config '$(RELEASE_CONFIG)' withdraw --release '$(RELEASE)' --channel '$(CHANNEL)'
release-check:
	$(RELEASE_PYTHON) tools/releases/release.py --config '$(RELEASE_CONFIG)' check --release '$(RELEASE)'
release-refresh-online:
	$(RELEASE_PYTHON) tools/releases/release.py --config '$(RELEASE_CONFIG)' refresh-online
release-test-setup:
	$(RELEASE_PYTHON) tools/releases/release.py init-test --keys '$(KEYS)' --state '$(STATE)'
release-test:
	$(RELEASE_PYTHON) tools/releases/release.py test-release --keys '$(KEYS)' --state '$(STATE)' --image '$(IMAGE)' --release '$(RELEASE)'
release-tests:
	$(MAKE) -C esp32/components/ota/test tuf-client
	$(RELEASE_PYTHON) -m unittest discover -s tools/releases -p 'test_*.py'
