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

Pre-1.0, bootstrap complete: module layout, error taxonomy
(`waxerr`), config loading, CLI skeleton, distroless Docker image, CI and
release pipelines, ADR pack, and pinned [quality gates](docs/quality-gates.md).
The daemon serves liveness only so far; WAV/FLAC streaming is next, and
the service becomes broadly useful once MP3 encoding lands. Unfinished codecs stay
unregistered, so `/caps` never advertises what doesn't work.

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
make check     # gofmt + vet + go test -race + depcheck
make docker    # local image build
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
