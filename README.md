# WaxFlow

Self-hosted, **pure-Go**, on-the-fly audio transcoding for the Wax family
(WaxTap, WaxBin, WaxLabel, WaxSeal): request -> decode -> DSP -> encode ->
stream, tuned for time-to-first-audio, sample-exact seeking, and flaky
mobile networks. No ffmpeg at runtime, ever (`CGO_ENABLED=0`).

The codecs (Opus, MP3, AAC-LC, FLAC, ALAC, and WAV encoders, plus a wider
decoder set) are written from scratch for Go 1.26 and published as public,
**stdlib-only** packages under this module, CI-enforced by `make depcheck`,
so anyone can import them.

## Status

**v1.0 feature-complete.** Everything below is tested, capability-gated
(`/caps` never advertises what does not work), and held to the pinned
gates in [docs/quality-gates.md](docs/quality-gates.md).

- **Encoders** (all from scratch, stdlib-only): Opus (full SILK, hybrid,
  and CELT with the tonality analyser; at quality parity with libopus
  on the reference `opus_compare` metric across music and speech
  corpora), MP3 (psychoacoustic model, joint stereo, CBR and VBR, LAME
  gapless tag), AAC-LC (window switching, TNS, M/S, two-loop
  quantization; at parity with ffmpeg's native encoder on the ODG-proxy
  gate), FLAC (levels 0-8, smaller than `flac -5` at level 5), ALAC
  (bit-exact round trip), and WAV/AIFF PCM.
- **Decoders / inputs**: FLAC (bit-exact on the IETF suite), WAV, AIFF,
  MP3, AAC-LC and ALAC in MP4/M4A/M4B, ADTS, Opus (all RFC 6716/8251
  conformance vectors pass), Vorbis, Ogg, Matroska/WebM. Sample-exact
  seeking everywhere, gapless honored per format (LAME tag, iTunSMPB,
  edit lists, Ogg pre-skip/end-trim, Matroska CodecDelay).
- **DSP**: Kaiser windowed-sinc resampling (`hq`/`fast`), BS.775
  downmix, gain with true-peak limiting, TPDF and shaped dither, EBU
  R128 / BS.1770-4 loudness (differential-verified against ffmpeg).
- **Service**: progressive streaming with a direct-play/transmux/
  transcode ladder, sample-exact `t=` seeks, HMAC-signed URLs pinned to
  source identity, a write-through cache with read-behind delivery and
  full ranges on completed entries, HLS (CMAF/fMP4, stateless signed
  URLs, bitrate ladders, byte-identical segment regeneration), async
  jobs with restart safety (transcode, analyze, and the gapless
  merge/split pair: a lossless split rejoins bit for bit, and a split
  takes a CUE sheet or sample cut points), uploads,
  loudness analysis with ReplayGain
  tagging, metadata passthrough (tags, chapters, cover art, lyrics),
  admission control, Prometheus metrics, named client delivery profiles
  in `/caps`, and a full API contract in [docs/api.md](docs/api.md).
- **Structure**: the root module is the importable, dependency-free
  audio library; its `go.mod` has an **empty require block**, so
  importing `codec/flac` (or any public package) pulls in nothing.
  The CLI/daemon binary lives in the nested `cli/` module (cobra +
  waxlabel), the WaxBin flavor in `resolver/`, and the tests that need
  third-party oracles in `oracletest/`.

## Performance

Measured by the committed harnesses on the reference dev box (12-core
x86-64, Linux, loopback HTTP; `server/load_soak_test.go`, re-run per
release with `make soak`):

- **Time to first audio** (`TestTTFAPercentiles`, n=50): cold transcode
  (also the cost of every `t=` seek, since each offset is its own cache
  entry) p50 0.6 ms / p95 0.9 ms; warm (completed cache entry) p50
  0.21 ms / p95 0.24 ms; seek into a 60 s FLAC source (bisection seek +
  pipeline start) p50 1.7 ms / p95 2.7 ms. Targets: p95 <300 ms warm,
  <800 ms cold, met with three orders of headroom on this box; network
  and disk latency dominate in deployment.
