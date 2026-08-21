# Every recipe here is POSIX. Make resolves SHELL to a full path when it finds
# a shell on PATH (Git Bash, msys2) and leaves a bare name when it does not, in
# which case it falls back to cmd.exe and the recipes die on the first POSIX
# construct. Only that case is repointed, at the shell Git for Windows ships.
# It has to be the launcher in bin/: usr/bin/sh.exe runs without the POSIX
# utilities on PATH, so depcheck's grep pipeline exits 0 having run no grep.
# The ? stands in for a space in the wildcards, since make splits their
# argument on spaces and both "Program Files" and a profile name can hold one.
ifeq ($(OS),Windows_NT)
ifeq ($(findstring /,$(SHELL))$(findstring \,$(SHELL)),)
WIN_EMPTY :=
WIN_SPACE := $(WIN_EMPTY) $(WIN_EMPTY)
WIN_LOCALGIT := $(subst \,/,$(LOCALAPPDATA))/Programs/Git
ifneq ($(wildcard C:/Program?Files/Git/bin/sh.exe),)
SHELL := C:/Program Files/Git/bin/sh.exe
else ifneq ($(wildcard C:/Program?Files?(x86)/Git/bin/sh.exe),)
SHELL := C:/Program Files (x86)/Git/bin/sh.exe
else ifneq ($(wildcard $(subst $(WIN_SPACE),?,$(WIN_LOCALGIT))/bin/sh.exe),)
SHELL := $(WIN_LOCALGIT)/bin/sh.exe
else
$(error no POSIX shell found; install Git for Windows, run make from Git Bash, or pass SHELL=/path/to/sh.exe)
endif
endif
endif

MODULE  := github.com/colespringer/waxflow
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The public, stdlib-only tree (ADR-0002). Grows as public packages land; every
# new public package MUST be added here. depcheck is the CI gate behind
# the "stdlib-only codecs" promise.
PUBLIC_PKGS := . ./waxerr ./audio ./dsp/... ./codec/... ./container/... ./format ./source ./server ./client

.PHONY: build test test-race test-cli test-oracle test-example vet fmt fmt-check depcheck check docker clean verify-vectors goldens bench encoder-quality fuzz opus-tools ape-tools client-e2e hls-e2e soak

build:
	cd cli && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/waxflow ./cmd/waxflow

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
# step. cli is the cobra/waxlabel binary module, oracletest the
# third-party-oracle tests (waxlabel round trips, go-mp3 differential)
# that keep the root module's require block empty.
test-cli:
	cd cli && go vet ./... && go test -race -timeout 10m ./...

test-oracle:
	cd oracletest && go vet ./... && go test -timeout 10m ./...

# The out-of-prefix canary, and the worked example a consumer copies.
# Its module path is deliberately not under $(MODULE)/, so Go's internal
# rule applies to it exactly as to any third-party module: it builds a
# waxflow CLI through the cli.Flavor seam, and if that seam ever widens
# back to an internal type this is the only thing in the tree that can
# fail. The compile is the check; the test runs because an example that
# has never executed is a poor reference.
test-example:
	cd examples/catalogcli && go vet ./... && go test -timeout 10m ./...

# The second pass type-checks the build-tagged table extractor, which nothing
# else in the tree compiles. Without it a drifted pin or a broken parser
# surfaces only when someone attempts the audit THIRD-PARTY-NOTICES promises.
vet:
	go vet ./...
	go vet -tags wmatablesgen ./codec/wma/

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

check: fmt-check vet test test-race test-cli test-oracle test-example depcheck

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

