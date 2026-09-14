# Bump VERSION on every change that ships: patch = fixes, minor = features.
VERSION ?= 0.5.1
LDFLAGS := -s -w -X dockervc/internal/cli.Version=$(VERSION)

# The per-platform dist targets are directories that exist after the first
# run; without .PHONY, make would treat them as up to date and package stale
# binaries on later builds.
#
# Supported release platform: Apple-silicon macOS (darwin-arm64). Everything
# else builds from source with `go build .` (Go cross-compiles).
.PHONY: all build test clean dist dist/darwin-arm64

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o dockervc .

test:
	go vet ./... && go test ./...

# Cross-compile targets place binaries under dist/<os>-<arch>/.
dist/darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/darwin-arm64/dockervc .

# Tar.gz packaging.
define package_unix
	cp packaging/install.sh packaging/uninstall.sh README.md dist/$(1)/
	chmod +x dist/$(1)/install.sh dist/$(1)/uninstall.sh
	cd dist && tar czf dockervc-$(VERSION)-$(1).tar.gz $(1)
endef

dist: dist/darwin-arm64
	$(call package_unix,darwin-arm64)
	@echo "Packages in dist/:"
	@ls -lh dist/*.tar.gz

clean:
	rm -rf dist dockervc
