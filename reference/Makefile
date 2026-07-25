# navlistener reference-library sync.
#
# The `reference/icd/` ICDs are the citation source of record for every spec value in the
# codebase (see reference/REFERENCES.md and README.md "Never invent spec values"). Some
# carry a redistribution grant and are committed to git; the rest are git-ignored because
# their terms forbid or do not grant redistribution. This Makefile re-fetches ALL of them
# from their official sources so any machine matches the team byte-for-byte, verifying each
# download against the SHA-256 pinned in SOURCES.tsv.
#
#   make update   # fetch every ICD in SOURCES.tsv into icd/, then verify SHA-256 (strict)
#   make fetch    # alias for update
#   make verify   # verify SHA-256 of the files already on disk (no download)
#   make missing  # fetch only the local-only ICDs absent from a fresh clone
#   make list     # print the manifest
#
# The committed ICDs are already present after `git clone`; a fresh checkout typically only
# needs `make missing` to pull the local-only (BeiDou/GLONASS/QZSS/NavIC) set.
#
# STATUS CONTRACT. Every target here is fail-closed: it reports ALL rows first,
# then exits nonzero if any row failed. These are gates, not best-effort reports — a green
# exit means "the citation source of record matches the pinned SHA-256, byte for byte."
#
#   MISMATCH                 always fails. A hash change means the local file is not the
#                            document the code cites (corruption, or an upstream re-publish
#                            that needs a new manifest row + a re-read of every citation).
#   MISSING, policy=commit   always fails. Committed ICDs are in git and present after clone;
#                            their absence is a broken checkout, not a normal state.
#   MISSING, policy=local    fails only under STRICT=1. Local-only ICDs are git-ignored and
#                            legitimately absent from a fresh clone until `make missing`, so
#                            the default must not make a correct fresh clone un-verifiable.
#   FETCH FAILED             always fails (`update`, `missing`).
#
# Automation that wants the whole library proven runs:  make missing && make verify STRICT=1
# `go/Makefile check` runs the default (non-strict) `verify`, which gates the committed set.

ICD_DIR   := icd
MANIFEST  := SOURCES.tsv

# STRICT=1 also requires the git-ignored local-only ICDs to be present (see the status
# contract above). Default 0 so a fresh clone that has not yet run `make missing` still
# gets a meaningful, passing check of everything git actually gave it.
STRICT    ?= 0

# User-Agent for every fetch. Several issuing sites (NAVCEN, beidou.gov.cn, isro.gov.in)
# reject curl's default UA, so we send an identifiable browser-compatible one. Override on
# the command line if a site starts refusing it, e.g. `make update UA="Mozilla/5.0"`.
UA        ?= Mozilla/5.0 (Macintosh; Intel Mac OS X) navlistener-refsync/1.0

# --max-time is generous: the Chinese and Russian government hosts are slow.
CURL      := curl --location --fail --show-error --silent --max-time 300 \
                  --retry 2 --retry-delay 3 --user-agent '$(UA)'

# Portable SHA-256: coreutils sha256sum (Linux/FreeBSD) or macOS/perl shasum.
SHACMD    := $(shell command -v sha256sum >/dev/null 2>&1 && echo 'sha256sum' || echo 'shasum -a 256')

# awk row filter, shared by every recipe: emit the 5 fields of each real manifest row,
# skipping blank lines and '#' comments. The '#' is built with sprintf so the literal
# never reaches Make's parser as a comment. Recipes pick the fields they need.
AWKROWS   := awk -F'\t' 'BEGIN{h=sprintf("%c",35)} NF>=5 && substr($$1,1,1)!=h {print $$1"\t"$$2"\t"$$3"\t"$$4"\t"$$5}'

.PHONY: help update fetch verify missing list clean-local

help:
	@echo 'navlistener reference-library sync — targets (all fail-closed, exit nonzero on any bad row):'
	@echo '  make update   fetch every ICD in $(MANIFEST), then verify SHA-256 (implies STRICT=1)'
	@echo '  make missing  fetch only ICDs not yet present on disk (typical after a fresh clone)'
	@echo '  make verify   verify SHA-256 of files already on disk (no download)'
	@echo '  make list     print the manifest (filename / policy / size / url)'
	@echo ''
	@echo 'STRICT=1 also requires the git-ignored local-only ICDs:  make missing && make verify STRICT=1'
	@echo 'Override the fetch User-Agent with:  make update UA="..."'

fetch: update