# Build the reference Monkey's Audio console tool (`mac`) from the pinned SDK
# source into testdata/tools (CI-cached, never committed). It is the only APE
# encoder there is -- ffmpeg decodes the format but does not write it, and no
# distribution packages the tool -- so the APE fixtures and the reference-tool
# differentials come from here. A test-time oracle only, never a runtime
# dependency. Tests that need it self-skip until this has run;
# WAXFLOW_REQUIRE_MAC=1 escalates.
APE_TOOLS_VERSION := mac-13.25
APE_TOOLS_DIR := testdata/tools/$(APE_TOOLS_VERSION)
# The SDK picks its file-I/O backend and its wide-character entry point from a
# platform define, so the build needs one of three spellings. These are
# recursively expanded on purpose: only the ape-tools recipe reads them, and an
# immediate assignment would fork uname on every make invocation.
APE_TOOLS_UNAME = $(shell uname -s)
APE_TOOLS_WINDOWS = $(findstring MINGW,$(APE_TOOLS_UNAME))$(findstring MSYS,$(APE_TOOLS_UNAME))$(findstring CYGWIN,$(APE_TOOLS_UNAME))
APE_TOOLS_CXXFLAGS = $(if $(APE_TOOLS_WINDOWS),-DPLATFORM_WINDOWS -municode,$(if $(findstring Darwin,$(APE_TOOLS_UNAME)),-DPLATFORM_APPLE,-DPLATFORM_LINUX))
APE_TOOLS_IO_SRC = $(if $(APE_TOOLS_WINDOWS),Source/Shared/WinFileIO.cpp,Source/Shared/StdLibFileIO.cpp)
APE_TOOLS_EXE = $(if $(APE_TOOLS_WINDOWS),.exe,)
ape-tools:
	@if [ -x "$(APE_TOOLS_DIR)/mac$(APE_TOOLS_EXE)" ]; then \
		echo "$(APE_TOOLS_DIR)/mac$(APE_TOOLS_EXE) is already built"; exit 0; \
	fi; \
	set -e; \
	go run ./internal/testutil/cmd/vectorfetch ape/MAC_1325_SDK.zip; \
	rm -rf testdata/tools/ape-build; \
	mkdir -p testdata/tools/ape-build $(APE_TOOLS_DIR); \
	( cd testdata/tools/ape-build && unzip -q ../../vectors/ape/MAC_1325_SDK.zip ); \
	( cd testdata/tools/ape-build && $(CXX) -O2 -std=c++11 -w $(APE_TOOLS_CXXFLAGS) \
		-ISource/MACLib -ISource/MACLib/Old -ISource/Shared -IShared -ISource/Console \
		Source/MACLib/*.cpp Source/MACLib/Old/*.cpp \
		Source/Shared/BufferIO.cpp Source/Shared/CharacterHelper.cpp \
		Source/Shared/CircleBuffer.cpp Source/Shared/CPUFeatures.cpp \
		Source/Shared/CRC.cpp Source/Shared/GlobalFunctions.cpp \
		Source/Shared/MemoryIO.cpp Source/Shared/Semaphore.cpp \
		Source/Shared/Thread.cpp Source/Shared/WholeFileIO.cpp $(APE_TOOLS_IO_SRC) \
		Source/Console/Console.cpp -o mac$(APE_TOOLS_EXE) -lpthread ); \
	cp testdata/tools/ape-build/mac$(APE_TOOLS_EXE) $(APE_TOOLS_DIR)/; \
	rm -rf testdata/tools/ape-build; \
	echo "built $(APE_TOOLS_DIR)/mac$(APE_TOOLS_EXE)"

# Encoder-quality gates: encode a corpus with our lossy encoders and the
# reference baselines, score both (ODG-proxy vs Shine for MP3 and vs
# ffmpeg's native aac for AAC; reference opus_compare vs libopus for Opus,
# on the music corpus at 96/128/160k stereo and the TSP speech corpus at
# 24/32/48k mono), enforce the docs/quality-gates.md gates, and publish
# HTML reports. MP3 requires ffmpeg with libshine, AAC plain ffmpeg; Opus
# requires `make opus-tools` and the fetched corpora (`make
# verify-vectors`). Override the output paths with QUALITY_REPORT /
# AAC_QUALITY_REPORT / HEAAC_QUALITY_REPORT / OPUS_QUALITY_REPORT /
# OPUS_SPEECH_QUALITY_REPORT.
QUALITY_REPORT ?= quality-report.html
AAC_QUALITY_REPORT ?= aac-quality-report.html
HEAAC_QUALITY_REPORT ?= heaac-quality-report.html
HEAACV2_QUALITY_REPORT ?= heaacv2-quality-report.html
OPUS_QUALITY_REPORT ?= opus-quality-report.html
OPUS_SPEECH_QUALITY_REPORT ?= opus-speech-quality-report.html
encoder-quality:
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_REQUIRE_SHINE=1 WAXFLOW_QUALITY_REPORT=$(QUALITY_REPORT) \
		go test -run TestMP3EncoderQuality -count=1 -v ./tests
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_QUALITY_REPORT=$(AAC_QUALITY_REPORT) \
		go test -run TestAACEncoderQuality -count=1 -v ./tests
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_QUALITY_REPORT=$(HEAAC_QUALITY_REPORT) \
		go test -run TestHEAACEncoderQuality -count=1 -v ./tests
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_QUALITY_REPORT=$(HEAACV2_QUALITY_REPORT) \
		go test -run TestHEAACv2EncoderQuality -count=1 -v ./tests
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
