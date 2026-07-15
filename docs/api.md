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
| `POST /hls/timeline` | key | mint a multi-source timeline (a play queue) into a `tl` digest |
| `GET /cache/stats`, `POST /cache/gc` | key | cache operations |
| `GET /metrics` | key or metricsKey | Prometheus text exposition |
| `GET /demo` | none (dev mode only, `demo: true`) | browser test page |
| `POST /uploads`, `DELETE /uploads/{id}` | key | spool one-shot sources; reference as `src=upload:<id>` |
| `POST /jobs`, `GET /jobs[/{id}]`, `DELETE /jobs/{id}` | key | async full-file transcode/analyze jobs |
| `GET /jobs/{id}/events` | key or sig | server-sent job progress events (`EventSource` cannot set headers) |
| `GET /jobs/{id}/result` | key or sig | finished output file (full ranges) or analysis JSON |
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

    /stream?src=<ref>&format=auto|wav|flac|alac|mp3|aac|opus&rate=&ch=&bits=16|24&bitrate=|q=&container=&gain=&dynamics=&t=&track=&maxBitRate=

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
  number. `track` and `album` resolve against the source's ReplayGain
  2 tags (Opus `R128_*` tags convert from the -23 LUFS reference), fall
  back from album to track, and resolve to 0 dB when the source carries
  none. Exact measured loudness belongs to jobs (`loudness: "analyze"`).
  Positive gain engages the true-peak limiter and is **clamped at
  +12 dB, or +24 dB with `dynamics=voice`** (see below). Read the
  ceiling off `/caps` (`dsp.gainMaxDb`, `dsp.gainMaxVoiceDb`) rather
  than assuming one.
- `dynamics`: `off` (default) or `voice`. `voice` is a spoken-word
  leveller: a gentle 2.5:1 compressor with makeup gain, meant to make an
  audiobook or podcast intelligible at low volume, where the quiet half
  of a wide-range reading otherwise falls under the room. It is
  deliberately audible: that is the feature. It always engages the
  true-peak limiter.

  **Dynamics acts on the post-gain signal, and composes with `gain`
  rather than replacing it.** The preset's curve has a fixed threshold,
  so level the signal to a known point first and let the preset shape it:

      /stream?src=book.m4b&format=opus&gain=-6.2&dynamics=voice

  The daemon cannot do the levelling for you on a live stream, because it
  cannot measure one before serving it; that is what analyze jobs are
  for. A client with a measured loudness sends the exact dB.

  **The gain ceiling is a function of this parameter**, which is worth
  stating plainly rather than leaving to be discovered: `gain=16` alone
  resolves to +12 dB, and `gain=16&dynamics=voice` resolves to 16. The
  +12 bound is calibrated for ReplayGain-style music normalization, where
  more is amplifying noise. Spoken word is a different taste: amateur
  podcast and audiobook recordings sit near -30 LUFS routinely and cannot
  reach a -14 LUFS target in one pass under +12. Declaring the source is
  speech raises the ceiling to +24. Both bounds are taste rather than
  safety: the true-peak limiter is what makes either one safe, and a
  dynamics preset always engages it, so the higher ceiling cannot clip.

  A `dynamics` request never direct-plays: unlike `gain=track` on an
  untagged file, which resolves to 0 dB and is a genuine no-op, a preset
  has no no-op state.
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
     "ch":2, "gain":"track", "dynamics":"off", "segDur":4}

`bitrates` (the ladder) appears only in master URLs; every other URL is
per-variant. Descriptors are minted by the daemon (`POST /sign` with
`path:/hls/master.m3u8`, or the keyed raw-parameter master form below),
never hand-built by clients. Auth is an API key or the signed query,
like `/stream`; playlist child URLs come out signed (when signing is
configured) with the parent's expiry, so one minting governs a playback
session.

