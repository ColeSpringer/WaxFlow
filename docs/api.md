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
| `GET /version` | key | `{"schemaVersion":1,"version":"v1.0.0"}`; `version` is the build's `git describe` stamp (a tag, a tag-commit-SHA, or `dev` for a plain `go build`), not a fixed constant |
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
| `POST /jobs`, `GET /jobs[/{id}]`, `DELETE /jobs/{id}` | key | async full-file transcode/analyze/merge/split jobs |
| `GET /jobs/{id}/events` | key or sig | server-sent job progress events (`EventSource` cannot set headers) |
| `GET /jobs/{id}/result[/{n}]` | key or sig | finished output file (full ranges), or the job's product as JSON when it wrote no file |
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
excluded, `hasLyrics` plus `GET /lyrics` cover it), `hasArt`, and
`hasLyrics`.

`chapters` (`[{"startSeconds", "endSeconds", "title"}]`) needs **no**
mapper: a container that parses chapters surfaces them either way, so a
daemon embedded without one still reports them. A mapper's chapters win
when one is wired, since a tag library may know forms the container
package does not. `endSeconds` is omitted for the start-only chapter
forms (Nero `chpl`) that mean "until the next chapter, or end of
stream"; a caller deriving a span from chapter *n* reads it when present
and chapter *n+1*'s `startSeconds` when not.

`warnings` lists input damage the tolerant parser worked around; `strict`
turns damage into errors. `samples: -1` means unknown length. This is
byte-identical to `waxflow probe --json`.

## GET /stream

    /stream?src=<ref>&format=auto|wav|flac|alac|mp3|aac|opus&rate=&ch=&bits=16|24&bitrate=|q=&container=&gain=&dynamics=&t=&from=&to=&track=&maxBitRate=

Source references (`src`): `<root>/<relative/path>` under a configured
library root; `upload:<id>` for a spooled one-shot upload (POST
/uploads); `pid:<ULID>` for a WaxBin catalog item, served by a build with
a catalog resolver and `catalogDB` configured (`delivery.pid` in
`/caps`) and `501 unsupported-source` everywhere else. A pid reference re-resolves to the
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
- `from`, `to`: bound the stream to a **sample range** of the source, `to`
  exclusive. This is the virtual-track surface: one offset range of one
  file, served as a stream in its own right. `from` defaults to the start
  and `to` to the end; `to=0` is `400` (the span would be empty; omit the
  parameter to mean the end). A range outside the source is `400` rather
  than clamped, since a cut point past the end means the caller's cut
  points describe a different file.

  A span is streamed, not seeked: `plan.Samples`, `X-Content-Duration`,
  and the HLS segment count all describe the span, so
  `/hls/master.m3u8?v={...,"from":10804500,"to":18744300}` is a
  playlist for the virtual track alone. A spanned request never
  direct-plays, including one that only sets `to`.

  **Samples, not seconds, and the difference is load-bearing.** `t` is a
  seek, where a sample either way is meaningless, so it takes seconds. A
  span is not a seek: it declares *which samples are this track*, so it is
  content identity. A CUE boundary at 245.32 s is not exactly
  representable in binary (`245.32 * 44100` floors to 10818611, not
  10818612), which puts a one-sample error at every track boundary of a
  gapless album. That is exactly the failure a CUE split exists to avoid.

  The two compose, with different jobs: `from`/`to` say which samples the
  track is, `t` seeks **within** it.

      /stream?src=rip.flac&from=10804500&to=18744300        the virtual track
      /stream?src=rip.flac&from=10804500&to=18744300&t=30   30 s into it

  A span with no rate change is bit-exact: the same samples a split job
  writes for the same cut points. An Opus or AAC-LC span requested in its
  own `format` goes further and skips the decoder altogether, moving the
  source's own packets (the cut rung below). A **resampled** span is primed rather
  than started cold, which matters because HLS Opus is always 48 kHz and a
  CUE rip is 44.1 kHz, so a virtual track is resampled by construction.
  Unlike a file, a span has real audio ahead of its own first sample, and
  the segmented path feeds the resampler from it and discards the
  pre-roll. Its first samples are therefore the ones a continuous run of
  the whole source delivers at that offset, not a transient out of a
  zero-filled filter window. (The progressive form does not prime, matching
  what `t=` has always done at a seek; HLS is where a virtual track is
  served.)
