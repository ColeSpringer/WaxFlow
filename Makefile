MODULE  := github.com/colespringer/waxflow
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The public, stdlib-only tree (ADR-0002). Grows as public packages land; every
# new public package MUST be added here. depcheck is the CI gate behind
# the "stdlib-only codecs" promise.
PUBLIC_PKGS := . ./waxerr ./audio ./dsp/... ./codec/... ./container/... ./format ./source ./server ./client

.PHONY: build test vet fmt fmt-check depcheck check docker clean verify-vectors goldens bench encoder-quality fuzz opus-tools

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/waxflow ./cmd/waxflow

test:
	go test -race -timeout 30m ./...

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

# Run every Fuzz* target and classify findings (scripts/fuzz.sh). Only a real
# crasher fails; Go's end-of-run "context deadline exceeded" is treated as a
# pass. Override the per-target budget with FUZZTIME (CI uses 2m/20m).
FUZZTIME ?= 30s
fuzz:
	./scripts/fuzz.sh $(FUZZTIME)

# Regenerate muxer golden files. Review the diff before committing.
goldens:
	go test -run TestGoldenMuxOutputs ./container/riff ./container/aiff ./container/flacn ./container/mpa -update

# Decode/encode throughput; the x-realtime metric is judged against the
# per-codec floors in docs/quality-gates.md (nightly benchstat ratchets
# land at M19).
bench:
	go test -run '^$$' -bench . -benchtime 2s ./...

# Build the reference libopus tools (opus_demo + opus_compare), the Opus
# encoder-quality oracle, from the pinned source tarball into testdata/tools
# (CI-cached, never committed). Requires a C toolchain; like ffmpeg this is a
# test-time oracle only, never a runtime dependency. Tests that need the
# tools self-skip until this has run; WAXFLOW_REQUIRE_OPUS_TOOLS=1 escalates.
OPUS_TOOLS_VERSION := opus-1.6.1
OPUS_TOOLS_DIR := testdata/tools/$(OPUS_TOOLS_VERSION)
opus-tools:
	go run ./internal/testutil/cmd/vectorfetch opus/$(OPUS_TOOLS_VERSION).tar.gz
	rm -rf testdata/tools/opus-build
	mkdir -p testdata/tools/opus-build
	tar -xzf testdata/vectors/opus/$(OPUS_TOOLS_VERSION).tar.gz -C testdata/tools/opus-build --strip-components=1
	cd testdata/tools/opus-build && ./configure --disable-shared --disable-doc >/dev/null && $(MAKE) -s opus_demo opus_compare >/dev/null
	mkdir -p $(OPUS_TOOLS_DIR)
	cp testdata/tools/opus-build/opus_demo testdata/tools/opus-build/opus_compare $(OPUS_TOOLS_DIR)/
	rm -rf testdata/tools/opus-build
	@echo "built $(OPUS_TOOLS_DIR)/{opus_demo,opus_compare}"

# Encoder-quality gates: encode a corpus with our lossy encoders and the
# reference baselines, score both (ODG-proxy vs Shine for MP3; reference
# opus_compare vs libopus for Opus), enforce the docs/quality-gates.md parity
# gates, and publish HTML reports. MP3 requires ffmpeg with libshine; Opus
# requires `make opus-tools` and the fetched corpus (`make verify-vectors`).
# Override the output paths with QUALITY_REPORT / OPUS_QUALITY_REPORT.
QUALITY_REPORT ?= quality-report.html
OPUS_QUALITY_REPORT ?= opus-quality-report.html
encoder-quality:
	WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_REQUIRE_SHINE=1 WAXFLOW_QUALITY_REPORT=$(QUALITY_REPORT) \
		go test -run TestMP3EncoderQuality -count=1 -v .
	WAXFLOW_REQUIRE_OPUS_TOOLS=1 WAXFLOW_REQUIRE_VECTORS=1 WAXFLOW_QUALITY_REPORT=$(OPUS_QUALITY_REPORT) \
		go test -run TestOpusEncoderQuality -count=1 -timeout 30m -v .

docker:
	docker build --build-arg VERSION=$(VERSION) -t waxflow:$(VERSION) .

clean:
	rm -rf bin dist
