MODULE  := github.com/colespringer/waxflow
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The public, stdlib-only tree (ADR-0002). Grows as public packages land; every
# new public package MUST be added here. depcheck is the CI gate behind
# the "stdlib-only codecs" promise.
PUBLIC_PKGS := . ./waxerr ./audio ./dsp/... ./codec/... ./container/... ./format ./source ./server ./client

.PHONY: build build-waxbin test test-race test-cli test-resolver test-oracle vet fmt fmt-check depcheck check docker docker-waxbin clean verify-vectors goldens bench encoder-quality fuzz opus-tools client-e2e hls-e2e soak

build:
	cd cli && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/waxflow ./cmd/waxflow

# The WaxBin resolver flavor: the identical CLI with pid:<ULID> source
# support, built from the nested resolver/ module (which is what keeps
# WaxBin's SQLite dependency out of the main module's tree).
build-waxbin:
	cd resolver && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/waxflow-waxbin ./cmd/waxflow

# The default loop: the whole suite without the race detector. The codecs and
# DSP are single-goroutine numeric code, so -race there is a many-fold
# slowdown; this pass is where the heavy conformance suites (FLAC, Opus) and
# the encoder-quality gates run. The gates self-skip unless WAXFLOW_ENCODER_-
# QUALITY=1 (they belong to `make encoder-quality`).
test:
	go test -timeout 30m ./...

# The race pass: the whole tree under the detector, so any data race anywhere
# is caught. It stays fast because the two largest pure-numeric FLAC suites and
# the Opus conformance suite self-skip under -race (they have no goroutines and
# run in full in the non-race pass above). Concurrency lives in the server and
# internal packages; the engine's shared plans cache is exercised concurrently
# by the server tests, which run here under the detector.
test-race:
	go test -race -timeout 30m ./...

# The nested modules: ./... at the root stops at their go.mod
# boundaries, so each gets its own vet+test here and a dedicated CI
# step. cli is the cobra/waxlabel binary module, resolver the WaxBin
# flavor (race included: the poll loop is concurrent), oracletest the
# third-party-oracle tests (waxlabel round trips, go-mp3 differential)
# that keep the root module's require block empty.
test-cli:
	cd cli && go vet ./... && go test -race -timeout 10m ./...

test-resolver:
	cd resolver && go vet ./... && go test -race -timeout 10m ./...

test-oracle:
	cd oracletest && go vet ./... && go test -timeout 10m ./...

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

check: fmt-check vet test test-race test-cli test-resolver test-oracle depcheck

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
	go test -run TestGoldenMuxOutputs ./container/riff ./container/aiff ./container/flacn ./container/mpa ./container/mka -update
	go test -run TestGoldenSegments ./tests -update
	cd oracletest && go test -run TestGoldenM4BChapters . -update

# Decode/encode throughput; the x-realtime metric is judged against the
# per-codec floors in docs/quality-gates.md.
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
# reference baselines, score both (ODG-proxy vs Shine for MP3 and vs
# ffmpeg's native aac for AAC; reference opus_compare vs libopus for Opus,
# on the music corpus at 96/128/160k stereo and the TSP speech corpus at
# 24/32/48k mono), enforce the docs/quality-gates.md gates, and publish
# HTML reports. MP3 requires ffmpeg with libshine, AAC plain ffmpeg; Opus
# requires `make opus-tools` and the fetched corpora (`make
# verify-vectors`). Override the output paths with QUALITY_REPORT /
# AAC_QUALITY_REPORT / OPUS_QUALITY_REPORT / OPUS_SPEECH_QUALITY_REPORT.
QUALITY_REPORT ?= quality-report.html
AAC_QUALITY_REPORT ?= aac-quality-report.html
OPUS_QUALITY_REPORT ?= opus-quality-report.html
OPUS_SPEECH_QUALITY_REPORT ?= opus-speech-quality-report.html
encoder-quality:
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_REQUIRE_SHINE=1 WAXFLOW_QUALITY_REPORT=$(QUALITY_REPORT) \
		go test -run TestMP3EncoderQuality -count=1 -v ./tests
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_QUALITY_REPORT=$(AAC_QUALITY_REPORT) \
		go test -run TestAACEncoderQuality -count=1 -v ./tests
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_OPUS_TOOLS=1 WAXFLOW_REQUIRE_VECTORS=1 WAXFLOW_QUALITY_REPORT=$(OPUS_QUALITY_REPORT) \
		go test -run 'TestOpusEncoderQuality$$' -count=1 -timeout 30m -v ./tests
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_OPUS_TOOLS=1 WAXFLOW_REQUIRE_VECTORS=1 WAXFLOW_QUALITY_REPORT=$(OPUS_SPEECH_QUALITY_REPORT) \
		go test -run TestOpusSpeechEncoderQuality -count=1 -timeout 30m -v ./tests

# Browser client-matrix e2e: a real daemon, the committed /demo page,
# and headless Chromium via Playwright (scripts/client-e2e.mjs) driving
# every browser cell of docs/client-matrix.md: HLS variants through
# hls.js plus progressive and direct-play streams through <audio>. This
# run is the automated basis behind the hls-js /caps profile. Gated
# tooling: needs Node plus `npm install playwright && npx playwright
# install chromium`.
client-e2e: build
	WAXFLOW_BIN=./bin/waxflow node scripts/client-e2e.mjs

# The old name, kept as an alias.
hls-e2e: client-e2e

docker:
	docker build --build-arg VERSION=$(VERSION) -t waxflow:$(VERSION) .

docker-waxbin:
	docker build --build-arg VERSION=$(VERSION) --build-arg MAIN_PKG=./resolver/cmd/waxflow \
		-t waxflow:$(VERSION)-waxbin .

clean:
	rm -rf bin dist

# The v1.0 hardening harnesses at nightly scale: a long streaming soak
# with the goroutine/heap leak watch, a sustained mixed-traffic load
# test, and the TTFA/seek percentiles with the p95 targets enforced.
# The same tests run for seconds inside `make test`; this target is the
# real thing (also the nightly soak job).
soak:
	WAXFLOW_SOAK_DURATION=$${WAXFLOW_SOAK_DURATION:-30m} \
	WAXFLOW_LOAD_DURATION=$${WAXFLOW_LOAD_DURATION:-5m} \
	WAXFLOW_PERF=1 WAXFLOW_PERF_ITERS=$${WAXFLOW_PERF_ITERS:-50} \
		go test -run 'TestStreamingSoak|TestLoadMixedTraffic|TestTTFAPercentiles' -count=1 -timeout 90m -v ./server
