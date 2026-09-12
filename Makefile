# Bump VERSION on every change that ships: patch = fixes, minor = features.
VERSION ?= 0.4.3
LDFLAGS := -s -w -X dockervc/internal/cli.Version=$(VERSION)

# The per-platform dist targets are directories that exist after the first
# run; without .PHONY, make would treat them as up to date and package stale
# binaries on later builds.
.PHONY: all build test clean dist dist-windows dist-linux dist-macos \
	dist/windows-amd64 dist/windows-arm64 \
	dist/linux-amd64 dist/linux-arm64 \
	dist/darwin-amd64 dist/darwin-arm64

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o dockervc .

test:
	go vet ./... && go test ./...

# Cross-compile targets place binaries under dist/<os>-<arch>/.
dist/windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/windows-amd64/dockervc.exe .

dist/windows-arm64:
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/windows-arm64/dockervc.exe .

dist/linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/linux-amd64/dockervc .

dist/linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/linux-arm64/dockervc .

dist/darwin-amd64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/darwin-amd64/dockervc .

dist/darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/darwin-arm64/dockervc .

# Tar.gz packaging shared by every non-Windows platform.
define package_unix
	cp packaging/install.sh README.md dist/$(1)/
	chmod +x dist/$(1)/install.sh
	cd dist && tar czf dockervc-$(VERSION)-$(1).tar.gz $(1)
endef

dist-macos: dist/darwin-amd64 dist/darwin-arm64
	$(call package_unix,darwin-amd64)
	$(call package_unix,darwin-arm64)

dist: dist/windows-amd64 dist/windows-arm64 dist/linux-amd64 dist/linux-arm64 dist-macos
	# Windows packages: zip with installer + README
	cp packaging/install.ps1 README.md dist/windows-amd64/
	cp packaging/install.ps1 README.md dist/windows-arm64/
	cd dist && zip -q -r dockervc-$(VERSION)-windows-amd64.zip windows-amd64 \
		&& zip -q -r dockervc-$(VERSION)-windows-arm64.zip windows-arm64
	# Linux packages: tar.gz with installer + README
	$(call package_unix,linux-amd64)
	$(call package_unix,linux-arm64)
	@echo "Packages in dist/:"
	@ls -lh dist/*.zip dist/*.tar.gz

clean:
	rm -rf dist dockervc
