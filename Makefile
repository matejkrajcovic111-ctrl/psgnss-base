# PSGNSS_base — build
BINARY   := psgnssd
PKG      := ./cmd/psgnssd
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GO       ?= go

# Reproducible: no cgo, trimmed paths, pinned build id, static.
LDFLAGS := -s -w -buildid= \
	-X github.com/psgnss/psgnss-base/internal/version.Version=$(VERSION) \
	-X github.com/psgnss/psgnss-base/internal/version.Commit=$(COMMIT)
GOFLAGS := -trimpath -buildvcs=false

.PHONY: all arm64 host test vet clean verify release release-tool sign-release

all: arm64

## arm64: the deployment target — static binary for the Raspberry Pi
arm64:
	@mkdir -p dist
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 $(PKG)
	@echo "built: dist/$(BINARY)-linux-arm64"
	@sha256sum dist/$(BINARY)-linux-arm64

## host: build for the machine you're on (development)
host:
	@mkdir -p dist
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/$(BINARY) $(PKG)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

## verify: build twice and confirm the hashes match
verify:
	@$(MAKE) --no-print-directory arm64 >/dev/null
	@sha256sum dist/$(BINARY)-linux-arm64 | awk '{print $$1}' > /tmp/psgnss.h1
	@rm -f dist/$(BINARY)-linux-arm64
	@$(MAKE) --no-print-directory arm64 >/dev/null
	@sha256sum dist/$(BINARY)-linux-arm64 | awk '{print $$1}' > /tmp/psgnss.h2
	@if cmp -s /tmp/psgnss.h1 /tmp/psgnss.h2; then \
		echo "REPRODUCIBLE: $$(cat /tmp/psgnss.h1)"; \
	else echo "NOT REPRODUCIBLE"; exit 1; fi

## release: what gets deployed. Refuses a dirty tree, because the version
## string is baked in at build time: a binary built mid-work reports the last
## commit plus "-dirty" and production then claims a release it is not running.
## This has happened twice.
release:
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "refusing to build a release: the working tree is dirty."; \
		echo "commit first, so the embedded version matches what is deployed:"; \
		git status --short; exit 1; \
	fi
	@$(MAKE) --no-print-directory verify
	@echo "release: $$(git describe --tags --always) ($$(git rev-parse --short HEAD))"

## release-tool: the signing tool. Maintainer's machine only; a station only
## ever verifies.
release-tool:
	@mkdir -p dist
	$(GO) build -o dist/psgnss-release ./tools/psgnss-release

## sign-release: sign what `release` built, so stations can take it.
##   make sign-release BASE=https://host/path/v0.7.0 [NOTES="..."]
## Needs the signing key (default ~/.ssh/psgnss-release.key). Losing that key
## means no installed station can be updated in place again.
sign-release: release release-tool
	@test -n "$(BASE)" || { echo "set BASE to where the artifacts will be published"; exit 1; }
	$(GO) run ./cmd/psgnssd --dependencies-json > dist/dependencies.json
	./dist/psgnss-release sign -version "$(VERSION)" -commit "$(COMMIT)" \
		-base "$(BASE)" -notes "$(NOTES)" -deps dist/dependencies.json \
		-out dist/release.json dist/$(BINARY)-linux-arm64
	@echo
	@echo "Publish dist/release.json and dist/$(BINARY)-linux-arm64 under $(BASE),"
	@echo "then point update.manifest_url at the release.json URL."

clean:
	rm -rf dist
