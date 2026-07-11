# navlistener — the GNSS collector daemon.
# Primary deploy target is FreeBSD/amd64 (collector-host); linux/arm64 is the SBC field box.
BINARY      := navlistener
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_TIME  := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
PKG         := github.com/ptudor/navlistener/internal/version
LDFLAGS     := -ldflags "-s -w -X $(PKG).Version=$(VERSION) -X $(PKG).BuildTime=$(BUILD_TIME)"
BUILD_DIR   := build
CMD         := ./cmd/navlistener

GO          := go

.PHONY: all check build build-linux build-linux-arm64 build-darwin build-darwin-arm64 \
        build-freebsd build-all install test race fuzz vet fmt tidy clean run help

all: check build

# check runs the fast quality gate: fmt-check + vet + tests.
check: vet test

build:
	$(GO) build $(LDFLAGS) -o $(BINARY) $(CMD)

build-linux:
	mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-amd64 $(CMD)

build-linux-arm64:
	mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-arm64 $(CMD)

build-darwin:
	mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-darwin-amd64 $(CMD)

build-darwin-arm64:
	mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-darwin-arm64 $(CMD)

build-freebsd:
	mkdir -p $(BUILD_DIR)
	GOOS=freebsd GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-freebsd-amd64 $(CMD)

build-all: build-linux build-linux-arm64 build-darwin build-darwin-arm64 build-freebsd

# Local install (developer box). On collector-host, copy the freebsd binary to /usr/local/bin.
install: build
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)

test:
	$(GO) test ./...

# race runs the full suite under the data-race detector (the C1–C11 discipline).
race:
	$(GO) test -race ./...

# fuzz smoke-tests every fuzz target across both workspace modules -- this one
# (navlistener) and ../gnss, where most decoder fuzz targets actually live
# (docs/MATH.md §12). `-fuzz../...` cannot work at all -- `go test -fuzz`
# accepts exactly one package per invocation ("cannot use -fuzz flag with multiple
# packages") AND exactly one matching fuzz function per invocation (a package
# with more than one Fuzz* errors "-fuzz matches more than one fuzz test"), so
# this discovers every (module, package, FuzzXxx) triple via `go test -list` and
# runs each individually. FUZZTIME overrides the per-target duration (default 30s;
# use e.g. `make fuzz FUZZTIME=5s` for a quick local check).
FUZZTIME ?= 30s
fuzz:
	@set -e; \
	for modroot in . ../gnss; do \
		( cd $$modroot && \
		  for pkg in $$($(GO) list ./...); do \
		    for fn in $$($(GO) test -list '^Fuzz' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
		      echo "==> $$modroot :: $$pkg :: $$fn"; \
		      $(GO) test -run '^$$' -fuzz "^$${fn}$$" -fuzztime $(FUZZTIME) $$pkg; \
		    done; \
		  done ); \
	done

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

run: build
	./$(BINARY) -config ./navlistener.toml

clean:
	rm -f $(BINARY)
	rm -rf $(BUILD_DIR)

help:
	@echo "targets: check build build-freebsd build-linux-arm64 build-all install test race fuzz vet fmt tidy run clean"
