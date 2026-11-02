# Works with GNU make on macOS/Linux and in Termux (`pkg install make`).
# All builds are pure Go (CGO_ENABLED=0) so the same code builds natively in
# Termux and cross-compiles from a workstation.

BINARY   := assistant
PKG      := ./cmd/assistant
BIN_DIR  := bin
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS  := -s -w \
            -X personal-assistant/internal/buildinfo.Version=$(VERSION) \
            -X personal-assistant/internal/buildinfo.Commit=$(COMMIT)
GOFLAGS  := -trimpath

# Inside Termux, never let `go` download an upstream toolchain to satisfy the
# go.mod toolchain line: upstream Go binaries cannot resolve DNS on Android
# (no /etc/resolv.conf). Termux's own patched Go is used as-is.
ifneq ($(TERMUX_VERSION)$(wildcard /data/data/com.termux/files/usr/bin/go),)
export GOTOOLCHAIN := local
endif

# Remote Termux target for `make deploy` (SSH over `adb forward tcp:8022 tcp:8022`).
TERMUX_SSH  ?= ssh -p 8022 127.0.0.1
TERMUX_DIR  ?= personal-assistant

.PHONY: all build build-android test test-race vet fmt-check check run clean deploy

all: check build

## build: native build for the current machine (in Termux this is android/arm64)
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(PKG)

## build-android: cross-compile an android/arm64 binary from a workstation.
## WARNING: verified on-device that DNS resolution FAILS in this binary
## (upstream Go falls back to [::1]:53; Android has no /etc/resolv.conf).
## Use it only for local smoke tests. Production builds: `make build` in Termux,
## whose patched Go toolchain reads $PREFIX/etc/resolv.conf.
build-android:
	CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" \
		-o $(BIN_DIR)/$(BINARY)-android-arm64 $(PKG)

## test: unit tests (race detector is not available on android/arm64)
test:
	go test -count=1 -cover ./...

## test-race: unit tests with the race detector (workstation only)
test-race:
	go test -race -count=1 -cover ./...

vet:
	go vet ./...

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## check: formatting, vet and tests
check: fmt-check vet test

## run: build and run in the foreground with the example config
run: build
	./$(BIN_DIR)/$(BINARY) -config configs/config.example.json

clean:
	rm -rf $(BIN_DIR)

## deploy: copy the source tree to Termux over SSH (excludes runtime state)
deploy:
	tar --exclude=./bin --exclude=./data --exclude=./logs --exclude=./models \
	    --exclude=./run --exclude=./.git -czf - . | \
	  $(TERMUX_SSH) 'mkdir -p $(TERMUX_DIR) && tar -xzf - -C $(TERMUX_DIR)'
