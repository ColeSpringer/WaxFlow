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
PUBLIC_PKGS := . ./waxerr ./audio ./dsp/... ./codec/... ./container/... ./cue ./m3u8 ./format ./source ./server ./client

.PHONY: build test test-race test-cli test-oracle test-example test-386 vet fmt fmt-check depcheck check docker clean verify-vectors goldens bench encoder-quality encoder-quality-mp3 encoder-quality-aac encoder-quality-heaac encoder-quality-heaacv2 encoder-quality-vorbis encoder-quality-opus encoder-quality-opus-speech fuzz opus-tools ape-tools mpc-tools client-e2e apple-smoke hls-e2e soak

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

# The GOOS=windows pass compiles every test file as Windows would. The
# cross-compile job's go build skips test files and the Windows job tests only
# ./source/, so without it a call into a unix-only test helper breaks Windows
# unseen. The -tags passes type-check the build-tagged table extractor, which
# nothing else in the tree compiles. Without them a drifted pin or a broken
# parser surfaces only when someone attempts the audit THIRD-PARTY-NOTICES
# promises.
vet:
	go vet ./...
	GOOS=windows go vet ./...
	go vet -tags wmatablesgen ./codec/wma/
	go vet -tags wmaprotablesgen ./codec/wmapro/
	go vet -tags wmavoicetablesgen ./codec/wmavoice/
	go vet -tags mpcfixtures ./codec/musepack/

# The 32-bit pass. Go's int is 32 bits there, so a constant or a conversion
# that is fine here stops compiling or silently wraps: a uint32 field read into
# an int, a 2^32 ceiling written as an untyped constant, a test value built
# from MaxInt64. The whole tree stopped building for 386 without anyone
# noticing, which is what this exists to prevent. vet covers every module and
# is the fast half; the test run is the slow half and there is no race
# detector on 386.
test-386:
	GOARCH=386 go vet ./...
	cd cli && GOARCH=386 go vet ./...
	cd oracletest && GOARCH=386 go vet ./...
	cd examples/catalogcli && GOARCH=386 go vet ./...
	GOARCH=386 go test -timeout 30m ./...
	cd cli && GOARCH=386 go test -timeout 10m ./...
	cd oracletest && GOARCH=386 go test -timeout 10m ./...

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
# pass. Override the per-target budget with FUZZTIME (CI uses 45s, nightly
# 20m, each split across parallel jobs with FUZZ_SHARD=k/N, which the script
# reads from the environment).
FUZZTIME ?= 30s
fuzz:
	./scripts/fuzz.sh $(FUZZTIME)

# Regenerate the golden files: the muxers' bytes, and the server's two tables
# (the cache keys it builds, what it answers to an encoder knob). Review the
# diff before committing.
goldens:
	go test -run TestGolden ./container/riff ./container/aiff ./container/flacn ./container/mpa \
		./container/mka ./container/mp4 ./container/ogg ./container/wv ./container/apen ./container/adts -update
	go test -run TestGoldenSegments ./tests -update
	cd oracletest && go test -run TestGoldenM4BChapters . -update
	go test -run 'TestGolden(CacheKeys|KnobAnswers)$$' ./server -update

# Decode/encode throughput; the x-realtime metric is judged against the
# per-codec floors in docs/quality-gates.md.
bench:
	go test -run '^$$' -bench . -benchtime 2s ./...

# The reference tools (opus-tools, ape-tools, mpc-tools) are built by
# tools.mk, kept apart so that each build's stamp can hash its recipes
# without the rest of this file.
include tools.mk