| Path | Purpose |
|---|---|
| `GET /hls/master.m3u8?v=...` | master playlist: one rung per ladder bitrate with `BANDWIDTH` and `CODECS` (`Opus`, `fLaC`, `alac`, `mp4a.40.2`). With an API key, raw parameters (`src`, `format`, `bitrate` or `bitrates`, `bits`, `rate`, `ch`, `gain`, `dynamics`, `segDur`) also work; the daemon builds the descriptor. |
| `GET /hls/media.m3u8?v=...` | variant VOD playlist: `EXT-X-VERSION:7`, `EXT-X-MAP`, `EXT-X-INDEPENDENT-SEGMENTS`, every segment listed with its exact duration, `EXT-X-ENDLIST`. Unknown source lengths are measured (frame-index walk), never estimated. |
| `GET /hls/init.mp4?v=...` | the CMAF init header (codec config; the edit list carries encoder delay and the exact length) |
| `GET /hls/seg/{n}.m4s?v=...` | media segment n (0-based). Cached segments serve with ranges and strong ETags; misses wait on the variant worker (within a 3-segment lookahead) or restart it at n. |

Segments are `styp` + `moof`+`mdat` fragments, boundaries snapped to
whole encoder frames (`segDur` is a target; the playlist carries exact
durations), all sync samples, decode timeline in sample units. Formats
with a segmented form: `opus`, `flac`, `alac`, `aac` (see `/caps`
`delivery.hlsFormats`). A `410 source-changed` means the file changed
since minting: re-mint and reload.

### Multi-source timelines (`tl`)

A play queue can be streamed as one gapless timeline: several files
delivered as a single continuous stream, so a gapless album crosses its
track boundaries with no seam, no re-buffer, and no gap. It is one media
playlist, one init header, and one edit list, with no
`EXT-X-DISCONTINUITY` anywhere, because a concatenated timeline really is
continuous.

Mint the queue first, then sign a master against the digest:

    POST /hls/timeline
    {"srcs": [{"src": "lib/Album/01.flac"}, {"src": "lib/Album/02.flac"}]}

    201 {"schemaVersion": 1, "tl": "kJ3n...pQ", "members": 2, "durationSeconds": 2998.5}

    POST /sign
    {"path": "/hls/master.m3u8", "params": {"tl": "kJ3n...pQ", "format": "opus", "gain": "off"}}

`tl` and `src` are exclusive: a URL names one stream or one timeline.
Everything else (`format`, `bitrate`/`bitrates`, `bits`, `rate`, `ch`,
`dynamics`, `segDur`) means what it means for a single source.

Things worth knowing before you build on it:

- **The digest is the identity.** It covers every member's reference and
  its source identity, so a member replaced on disk yields a different
  digest and the old URL is a `410 source-changed`, exactly as for a
  single source. Minting the same queue twice returns the same digest, so
  a client that did not keep it pays nothing to ask again.
- **`gain=track` and `gain=album` are refused.** A timeline is one
  processing chain, so it has one gain, and there is no honest single
  answer to read out of several members' tags; per-track gain would step
  the level at every track boundary, which is the artifact album gain
  exists to prevent. Pass `gain=off` or the dB you want. Note that this
  bites a request with no `gain=` at all when the daemon's default is a
  tag mode, which is why the refusal names the default rather than a
  parameter you did not send.
- **Members are normalized, not refused.** Mixed rates, channel counts,
  and bit depths are converted to the envelope no member loses information
  reaching (the maximum of each). A uniform queue, which is what a gapless
  album is, is passed through untouched and costs nothing.
- **`202` means the mint had to measure.** A timeline's positions are a
  prefix sum, so every member's length is measured rather than read off
  its headers. That is a sub-millisecond walk for formats whose demuxer
  can find its end from a table (FLAC, WAV, Ogg, mp4), and a whole-file
  scan for MP3. When a cold queue needs enough of the latter to be worth
  it, the response is `202` with a job instead of `201` with a digest;
  poll `GET /jobs/{id}` or follow its events, and the finished job's
  `timeline` field carries the same three values the `201` body would. The
  cost is once per file, so the same queue mints in one round trip
  afterwards.
