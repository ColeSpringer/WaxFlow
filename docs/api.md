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
| `POST /uploads`, `DELETE /uploads/{id}` | key | spool one-shot sources; reference as `src=upload:<id>` |
| `POST /jobs`, `GET /jobs[/{id}]`, `DELETE /jobs/{id}` | key | async full-file transcode/analyze jobs |
| `GET /jobs/{id}/events` | key or sig | server-sent job progress events (`EventSource` cannot set headers) |
| `GET /jobs/{id}/result` | key or sig | finished output (full ranges) or analysis JSON |
| `GET /art`, `GET /lyrics` | key or sig | embedded cover art / lyrics passthrough (raw bytes, ETag'd, no resizing) |

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

With metadata mapping (the CLI daemon always has it), the body also
carries the source's tag summary when present: `tags` (canonical
uppercase keys to value lists, ReplayGain included; the lyric sheet is
excluded, `hasLyrics` plus `GET /lyrics` cover it), `chapters`
(`[{"startSeconds", "title"}]`), `hasArt`, and `hasLyrics`.

`warnings` lists input damage the tolerant parser worked around; `strict`
turns damage into errors. `samples: -1` means unknown length. This is
byte-identical to `waxflow probe --json`.

## GET /stream

    /stream?src=<ref>&format=auto|wav|flac|alac|mp3|aac|opus&rate=&ch=&bits=16|24&bitrate=|q=&container=&gain=&t=&track=&maxBitRate=

Source references (`src`): `<root>/<relative/path>` under a configured
library root; `upload:<id>` for a spooled one-shot upload (POST
/uploads); `pid:<ULID>` for a WaxBin catalog item, served by the waxbin
flavor with `catalogDB` configured (`delivery.pid` in `/caps`) and `501
unsupported-source` everywhere else. A pid reference re-resolves to the
item's current path on every request, so catalog renames and moves are
transparent: the source identity pins bytes, not locations.

Parameters (unknown parameter names are rejected):

- `format`: `auto` (default) engages the decision ladder; `wav` forces a
  WAV transcode; `flac` a FLAC one (lossless, level 5; a FLAC source
  under `format=flac` with no transforming parameters direct-plays
  instead); `alac` a lossless Apple Lossless stream in progressive
  fragmented MP4 (`audio/mp4`); `mp3` a baseline CBR MP3 (128 kbit/s
  default, `bitrate`/`q` select it); `aac` an AAC-LC stream in
  progressive fragmented MP4 (`audio/mp4`, 128 kbit/s default,
  `bitrate`/`q` select it; the init header's edit list carries the
  gapless trims); `opus` an Ogg-Opus stream (`audio/ogg`, 96 kbit/s
  default, `bitrate`/`q` select it) from the full Opus encoder: SILK,
  hybrid, and CELT modes with analyser-driven speech/music selection.
  Other formats
  join as encoders land (`/caps` is the truth). `aiff` exists for jobs but
  has no streaming form: 415. Live FLAC and ALAC streams omit the size hints
  and byte-rate pacing: a lossless encoder's output size is signal-dependent
  and unknown up front. CBR MP3 and Opus carry a size estimate. Completed
  cache entries serve with exact sizes like any other.
- `rate`, `ch`, `bits`: output sample rate, channel count (1 or 2), bit
  depth (16 or 24, dithered when reducing). Absent keeps the source's.
- `gain`: `off`, `track` (default), `album`, or an explicit `+/-dB`
  number (clamped at +12 dB; positive gain engages the true-peak
  limiter). `track` and `album` resolve against the source's ReplayGain
  2 tags (Opus `R128_*` tags convert from the -23 LUFS reference), fall
  back from album to track, and resolve to 0 dB when the source carries
  none. Exact measured loudness belongs to jobs (`loudness: "analyze"`).
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

`path` defaults to `/stream`. Also signable: `/hls/master.m3u8` (its
`params` are the raw HLS master parameters above, and every rung is
planned at mint time so a URL that mints is a URL that plays), `/art`
and `/lyrics` (`params` take only `src`; identity is embedded), and
`/jobs/<id>/events` / `/jobs/<id>/result` (no `params`; the signature
pins the job id through the path). `params` are validated like a live
request, the source identity is resolved and embedded, and the response
is:

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
                   "jobs": false, "uploads": false, "pid": false},
      "profiles": {
        "apple-native": {
          "delivery": "hls",
          "progressive": ["aac", "mp3", "flac", "alac", "opus"],
          "hls": ["aac", "alac", "flac", "opus"],
          "basis": "vendor-documented + manual checklist (docs/client-matrix.md)",
          "notes": ["hls opus needs iOS/tvOS 17 or macOS 14", "..."]
        },
        "hls-js": {"delivery": "hls", "progressive": ["opus", "flac", "mp3", "aac", "wav"],
                   "hls": ["opus", "flac", "aac"],
                   "basis": "automated: hls.js + <audio> in Chromium (make client-e2e, nightly)"},
        "android-exoplayer": {"...": "..."},
        "desktop-mpv": {"...": "..."}
      }
    }

