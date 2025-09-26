APP_NAME=temperature-exporter
PKG=github.com/Tutanka01/Temperature-Exporter-Proxmox/cmd/temperature-exporter
VERSION?=0.1.0
COMMIT:=$(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
DATE:=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS=-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build clean test run

build:
	GOFLAGS="-trimpath" CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/$(APP_NAME) $(PKG)

run:
	./bin/$(APP_NAME)

clean:
	rm -rf bin