- `track`: must name the default track until multi-track containers
  land.
- `bitrate`, `q`: lossy quality selection, mutually exclusive. `bitrate`
  is an explicit CBR rate in kbit/s; `q` is a preset (`low` 96, `med`
  128, `high` 192). Both require an explicit lossy `format` (`mp3`,
  `aac`, `opus`); on a lossless output they are `415`. A `bitrate`/`q` request
  forces a re-encode (never direct-play), and the resolved bit rate is
  part of the cache key, so two rates never share an entry.
- `container`: overrides the format's packaging where an alternative
  exists. It requires an explicit `format`, and a name the format cannot
  produce is `400` rather than a silent fall back to the default form.
  A container override never direct-plays: direct play serves the file in
  the wrapper it already has, so an override is always a request for bytes
  rung 1 does not hold. Where the codec survives, rung 2 answers it without
  re-encoding. The available forms, by format:

  | format | `container=` |
  |---|---|
  | `aac` | `adts`, `progressive`, `mka` (empty is fragmented MP4) |
  | `alac` | `progressive` (empty is fragmented MP4) |
  | `flac` | `mka`, `ogg` (empty is native FLAC) |
  | `opus` | `mka`, `webm` (empty is Ogg) |
  | `vorbis` | `mka`, `webm` (empty is Ogg) |
  | `wav` | `mka` (empty is RIFF) |

  `container=adts` selects the raw ADTS elementary stream (`audio/aac`), a
  legacy opt-out for players without fMP4 support; it carries no gapless
  signaling, which is why fMP4 is the default. `container=progressive`
  flattens the MP4 boxes (`moov`+`mdat`, the `.m4a` most players expect)
  and back-patches its header, so it needs a seekable destination and is
  not a streaming form: `/stream` refuses it and a job output takes it.
  `webm` is Opus and Vorbis only, which is webm's audio subset.
- `maxBitRate`: a kbit/s cap for the decision ladder. For direct play the
  cap is checked against whole-file bytes over duration (tags and
  embedded art included): direct play ships the entire file, so the wire
  cost is what the cap protects. A VBR-lossless plan has no fixed rate to
  hold under the cap, so a cap on it is `415` rather than silently
  unenforced.

**Decision ladder (v1)**, cheapest first:

1. **Direct play.** The source already satisfies the request (`format=auto`
   or a matching container, no `container` override, no transforming
   parameters, under `maxBitRate` if given), so the original bytes are
   served: `200`, `Content-Type` per container, strong identity `ETag`,
   `Accept-Ranges: bytes` with full RFC 7233 range and `If-None-Match`
   support.
2. **Transmux.** The codec survives but the wrapper does not: the request
   names a container the source is not in, and nothing asks for a sample
   transform. The container is rewritten around the source's own packets,
   so there is no decode, no re-encode, and no generation loss for a lossy
   source. Gapless trims are carried across and re-signalled in whatever
   the target container uses (iTunSMPB, an edit list, an Ogg pre-skip).
   `format=flac&container=mka` on a FLAC file is this rung, as is
   `format=opus` on an Opus file over HLS.
3. **Transcode.** Anything else: decode, DSP, encode.

