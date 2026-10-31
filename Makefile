BINARY  := mcp-matrix-ro
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath
DIST    := dist
MAX_SIZE_MB := 15
IMAGE   ?= ghcr.io/roiarthurb/mcp-matrix-ro

# Release targets: GOOS/GOARCH[/GOARM]
PLATFORMS := linux/amd64 linux/arm64 linux/arm/7 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64 freebsd/amd64

export CGO_ENABLED=0

.PHONY: build build-arm64 build-amd64 dist test lint release size docker clean

build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/mcp-matrix-ro

build-arm64:
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64 ./cmd/mcp-matrix-ro

build-amd64:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64 ./cmd/mcp-matrix-ro

# Cross-compile every release platform into dist/.
dist:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$$(echo $$p | cut -d/ -f1); arch=$$(echo $$p | cut -d/ -f2); arm=$$(echo $$p | cut -d/ -s -f3); \
		name=$(BINARY)-$$os-$$arch$${arm:+v$$arm}; [ $$os = windows ] && name=$$name.exe; \
		echo "building $$name"; \
		GOOS=$$os GOARCH=$$arch GOARM=$$arm go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$$name ./cmd/mcp-matrix-ro || exit 1; \
	done

test:
	go test ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "staticcheck not installed, skipped"; fi

# Fails if any binary in dist/ exceeds the SPEC §8 size budget.
size:
	@for f in $(DIST)/$(BINARY)-*; do \
		s=$$(stat -c %s $$f); echo "$$f: $$((s/1024/1024)) MB"; \
		[ $$s -le $$(($(MAX_SIZE_MB)*1024*1024)) ] || (echo "$$f exceeds $(MAX_SIZE_MB) MB"; exit 1); \
	done

release: clean dist size
	cd $(DIST) && sha256sum $(BINARY)-* > sha256sums.txt
	@cat $(DIST)/sha256sums.txt

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

clean:
	rm -rf $(DIST) $(BINARY)
