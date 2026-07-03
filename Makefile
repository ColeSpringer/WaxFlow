MODULE  := github.com/colespringer/waxflow
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The public, stdlib-only tree (ADR-0002). Grows as public packages land; every
# new public package MUST be added here. depcheck is the CI gate behind
# the "stdlib-only codecs" promise.
PUBLIC_PKGS := . ./waxerr ./audio ./dsp/... ./codec/... ./container/... ./format ./source ./server ./client

.PHONY: build test vet fmt fmt-check depcheck check docker clean verify-vectors goldens bench

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/waxflow ./cmd/waxflow

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then \
		echo "gofmt needed on:"; echo "$$out"; exit 1; fi

depcheck:
	@bad="$$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' $(PUBLIC_PKGS) \
		| grep -v '^$$' | grep -v '^$(MODULE)' || true)"; \
	if [ -n "$$bad" ]; then \
		echo "depcheck FAILED: non-stdlib imports in the public tree:"; \
		echo "$$bad"; exit 1; fi; \
	echo "depcheck ok: public tree ($(PUBLIC_PKGS)) is stdlib-only"

check: fmt-check vet test depcheck

# Fetch the SHA-256-pinned conformance vectors into testdata/vectors
# (CI-cached, never committed). Vector-gated tests self-skip until run;
# WAXFLOW_REQUIRE_VECTORS=1 escalates skips to failures.
verify-vectors:
	go run ./internal/testutil/cmd/vectorfetch

# Regenerate muxer golden files. Review the diff before committing.
goldens:
	go test -run TestGoldenMuxOutputs ./container/riff ./container/aiff -update

# Decode/encode throughput; the x-realtime metric is judged against the
# per-codec floors in docs/quality-gates.md (nightly benchstat ratchets
# land at M19).
bench:
	go test -run '^$$' -bench . -benchtime 2s ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t waxflow:$(VERSION) .

clean:
	rm -rf bin dist
