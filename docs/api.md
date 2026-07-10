# WaxFlow HTTP API

Status: progressive v0, the first service surface. This file is the
committed API contract: golden response fixtures in
`server/testdata/golden/` are asserted in tests, and the `client` package
is the reference consumer. Endpoints marked *later* are designed but not
yet served; requesting them returns the 404 envelope.

Conventions:

- Default port **4418**. No `/api/v1` prefix; JSON bodies carry
  `"schemaVersion": 1` instead.
- Errors are always the family envelope, kebab-case codes shared with the
  CLI exit-code contract (`waxflow exit-codes`):

      {"error": "human text", "code": "not-found", "schemaVersion": 1}

- Codes: `invalid-request unauthorized signature-invalid
  signature-expired source-changed not-found unsupported-format
  unsupported-source payload-too-large source-unreadable output-unwritable
  overloaded canceled catalog-unavailable internal`.
- Status mapping: 400 invalid-request, 401 unauthorized, 403
  signature-invalid/expired, 404 not-found, 410 source-changed, 413
  payload-too-large, 415 unsupported-format, 416 (range refusal, code
  invalid-request), 501 unsupported-source, 503 overloaded (with
  `Retry-After: 2`) and catalog-unavailable, 500 the rest.

## Authentication

Control endpoints require an API key: `X-API-Key: <key>` or
`Authorization: Bearer <key>` (SHA-256 + constant-time compare, multiple
keys supported). Playback endpoints (`/stream`) accept an API key **or**
a signed query. A valid API key wins outright: signature parameters on a
keyed request (even stale or tampered ones) are ignored, so a trusted
caller re-fetching an expired signed URL with its key just works. Source
identity is separate from auth: any request carrying `id` gets `410
source-changed` when the file changed, keys included; drop `id` to opt
out of pinning.

**Fail closed:** with a non-loopback `addr` and no `apiKeys`, the daemon
refuses to start unless `allowUnauthenticated: true` is explicit.

### Signed URLs (ADR-0003)

`exp` (unix seconds) + `kid` (key id) + `sig` = base64url (no padding) of
HMAC-SHA256 over:

    "waxflow-v1" "\n" method "\n" path "\n" canonicalQuery

- `canonicalQuery`: every query parameter except `sig`, percent-encoded
  per RFC 3986 (uppercase hex, `%20` for space, `~` bare), sorted by key
  then value, joined with `&`. Every playback-affecting parameter is
  inside the signature.
- **HEAD signs and verifies as GET**, so players' preflight HEADs pass.
- Expiry leeway: **60 seconds** of clock skew is tolerated.
- Signed URLs must carry `id=<size>-<mtimeNS>`, the source identity they
  were minted for. If the file changed, playback returns `410
  source-changed` and the client re-mints. Key-authed requests may send
  `id` voluntarily to get the same pinning.
- Default TTL: `max(6h, 2 x source duration)`.

Mint with `POST /sign`, the `client` package (`client.MintURL` offline),
or `waxflow sign`.

## Endpoints

| Method and path | Auth | Purpose |
|---|---|---|
| `GET /ping` | none | liveness (Docker HEALTHCHECK): `{"status":"ok","schemaVersion":1}` |
| `GET /version` | key | `{"schemaVersion":1,"version":"v0.4.0"}` |
| `GET /caps` | key | capability discovery (see below) |
| `GET/POST /probe` | key | container/track/warning report for a source |
| `POST /sign` | key | mint a signed playback URL |
| `GET/HEAD /stream` | key or sig | progressive stream (decision ladder) |
| `POST /transcode` | key | synchronous one-shot; the response body is the transcode |
| `GET /hls/master.m3u8` | key or sig | HLS master playlist (ladder; see the HLS section) |
| `GET /hls/media.m3u8`, `/hls/init.mp4`, `/hls/seg/{n}.m4s` | key or sig | HLS variant playlist, init header, media segments |
| `GET /cache/stats`, `POST /cache/gc` | key | cache operations |
| `GET /metrics` | key or metricsKey | Prometheus text exposition |
| `GET /demo` | none (dev mode only, `demo: true`) | browser test page |
| `POST /uploads`, `/jobs`, `/art`, `/lyrics` | | *later* (job store, metadata mapping) |

## GET /probe, POST /probe

`GET /probe?src=<ref>[&strict=1]` or `POST /probe` with
`{"src": "<ref>", "strict": false}`.

    {
      "schemaVersion": 1,
      "container": "wav",
      "tracks": [{
        "id": 0, "codec": "pcm", "rate": 44100, "channels": 2,
        "layout": "FL|FR", "sampleType": "int", "bitDepth": 16,
        "samples": 2205, "durationSeconds": 0.05, "default": true
      }],
      "warnings": ["..."]
    }

`warnings` lists input damage the tolerant parser worked around; `strict`
turns damage into errors. `samples: -1` means unknown length. This is
byte-identical to `waxflow probe --json`.