**The cut** sits between transmux and transcode for a `from`/`to` span. When
the source codec survives being repositioned (Opus or AAC-LC) and the requested
`format` matches it (`format=opus`, or `format=aac` for AAC-LC), the span is
served by moving the source's own packets into a new stream: no decode, no
re-encode, no generation loss. The head and tail land exactly where asked, since
their snap-to-packet slop becomes the stream's gapless trims. It needs a
source-matching format, because `format=auto` resolves to `wav` and would
transcode, so a span that must not re-encode names its codec explicitly. It
serves both progressive `/stream` and segmented HLS: over HLS each media segment
holds the kept access units, and a worker restarted at a mid-stream segment
reproduces a continuous run's segment bytes exactly, since the packets are the
source's own and need no priming. `waxflow_cut_total` in `/metrics` counts the
pipelines it served, on either surface.

Transmux declines, and the request then tries the cut (for a span) before
falling to a transcode, whenever the codec would not survive (`track.Codec` is
not the output format's), any parameter transforms samples (`rate`, `ch`,
`bits`, `gain`, `dynamics`, `t=`, `bitrate`/`q`), the request names a span
(`from`/`to` cut at an arbitrary sample, which means mid-packet), the URL names
a timeline (one fMP4 timeline carries one edit list, and the packets at a seam
overlap), or `maxBitRate` is set (this rung reads headers, and a source's real
bit rate is in its packets). Over HLS it also declines a source whose packet
durations vary, since there is then no grid to lay segment boundaries on.
`waxflow_remux_total` counts the pipelines it served. The cut declines in turn
(a codec off the Opus/AAC-LC allowlist, `maxBitRate` set, a snapped window the
destination cannot express, or over HLS a source whose packet durations vary and
so give no grid to lay segment boundaries on) and falls to a transcode of the
same span, so a span is always served: zero-generation when it can be,
sample-exact through the decoder when it cannot.

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
     "ch":2, "gain":"track", "dynamics":"off", "segDur":4,
     "from":10804500, "to":18744300}

`from`/`to` are the virtual-track span: samples on the source timeline,
`to` exclusive and omitted for the end. They mint from `from=`/`to=`
parameters exactly as `/stream` spells them, and they are `int64` samples
rather than `segDur`'s float seconds for the reason `/stream` gives above
(a span is content identity, a segment duration is a target the plan snaps
anyway). The playlist then describes the span alone: segment 0 is the
span's first sample and the segment count covers `to-from`. A span is
exclusive with `tl`, since it bounds one source and a timeline is several.

`bitrates` (the ladder) appears only in master URLs; every other URL is
per-variant. Descriptors are minted by the daemon (`POST /sign` with
`path:/hls/master.m3u8`, or the keyed raw-parameter master form below),
never hand-built by clients. Auth is an API key or the signed query,
like `/stream`; playlist child URLs come out signed (when signing is
configured) with the parent's expiry, so one minting governs a playback
session.

| Path | Purpose |
|---|---|
| `GET /hls/master.m3u8?v=...` | master playlist: one rung per ladder bitrate with `BANDWIDTH` and `CODECS` (`Opus`, `fLaC`, `alac`, `mp4a.40.2`). With an API key, raw parameters (`src`, `format`, `bitrate` or `bitrates`, `bits`, `rate`, `ch`, `gain`, `dynamics`, `segDur`, and `crossfadeSeconds` for a `tl` timeline) also work; the daemon builds the descriptor. |
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

    201 {"schemaVersion": 1, "tl": "kJ3n...pQ", "members": 2, "durationSeconds": 2500,
         "envelopeRate": 44100,
         "boundaries": [{"offsetSamples": 0, "durationSamples": 66150000},
                        {"offsetSamples": 66150000, "durationSamples": 44100000}]}

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
- **`boundaries` say where each member lands.** `envelopeRate` is the
  timeline's normalized rate (the maximum member rate), and each
  `boundaries[i]` gives that member's `offsetSamples` (its start on the
  timeline) and `durationSamples` (its own length), both on that clock, so
  a client need not probe the members itself. They are derived from the
  measured members, not part of the identity, so the digest does not cover
  them. With no crossfade the members tile exactly:
  `offsetSamples[i+1] == offsetSamples[i] + durationSamples[i]`, and the
  last member's end is `durationSeconds * envelopeRate`. Under a crossfade
  the members overlap, so `offsetSamples + durationSamples` can exceed the
  next member's `offsetSamples`. Build on the offsets, not on the tiling.
