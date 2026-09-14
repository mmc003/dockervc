# Bump VERSION on every change that ships: patch = fixes, minor = features.
VERSION ?= 0.5.0
LDFLAGS := -s -w -X dockervc/internal/cli.Version=$(VERSION)

# The per-platform dist targets are directories that exist after the first
# run; without .PHONY, make would treat them as up to date and package stale
# binaries on later builds.
#
# Supported release platforms: Apple-silicon macOS (darwin-arm64) and x64
# Windows (windows-amd64). Other platforms build from source with
# `go build .` (Go cross-compiles).
.PHONY: all build test clean dist \
	dist/windows-amd64 dist/darwin-arm64

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o dockervc .

test:
	go vet ./... && go test ./...

# Cross-compile targets place binaries under dist/<os>-<arch>/.
dist/windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/windows-amd64/dockervc.exe .

dist/darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/darwin-arm64/dockervc .

# Tar.gz packaging shared by every non-Windows platform.
define package_unix
	cp packaging/install.sh README.md dist/$(1)/
	chmod +x dist/$(1)/install.sh
	cd dist && tar czf dockervc-$(VERSION)-$(1).tar.gz $(1)
endef

dist: dist/darwin-arm64 dist/windows-amd64
	# macOS package: tar.gz with installer + README
	$(call package_unix,darwin-arm64)
	# Windows package: zip with installer + README
	cp packaging/install.ps1 README.md dist/windows-amd64/
	cd dist && zip -q -r dockervc-$(VERSION)-windows-amd64.zip windows-amd64
	@echo "Packages in dist/:"
	@ls -lh dist/*.zip dist/*.tar.gz

clean:
	rm -rf dist dockervc