- **HLS seek-to-segment** (variant-worker restart): worst 49 ms in the
  restart-heavy e2e fetch pattern, p95 well under the 1 s target.
- **Load** (`TestLoadMixedTraffic`): 6,350 req/s of mixed live
  transcodes, seeks, direct play, probes, and HLS over 8 concurrent
  workers with zero malformed responses (503s under saturation carry
  the honest `overloaded` envelope).
- **Streaming soak** (`TestStreamingSoak`, goroutine/heap leak watch):
  918k requests over a sustained run with full reads, mid-body client
  disconnects, seeks, and HLS fetches; goroutines returned to baseline
  and heap stayed flat (~1.3 MiB). The nightly job runs a 20-minute
  soak; `make soak` runs 30 minutes locally.
- **Codec throughput** (per core, `make bench`): every codec clears its
  pinned floor in [docs/quality-gates.md](docs/quality-gates.md) by a
  wide margin (Opus encode 55-67x realtime, decode 274-514x; MP3 encode
  50-69x, decode ~200x; FLAC encode ~200x, decode 490-870x; AAC encode
  20-68x, decode ~240x).

## Quick start

```sh
# Hardened standalone deployment, port 4418. Publishing a non-loopback
# port requires API keys (the daemon fails closed without them).
WAXFLOW_API_KEYS=$(openssl rand -hex 24) docker compose up -d
curl http://localhost:4418/ping
curl -H "X-API-Key: $WAXFLOW_API_KEYS" http://localhost:4418/caps
```

Put music under `./library` (or set `WAXFLOW_LIBRARY=/path/to/music`)
and stream: mint a URL with `POST /sign`, or try the dev demo page
(`--demo`). Or from source (Go 1.26.3):

```sh
make build
./bin/waxflow server --demo &   # loopback: keyless is allowed
./bin/waxflow ping
open http://127.0.0.1:4418/demo
```

## Configuration

Precedence: **flag > `WAXFLOW_*` env > JSON config file > default**
(config file via `--config` or `WAXFLOW_CONFIG`; unknown keys are rejected).

