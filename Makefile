# Bump VERSION on every change that ships: patch = fixes, minor = features.
VERSION ?= 0.7.0
LDFLAGS := -s -w -X dockervc/internal/cli.Version=$(VERSION)

# The per-platform dist targets are directories that exist after the first
# run; without .PHONY, make would treat them as up to date and package stale
# binaries on later builds.
#
# Supported release platforms: Apple-silicon macOS and Windows amd64.
.PHONY: all build test clean dist dist/darwin-arm64 dist/windows-amd64

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o dockervc .

test:
	go vet ./... && go test ./...

# Cross-compile targets place binaries under dist/<os>-<arch>/.
dist/darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/darwin-arm64/dockervc .

dist/windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/windows-amd64/dockervc.exe .

# Tar.gz packaging.
define package_unix
	cp packaging/install.sh packaging/uninstall.sh README.md dist/$(1)/
	chmod +x dist/$(1)/install.sh dist/$(1)/uninstall.sh
	cd dist && tar czf dockervc-$(VERSION)-$(1).tar.gz $(1)
endef

dist: dist/darwin-arm64 dist/windows-amd64
	$(call package_unix,darwin-arm64)
	cp packaging/install.ps1 packaging/uninstall.ps1 README.md dist/windows-amd64/
	cd dist && zip -q -r dockervc-$(VERSION)-windows-amd64.zip windows-amd64
	@echo "Packages in dist/:"
	@ls -lh dist/*.tar.gz dist/*.zip

ifeq ($(OS),Windows_NT)
clean:
	powershell.exe -NoProfile -Command "if (Test-Path -LiteralPath 'dist') { Remove-Item -LiteralPath 'dist' -Recurse -Force -ErrorAction Stop }; if (Test-Path -LiteralPath 'dockervc.exe') { Remove-Item -LiteralPath 'dockervc.exe' -Force -ErrorAction Stop }"
else
clean:
	rm -rf dist dockervc
endif
