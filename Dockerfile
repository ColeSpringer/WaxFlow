# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
# MAIN_PKG lets the future WaxBin resolver flavor build its "-waxbin"
# main from the same Dockerfile.
ARG MAIN_PKG=./cmd/waxflow
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/waxflow ${MAIN_PKG}

# Fully static binary -> distroless static: the first Wax image with no OS
# layer. No tini: waxflow spawns no subprocesses (no ffmpeg at runtime, ever).
FROM gcr.io/distroless/static-debian12
ARG VERSION=dev
LABEL org.opencontainers.image.title="waxflow" \
      org.opencontainers.image.description="Pure-Go audio transcoding service for the Wax family" \
      org.opencontainers.image.url="https://github.com/colespringer/waxflow" \
      org.opencontainers.image.source="https://github.com/colespringer/waxflow" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/waxflow /usr/local/bin/waxflow
USER 10001:10001
EXPOSE 4418
# `waxflow ping` reads WAXFLOW_ADDR like the server and rewrites wildcard
# binds to loopback, so one env var drives both.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/waxflow", "ping"]
ENTRYPOINT ["/usr/local/bin/waxflow"]
CMD ["server"]