| Key | Env | Default | Purpose |
|---|---|---|---|
| `addr` | `WAXFLOW_ADDR` | `127.0.0.1:4418` | listen address (compose widens to `0.0.0.0`) |
| `logLevel` | `WAXFLOW_LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` |
| `roots` | `WAXFLOW_ROOTS` | none | named library roots; JSON `[{"name","path"}]`, env `name=path,name2=path2`; each opened via `os.Root` (no escape, symlinks confined), files validated regular and size-capped |
| `catalogDB` | `WAXFLOW_CATALOG_DB` | none | WaxBin catalog SQLite path, opened read-only for `pid:<ULID>` source references. Waxbin flavor only: the stock server refuses to start with it set (one-shots on plain paths never read config, so they neither honor nor refuse it) |
| `apiKeys` | `WAXFLOW_API_KEYS` | none | control-API keys (comma-separated in env). **Fail closed**: required on a non-loopback `addr` unless `allowUnauthenticated` |
| `allowUnauthenticated` | `WAXFLOW_ALLOW_UNAUTHENTICATED` | `false` | explicit opt-in to keyless on non-loopback |
| `sourceMaxBytes` | `WAXFLOW_SOURCE_MAX_BYTES` | 4 GiB | per-source open cap |
| `metricsKey` | `WAXFLOW_METRICS_KEY` | none | additionally unlocks `GET /metrics` |
| `signingSecret` | `WAXFLOW_SIGNING_SECRET` | auto-generated into `dataDir` (0600) | HMAC key for signed URLs; `kid:hex,kid2:hex` rotation list or a literal secret |
| `allowedOrigins` | `WAXFLOW_ALLOWED_ORIGINS` | none | CORS allowlist for playback endpoints |
| `dataDir` / `cacheDir` | `WAXFLOW_DATA_DIR` / `WAXFLOW_CACHE_DIR` | platform dirs | daemon state (signing secret, job store) / transcode cache |
| `scratchDir` | `WAXFLOW_SCRATCH_DIR` | temp dir + `/waxflow` | upload spool (the hardened container mounts a tmpfs) |
| `uploadMaxBytes` / `uploadTTL` | `WAXFLOW_UPLOAD_MAX_BYTES` / `WAXFLOW_UPLOAD_TTL` | 2 GiB / `1h` | one upload's size cap / spool eviction after creation (Go duration, `0` never) |
| `scratchMaxBytes` | `WAXFLOW_SCRATCH_MAX_BYTES` | 8 GiB | aggregate spool cap (`uploadMaxBytes` only caps one upload) |
| `cacheMaxBytes` / `cacheMaxAge` | `WAXFLOW_CACHE_MAX_*` | 10 GiB / off | LRU eviction policy (`cacheMaxAge` is a Go duration) |
| `liveSlots` / `jobSlots` | `WAXFLOW_*_SLOTS` | NumCPU-1 / 2 | live admission pool (over limit means 503 + `Retry-After: 2`) / concurrent job workers (jobs queue and also pause while the live pool is saturated) |
| `defaultGain` | `WAXFLOW_DEFAULT_GAIN` | `track` | gain mode when `gain=` absent |
| `resampleProfile` | `WAXFLOW_RESAMPLE_PROFILE` | `hq` | `hq` or `fast` (constrained hosts) |
| `tlsCert` / `tlsKey` | `WAXFLOW_TLS_*` | none | native TLS; else put a terminating proxy in front (ADR-0007) |
| `debugAddr` | `WAXFLOW_DEBUG_ADDR` | off | loopback-only pprof listener |
| `paceBurstSeconds` / `paceFactor` | `WAXFLOW_PACE_*` | 30 / 2.0 | read-behind delivery pacing (factor 0 disables) |
| `demo` | `WAXFLOW_DEMO` | `false` | serve the browser test page at `/demo` (dev only) |

## CLI

