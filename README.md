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

Pre-1.0. The audio core is in: the planar PCM model (`audio`), the
codec/container interfaces, the PCM codec, WAV (including RF64/BW64 read
and automatic RF64 write past the 4 GiB mark) and AIFF/AIFF-C containers,
format probing with sample-exact seeking, the engine facade (`New`,
`Probe`, `Transcode`, `OpenStream`), and the test harness (ffmpeg as
differential oracle, never a runtime dependency). FLAC decoding is in:
the `codec/flac` decoder (RFC 9639, bit-exact on the complete IETF
decoder testbench), the native FLAC container with checksum-confirmed
frame boundaries and SEEKTABLE/bisection seeking, and Ogg demuxing with
the Ogg-FLAC mapping. The DSP core is in: a streaming Kaiser
windowed-sinc polyphase resampler (`hq`: alias rejection >= 110 dB with
passband ripple <= 0.05 dB out to 0.91x Nyquist; `fast`: ~70 dB for
constrained hosts), energy-normalized channel downmix, gain with a
look-ahead true-peak limiter, TPDF and noise-shaped dither, all
deterministic and assembled by a pull-based stage chain that inserts
only the nodes a conversion needs. Local `waxflow probe` and `waxflow
transcode` work today, the latter with `--rate`, `--channels`, `--bits`,
`--gain`, `--resample-profile`, `--dither`, and `--flac-level`; WAV/AIFF
round-trips are bit-exact by construction, FLAC and Ogg-FLAC decode
bit-exactly, and resampled output is level-matched against ffmpeg's
soxr, all verified against ffmpeg. FLAC encoding is in: `codec/flac`
gained a from-scratch encoder (fixed and LPC prediction, full stereo
decorrelation search, Rice partition optimization, levels 0-8 inside
the streamable subset) and `container/flacn` a muxer (streaming form on
a plain writer; exact STREAMINFO with MD5 signature plus a SEEKTABLE on
seekable output). The whole IETF suite re-encodes losslessly, `flac -t`
accepts every output, and level 5 lands within the pinned size gate of
`flac -5` (currently 0.996x on the suite). MP3 decoding is in: a
from-scratch `codec/mp3` Layer III decoder (MPEG-1/2/2.5, both stereo
modes, bit reservoir) and the `container/mpa` elementary-stream demuxer
(ID3 handling, Xing/Info/VBRI metadata, LAME gapless trims, and a lazy
exact frame index that makes VBR seeking sample-exact and persists
across sessions via the cache's index sidecar). Decoded output sits
around 1e-7 RMS of ffmpeg's float decoder against the 1e-4 gate, the
LAME gapless sample-count invariant holds end to end, and seeks land
bit-identical to a linear decode at 100 random offsets in VBR streams.
MP3 encoding is in: a from-scratch baseline CBR Layer III encoder
(polyphase analysis filterbank and forward MDCT that invert the decoder
exactly, a global-gain rate-control loop, Huffman table selection, and a
bit reservoir; long blocks only, from ISO 11172-3 and textbooks) plus a
`container/mpa` muxer whose leading Xing/Info frame carries a LAME-format
gapless tag. Output decodes in ffmpeg, go-mp3, and our own decoder; the
gapless round-trip holds (decoded length equals the source length) across
sample rates and channel counts; encoding runs 68-111x realtime against
the 40x floor; and the first-lossy-encoder quality harness scores it at or
above the Shine baseline on every corpus track via a ported ODG-proxy
(the nightly report the quality gates name).

The service is live: the daemon streams progressive audio (`GET
/stream`) with a direct-play/transcode decision ladder, sample-exact
`t=` seeking, short-lived HMAC-signed playback URLs pinned to source
identity, a write-through transcode cache with read-behind delivery
(slow clients never backpressure the encoder; a full cache disk degrades
to ring-fed streaming instead of killing playback), admission control,
Prometheus metrics, and a full API contract in
[docs/api.md](docs/api.md). WAV, FLAC, and MP3 streams live-transcode
today; compliant sources direct-play, and each new encoder widens
`format=`.
With MP3 encoding landed, the service is broadly useful: any supported
source streams as MP3 to essentially every player. Unfinished
codecs stay unregistered, so probe and `/caps` never advertise what
doesn't work. Quality bars are pinned in
[docs/quality-gates.md](docs/quality-gates.md).

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
| `apiKeys` | `WAXFLOW_API_KEYS` | none | control-API keys (comma-separated in env). **Fail closed**: required on a non-loopback `addr` unless `allowUnauthenticated` |
| `allowUnauthenticated` | `WAXFLOW_ALLOW_UNAUTHENTICATED` | `false` | explicit opt-in to keyless on non-loopback |
| `sourceMaxBytes` | `WAXFLOW_SOURCE_MAX_BYTES` | 4 GiB | per-source open cap |
| `metricsKey` | `WAXFLOW_METRICS_KEY` | none | additionally unlocks `GET /metrics` |
| `signingSecret` | `WAXFLOW_SIGNING_SECRET` | auto-generated into `dataDir` (0600) | HMAC key for signed URLs; `kid:hex,kid2:hex` rotation list or a literal secret |
| `allowedOrigins` | `WAXFLOW_ALLOWED_ORIGINS` | none | CORS allowlist for playback endpoints |
| `dataDir` / `cacheDir` | `WAXFLOW_DATA_DIR` / `WAXFLOW_CACHE_DIR` | platform dirs | daemon state / transcode cache |
| `cacheMaxBytes` / `cacheMaxAge` | `WAXFLOW_CACHE_MAX_*` | 10 GiB / off | LRU eviction policy (`cacheMaxAge` is a Go duration) |
| `liveSlots` / `jobSlots` | `WAXFLOW_*_SLOTS` | NumCPU-1 / 2 | admission pools; over limit means 503 + `Retry-After: 2` |
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
  through the same engine the daemon uses (`--format wav|aiff|flac|mp3`,
  `--flac-level`, `--mp3-bitrate`, default from the output extension;
  `--force` to overwrite)
- `waxflow sign --src lib/a.flac`: mint a signed playback URL offline
  (ADR-0003; uses the same secret and roots the daemon holds)
- `waxflow cache stats|gc`: inspect or evict a running daemon's cache
- `waxflow ping`: liveness probe; the container HEALTHCHECK
- `waxflow version`: version and build info
- `waxflow exit-codes`: print the documented exit-code contract (0 ok,
  1 internal, 2 invalid, 3 not-found, 4 io, 5 unsupported, 6 canceled,
  7 unauthorized, 8 overloaded)

The HTTP surface is documented in [docs/api.md](docs/api.md).

## Non-goals for v1.0

Video; HE-AAC/SBR/xHE; Vorbis/WMA/APE/WavPack **encoding**; WMA/APE/WavPack
decoding; DASH manifests (the CMAF segments are already DASH-compatible);
DRM/HLS-AES; Opus PLC; CD ripping; any database (WaxBin owns cataloging);
tag *editing* (WaxLabel owns it; WaxFlow only maps and passes metadata);
Icecast/radio ingest; CUE splitting; waveform peaks (WaxBin has them);
distributed cache; two-pass loudness on live streams (jobs only).

## Development

```sh
make check           # gofmt + vet + go test -race + depcheck
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