# Encoder-quality gates: encode a corpus with our lossy encoders and the
# reference baselines, score both (ODG-proxy vs Shine for MP3, vs ffmpeg's
# native aac for AAC and vs libvorbis for Vorbis; reference opus_compare vs
# libopus for Opus, on the music corpus at 96/128/160k stereo and the TSP
# speech corpus at 24/32/48k mono), enforce the docs/quality-gates.md gates,
# and publish HTML reports. MP3 requires ffmpeg with libshine, AAC plain
# ffmpeg, Vorbis ffmpeg with libvorbis; Opus requires `make opus-tools` and
# the fetched corpora (`make verify-vectors`). Override the output paths
# with QUALITY_REPORT / AAC_QUALITY_REPORT / HEAAC_QUALITY_REPORT /
# HEAACV2_QUALITY_REPORT / VORBIS_QUALITY_REPORT / OPUS_QUALITY_REPORT /
# OPUS_SPEECH_QUALITY_REPORT. go test runs the gates from tests/, so
# QUALITY_ABSPATH pins each path to the directory make runs in, quoted and
# with any spaces carried through abspath (which splits on them) as a
# placeholder. Each gate is its own target: `make -k encoder-quality`, as
# the nightly runs it, carries on past a failing gate so the later ones
# still report.
QUALITY_REPORT ?= quality-report.html
AAC_QUALITY_REPORT ?= aac-quality-report.html
HEAAC_QUALITY_REPORT ?= heaac-quality-report.html
HEAACV2_QUALITY_REPORT ?= heaacv2-quality-report.html
VORBIS_QUALITY_REPORT ?= vorbis-quality-report.html
OPUS_QUALITY_REPORT ?= opus-quality-report.html
OPUS_SPEECH_QUALITY_REPORT ?= opus-speech-quality-report.html
QUALITY_EMPTY :=
QUALITY_SPACE := $(QUALITY_EMPTY) $(QUALITY_EMPTY)
QUALITY_ABSPATH = "$(subst |,$(QUALITY_SPACE),$(abspath $(subst $(QUALITY_SPACE),|,$(1))))"
encoder-quality: encoder-quality-mp3 encoder-quality-aac encoder-quality-heaac encoder-quality-heaacv2 \
	encoder-quality-vorbis encoder-quality-opus encoder-quality-opus-speech
encoder-quality-mp3:
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_REQUIRE_SHINE=1 WAXFLOW_QUALITY_REPORT=$(call QUALITY_ABSPATH,$(QUALITY_REPORT)) \
		go test -run TestMP3EncoderQuality -count=1 -v ./tests
encoder-quality-aac:
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_QUALITY_REPORT=$(call QUALITY_ABSPATH,$(AAC_QUALITY_REPORT)) \
		go test -run TestAACEncoderQuality -count=1 -v ./tests
encoder-quality-heaac:
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_QUALITY_REPORT=$(call QUALITY_ABSPATH,$(HEAAC_QUALITY_REPORT)) \
		go test -run TestHEAACEncoderQuality -count=1 -v ./tests
encoder-quality-heaacv2:
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_QUALITY_REPORT=$(call QUALITY_ABSPATH,$(HEAACV2_QUALITY_REPORT)) \
		go test -run TestHEAACv2EncoderQuality -count=1 -v ./tests
encoder-quality-vorbis:
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_FFMPEG=1 WAXFLOW_QUALITY_REPORT=$(call QUALITY_ABSPATH,$(VORBIS_QUALITY_REPORT)) \
		go test -run TestVorbisEncoderQuality -count=1 -v ./tests
encoder-quality-opus:
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_OPUS_TOOLS=1 WAXFLOW_REQUIRE_VECTORS=1 WAXFLOW_QUALITY_REPORT=$(call QUALITY_ABSPATH,$(OPUS_QUALITY_REPORT)) \
		go test -run 'TestOpusEncoderQuality$$' -count=1 -timeout 30m -v ./tests
encoder-quality-opus-speech:
	WAXFLOW_ENCODER_QUALITY=1 WAXFLOW_REQUIRE_OPUS_TOOLS=1 WAXFLOW_REQUIRE_VECTORS=1 WAXFLOW_QUALITY_REPORT=$(call QUALITY_ABSPATH,$(OPUS_SPEECH_QUALITY_REPORT)) \
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

# The AVPlayer spike (macOS only): does AVPlayer advance on a machine with no
# audio output, and does App Transport Security block plain-http loopback from
# a command-line Swift binary? Two answers, one run. It is a spike ahead of the
# Apple cells of docs/client-matrix.md, not a test, so it is not in `check` and
# runs on demand, on a Mac: `make apple-smoke`. No workflow runs it.
apple-smoke: build
	WAXFLOW_BIN=./bin/waxflow node scripts/apple-e2e/smoke.mjs

# The old name, kept as an alias.
hls-e2e: client-e2e

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg GO_VERSION=$$(cat .go-version) -t waxflow:$(VERSION) .

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