- **`crossfadeSeconds` blends the seams, and is a render option.** Pass it
  in the mint body to shape this response's `durationSeconds` and
  `boundaries`: an equal-power blend of `crossfadeSeconds` at each of the
  `members - 1` seams shortens the timeline by `(members - 1) *
  crossfadeSeconds`. Then pass the **same** value on the signed
  `master.m3u8` you build, exactly as you pass `format`, because the mint's
  boundaries reflect the crossfade minted with and a stream rendered with a
  different one disagrees with them. It is a render option, not identity:
  the digest covers the members alone, so a queue minted with two different
  crossfades keeps one `tl` and the two renders are kept apart by the cache.
  Omit it (or send `0`) for a gapless butt-join, which is the default and
  what a gapless album needs. It is refused on a single `src` URL (which has
  no seam, being one file rather than a queue), when it is longer than the
  shortest member can carry, or when it exceeds the largest blend buffer
  (tens of seconds, the one bound that applies to a timeline of any length).
  A one-member timeline is not refused for want of a seam, though: it has zero
  seams, so an ordinary crossfade meets none of them (a butt-join), which lets
  a queue-driven client send one render config whatever the queue length. Only
  the blend-buffer bound can still refuse it.
- **`202` means the mint had to measure.** A timeline's positions are a
  prefix sum, so every member's length is measured rather than read off
  its headers. That is a sub-millisecond walk for formats whose demuxer
  can find its end from a table (FLAC, WAV, Ogg, mp4), and a whole-file
  scan for MP3. When a cold queue needs enough of the latter to be worth
  it, the response is `202` with a job instead of `201` with a digest;
  poll `GET /jobs/{id}` or follow its events, and the finished job's
  `timeline` field carries the same values the `201` body would. The
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
`/jobs/<id>/events` / `/jobs/<id>/result` / `/jobs/<id>/result/<n>` (no
`params`; the signature pins the job id, and the index, through the path;
the index is checked for shape only, since a URL worth signing is minted
while the job is still queued and has no outputs yet). `params` are validated like a live
request, the source identity is resolved and embedded, and the response
is:

    {"schemaVersion": 1, "url": "/stream?exp=...&format=wav&id=...&kid=1&sig=...&src=...", "exp": 1767225600}