- **`404 not-found` on a timeline means re-mint it.** A stored timeline
  outlives every URL minted against it, so a correct client does not hit
  this during normal playback; it means the daemon's store was wiped or
  the timeline aged out unused. Re-mint from the queue you still have and
  carry on. Do **not** reset the queue position: the digest is a function
  of the members, so the re-minted timeline is the same timeline.
- **`maxTimelineMembers`** (in `/caps`) bounds one timeline at 1000. A
  timeline is a play queue, not a library.

## POST /sign

    {"path": "/stream", "params": {"src": "lib/a.flac", "format": "wav"}, "ttlSeconds": 3600}

`path` defaults to `/stream`. Also signable: `/hls/master.m3u8` (its
`params` are the raw HLS master parameters above, `src` or `tl`, and
every rung is planned at mint time so a URL that mints is a URL that
plays), `/art`
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
                   "jobs": false, "uploads": false, "pid": false,
                   "timelines": true, "maxTimelineMembers": 1000},
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
`delivery.timelines` reports whether `POST /hls/timeline` and the `tl`
parameter are served, and `maxTimelineMembers` bounds one timeline.

`dsp` is the signal-processing surface, so a format policy routes by
capability instead of sniffing a version:

    "dsp": {
      "gainModes": ["off", "track", "album"],
      "gainMaxDb": 12, "gainMaxVoiceDb": 24,
      "dynamics": ["off", "voice"],
      "loudness": ["analyze"],
      "truePeakCeilingDb": -1
    }

Every advertised value parses, which is why `gainModes` does not list a
`"<db>"` placeholder for the scalar escape hatch: a dB number is always
accepted, and the two ceilings are what a client actually needs to know
about it. **Read both ceilings, not one.** The clamp on positive gain
depends on `dynamics` (see /stream), so `gainMaxDb` alone does not tell
you whether `gain=16` is legal. `loudness` is jobs-only by construction:
a live stream cannot be measured before it is served.

`dsp` is deliberately orthogonal to `profiles`. Profiles are about what a
client can decode; dynamics is server-side and client-agnostic, so no
profile has anything to say about it.

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
    {"type": "analyze", "src": "lib/a.flac", "silence": true}

Transcode jobs take the /stream shaping parameters (`format` required,
plus `container`, `rate`, `ch`, `bits`, `bitrate`, `gain`, `flacLevel`)
and, unlike /stream, may target non-streaming formats (`aiff`): job
outputs are seekable files, so every muxer back-patch applies (exact WAV
sizes, FLAC seek tables, the MP4 `iTunSMPB` gapless atom). `loudness:
"analyze"` selects the two-pass form: measure the source, apply the
exact gain to the ReplayGain reference (replacing `gain`), and write
measured `REPLAYGAIN_TRACK_*` tags describing the finished output.
Analyze jobs measure EBU R128 loudness (integrated LUFS, loudness
range, true peak, sample peak) without producing audio. Each field
belongs to exactly one job type: a transcode field on an analyze job is
a 400, and so is a silence field on a transcode job.

A third type, `timeline`, appears in `GET /jobs` but cannot be created
here: `POST /hls/timeline` creates one on its own when a cold queue needs
enough measuring to be worth it (see the HLS section). Its product is the
`timeline` field, `{"tl", "members", "durationSeconds"}`, rather than an
output file, since a timeline lives in the timeline store under the digest
that is its identity. The restart contract still holds, by
content-addressing rather than by the usual rule: re-running the mint from
zero writes the same digest.

### Silence detection

`silence: true` on an analyze job maps the source's silent spans from
the same decode the loudness measurement runs on, so it costs no extra
I/O. Two optional parameters shape it:

