# WaxFlow

A self-hosted audio transcoding and streaming server, written in pure Go.
Point it at an audio file library and it streams those files as Opus, AAC, MP3,
FLAC and more, progressively or over HLS, with sample-exact seeking and
gapless playback. It also signs playback URLs, caches transcodes, applies ReplayGain
loudness, carries tags and cover art through, and runs background jobs
(whole-file transcodes, loudness analysis, splitting a CUE rip into tracks).

It ships as one static binary with no ffmpeg and no cgo. The codecs are
written from scratch and can be imported on their own.

## Formats

| Codec | Reads from | Writes to |
|---|---|---|
| FLAC | .flac, Ogg, MP4, Matroska | .flac, Ogg, Matroska, HLS |
| ALAC | MP4 | MP4, HLS |
| WavPack | .wv | .wv |
| Monkey's Audio | .ape | .ape |
| PCM | WAV, AIFF, MP4/MOV, Matroska | WAV, AIFF, Matroska |
| MP3 | .mp3, MP4/MOV, WAV, AIFF-C, Matroska | .mp3 |
| AAC-LC, HE-AAC v1/v2 | MP4, ADTS, Matroska | MP4, ADTS, Matroska, HLS |
| Opus | Ogg, Matroska/WebM, MP4 | Ogg, Matroska/WebM, HLS |
| Vorbis | Ogg, Matroska/WebM | Ogg, Matroska/WebM |
| Musepack SV7/SV8 | .mpc | |
| WMA, WMA Pro, WMA Lossless, WMA Voice | ASF (.wma) | |
| G.711, IMA ADPCM | WAV, AIFF-C, MOV | |
| Microsoft ADPCM | WAV, MOV | |

Lossless decoders are bit-exact, lossy ones match the reference decoder or
ffmpeg within tight error bounds, and Opus passes all the RFC 6716/8251 test
vectors; [docs/quality-gates.md](docs/quality-gates.md) has the numbers. A
variant a decoder does not handle (WavPack hybrid, say) is refused by name,
and `waxflow probe` tells you what a file is.

## Quick start

```sh
git clone https://github.com/ColeSpringer/WaxFlow && cd WaxFlow
grep -qs '^WAXFLOW_API_KEYS=' .env || echo "WAXFLOW_API_KEYS=$(openssl rand -hex 24)" >> .env
echo "WAXFLOW_LIBRARY=$HOME/Music" >> .env
docker compose up -d
```

Docker Compose reads `.env` on every run, so the key stays put, and mounts
the `WAXFLOW_LIBRARY` folder (default `./library`) as the root `lib`. Every
`up` pulls the published image, so rerunning it updates. Until a release
exists, the pull reports `denied` and Compose uses an image built from the
checkout (add `--build` after a `git pull`). The port is plain HTTP: put
TLS in front (`tlsCert`/`tlsKey`, or a reverse proxy) before exposing it
beyond your LAN.

Stream a file. `src` is the root name `lib` plus a path inside your library;
add `&t=95.5` to start 95.5 seconds in.

```sh
KEY=$(sed -n 's/^WAXFLOW_API_KEYS=//p' .env)
curl -H "X-API-Key: $KEY" -o song.opus \
  "http://localhost:4418/stream?src=lib/Artist/Album/01.flac&format=opus"
```

Players that cannot send a header get a signed URL instead, for a
progressive stream or an HLS playlist. Each command prints a path to put
after `http://localhost:4418`.

```sh
curl -s -H "X-API-Key: $KEY" http://localhost:4418/sign \
  -d '{"params": {"src": "lib/Artist/Album/01.flac", "format": "aac"}}' | jq -r .url
curl -s -H "X-API-Key: $KEY" http://localhost:4418/sign \
  -d '{"path": "/hls/master.m3u8", "params": {"src": "lib/Artist/Album/01.flac", "format": "aac"}}' | jq -r .url
```

From source (Go 1.26), where loopback needs no key and
http://127.0.0.1:4418/demo probes and plays files in the browser:

```sh
make build
WAXFLOW_ROOTS=lib=$HOME/Music ./bin/waxflow server --demo
```

Tagged releases publish binaries for Linux, macOS and Windows, and an
amd64/arm64 image at `ghcr.io/colespringer/waxflow`.

