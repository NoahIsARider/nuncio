SHELL := /bin/sh

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: all build test vet release clean

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/nuncio ./cmd/nuncio

test:
	go test ./... -count=1

vet:
	go vet ./...

# Produces exactly the asset names install.sh expects, plus checksums.txt.
release:
	rm -rf dist
	mkdir -p dist
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=nuncio_$${os}_$${arch}; \
		ext=; [ "$$os" = windows ] && ext=.exe; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "$(LDFLAGS)" -o dist/$$name/nuncio$$ext ./cmd/nuncio || exit 1; \
		tar -C dist/$$name -czf dist/$$name.tar.gz . || exit 1; \
		rm -rf dist/$$name; \
	done
	cd dist && sha256sum *.tar.gz > checksums.txt

clean:
	rm -rf bin dist