| field | default | range |
|---|---|---|
| `silenceThresholdDb` | -50 | -90 up to (not including) 0 |
| `silenceMinSeconds` | 0.5 | above 0, at most 60 |

Either without `silence: true` is a 400 rather than a silently ignored
field. These are raw numbers rather than named levels, unlike `gain` and
`dynamics`, and deliberately: a closed vocabulary belongs where a value
enters a cache key or a validated signal path, where it must mean the
same thing forever. These do neither. They shape a report, nothing is
keyed by them, and **the right threshold is a property of the content**,
which the caller knows and the daemon does not, because what counts as
silence is wherever the source's own noise floor sits: a studio podcast or
any clean digital capture near -80 dBFS, an audiobook's room tone near
-55, an analog or vinyl transfer near -40.

Getting it wrong does not fail cleanly. A threshold below the source's
noise floor makes the signal cross it repeatedly, which fragments one
long silence into many short ones that each fall under
`silenceMinSeconds` and are dropped, reporting a plainly quiet source as
having **no silence at all**. `droppedSeconds` is how that shows up: read
it against the source's duration. A healthy source leaves it a fraction
of a percent; a sizeable share of the stream means the threshold is
wrong for this source rather than that the source is loud.

Read `dropped` (the count) only alongside it, never alone. Ordinary audio
dips under any threshold at every zero crossing, so even a clean source
with well-formed silences drops hundreds of one-sample runs per second
and the count is large either way.

The job carries only the summary:

    "silence": {"version": "silence-1", "thresholdDb": -50, "minSeconds": 0.5,
                "spans": 12, "dropped": 431, "droppedSeconds": 0.009, "totalSeconds": 84.2}

The spans themselves are the job's **output file**, served by `GET
/jobs/{id}/result`, because a 40-hour audiobook pausing every 30 s is
~4800 spans and the job document is broadcast whole on every progress
event. It is `silence.json`:

    {"schemaVersion": 1, "version": "silence-1", "thresholdDb": -50, "minSeconds": 0.5,
     "rate": 44100, "samples": 220500, "durationSeconds": 5,
     "spans": [{"fromSample": 44100, "toSample": 88200, "fromSeconds": 1, "toSeconds": 2}],
     "dropped": 431, "droppedSeconds": 0.009, "totalSeconds": 3}

Spans are half-open (`toSample` exclusive) and carry both spellings.
The samples are the exact ones, and are what a cut-point list wants;
the seconds are the convenience. `version` is the detector revision: a
caller caching the map needs it to know when the map went stale.
Detection matches ffmpeg's `silencedetect` exactly (a frame is silent
when every channel is strictly inside the threshold band), which is what
lets the differential test assert span counts rather than approximate
them.

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
proxies from timing it out). `GET /jobs/{id}/result` serves the job's
output file (real `Content-Length`, full ranges, strong per-job `ETag`,
`Content-Disposition: attachment`), which is a transcode's audio or an
analyze job's `silence.json`, and an analyze job's analysis JSON when it
produced no file; a queued or running job answers 400, a failed one
replays its error envelope. Both accept signed URLs, because a browser
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
`{"schemaVersion":1,"removed":n,"freedBytes":n,"timelinesRemoved":n}`.
It also sweeps stored timelines past their expiry, which is why the last
field is there: timelines are not cache entries and free no cache bytes,
so they are counted apart, but they are too small and too rarely minted
to be worth a janitor of their own.

## GET /metrics

Prometheus text: `waxflow_build_info`, `waxflow_sessions_active`,
`waxflow_sessions_total{kind}` (live, sync, hls),
`waxflow_direct_play_total`, `waxflow_hls_segments_total`,
`waxflow_cache_{hits,misses}_total`, `waxflow_cache_{bytes,entries}`,
`waxflow_admission_rejects_total`, `waxflow_admission_in_use{pool}`,
`waxflow_session_degradations_total`, `waxflow_ttfb_seconds` histogram.