Capability-gated: only what works is listed. `delivery.jobs` and
`delivery.uploads` report whether this daemon runs with a job store and
an upload spool (the CLI daemon always does; a bare library embedding
may not). `delivery.pid` reports whether `pid:<ULID>` source references
resolve against a WaxBin catalog (the waxbin flavor with `catalogDB`).

`profiles` are the named delivery profiles: per client family, the
formats verified to play on each surface, in preference order (first is
the default choice), plus the recommended surface (`delivery`). Pick
the profile matching your player stack instead of guessing codecs:
`apple-native` (AVPlayer/Safari), `hls-js` (browser web players),
`android-exoplayer` (Media3), `desktop-mpv` (libmpv/ffmpeg players).
Each profile states the `basis` for its facts; the per-cell evidence
and the manual checklists live in docs/client-matrix.md. Profile lists
are filtered against this build's real outputs, so they never advertise
more than the daemon serves.

## Uploads

`POST /uploads` spools the raw request body as a one-shot source and
returns

    {"schemaVersion": 1, "id": "01J...", "ref": "upload:01J...", "name": "in.flac", "bytes": 123, "expiresAt": 1767225600}

The optional `name` query parameter supplies the original filename (its
extension seeds the probe hint). Reference the upload anywhere a `src`
is accepted, `upload:<id>`. Uploads expire `uploadTTL` after creation
(default 1 h; `expiresAt` is absent when expiry is off) and are capped
by `uploadMaxBytes` each and `scratchMaxBytes` together.
`DELETE /uploads/{id}` removes one immediately (204).

## Jobs

Async full-file work that outlives any request, persisted under
`dataDir/jobs`: completed results survive daemon restarts, and a job
interrupted mid-run restarts cleanly from zero on the next start.

`POST /jobs` with a JSON body:

    {"type": "transcode", "src": "lib/a.flac", "format": "opus", "bitrate": 96}
    {"type": "transcode", "src": "lib/a.flac", "format": "flac", "loudness": "analyze"}
    {"type": "analyze", "src": "lib/a.flac"}

Transcode jobs take the /stream shaping parameters (`format` required,
plus `container`, `rate`, `ch`, `bits`, `bitrate`, `gain`, `flacLevel`)
and, unlike /stream, may target non-streaming formats (`aiff`): job
outputs are seekable files, so every muxer back-patch applies (exact WAV
sizes, FLAC seek tables, the MP4 `iTunSMPB` gapless atom). `loudness:
"analyze"` selects the two-pass form: measure the source, apply the
exact gain to the ReplayGain reference (replacing `gain`), and write
measured `REPLAYGAIN_TRACK_*` tags describing the finished output.
Analyze jobs measure EBU R128 loudness (integrated LUFS, loudness
range, true peak, sample peak) without producing audio.

The response (and `GET /jobs/{id}`) is the job document:

    {"schemaVersion": 1, "id": "01J...", "type": "transcode", "state": "queued",
     "request": {...}, "created": "...",
     "progress": {"phase": "transcode", "done": 123, "total": 456, "percent": 26.9},
     "output": {"file": "out.opus", "mediaType": "audio/ogg", "container": "opus", "bytes": 1, "samples": 1, "rate": 48000},
     "analysis": {"integratedLufs": -17.2, "loudnessRange": 4.1, "truePeakDb": -0.9, ...},
     "error": {"code": "...", "message": "..."}}

States: `queued`, `running`, then one of `done`, `failed`, `canceled`.
`analysis` peak and loudness fields are `null` for digital silence
(negative infinity does not survive JSON). Jobs queue beyond the
`jobSlots` worker count, and running jobs pause between chunks while
the live pool is saturated: interactive streams always win.

`GET /jobs` lists all jobs (`{"schemaVersion": 1, "jobs": [...]}`).
`DELETE /jobs/{id}` cancels the job if running and removes it (204).

`GET /jobs/{id}/events` is a server-sent event stream: one `event: job`
per state or progress update, each `data:` line a full job document,
ending after the terminal event (comment heartbeats every 15 s keep
proxies from timing it out). `GET /jobs/{id}/result` serves a finished
transcode's file (real `Content-Length`, full ranges, strong per-job
`ETag`, `Content-Disposition: attachment`) or an analyze job's analysis
JSON; a queued or running job answers 400, a failed one replays its
error envelope. Both accept signed URLs, because a browser
`EventSource` and a plain download link cannot set headers.

## GET /art, GET /lyrics

`?src=<ref>` (plus optional `id` identity pin; both signable). `/art`
serves the source's embedded cover art verbatim, preferring the front
cover, with its own MIME type and a strong identity-derived `ETag`: a
remote player streaming through WaxFlow has no other channel for
artwork. `/lyrics` serves embedded lyrics as `text/plain` (unsynced
text, or an LRC rendering of synced lyrics when that is all the source
has). Sources without the datum answer 404.

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
