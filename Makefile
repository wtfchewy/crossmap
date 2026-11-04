# Build and install crossmap from source. Needs Go.
#
#   make && sudo make install     the binary, /usr/local/bin/crossmap
#   sudo make uninstall
#
# crossmap talks to macOS frameworks (usbmuxd, dns-sd, CoreDevice), so it is
# only useful on macOS. It still builds on Linux so CI can vet it there.

PREFIX  ?= /usr/local
DESTDIR ?=
BINDIR  := $(DESTDIR)$(PREFIX)/bin
VERSION ?= $(or $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//'),dev)
GO      ?= go
export GOTOOLCHAIN ?= auto
LDFLAGS := -s -w -X main.version=$(VERSION)

SOURCES := go.mod go.sum $(shell find cmd internal -name '*.go' 2>/dev/null)

.PHONY: all install uninstall test lint clean

all: crossmap

# A file target, so `sudo make install` uses what you built instead of
# rebuilding as root.
crossmap: $(SOURCES)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o crossmap ./cmd/crossmap

install: crossmap
	install -d $(BINDIR)
	install -m755 crossmap $(BINDIR)/crossmap

uninstall:
	rm -f $(BINDIR)/crossmap

# Tests that need a real usbmuxd, dns-sd or an attached device skip
# themselves, so this runs anywhere.
test:
	$(GO) test -count=1 -timeout 10m ./...

lint:
	$(GO) vet ./...
	gofmt -l .

clean:
	rm -f crossmap
