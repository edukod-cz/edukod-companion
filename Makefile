SHELL := /bin/sh

GO ?= go
VERSION ?= $(shell git -C .. describe --tags --always --dirty 2>/dev/null || printf dev)
DIST_DIR ?= dist
PLATFORMS ?= linux/amd64 linux/arm64
BUILD_SCRIPT := GO_BIN="$(GO)" sh scripts/build.sh
SOURCE_DATE_EPOCH ?= 0

.PHONY: all build test check clean dist sign deb

all: check build

build:
	$(BUILD_SCRIPT) bin/edukod-companion "$(VERSION)" linux "$$(go env GOARCH)"

test:
	$(GO) test ./...

check: test
	test -z "$$($(GO)fmt -d $$(find . -name '*.go' -type f -print))"
	$(GO) vet ./...

clean:
	rm -rf bin $(DIST_DIR)

dist: check
	rm -rf $(DIST_DIR)
	mkdir -p $(DIST_DIR)
	set -eu; for platform in $(PLATFORMS); do \
		goos=$${platform%/*}; goarch=$${platform#*/}; \
		name=edukod-companion_$(VERSION)_$${goos}_$${goarch}; \
		work=$(DIST_DIR)/.$$name; mkdir -p $$work; \
		$(BUILD_SCRIPT) $$work/edukod-companion "$(VERSION)" $$goos $$goarch; \
		cp README.md $$work/README.md; \
		cp LICENSE $$work/LICENSE; \
		cp packaging/systemd/edukod-companion.service $$work/edukod-companion.service; \
		tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='@$(SOURCE_DATE_EPOCH)' \
			-C $$work -cf - edukod-companion README.md LICENSE edukod-companion.service | gzip -n -9 >$(DIST_DIR)/$$name.tar.gz; \
		SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) sh scripts/build-deb.sh \
			$$work/edukod-companion "$(VERSION)" $$goarch $(DIST_DIR)/$$name.deb; \
		rm -rf $$work; \
	done
	cd $(DIST_DIR) && sha256sum *.tar.gz *.deb > SHA256SUMS

# Release jobs must set MINISIGN_SECRET_KEY to a protected key file. The
# matching public key is published separately by the fleet operator.
sign: dist
	test -n "$(MINISIGN_SECRET_KEY)"
	command -v minisign >/dev/null
	minisign -Sm $(DIST_DIR)/SHA256SUMS -s "$(MINISIGN_SECRET_KEY)" -W

deb: dist
