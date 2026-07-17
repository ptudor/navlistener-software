# navlistener reference-library sync.
#
# The `reference/icd/` ICDs are the citation source of record for every spec value in the
# codebase (see reference/REFERENCES.md and README.md "Never invent spec values"). Some
# carry a redistribution grant and are committed to git; the rest are git-ignored because
# their terms forbid or do not grant redistribution. This Makefile re-fetches ALL of them
# from their official sources so any machine matches the team byte-for-byte, verifying each
# download against the SHA-256 pinned in SOURCES.tsv.
#
#   make update   # fetch every ICD in SOURCES.tsv into icd/, then verify SHA-256
#   make fetch    # alias for update
#   make verify   # check SHA-256 of the files already on disk (no download)
#   make missing  # fetch only the local-only ICDs absent from a fresh clone
#   make list     # print the manifest
#
# The committed ICDs are already present after `git clone`; a fresh checkout typically only
# needs `make missing` to pull the local-only (BeiDou/GLONASS/QZSS/NavIC) set.

ICD_DIR   := icd
MANIFEST  := SOURCES.tsv

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
	@echo 'navlistener reference-library sync — targets:'
	@echo '  make update   fetch every ICD in $(MANIFEST) into $(ICD_DIR)/, then verify SHA-256'
	@echo '  make missing  fetch only ICDs not yet present on disk (typical after a fresh clone)'
	@echo '  make verify   verify SHA-256 of files already on disk (no download)'
	@echo '  make list     print the manifest (filename / policy / size / url)'
	@echo ''
	@echo 'Override the fetch User-Agent with:  make update UA="..."'

fetch: update

update:
	@mkdir -p $(ICD_DIR)
	@echo '==> fetching (UA: $(UA))'
	@$(AWKROWS) $(MANIFEST) | \
	while IFS="$$(printf '\t')" read -r file policy sha bytes url; do \
		printf '  %-28s ' "$$file"; \
		if $(CURL) -o "$(ICD_DIR)/$$file.tmp" "$$url"; then \
			got=$$($(SHACMD) "$(ICD_DIR)/$$file.tmp" | awk '{print $$1}'); \
			if [ "$$got" = "$$sha" ]; then \
				mv "$(ICD_DIR)/$$file.tmp" "$(ICD_DIR)/$$file"; echo 'OK'; \
			else \
				rm -f "$(ICD_DIR)/$$file.tmp"; \
				echo "SHA MISMATCH (upstream re-published?) got $$got"; \
			fi; \
		else \
			rm -f "$(ICD_DIR)/$$file.tmp"; echo 'FETCH FAILED'; \
		fi; \
	done
	@$(MAKE) --no-print-directory verify

missing:
	@mkdir -p $(ICD_DIR)
	@$(AWKROWS) $(MANIFEST) | \
	while IFS="$$(printf '\t')" read -r file policy sha bytes url; do \
		[ -f "$(ICD_DIR)/$$file" ] && continue; \
		printf '  %-28s ' "$$file"; \
		if $(CURL) -o "$(ICD_DIR)/$$file.tmp" "$$url"; then \
			got=$$($(SHACMD) "$(ICD_DIR)/$$file.tmp" | awk '{print $$1}'); \
			if [ "$$got" = "$$sha" ]; then mv "$(ICD_DIR)/$$file.tmp" "$(ICD_DIR)/$$file"; echo 'OK'; \
			else rm -f "$(ICD_DIR)/$$file.tmp"; echo "SHA MISMATCH got $$got"; fi; \
		else rm -f "$(ICD_DIR)/$$file.tmp"; echo 'FETCH FAILED'; fi; \
	done
	@echo '==> done (run `make verify` for a full check)'

verify:
	@$(AWKROWS) $(MANIFEST) | \
	while IFS="$$(printf '\t')" read -r file policy sha bytes url; do \
		if [ ! -f "$(ICD_DIR)/$$file" ]; then printf '  %-28s MISSING\n' "$$file"; continue; fi; \
		got=$$($(SHACMD) "$(ICD_DIR)/$$file" | awk '{print $$1}'); \
		if [ "$$got" = "$$sha" ]; then printf '  %-28s OK\n' "$$file"; \
		else printf '  %-28s MISMATCH (have %s)\n' "$$file" "$$got"; fi; \
	done; \
	echo '==> verify complete (MISSING = local-only ICD not yet fetched; run `make missing`)'

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
