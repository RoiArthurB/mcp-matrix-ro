# syntax=docker/dockerfile:1
# Multi-arch image. The Go build runs on the build machine's platform and
# cross-compiles, so no emulation is needed.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
RUN GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mcp-matrix-ro ./cmd/mcp-matrix-ro \
 && mkdir -p /out/state

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="mcp-matrix-ro" \
      org.opencontainers.image.description="Read-only Matrix MCP server and digest CLI" \
      org.opencontainers.image.source="https://github.com/RoiArthurB/mcp-matrix-ro" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/mcp-matrix-ro /usr/local/bin/mcp-matrix-ro
# Cache dir owned by the distroless nonroot user (65532).
COPY --from=build --chown=65532:65532 /out/state /state
ENV MCP_MATRIX_RO_STATE_DIR=/state
VOLUME ["/state"]
ENTRYPOINT ["/usr/local/bin/mcp-matrix-ro"]
CMD ["serve"]