- `waxflow server`: run the daemon (`--demo` for the browser test page)
- `waxflow probe <file>`: identify a file and print stream parameters
  (`--json` for the schemaVersion'd machine shape, identical to `GET
  /probe`; `--strict` to treat tolerated input damage as errors)
- `waxflow transcode <in> <out>`: local one-shot file-to-file transcode
  through the same engine the daemon uses (`--format wav|aiff|flac|mp3|aac|alac|opus`,
  `--flac-level`, `--mp3-bitrate`, default from the output extension;
  `--force` to overwrite). Metadata (tags, chapters, cover art, lyrics)
  passes through onto the output automatically (`--no-tags` to skip);
  `--loudness analyze` measures the source, applies the exact gain to
  the ReplayGain reference, and writes measured RG tags on the output
- `waxflow split <in> <dir>`: cut a single-file rip into one output per
  track, from a CUE sheet (`--cue album.cue`) or explicit source-sample
  offsets (`--at`). Cut points are samples either way: a sheet's `MM:SS:FF`
  times are CD frames of 1/75 s, which every CD-family rate divides exactly
  (44100/75 = 588), so a boundary converts with no rounding, where seconds
  would land a sample off and click at every join. The default FLAC output
  at the source's own rate makes the cut bit-exact, so the pieces rejoin
  into the original. `--dry-run` prints the pieces and their ranges
- `waxflow sign --src lib/a.flac`: mint a signed playback URL offline
  (ADR-0003; uses the same secret and roots the daemon holds)
- `waxflow cache stats|gc`: inspect or evict a running daemon's cache
- `waxflow doctor`: check the local environment a daemon needs: config
  resolves, every root opens and reads, the cache/data/scratch dirs
  accept writes, the WaxBin catalog opens (flavor builds), a quick
  self-bench transcodes faster than realtime, and the absence of ffmpeg
  is confirmed to be fine (`--json` for the machine shape)
- `waxflow ping`: liveness probe; the container HEALTHCHECK
- `waxflow version`: version and build info
- `waxflow exit-codes`: print the documented exit-code contract (0 ok,
  1 internal, 2 invalid, 3 not-found, 4 io, 5 unsupported, 6 canceled,
  7 unauthorized, 8 overloaded)

The HTTP surface is documented in [docs/api.md](docs/api.md).

## WaxBin resolver flavor

`ghcr.io/colespringer/waxflow:latest-waxbin` (or `make build-waxbin`) is
the identical CLI with one addition: `pid:<ULID>` source references
resolve against a WaxBin catalog. Point `catalogDB` at WaxBin's
database (created by WaxBin first; opened read-only, never taking
WaxBin's write lock) and every surface that accepts a source reference
accepts `pid:`: `/stream`, `/probe`, `/sign`, jobs, HLS, plus `waxflow
probe|transcode|sign` on the command line. `/caps` reports it as
`delivery.pid`.

Resolved paths are cached and the catalog's change feed is polled every
5 seconds, so a rename or move by WaxBin's organizer is picked up
within one poll (a stale path self-heals immediately on the next
request). Signed URLs pin bytes, not locations: a rename does not kill
them, while replaced content still dies with `410 source-changed`.
`compose.full.yaml` runs this flavor against WaxBin's volumes; note the
catalog directory mounts writable because SQLite WAL readers write
read-marks into the `-shm` sidecar, even though the database itself is
opened read-only.

The flavor lives in the nested module `resolver/`, which is what keeps
WaxBin's SQLite dependency out of the main module's tree.

## Non-goals for v1.0

Video; HE-AAC/SBR/xHE; Vorbis/WMA/APE/WavPack **encoding**; WMA/APE/WavPack
decoding; DASH manifests (the CMAF segments are already DASH-compatible);
DRM/HLS-AES; Opus PLC; CD ripping; any database (WaxBin owns cataloging);
tag *editing* (WaxLabel owns it; WaxFlow only maps and passes metadata);
Icecast/radio ingest; waveform peaks (WaxBin has them);
distributed cache; two-pass loudness on live streams (jobs only).

## Development

```sh
make check           # gofmt + vet + test + test-race + nested modules + depcheck
make test            # root-module suite, no race detector (the fast default loop)
make test-race       # race detector over the root module (heavy numeric suites self-skip)
make test-cli        # the cli module (cobra CLI + waxlabel mapper), race included
make test-resolver   # the WaxBin resolver flavor module, race included
make test-oracle     # the third-party-oracle tests (waxlabel round trips, go-mp3)
make soak            # 30m streaming soak + load + TTFA percentiles (nightly-scale)
make client-e2e      # browser client-matrix cells via Playwright (gated tooling)
make docker          # local image build
make verify-vectors  # fetch SHA-256-pinned conformance vectors (CI-cached)
make goldens         # regenerate muxer golden files (review the diff)
```

- Architecture invariants live in [docs/adr/](docs/adr/README.md). Read
  ADR-0001 (clean-room policy) before touching codec code.
- Encoder/decoder acceptance thresholds are pinned in
  [docs/quality-gates.md](docs/quality-gates.md); gates only ratchet up.
- ffmpeg is a **test oracle only** (differential CI job), never a runtime
  dependency.
- Releases are tag-driven: pushing `vX.Y.Z` publishes binaries + SHA256SUMS
  and a multi-arch (amd64/arm64) image to `ghcr.io/colespringer/waxflow`.

## License

[MIT](LICENSE). Third-party attributions: [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