## Configuration

Flags beat `WAXFLOW_*` environment variables, which beat a JSON config file
(`--config`), which beats the defaults. The settings most deployments touch:

| Key | Env | Default | Purpose |
|---|---|---|---|
| `roots` | `WAXFLOW_ROOTS` | none | library folders (env form `name=path,name2=path2`) |
| `apiKeys` | `WAXFLOW_API_KEYS` | none | required off loopback, unless `allowUnauthenticated` is set |
| `addr` | `WAXFLOW_ADDR` | `127.0.0.1:4418` | listen address |
| `tlsCert`, `tlsKey` | `WAXFLOW_TLS_CERT`, `WAXFLOW_TLS_KEY` | none | serve TLS directly |
| `cacheDir`, `cacheMaxBytes` | `WAXFLOW_CACHE_DIR`, `WAXFLOW_CACHE_MAX_BYTES` | platform dir, 10 GiB | transcode cache |

Every setting is in [docs/configuration.md](docs/configuration.md).
`waxflow doctor` checks a configuration before you deploy it.

## CLI

```
waxflow server                       run the daemon
waxflow transcode <in> <out>         convert a file locally (format from the extension or --format)
waxflow probe <file>                 show what a file contains (--json for the API's shape)
waxflow split <in> <dir> --cue <f>   cut a single-file rip into tracks, losslessly
waxflow sign --src <ref>             mint a signed playback URL
waxflow doctor                       check config, roots, directories and speed
waxflow cache stats|gc               inspect or trim a running daemon's cache
waxflow ping | version | exit-codes
```

`waxflow <command> --help` lists each command's flags. Ctrl-C cancels a
command, removes the file it was writing and exits 6; a second one kills it.
`waxflow server` drains its requests instead.

## Go library

The root module requires nothing, so `go get github.com/colespringer/waxflow`
pulls in no other module.

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
)

func main() {
	in, err := os.Open("in.flac")
	if err != nil {
		log.Fatal(err)
	}
	defer in.Close()
	src, err := container.FileSource(in)
	if err != nil {
		log.Fatal(err)
	}
	out, err := os.Create("out.opus")
	if err != nil {
		log.Fatal(err)
	}
	opts := waxflow.TranscodeOptions{Format: "opus"}
	if _, err := waxflow.New().Transcode(context.Background(), src, "", out, opts); err != nil {
		log.Fatal(err)
	}
	if err := out.Close(); err != nil {
		log.Fatal(err)
	}
}
```

The pieces are packages of their own: `codec/...` (one per codec),
`container/...`, `dsp/...` (resampling, mixing, gain, loudness), `cue`,
`m3u8`, and `client`, a Go client for the HTTP API.

## Performance

On a 12-core x86-64 Linux box over loopback (`make soak`, `make bench`):

- First audio in under 1 ms at p95 for a fresh transcode, 0.24 ms from
  cache.
- 6,350 requests/s of mixed streaming, seeking, probing and HLS on 8
  workers; a 918k-request soak showed no goroutine or heap growth.
- Per core, Opus encodes at 55-67x realtime, MP3 at 50-69x, AAC at 20-68x
  and FLAC at about 200x; FLAC, MP3, AAC, Opus and Musepack decode at 200
  to 1900x.

## Documentation

- [docs/api.md](docs/api.md): the HTTP API (streaming, HLS, signed URLs, jobs, `pid:` lookup)
- [docs/configuration.md](docs/configuration.md): every setting
- [docs/client-matrix.md](docs/client-matrix.md): which players were tested with which formats
- [docs/quality-gates.md](docs/quality-gates.md): codec quality and speed gates
- [docs/adr/](docs/adr/README.md): design decisions

## Development

```sh
make test             # root module without the race detector: the everyday loop
make verify-vectors   # fetch the pinned conformance vectors; those tests skip without them
make check            # the full gate (slow): fmt, vet, race, every module, depcheck
```

`cli/` (the command, with cobra and waxlabel) and `oracletest/` (tests
against third-party libraries) are nested modules, so `go test ./...` at the
root does not reach them; `make check` does.

## License

[MIT](LICENSE). Third-party attributions are in
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