## GET /stream

    /stream?src=<ref>&format=auto|wav|flac|alac|mp3|aac|opus&rate=&ch=&bits=16|24&bitrate=|q=&container=&gain=&t=&track=&maxBitRate=

Source references (`src`): `<root>/<relative/path>` under a configured
library root; `upload:<id>` and `pid:<ULID>` return `501
unsupported-source` until the job store and the WaxBin resolver flavor
land.

Parameters (unknown parameter names are rejected):

- `format`: `auto` (default) engages the decision ladder; `wav` forces a
  WAV transcode; `flac` a FLAC one (lossless, level 5; a FLAC source
  under `format=flac` with no transforming parameters direct-plays
  instead); `alac` a lossless Apple Lossless stream in progressive
  fragmented MP4 (`audio/mp4`); `mp3` a baseline CBR MP3 (128 kbit/s
  default, `bitrate`/`q` select it); `aac` an AAC-LC stream in
  progressive fragmented MP4 (`audio/mp4`, 128 kbit/s default,
  `bitrate`/`q` select it; the init header's edit list carries the
  gapless trims); `opus` a CELT-only Ogg-Opus stream
  (`audio/ogg`, 96 kbit/s default, `bitrate`/`q` select it). Other formats
  join as encoders land (`/caps` is the truth). `aiff` exists for jobs but
  has no streaming form: 415. Live FLAC and ALAC streams omit the size hints
  and byte-rate pacing: a lossless encoder's output size is signal-dependent
  and unknown up front. CBR MP3 and Opus carry a size estimate. Completed
  cache entries serve with exact sizes like any other.
- `rate`, `ch`, `bits`: output sample rate, channel count (1 or 2), bit
  depth (16 or 24, dithered when reducing). Absent keeps the source's.
- `gain`: `off`, `track` (default), `album`, or an explicit `+/-dB`
  number (clamped at +12 dB; positive gain engages the true-peak
  limiter). Tag-based ReplayGain (`track`/`album`) resolves to 0 dB until
  metadata mapping lands.
- `t`: start position in **seconds** (decimal allowed). Seeks are
  sample-exact: the decoder pre-rolls from the nearest sync point. A
  `t>0` request is always a transcode (a new request per seek; live
  streams are not byte-addressable). Seeking at or past the end of the
  track is not an error: the response is a valid empty stream
  (`X-Content-Duration: 0.000`).
- `track`: must name the default track until multi-track containers
  land.
- `bitrate`, `q`: lossy quality selection, mutually exclusive. `bitrate`
  is an explicit CBR rate in kbit/s; `q` is a preset (`low` 96, `med`
  128, `high` 192). Both require an explicit lossy `format` (`mp3`,
  `aac`, `opus`); on a lossless output they are `415`. A `bitrate`/`q` request
  forces a re-encode (never direct-play), and the resolved bit rate is
  part of the cache key, so two rates never share an entry.
- `container`: overrides the format's packaging where an alternative
  exists; today only `format=aac` has one: `container=adts` selects the
  raw ADTS elementary stream (`audio/aac`), a legacy opt-out for players
  without fMP4 support. ADTS carries no gapless signaling, which is why
  fMP4 is the default. On any other format (or with `format=auto`) the
  parameter is `400`.
- `maxBitRate`: a kbit/s cap for the decision ladder. For direct play the
  cap is checked against whole-file bytes over duration (tags and
  embedded art included): direct play ships the entire file, so the wire
  cost is what the cap protects. A VBR-lossless plan has no fixed rate to
  hold under the cap, so a cap on it is `415` rather than silently
  unenforced.

**Decision ladder (v1)**: if the source already satisfies the request
(`format=auto` or matching container, no transforming parameters, under
`maxBitRate` if given), the original bytes are served: `200`,
`Content-Type` per container, strong identity `ETag`, `Accept-Ranges:
bytes` with full RFC 7233 range and `If-None-Match` support. Transmux
rungs light up as muxer pairs land; otherwise a WAV transcode streams.

**Live transcode responses**: `200` chunked, `Accept-Ranges: none`,
`Cache-Control: no-store`, `X-Accel-Buffering: no`, plus hints
`X-Content-Duration` (seconds) and `X-Estimated-Content-Length`. Range
policy per RFC 9110's permission to ignore: `Range: bytes=0-` gets the
plain 200 full stream (Safari/AVPlayer attach it to everything); any
nonzero offset gets `416` plus an envelope hinting at `t=`. Delivery
bursts `paceBurstSeconds` of audio then caps at `paceFactor` x realtime.
A write stalled for 60 s kills the session.

**Cached responses**: the pipeline writes through the transcode cache;
once the entry completes the same URL serves it with real
`Content-Length`, full ranges, and the cache key as strong `ETag`.

**HEAD** never spawns a pipeline: headers and hints come from probe/cache
metadata only.

## POST /transcode

Same query parameters as `/stream`; the response body is the transcode
(uncacheable, ring-fed, dies with the request). Sets
`Content-Disposition: attachment`. Counts against live admission slots
for its whole duration, so scripts do not starve playback.

