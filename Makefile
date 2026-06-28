# Revquic Phase 0/1 spike Makefile
GO       ?= go
BIN      ?= bin
PKGS     := ./...

# CGO_ENABLED=0 -> pure-Go internal linker. Required on macOS hosts whose external linker emits
# Mach-O without LC_UUID (dyld "Abort trap: 6"); also yields static, portable Linux binaries.
export CGO_ENABLED = 0

.PHONY: all tidy build broker exit client certgen darwin fmt vet test clean smoke release package source web

all: build

tidy:
	$(GO) mod tidy

build: broker exit client certgen

broker:
	$(GO) build -o $(BIN)/revquic-broker ./cmd/revquic-broker

exit:
	$(GO) build -o $(BIN)/revquic-exit ./cmd/revquic-exit

client:
	$(GO) build -o $(BIN)/revquic-client ./cmd/revquic-client

certgen:
	$(GO) build -o $(BIN)/revquic-certgen ./cmd/revquic-certgen

# Build the macOS-native binaries into bin/. The client data plane is supported on darwin
# (utun + ifconfig/route); broker and certgen run anywhere. The exit's NAT is Linux-only, so it
# is intentionally not built here (run exit nodes on Linux).
darwin:
	GOOS=darwin $(GO) build -o $(BIN)/revquic-broker  ./cmd/revquic-broker
	GOOS=darwin $(GO) build -o $(BIN)/revquic-client  ./cmd/revquic-client
	GOOS=darwin $(GO) build -o $(BIN)/revquic-certgen ./cmd/revquic-certgen

# Cross-compile every binary for the platform matrix into $(DIST)/<os>-<arch>/.
# All binaries are pure Go (CGO disabled, modernc.org/sqlite), so no C toolchain is needed.
# Override the set with: make release PLATFORMS="linux/amd64 darwin/arm64"
DIST      ?= dist
CMDS      := revquic-broker revquic-exit revquic-client revquic-certgen
PLATFORMS ?= linux/amd64 linux/arm64 linux/arm linux/386 linux/riscv64 linux/ppc64le linux/s390x \
             darwin/amd64 darwin/arm64 \
             windows/amd64 windows/arm64 windows/386 \
             freebsd/amd64 freebsd/arm64 openbsd/amd64
release:
	@rm -rf $(DIST)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	  out=$(DIST)/$$os-$$arch; mkdir -p $$out; \
	  for c in $(CMDS); do \
	    echo "  $$os/$$arch  $$c$$ext"; \
	    GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w" -o $$out/$$c$$ext ./cmd/$$c || exit 1; \
	  done; \
	  cp -r conf $$out/ 2>/dev/null || true; \
	done
	@echo "release artifacts in $(DIST)/"

# Package release artifacts for GitHub Releases:
#   - one archive per platform: revquic_<version>_<os>-<arch>.{tar.gz|zip}, each containing
#     <os>-<arch>/<binaries> plus README.md + LICENSE
#   - a source tarball: revquic_<version>_source.tar.gz
#   - SHA256SUMS over every archive
# Version comes from git (tag/describe) or VERSION=... override.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
package: release source
	@cd $(DIST) && for d in */; do \
	  d=$${d%/}; \
	  cp ../README.md ../LICENSE $$d/ 2>/dev/null || true; \
	  name=revquic_$(VERSION)_$$d; \
	  if [ "$${d%%-*}" = "windows" ]; then \
	    arch=$${d#windows-}; [ "$$arch" = "386" ] && arch=x86; \
	    if cp ../third_party/wintun/$$arch/wintun.dll $$d/ 2>/dev/null; then echo "  bundled wintun.dll ($$arch) into $$d"; \
	    else echo "  NOTE: ../third_party/wintun/$$arch/wintun.dll not found — $$d.zip will NOT include wintun.dll (get it from https://www.wintun.net)"; fi; \
	    zip -qr $$name.zip $$d && echo "  packaged $$name.zip"; \
	  else \
	    tar czf $$name.tar.gz $$d && echo "  packaged $$name.tar.gz"; \
	  fi; \
	done
	@cd $(DIST) && { shasum -a 256 *.tar.gz *.zip 2>/dev/null || sha256sum *.tar.gz *.zip 2>/dev/null; } > SHA256SUMS && echo "  wrote SHA256SUMS"
	@echo "packages in $(DIST)/"

# Source tarball: prefer `git archive` (tracked files only); fall back to a filtered tar when there
# is no git repo yet.
source:
	@mkdir -p $(DIST)
	@if git rev-parse --is-inside-work-tree >/dev/null 2>&1 && git rev-parse HEAD >/dev/null 2>&1; then \
	  git archive --format=tar.gz --prefix=revquic-$(VERSION)/ -o $(DIST)/revquic_$(VERSION)_source.tar.gz HEAD && \
	  echo "  source: revquic_$(VERSION)_source.tar.gz (git archive)"; \
	else \
	  tar czf $(DIST)/revquic_$(VERSION)_source.tar.gz \
	    --exclude='./$(DIST)' --exclude='./$(BIN)' --exclude='./.git' \
	    --exclude='node_modules' --exclude='web/admin/dist' \
	    --exclude='*.db' --exclude='*.db-*' --exclude='certs' . && \
	  echo "  source: revquic_$(VERSION)_source.tar.gz (tar fallback, no git)"; \
	fi

fmt:
	$(GO) fmt $(PKGS)

# Build the Vue admin SPA and embed it into the broker (served at "/"). Uses the public npm registry
# (override REGISTRY= if your default registry needs auth). Requires node/npm.
NPM_REGISTRY ?= https://registry.npmjs.org/
web:
	cd web/admin && npm install --registry $(NPM_REGISTRY) --no-audit --no-fund && npm run build
	rm -rf internal/adminserver/web && mkdir -p internal/adminserver/web
	cp -r web/admin/dist/. internal/adminserver/web/

vet:
	$(GO) vet $(PKGS)

test:
	$(GO) test $(PKGS)

# Loopback smoke test (Linux + root). See scripts/smoke-test.sh.
smoke: build
	sudo ./scripts/smoke-test.sh

clean:
	rm -rf $(BIN) $(DIST)
