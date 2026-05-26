.PHONY: test lint govulncheck build rpm clean

BINARY := servermaster
DIST   := dist
UNIT   := servermaster.service

# RPM version: the latest git tag with the leading "v" stripped, or 0.0.0 when
# the tree has no tags. Override with: make rpm VERSION=1.2.3
GIT_VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//')
VERSION     ?= $(if $(GIT_VERSION),$(GIT_VERSION),0.0.0)
GO_VERSION  := $(shell sed -n 's/^go //p' go.mod | head -1)

# Target architecture. GOARCH builds the binary; RPM_ARCH tags the package.
GOARCH   ?= $(shell go env GOARCH)
RPM_ARCH := $(if $(filter arm64,$(GOARCH)),aarch64,$(if $(filter amd64,$(GOARCH)),x86_64,$(GOARCH)))

test:
	go test ./... -coverpkg=./... -coverprofile=coverage.out
	@go tool cover -func=coverage.out | tail -1

lint:
	GOTOOLCHAIN=go$(GO_VERSION) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run

govulncheck:
	./scripts/govulncheck-gate.sh

# Static (CGO-free) build so the binary runs on minimal edge images without a
# glibc version dependency. Honors GOARCH (default: host architecture).
build:
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOARCH=$(GOARCH) go build -trimpath -o $(DIST)/$(BINARY) .

# Package the built binary and the systemd unit into an RPM with the pure-Go
# cmd/mkrpm, so no rpmbuild or spec file is required on the build host. GOARCH is
# unset for the tool so it runs on the host even during a cross-build; the target
# arch is tagged explicitly via -arch.
rpm: build
	env -u GOARCH go run ./cmd/mkrpm \
		-version "$(VERSION)" \
		-arch "$(RPM_ARCH)" \
		-binary "$(DIST)/$(BINARY)" \
		-unit "$(UNIT)" \
		-license LICENSE \
		-out "$(DIST)/$(BINARY)-$(VERSION)-1.$(RPM_ARCH).rpm"

clean:
	rm -rf $(DIST) coverage.out