## HLS

Stateless CMAF/fMP4 delivery: every HLS URL carries a `v=` descriptor
(base64url JSON) holding the complete output selection plus the source
identity, so segments regenerate after eviction or restart with zero
session state. Descriptor schema (version 1):

    {"ver":1, "src":"lib/a.flac", "id":"<size-mtimeNS>", "format":"opus",
     "bitrate":96, "bitrates":[64,96,160], "bits":16, "rate":48000,
     "ch":2, "gain":"track", "segDur":4}

`bitrates` (the ladder) appears only in master URLs; every other URL is
per-variant. Descriptors are minted by the daemon (`POST /sign` with
`path:/hls/master.m3u8`, or the keyed raw-parameter master form below),
never hand-built by clients. Auth is an API key or the signed query,
like `/stream`; playlist child URLs come out signed (when signing is
configured) with the parent's expiry, so one minting governs a playback
session.

| Path | Purpose |
|---|---|
| `GET /hls/master.m3u8?v=...` | master playlist: one rung per ladder bitrate with `BANDWIDTH` and `CODECS` (`Opus`, `fLaC`, `alac`, `mp4a.40.2`). With an API key, raw parameters (`src`, `format`, `bitrate` or `bitrates`, `bits`, `rate`, `ch`, `gain`, `segDur`) also work; the daemon builds the descriptor. |
| `GET /hls/media.m3u8?v=...` | variant VOD playlist: `EXT-X-VERSION:7`, `EXT-X-MAP`, `EXT-X-INDEPENDENT-SEGMENTS`, every segment listed with its exact duration, `EXT-X-ENDLIST`. Unknown source lengths are measured (frame-index walk), never estimated. |
| `GET /hls/init.mp4?v=...` | the CMAF init header (codec config; the edit list carries encoder delay and the exact length) |
| `GET /hls/seg/{n}.m4s?v=...` | media segment n (0-based). Cached segments serve with ranges and strong ETags; misses wait on the variant worker (within a 3-segment lookahead) or restart it at n. |

Segments are `styp` + `moof`+`mdat` fragments, boundaries snapped to
whole encoder frames (`segDur` is a target; the playlist carries exact
durations), all sync samples, decode timeline in sample units. Formats
with a segmented form: `opus`, `flac`, `alac`, `aac` (see `/caps`
`delivery.hlsFormats`). A `410 source-changed` means the file changed
since minting: re-mint and reload.

## POST /sign

    {"path": "/stream", "params": {"src": "lib/a.flac", "format": "wav"}, "ttlSeconds": 3600}

`path` defaults to `/stream`; `/hls/master.m3u8` is also signable (its
`params` are the raw HLS master parameters above, and every rung is
planned at mint time so a URL that mints is a URL that plays). `params`
are validated like a live request, the source identity is resolved and
embedded, and the response is:

    {"schemaVersion": 1, "url": "/stream?exp=...&format=wav&id=...&kid=1&sig=...&src=...", "exp": 1767225600}

## GET /caps

    {
      "schemaVersion": 1,
      "inputs": ["flac", "wav", "aiff", "ogg", "mp4", "adts", "mp3"],
      "decoders": ["pcm", "flac", "mp3", "alac", "aac-lc"],
      "outputs": [{"name": "wav", "live": true, "exts": ["wav", "wave", "rf64", "bw64"]},
                   {"name": "aiff", "live": false, "exts": ["aif", "aiff", "aifc", "afc"]},
                   {"name": "flac", "live": true, "exts": ["flac"]},
                   {"name": "mp3", "live": true, "exts": ["mp3", "mpga"]},
                   {"name": "aac", "live": true, "exts": ["m4a", "aac"]},
                   {"name": "alac", "live": true, "exts": []}],
      "delivery": {"progressive": true, "hls": true, "hlsFormats": ["opus", "flac", "alac", "aac"],
                   "jobs": false, "uploads": false},
      "profiles": {}
    }

Capability-gated: only what works is listed. `profiles` fills once the
client matrix is verified, with tested delivery profiles
(`apple-native`, `hls-js`, ...).

## Cache operations

`GET /cache/stats` returns
`{"schemaVersion":1,"entries":n,"bytes":n,"hits":n,"misses":n}`.
`POST /cache/gc` runs eviction now and returns
`{"schemaVersion":1,"removed":n,"freedBytes":n}`.

## GET /metrics

Prometheus text: `waxflow_build_info`, `waxflow_sessions_active`,
`waxflow_sessions_total{kind}` (live, sync, hls),
`waxflow_direct_play_total`, `waxflow_hls_segments_total`,
`waxflow_cache_{hits,misses}_total`, `waxflow_cache_{bytes,entries}`,
`waxflow_admission_rejects_total`, `waxflow_admission_in_use{pool}`,
`waxflow_session_degradations_total`, `waxflow_ttfb_seconds` histogram.