# Each recipe below runs its row loop inside a `{ ...; exit $$fail; }` group on the RIGHT
# side of the pipe. The group IS the subshell make waits on, so a flag set inside the loop
# survives to the pipeline's exit status — the naive `fail=1` set after the pipe would be
# lost with the subshell. Rows all print before the status is decided, so a
# failing run still shows the operator every problem, not just the first.
update:
	@mkdir -p $(ICD_DIR)
	@echo '==> fetching (UA: $(UA))'
	@$(AWKROWS) $(MANIFEST) | { \
		fail=0; \
		while IFS="$$(printf '\t')" read -r file policy sha bytes url; do \
			printf '  %-28s ' "$$file"; \
			if $(CURL) -o "$(ICD_DIR)/$$file.tmp" "$$url"; then \
				got=$$($(SHACMD) "$(ICD_DIR)/$$file.tmp" | awk '{print $$1}'); \
				if [ "$$got" = "$$sha" ]; then \
					mv "$(ICD_DIR)/$$file.tmp" "$(ICD_DIR)/$$file"; echo 'OK'; \
				else \
					rm -f "$(ICD_DIR)/$$file.tmp"; fail=1; \
					echo "SHA MISMATCH (upstream re-published?) got $$got"; \
				fi; \
			else \
				rm -f "$(ICD_DIR)/$$file.tmp"; fail=1; echo 'FETCH FAILED'; \
			fi; \
		done; \
		exit $$fail; \
	}; st=$$?; \
	$(MAKE) --no-print-directory verify STRICT=1 || st=1; \
	if [ $$st -ne 0 ]; then echo '==> update FAILED (see rows above)'; fi; \
	exit $$st

missing:
	@mkdir -p $(ICD_DIR)
	@$(AWKROWS) $(MANIFEST) | { \
		fail=0; \
		while IFS="$$(printf '\t')" read -r file policy sha bytes url; do \
			if [ -f "$(ICD_DIR)/$$file" ]; then continue; fi; \
			printf '  %-28s ' "$$file"; \
			if $(CURL) -o "$(ICD_DIR)/$$file.tmp" "$$url"; then \
				got=$$($(SHACMD) "$(ICD_DIR)/$$file.tmp" | awk '{print $$1}'); \
				if [ "$$got" = "$$sha" ]; then mv "$(ICD_DIR)/$$file.tmp" "$(ICD_DIR)/$$file"; echo 'OK'; \
				else rm -f "$(ICD_DIR)/$$file.tmp"; fail=1; echo "SHA MISMATCH got $$got"; fi; \
			else rm -f "$(ICD_DIR)/$$file.tmp"; fail=1; echo 'FETCH FAILED'; fi; \
		done; \
		if [ $$fail -ne 0 ]; then echo '==> missing FAILED (see rows above)'; \
		else echo '==> done (run `make verify STRICT=1` for a full check)'; fi; \
		exit $$fail; \
	}

verify:
	@$(AWKROWS) $(MANIFEST) | { \
		fail=0; \
		while IFS="$$(printf '\t')" read -r file policy sha bytes url; do \
			if [ ! -f "$(ICD_DIR)/$$file" ]; then \
				if [ "$$policy" = "local" ] && [ "$(STRICT)" != "1" ]; then \
					printf '  %-28s MISSING (local-only, not fetched — run `make missing`)\n' "$$file"; \
				else \
					printf '  %-28s MISSING\n' "$$file"; fail=1; \
				fi; \
				continue; \
			fi; \
			got=$$($(SHACMD) "$(ICD_DIR)/$$file" | awk '{print $$1}'); \
			if [ "$$got" = "$$sha" ]; then printf '  %-28s OK\n' "$$file"; \
			else printf '  %-28s MISMATCH (have %s)\n' "$$file" "$$got"; fail=1; fi; \
		done; \
		if [ $$fail -ne 0 ]; then echo '==> verify FAILED (see rows above)'; \
		elif [ "$(STRICT)" = "1" ]; then echo '==> verify OK (strict: every manifest row present and matching)'; \
		else echo '==> verify OK (committed set; local-only ICDs are advisory — STRICT=1 to require them)'; fi; \
		exit $$fail; \
	}

list:
	@printf '%-28s %-7s %10s  %s\n' FILE POLICY BYTES URL
	@$(AWKROWS) $(MANIFEST) | \
	while IFS="$$(printf '\t')" read -r file policy sha bytes url; do \
		printf '%-28s %-7s %10s  %s\n' "$$file" "$$policy" "$$bytes" "$$url"; \
	done

# Remove only the git-ignored (local-only) ICDs; never touches committed ones.
clean-local:
	@$(AWKROWS) $(MANIFEST) | \
	while IFS="$$(printf '\t')" read -r file policy sha bytes url; do \
		[ "$$policy" = "local" ] && rm -f "$(ICD_DIR)/$$file" && echo "removed $$file"; \
	done; true