## GET /caps

    {
      "schemaVersion": 1,
      "inputs": ["flac", "wav", "aiff", "ogg", "mp4", "mka", "adts", "mp3"],
      "decoders": ["pcm", "flac", "mp3", "alac", "aac-lc", "vorbis", "opus"],
      "outputs": [{"name": "wav", "live": true, "exts": ["wav", "wave", "rf64", "bw64"]},
                   {"name": "opus", "live": true, "exts": ["opus"]},
                   {"name": "vorbis", "live": true, "exts": ["ogg", "oga"]},
                   {"name": "aiff", "live": false, "exts": ["aif", "aiff", "aifc", "afc"]},
                   {"name": "flac", "live": true, "exts": ["flac"]},
                   {"name": "mp3", "live": true, "exts": ["mp3", "mpga"]},
                   {"name": "aac", "live": true, "exts": ["m4a", "aac"]},
                   {"name": "alac", "live": true, "exts": []}],
      "delivery": {"progressive": true, "hls": true, "hlsFormats": ["opus", "flac", "aac", "alac"],
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
resolve against a WaxBin catalog (a build with a catalog resolver and
`catalogDB`).
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
    {"type": "merge", "srcs": ["lib/ch1.flac", "lib/ch2.flac"], "format": "alac"}
    {"type": "split", "src": "lib/album.flac", "format": "flac", "cuts": [10584000, 21344400]}
    {"type": "split", "src": "lib/album.flac", "format": "flac", "cue": "lib/album.cue"}

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

Each field belongs to a specific set of job types, and a field on a type
that does not take it is a 400 rather than a field silently ignored at
run time: `srcs` and `titles` are merge-only, `cuts` and `cue` split-only
(and exclusive with each other), the silence fields analyze-only, `gain`
and `loudness` transcode-only, and the shaping parameters belong to
transcode, merge, and split. `src` is required by every type but merge,
which refuses it.

### Merge and split

`merge` concatenates `srcs` into one output, gaplessly: it is the same
timeline primitive `tl=` streams over HLS, pointed at a file. Members are
normalized to a common envelope (the maximum rate and channel count, so
no member loses information), and their lengths are **measured** rather
than trusted, since a timeline's positions are a prefix sum and an
advisory length that is two samples out desyncs every seam after it. That
measuring is what a `POST /jobs` for a cold MP3 queue waits on; a queue
already minted as a timeline (or any queue of FLACs) measures nothing.

`split` cuts one `src` at `cuts` into N+1 outputs. Cut points are **source
sample offsets, strictly ascending, and interior**: `0` is implied before
the first and the source's end after the last, so N cuts make N+1 pieces
and piece *i* runs `[cuts[i-1], cuts[i])`. A leading `0`, a cut at or past
the end, a repeat, or a descending pair each ask for an empty piece or for
samples the source does not have, and are all a 400 at creation rather
than a job that dies on piece 7. Samples rather than seconds for the same
reason a span is: a cut declares which samples are this piece, so it is
content identity, and 245.32 s at 44100 floors to one sample short of the
boundary a CUE sheet's CD-frame arithmetic names exactly.

**`cue` names a CUE sheet instead**, exclusive with `cuts`. It is a source
reference like `src`, so the sheet can be uploaded (`upload:<id>`) or sit
in a library root beside its rip, and it resolves through the same
resolver. The sheet's track boundaries become this split's cut points at
creation: the first track's start is dropped (it is the implied 0, and a
cut there would ask for an empty piece), so a sheet of N tracks yields
N-1 cuts and N pieces.

The daemon parses the sheet rather than making each client do it, and that
is the point of the field. A CD frame is 1/75 s, which no nanosecond clock
can represent, so the obvious conversion lands a sample short at every
boundary; every CD-family rate divides by 75 exactly (44100/75 = 588), so
frames convert to samples directly and exactly. Pushing that to each
client is pushing each client to rediscover the same off-by-one. Sheets
are decoded best-effort (BOM stripped, UTF-8 when valid, CP1252
otherwise), and a sheet indexing several files is refused: its tracks are
already separate, so there is nothing to cut.

The sheet does not reach the job. It is resolved into `cuts`, which is
what the job carries, so `GET /jobs/{id}` shows the boundaries the 201
accepted and an edit to the sheet afterward cannot change what runs.
Sending `cue` and sending the samples it means produce the identical job.

The pair is exact, and that is the property worth having: **a split to a
lossless format at the source rate rejoins bit for bit through a merge.**
No resampler and no limiter means nothing to prime, so each piece's sample
0 is the source's sample `from` exactly. Ask for a rate change and each
piece carries the same ~1.5 ms head transient a seek already does.

Neither takes `gain` or `loudness`. Both answer "how loud should this one
track be", against that track's own measurement or its own ReplayGain
tags; a merge has N tracks in and one file out and a split one in and N
out, so either would have to apply one source's number to things it does
not describe. Normalize with a transcode job after the cut, where the
numbers name what they measure. Neither carries source tags, and a split
carries no chapters; an mp4-family merge stamps chapter markers, one per
member (below).

An mp4-family merge (`alac`, `aac`) writes the **flat** MP4 form by
default, `container: "progressive"`, which is what the job document will
say. That is the shape most players expect from an `.m4a`/`.m4b`, and the
only one that can carry a chapter text track; the fragmented CMAF form is
the row default only because `/stream` needs a container that streams, and
a job writes to a seekable file where the flat form's back-patch applies.
An explicit `container` wins, which also means a merge cannot ask for the
fragmented form: omitting the field is how you get the flat one.

That chapter text track is one chapter per member, at the member's start
on the concatenated timeline (the same boundaries `POST /hls/timeline`
reports). Title them with the optional `titles`, a string per member
index-aligned to `srcs` (a title per member, or none, else a 400). A
member's chapter title is its `titles` entry when non-empty, else the
member's own `TITLE` tag when a metadata mapper is wired, else a generated
`Chapter N`. An empty `titles[i]` does not force a blank title: it falls
through to the tag or the generated name, since the precedence cannot spell
"deliberately blank". A title past the text track's per-entry byte cap is
truncated on a rune boundary. Chapters are stamped only on the
flat/progressive form (the one that carries the track), so a merge to any
other format writes none and reads no per-member titles.

A fifth type, `timeline`, appears in `GET /jobs` but cannot be created
here: `POST /hls/timeline` creates one on its own when a cold queue needs
enough measuring to be worth it (see the HLS section). Its product is the
`timeline` field, `{"tl", "members", "durationSeconds"}`, rather than an
output file, since a timeline lives in the timeline store under the digest
that is its identity. The restart contract still holds, by
content-addressing rather than by the usual rule: re-running the mint from
zero writes the same digest.

Every job's sources are pinned by identity at creation, so a source
replaced before the job ran fails with `source-changed` rather than
quietly producing different audio. A merge pins each member, and the error
names the index that moved (`member 2 (lib/ch3.flac): the source changed
since the job was created`): naming one of forty candidates is the
difference between re-uploading a file and re-uploading a library.

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

    {"schemaVersion": 2, "id": "01J...", "type": "transcode", "state": "queued",
     "request": {...}, "created": "...",
     "progress": {"phase": "transcode", "done": 123, "total": 456, "percent": 26.9},
     "outputs": [{"file": "out.opus", "mediaType": "audio/ogg", "container": "opus", "bytes": 1, "samples": 1, "rate": 48000}],
     "analysis": {"integratedLufs": -17.2, "loudnessRange": 4.1, "truePeakDb": -0.9, ...},
     "error": {"code": "...", "message": "..."}}

`outputs` is a list because a split has one entry per piece, in cut order;
every other type has at most one. Its index is what `GET
/jobs/{id}/result/{n}` takes.

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
proxies from timing it out).

