BINARY  := mxread
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath
DIST    := dist
MAX_SIZE_MB := 15

export CGO_ENABLED=0

.PHONY: build build-arm64 build-amd64 test lint release clean size

build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/mxread

build-arm64:
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64 ./cmd/mxread

build-amd64:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64 ./cmd/mxread

test:
	go test ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "staticcheck not installed, skipped"; fi

# Fails if a release binary exceeds the SPEC §8 size budget.
size: build-arm64 build-amd64
	@for f in $(DIST)/$(BINARY)-linux-*; do \
		s=$$(stat -c %s $$f); echo "$$f: $$((s/1024/1024)) MB"; \
		[ $$s -le $$(($(MAX_SIZE_MB)*1024*1024)) ] || (echo "$$f exceeds $(MAX_SIZE_MB) MB"; exit 1); \
	done

release: clean size
	cd $(DIST) && sha256sum $(BINARY)-linux-* > sha256sums.txt
	@cat $(DIST)/sha256sums.txt

clean:
	rm -rf $(DIST) $(BINARY)
