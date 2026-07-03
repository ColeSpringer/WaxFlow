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
`--gain`, `--resample-profile`, and `--dither`; WAV/AIFF round-trips are
bit-exact by construction, FLAC and Ogg-FLAC decode bit-exactly, and
resampled output is level-matched against ffmpeg's soxr, all verified
against ffmpeg.

The daemon still serves liveness only; WAV/FLAC streaming is next, and
the service becomes broadly useful once MP3 encoding lands. Unfinished
codecs stay unregistered, so probe and `/caps` never advertise what
doesn't work. Quality bars are pinned in
[docs/quality-gates.md](docs/quality-gates.md).

## Quick start

```sh
docker compose up -d          # hardened standalone deployment, port 4418
curl http://localhost:4418/ping
```

Or from source (Go 1.26.3):

```sh
make build
./bin/waxflow server &
./bin/waxflow ping
```

## Configuration

Precedence: **flag > `WAXFLOW_*` env > JSON config file > default**
(config file via `--config` or `WAXFLOW_CONFIG`; unknown keys are rejected).

| Key | Env | Default | Purpose |
|---|---|---|---|
| `addr` | `WAXFLOW_ADDR` | `127.0.0.1:4418` | listen address (compose widens to `0.0.0.0`) |
| `logLevel` | `WAXFLOW_LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` |

The table grows as features land.

## CLI

- `waxflow server`: run the daemon
- `waxflow probe <file>`: identify a file and print stream parameters
  (`--json` for the schemaVersion'd machine shape, `--strict` to treat
  tolerated input damage as errors)
- `waxflow transcode <in> <out>`: local one-shot file-to-file transcode
  through the same engine the daemon uses (`--format wav|aiff`, default
  from the output extension; `--force` to overwrite)
- `waxflow ping`: liveness probe; the container HEALTHCHECK
- `waxflow version`: version and build info
- `waxflow exit-codes`: print the documented exit-code contract (0 ok,
  1 internal, 2 invalid, 3 not-found, 4 io, 5 unsupported, 6 canceled,
  7 unauthorized, 8 overloaded)

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