`GET /jobs/{id}/result/{n}` serves the job's nth output file (real
`Content-Length`, full ranges, strong per-output `ETag`,
`Content-Disposition: attachment`): a transcode's or a merge's audio, one
piece of a split, or an analyze job's `silence.json`. An index the job
does not have is a 404. A queued or running job answers 400, a failed one
replays its error envelope.

`GET /jobs/{id}/result`, no index, answers **only where it cannot be
wrong**: the single output of a job that has one, and a 400 naming the
indexed form for a job with several. Handing back piece 1 of 12 to a
caller who never learned the pieces existed would be a plausible-looking
wrong answer, and refusing is what the daemon does with every other
ambiguity it meets. A job whose product is not a file answers with the
product as JSON instead: an analyze job's `analysis`, a timeline job's
`{"tl", "members", "durationSeconds"}`.

Both accept signed URLs, because a browser `EventSource` and a plain
download link cannot set headers. `/jobs/<id>/result/<n>` is signable, so
a split's pieces can be handed out as N links.

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
`waxflow_direct_play_total`, `waxflow_remux_total`,
`waxflow_hls_segments_total`,
`waxflow_cache_{hits,misses}_total`, `waxflow_cache_{bytes,entries}`,
`waxflow_admission_rejects_total`, `waxflow_admission_in_use{pool}`,
`waxflow_session_degradations_total`, `waxflow_ttfb_seconds` histogram.
