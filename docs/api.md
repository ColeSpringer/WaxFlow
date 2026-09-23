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
  unsupported-source malformed-input payload-too-large source-unreadable
  output-unwritable overloaded canceled catalog-unavailable internal`.
- `unsupported-format` and `malformed-input` are different answers and are
  worth telling apart: the first is a well-formed stream this build does not
  cover (a codec it has no decoder for, a spec its encoders cannot produce),
  which a client can act on by asking for something else; the second is a file
  whose bytes deviate from their own format, which nothing can convert.
- Status mapping: 400 invalid-request, 401 unauthorized, 403
  signature-invalid/expired, 404 not-found, 410 source-changed, 413
  payload-too-large, 415 unsupported-format, 416 (range refusal, code
  invalid-request), 422 malformed-input, 501 unsupported-source, 503
  overloaded (with `Retry-After: 2`) and catalog-unavailable, 500 the rest.
- A path that exists under other methods answers **405** with an `Allow`
  header listing them, and code `invalid-request` (no code maps to 405).
  A path no endpoint claims stays 404 `not-found`.

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
| `POST /roots/reload` | key | reconcile library roots from the re-read config (present only when wired; see `delivery.rootsReload`) |
| `GET /metrics` | key or metricsKey | Prometheus text exposition |
| `GET /demo` | none (dev mode only, `demo: true`) | browser test page |
| `POST /uploads`, `DELETE /uploads/{id}` | key | spool one-shot sources; reference as `src=upload:<id>` |
| `POST /jobs`, `GET /jobs[/{id}]`, `DELETE /jobs/{id}` | key | async full-file transcode/analyze/merge/split jobs |
| `GET /jobs/{id}/events` | key or sig | server-sent job progress events (`EventSource` cannot set headers) |
| `GET /jobs/{id}/result[/{n}]` | key or sig | finished output file (full ranges), or the job's product as JSON when it wrote no file |
| `GET /art`, `GET /lyrics` | key or sig | embedded cover art / lyrics passthrough (raw bytes, ETag'd, no resizing) |

## GET /probe, POST /probe

`GET /probe?src=<ref>[&strict=1]` or `POST /probe` with
`{"src": "<ref>", "strict": false}`. Both forms reject names they do not
know: `src` and `strict` are the whole surface, and a misspelled `strict`
would otherwise return the opposite of what was asked.

    {
      "schemaVersion": 1,
      "container": "wav",
      "tracks": [{
        "id": 0, "codec": "pcm", "rate": 44100, "channels": 2,
        "layout": "FL|FR", "sampleType": "int", "bitDepth": 16,
        "samples": 2205, "durationSeconds": 0.05, "default": true,
        "samplesExact": true
      }],
      "warnings": ["..."],
      "notes": ["..."]
    }

The body also carries the source's tag summary when present: `tags`
(canonical uppercase keys to value lists, ReplayGain included; the lyric
sheet is excluded, `hasLyrics` plus `GET /lyrics` cover it), `hasArt`,
and `hasLyrics`.

`tags` and `chapters` (`[{"startSeconds", "endSeconds", "title"}]`) both
need **no** mapper: a container that parses them surfaces them either
way, so a daemon embedded without one still reports what the file
carries. A mapper's values win when one is wired, since a tag library may
know forms and containers the container package does not. `endSeconds` is
omitted for the start-only chapter forms (Nero `chpl`, Musepack SV8
chapter packets, ASF markers) that mean "until the next chapter, or end
of stream"; a caller deriving a span from chapter *n* reads it when
present and chapter *n+1*'s `startSeconds` when not.

`hasArt` and `GET /art` do still need a mapper, and that is deliberate
rather than an oversight: `hasArt` reports what `/art` **can serve**, and
`/art` serves only what the mapper hands it, so reporting `true` from a
cover-art atom the container merely saw would promise a payload the
endpoint then 404s. Text tags are what the container fallback covers.

`bitDepth` is the depth the **source** stores samples at. It is not
always the depth the pipeline decodes to: the signal path is float32, so
a 64-bit float source decodes at 32 bits and reports `bitDepth: 64`.
The companded and block-coded codecs go the other way: a G.711 track
reports `bitDepth: 8` and an IMA or MS ADPCM track `bitDepth: 4`, both
with `sampleType: "int"`, while all three decode to 16-bit samples. Those
are the numbers `ffprobe` puts in `bits_per_sample` for the same files.
`sampleType` (`"int"` or `"float"`) is the float discriminator; a client
must not infer one from a depth.

`warnings` lists input damage the tolerant parser worked around, and
`strict` turns damage into errors. On a frame-indexed payload (MP3, bare
or in a WAV or AIFF-C; ADTS) `strict` also walks the payload to its end, so
its verdict covers the whole file at the cost of reading it; without it a
probe reads headers, and damage past the head of such a payload is reported
by the read that reaches it (a transcode or analyze job lists it under
`warnings` with an `input damage:` prefix once the read has covered the
file; a merge job prefixes each finding with the member it came from,
`member 1:`). A strict probe takes a live slot for its walk, the way a
stream does, so a daemon whose pool is full answers it `503 overloaded`; a
tolerant probe takes none.
A strict probe of a frame-indexed payload reports the walked length as
exact: a count its headers stated is confirmed, or replaced by what a read
delivers when the run comes up short, which is reported as damage. Every
Matroska track joins those payloads: reading one exactly means walking its
clusters, which no open pays, so a tolerant probe reports the Info Duration
instead, or `samples: -1` where the file states none, and a strict one walks.
A read of such a file is gapless either way, because Matroska states its tail
trim per block rather than as a total. Any block may carry one, not just the
last (mkvmerge writes one at each append seam), and a laced block's trim
spreads over its laces from the last one backwards, as the spec places it at
the block's end; the timeline excludes the frames it names, so a seek across
one lands exactly. A packet copy of such a file into anything but Matroska is
declined for the transcode rung, which trims in PCM; a cut is declined the
same way when a kept packet carries a trim, and serves any destination when
the trimmed packets are dropped. `samplesExact` and `samplesAdvisory` say which kind
of number `samples` is: `samplesExact` a measured one, `samplesAdvisory` a
rounded total fit for display and not for arithmetic that has to add up
(ASF's ticks, a Matroska Info Duration, a WAV fact chunk over MP3 frames),
and neither the headers' own count taken at its word. FLAC, WavPack and
Ogg-FLAC report `samplesExact` from a tolerant probe: their opens verify the
declared total against the payload in both directions, from one read of the
tail, and report a shortfall as damage and an overrun as a note. Ogg-FLAC
checks STREAMINFO's total against the stream's final page granule, which is
the muxer's own statement of the playable end on a CRC-verified page; a read
finds any damage earlier in the file, where it is. A native FLAC or WavPack
whose measurement had to cross damage keeps the corrected length without the
flag, since an authoritative length caps a decode and one walked past a hole
should not. A fragmented MP4 states its length in an edit list where it has
one; where it does not, a segment index (`sidx`) covering the whole file is
read as the headers' own count, the same declared tier a progressive sample
table sits in. An index covering only the first fragment is ignored rather
than believed, and a movie header duration is advisory. A strict probe walks
the fragments once, which reads their headers and reports either count as
`samplesExact`. `notes` is the other half and is never damage: what this build did
with a file that is perfectly well formed, such
as ignoring a stream it cannot use, capping a chapter list at its own limit,
rescaling a timeline whose timescale is not the sample rate, or reporting a
band it does not synthesize. `strict` never refuses over a note, which is the
whole reason the two are separate fields; a client leading with "input
damage" wants `warnings` alone. `samples: -1` means unknown length. This is
byte-identical to `waxflow probe --json`.

## GET /stream

    /stream?src=<ref>&format=auto|wav|flac|alac|wavpack|mp3|aac|he-aac|opus|vorbis&rate=&ch=&bits=16|24&bitrate=|q=&hev2=&container=&gain=&dynamics=&t=&from=&to=&track=&maxBitRate=

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
  gapless trims); `he-aac` an HE-AAC v1 stream in the same wrappers
  (64 kbit/s default, output rates 32/44.1/48 kHz; a copyable HE-AAC
  source under `format=aac` remuxes rather than re-encoding, so
  `he-aac` is for encoding *to* SBR), or HE-AAC v2 under `hev2=1`
  (parametric stereo over a mono SBR core, 32 kbit/s default, stereo
  sources only); `opus` an Ogg-Opus stream (`audio/ogg`, 96 kbit/s
  default, `bitrate`/`q` select it) from the full Opus encoder: SILK,
  hybrid, and CELT modes with analyser-driven speech/music selection;
  `wavpack` a lossless native .wv stream (`audio/x-wavpack`), whose
  compression level is a job or CLI parameter rather than a /stream one,
  like FLAC's. Its metadata is an APEv2 block after the audio, written by
  the muxer itself rather than by the tagging post-pass: text tags only,
  under APEv2's own key spellings (`Track`, `Year`, `Album Artist`), with
  multi-valued fields NUL-joined into one item per key as the format
  requires. Cover art and chapters do not survive a .wv output.
  Other formats
  join as encoders land (`/caps` is the truth). `aiff` and `ape` exist for
  jobs but have no streaming form: 415. Monkey's Audio has no streaming form
  at all, not merely an inconvenient one -- a .ape opens with a seek table
  that is its only index, and with totals nothing knows until the audio has
  gone out. It carries its metadata the same way `wavpack` does, as an APEv2
  block the muxer writes after the audio. Live FLAC and ALAC streams omit the size hints
  and byte-rate pacing: a lossless encoder's output size is signal-dependent
  and unknown up front. CBR MP3 and Opus carry a size estimate. Completed
  cache entries serve with exact sizes like any other.
- `rate`, `ch`, `bits`: output sample rate, channel count (1, 2, or a
  conventional wider layout the source is mixed into with the positions it
  does not have left silent), bit depth (16 or 24, dithered when reducing).
  **Absent** keeps the source's; omitting the parameter is the only
  spelling of that. `rate=0`, `ch=0`, `bits=0`, `maxBitRate=0`, and a bare `rate=` are 400, because zero is
  not a rate, a channel count, a depth, or a cap. The same rule holds on
  the HLS master form.

  "Keeps the source's" has one exception, and it is the lossy/lossless
  split rather than a per-encoder quirk. A source wider than the output
  format can hold (5.1 to `aac`, `mp3` or `opus`, none of which encode
  more than two channels) is **downmixed to stereo** with a BS.775
  matrix, on `/stream` and on HLS alike, and the daemon logs it at
  `warn`. Lossless outputs refuse instead: `alac`, `wavpack` and `ape`
  answer 415, because a lossless file that silently dropped four channels
  would be lying about what it holds. `ape` applies the same rule to depth:
  it holds 8, 16 and 24-bit, and a 32-bit source is refused rather than
  narrowed, since every other lossless output here carries 32-bit through.
  An explicit `bits=24` still converts it, because then the caller asked. `flac`, `vorbis`, `wav` and `aiff` carry
  multichannel natively and are untouched. An explicit `ch` is never
  overridden in either direction; one wider than the source places the
  source's channels and leaves the rest silent, and one whose layout has no
  place for a source position (a back pair meeting a side pair) is 415.

  The fold is the re-encoding rung's, which is the only rung that can
  fold: direct play ships the source's own bytes and a container rewrite
  moves the source's own packets, so both deliver the source's layout
  whatever it is. A 5.1 AAC file requested as `format=aac` is therefore
  served as 5.1, by packet move, while the same content as FLAC is
  re-encoded and folded. That is not a special case to remember, it is
  what those rungs are; `ch=2` asks for stereo specifically and gets it
  from every rung, because a channel count the source does not already
  have declines the passthrough rungs by construction.

  On a timeline (`tl=`) the count is applied per member, before the seam,
  so a member's level is its own fold's rather than the assembled
  envelope's. See the `tl` section below.
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
  dynamics preset always engages it.

  What the limiter guarantees, precisely: for any legal `gain`, the PCM it
  emits holds `truePeakCeilingDb` as measured by its own 4x detector. Two
  caveats worth budgeting for, because measuring the output will show them:

  - **Any other detector may read slightly higher.** `/analyze` measures
    with a different 4x design and reads about 0.04 dB over on broadband
    transients; that is detector disagreement, not headroom lost. It also
    follows BS.1770-4's rate schedule (4x below 96 kHz, 2x to 192 kHz,
    none above), so at high rates it is not measuring the same quantity
    the limiter bounds.
  - **Lossy output is bounded at the encoder's input.** For `wav` and
    `flac` the guarantee reaches the file (dither adds below it). For
    `opus`, `vorbis`, `aac` and `mp3` a decoder can reconstruct a true
    peak a few tenths of a dB above what the encoder was handed.

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

  A span can also join a play queue: a timeline member takes the same
  `from`/`to` window, so consecutive carves of one rip play gaplessly
  inside a multi-source timeline (see Multi-source timelines).

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
- `hev2`: boolean, selects HE-AAC v2 (parametric stereo, `mp4a.40.29`)
  for `format=he-aac`; any other format is `415`. Selection is explicit
  rather than bitrate-automatic, and the flag is part of the cache key:
  v1 and v2 at one bitrate are two different codecs and never share an
  entry. Mono sources are refused (encode v1 instead); the v2 default
  bitrate is 32 kbit/s.
- `container`: overrides the format's packaging where an alternative
  exists. It requires an explicit `format`, and a name the format cannot
  produce is `400` rather than a silent fall back to the default form.
  A container override never direct-plays: direct play serves the file in
  the wrapper it already has, so an override is always a request for bytes
  rung 1 does not hold. Where the codec survives, rung 2 answers it without
  re-encoding. The available forms, by format:

  | format | `container=` |
  |---|---|
  | `aac` | `adts`, `progressive`, `fragmented`, `mka` (empty is fragmented MP4) |
  | `alac` | `progressive`, `fragmented` (empty is fragmented MP4) |
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
  `container=fragmented` names the CMAF form explicitly. It is what the
  empty override already means on `/stream` and HLS, and it exists because
  the empty override no longer means it everywhere: every **file** output
  rewrites empty to `progressive`, since a file can satisfy the back-patch
  and streaming buys it nothing. That is all three job types (transcode,
  split, merge) plus `waxflow transcode` and `waxflow split`, through one
  engine-side rule so the set cannot drift. Naming `fragmented` is how a
  file output asks for the delivery form anyway. `webm` is Opus and Vorbis
  only, which is webm's audio subset.
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
the source codec survives being repositioned (Opus or AAC-LC; HE-AAC cuts
exist but only for spans keeping the stream head, so they go unadvertised)
and the requested `format` matches it (`format=opus`, or `format=aac` for
AAC-LC), the span is
served by moving the source's own packets into a new stream: no decode, no
re-encode, no generation loss. The head and tail land exactly where asked, since
their snap-to-packet slop becomes the stream's gapless trims.

A cut of more than one span has interior joins, and those are not exact: no
container but Matroska states a trim per packet, so every interior edge snaps
INWARD onto the packet grid (ADR-0011). It used to snap outward with the
codec's decode pre-roll in front of it, which delivered 21 ms of the removed
span on AAC-LC, 80 ms on Opus and half a second on HE-AAC, at every join. A
cut therefore never delivers audio from outside the request, and gives up
under one packet of wanted audio at each interior edge; `CutPlan.Landed`
reports where every span really fell. Spans that touch share one boundary, so
a range named in two pieces still arrives whole. A span left holding no whole
packet declines to the transcode rung. The decoder
is not reset at a join either, so the first packet after one decodes against
the previous span's state for as long as that codec's memory runs, which is
the artefact every stream-copy tool has. `TranscodeOptions.SpliceTrims` buys
exact interior tails and an inaudible decoded pre-roll where the destination
can carry them, which is mka and webm alone; with it set every other
destination falls through to a re-encode, and Firefox rejects a file carrying
more than one `DiscardPadding`. It needs a
source-matching format, because `format=auto` resolves to `wav` and would
transcode, so a span that must not re-encode names its codec explicitly. A
client can feature-detect the formats that engage this rung from `/caps`
`delivery.cutFormats` instead of inferring cut availability from the source
codec plus a source-matching `format=`. It
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
overlap), the source trims samples inside its run and the destination is not
Matroska (only a container that states its trims per packet can carry one), or
`maxBitRate` is set (this rung reads headers, and a source's real bit rate is
in its packets). Over HLS it also declines a source whose packet
durations vary, since there is then no grid to lay segment boundaries on.
`waxflow_remux_total` counts the pipelines it served. The cut declines in turn
(a codec off the Opus/AAC-LC allowlist, `maxBitRate` set, a snapped window the
destination cannot express, a span that keeps no whole packet, two spans
closer together than the windows can be kept apart, a source that trims
samples inside its run at places the measure did not record, a kept packet
that carries such a trim and a destination other than Matroska, or over HLS a
source whose packet durations vary and so give no grid to lay segment
boundaries on) and falls to
a transcode of the same span, so a span is always served: zero-generation
when it can be, sample-exact through the decoder when it cannot. A cut plans
its windows on the source's raw packet timeline, past every trim before them,
so a span past a trim, or one whose gap holds it, moves the source's own
packets like any other.

**Live transcode responses**: `200` chunked, `Accept-Ranges: none`,
`Cache-Control: no-store`, `X-Accel-Buffering: no`, plus hints
`X-Content-Duration` (seconds) and `X-Estimated-Content-Length`. A source
whose headers only estimate its length (ASF, Matroska) is measured once for
that header, memoized per source, and so is a Matroska that states none at all,
since only a walk finds a trim inside its run and the copy rungs plan from one.
A source whose headers count is taken at its word, and one that states no length
in a container that cannot hide a trim (an untagged MP3, a bare ADTS stream)
sends no duration hint rather than reading the whole file to compute one before
the first byte goes out. The measure is a hint, not a bound on the run: the
body is whatever the file decodes to, so a source whose measure and whose
decode disagree streams whole rather than being cut to the advertised number.
Range
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

A `he-aac` descriptor may also carry `"hev2":true` (HE-AAC v2 selection;
the field is omitted when false, and refused on other formats).

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
| `GET /hls/master.m3u8?v=...` | master playlist: one rung per ladder bitrate with `BANDWIDTH` and `CODECS` (`Opus`, `fLaC`, `alac`, `mp4a.40.2`; `format=he-aac` encodes and advertises `mp4a.40.5`, or `mp4a.40.29` under `hev2`, and an HE-AAC remux advertises its own signalling, `mp4a.40.5` or `mp4a.40.29`). With an API key, raw parameters (`src`, `format`, `bitrate` or `bitrates`, `hev2`, `bits`, `rate`, `ch`, `gain`, `dynamics`, `segDur`, and `crossfadeSeconds` for a `tl` timeline) also work; the daemon builds the descriptor. |
| `GET /hls/media.m3u8?v=...` | variant VOD playlist: `EXT-X-VERSION:7`, `EXT-X-MAP`, `EXT-X-INDEPENDENT-SEGMENTS`, every segment listed with its exact duration, `EXT-X-ENDLIST`. Unknown source lengths are measured (frame-index walk), never estimated. |
| `GET /hls/init.mp4?v=...` | the CMAF init header (codec config; the edit list carries encoder delay and the exact length) |
| `GET /hls/seg/{n}.m4s?v=...` | media segment n (0-based). Cached segments serve with ranges and strong ETags; misses wait on the variant worker (within a 3-segment lookahead) or restart it at n. |

Segments are `styp` + `moof`+`mdat` fragments, boundaries snapped to
whole encoder frames (`segDur` is a target; the playlist carries exact
durations), all sync samples, decode timeline in sample units. Formats
with a segmented form: `opus`, `flac`, `aac`, `alac` (see `/caps`
`delivery.hlsFormats`); of these, `opus` and `aac` also cut over HLS without
re-encoding (see `/caps` `delivery.cutFormats`). A `410 source-changed` means
the file changed since minting: re-mint and reload.

### Multi-source timelines (`tl`)

A play queue can be streamed as one gapless timeline: several files
delivered as a single continuous stream, so a gapless album crosses its
track boundaries with no seam, no re-buffer, and no gap. It is one media
playlist, one init header, and one edit list, with no
`EXT-X-DISCONTINUITY` anywhere, because a concatenated timeline really is
continuous.

Mint the queue first, then sign a master against the digest. A member may be
a whole file or a `from`/`to` sample window of one (here, the virtual track
from the `/stream` example, joined to a queue):

    POST /hls/timeline
    {"srcs": [{"src": "lib/Album/01.flac"},
              {"src": "lib/rip.flac", "from": 10804500, "to": 18744300}]}

    201 {"schemaVersion": 1, "tl": "kJ3n...pQ", "members": 2, "durationSeconds": 1680.0408163265306,
         "envelopeRate": 44100,
         "boundaries": [{"offsetSamples": 0, "durationSamples": 66150000},
                        {"offsetSamples": 66150000, "durationSamples": 7939800}]}

    POST /sign
    {"path": "/hls/master.m3u8", "params": {"tl": "kJ3n...pQ", "format": "opus", "gain": "off"}}

`tl` and `src` are exclusive: a URL names one stream or one timeline.
Everything else (`format`, `bitrate`/`bitrates`, `hev2`, `bits`, `rate`,
`ch`, `dynamics`, `segDur`) means what it means for a single source.

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
- **A timeline is built at the width it is delivered at.** When the
  delivered channel count differs from the envelope's and the members do
  not all share the envelope's count, each member is conformed to the
  delivered count by its own conversion before it reaches the seam: a
  narrower member is placed, a wider one folded, each at its own width.
  Folding the assembled envelope instead would be wrong by an exact
  scalar, up to 6 dB, because the downmix matrix normalizes each output
  row over every source column and a placed member's silent columns still
  count in the divisor. The daemon resolves the width itself; a library
  caller sets `waxflow.ConcatOptions.Channels` from
  `Engine.TimelineChannels`, and a conversion applied to a mixed-width
  timeline afterwards is a 400.
- **Members take `from`/`to` sample windows.** A member may bound itself
  to a sample range of its own source: the virtual-track span (`/stream`'s
  `from`/`to`, with its exact semantics), joined to the queue as a member
  in its own right, so a client-computed CUE carve plays gaplessly inside
  a timeline. Source samples, `to` exclusive, `0` (or absent) running to
  the end; samples rather than seconds for the span's reason (a CUE
  boundary at 245.32 s is not exactly representable in binary, and a
  timeline puts that error at every seam); a window that does not fit the
  measured source is a `400`, never clamped. The identity rule is the
  opposite of `crossfadeSeconds`' and worth internalizing: a window says
  *which samples are this member*, so it joins the digest, and two
  windowings of one file are two timelines with two `tl`s. Consecutive
  windows tiling one file join gaplessly, byte-identically to streaming
  the file whole. One nuance: a windowed member that must **resample** to
  the envelope primes cold at its first samples (the same status quo as
  crossfade-seek exactness), while the motivating same-file carve is
  uniform and exact. Send windows only when `/caps`
  `delivery.timelineMemberWindows` is true: an older daemon `400`s them
  as unknown fields, so absent means fall back to per-item URLs.
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
  walk for a member whose demuxer defers one: MP3 (bare or in a WAV or
  AIFF-C), ADTS, and any Matroska file, whose cluster walk no open pays.
  ASF is slower still: it names its positions in milliseconds, so no walk or
  seek can answer exactly and the measure is a decode. A cold mint over any of
  these answers `202` rather than `201`.
  When a cold queue needs enough of the latter to be worth it,
  the response is `202` with a job instead of `201` with a digest; poll
  `GET /jobs/{id}` or follow its events, and the finished job's `timeline`
  field carries the same values the `201` body would. The cost is once per
  file, so the same queue mints in one round trip afterwards, and a member
  whose index sidecar is already complete mints inline from the start.
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
      "inputs": ["flac", "wav", "aiff", "ogg", "mp4", "mka", "adts", "ape", "wavpack", "wma", "musepack", "mp3"],
      "decoders": ["pcm", "alaw", "mulaw", "ima-adpcm", "ms-adpcm", "flac", "mp3", "alac", "aac-lc", "he-aac", "wavpack", "ape", "wma", "wmalossless", "wmapro", "wmavoice", "musepack", "vorbis", "opus"],
      "outputs": [{"name": "wav", "live": true, "exts": ["wav", "wave", "rf64", "bw64"]},
                   {"name": "opus", "live": true, "exts": ["opus"]},
                   {"name": "vorbis", "live": true, "exts": ["ogg", "oga"]},
                   {"name": "aiff", "live": false, "exts": ["aif", "aiff", "aifc", "afc"]},
                   {"name": "flac", "live": true, "exts": ["flac"]},
                   {"name": "mp3", "live": true, "exts": ["mp3", "mpga"]},
                   {"name": "aac", "live": true, "exts": ["m4a", "aac", "m4b"]},
                   {"name": "he-aac", "live": true, "exts": []},
                   {"name": "alac", "live": true, "exts": []},
                   {"name": "wavpack", "live": true, "exts": ["wv"]},
                   {"name": "ape", "live": false, "exts": ["ape"]}],
      "delivery": {"progressive": true, "hls": true, "hlsFormats": ["opus", "flac", "aac", "he-aac", "alac"],
                   "cutFormats": ["opus", "aac"],
                   "jobs": false, "uploads": false, "pid": false,
                   "timelines": true, "maxTimelineMembers": 1000,
                   "timelineMemberWindows": true, "rootsReload": true},
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
                   "basis": "automated: hls.js + <audio> in Chromium (make client-e2e, nightly)",
                   "notes": ["12- and 20-bit FLAC sources are widened to 16/24 bits losslessly for HLS, which re-encodes them instead of remuxing (Chromium's MP4 parser accepts only 8/16/24/32)",
                             "an explicit bits=12 or bits=20 is honored and mints a stream Chromium cannot play; omit bits, or ask for 16 or 24"]},
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
`delivery.timelineMemberWindows` reports that timeline members take
`from`/`to` sample windows; route by its presence, because a daemon too
old to have it `400`s a windowed member as an unknown field (fall back to
per-item URLs rather than trying).
`delivery.rootsReload` reports whether `POST /roots/reload` is served (see
that endpoint): `true` only for a file-configured daemon whose roots are
not pinned by `WAXFLOW_ROOTS`. It is always sent (`true` or `false`), so a
client detects support before calling; absent means a server too old to
advertise it.

`dsp` is the signal-processing surface, so a format policy routes by
capability instead of sniffing a version:

    "dsp": {
      "gainModes": ["off", "track", "album"],
      "gainMaxDb": 12, "gainMaxVoiceDb": 24,
      "dynamics": ["off", "voice"],
      "loudness": ["analyze"],
      "silenceDetector": "silence-1",
      "truePeakCeilingDb": -1
    }

Every advertised value parses, which is why `gainModes` does not list a
`"<db>"` placeholder for the scalar escape hatch: a dB number is always
accepted, and the two ceilings are what a client actually needs to know
about it. **Read both ceilings, not one.** The clamp on positive gain
depends on `dynamics` (see /stream), so `gainMaxDb` alone does not tell
you whether `gain=16` is legal. `loudness` is jobs-only by construction:
a live stream cannot be measured before it is served.
`silenceDetector` is the silence detector's algorithm revision, the same
value a silence map's own `version` field carries (see Silence
detection): a caller that persists maps invalidates any whose version
differs, without running a job to find out. Like `loudness`, it is
advertised even when `delivery.jobs` is false, because the dsp slot
describes the build's signal path, not this daemon's enabled routes. A
daemon new enough to have the field always sends it non-empty, so absent
(or empty) means a server too old to advertise the detector: fall back
to reading versions off the maps themselves, do not read it as every
cached map being stale (the same rule `cutFormats` states).

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

The `client` package wraps the whole surface (`client.CreateJob`,
`Job`, `Jobs`, `DeleteJob`, `JobResult`, `JobEvents`), with envelope
errors decoded to waxerr codes like every other method.

`POST /jobs` with a JSON body:

    {"type": "transcode", "src": "lib/a.flac", "format": "opus", "bitrate": 96}
    {"type": "transcode", "src": "lib/a.flac", "format": "flac", "loudness": "analyze"}
    {"type": "analyze", "src": "lib/a.flac"}
    {"type": "analyze", "src": "lib/a.flac", "silence": true}
    {"type": "merge", "srcs": ["lib/ch1.flac", "lib/ch2.flac"], "format": "alac"}
    {"type": "split", "src": "lib/album.flac", "format": "flac", "cuts": [10584000, 21344400]}
    {"type": "split", "src": "lib/game.flac", "format": "flac", "cuts": [5292000, 21344400], "skip": [0]}
    {"type": "split", "src": "lib/album.flac", "format": "flac", "cue": "lib/album.cue"}

Transcode jobs take the /stream shaping parameters (`format` required,
plus `container`, `rate`, `ch`, `bits`, `bitrate`, `hev2`, `gain`,
`flacLevel`, `wavpackLevel`, `apeLevel`)
and, unlike /stream, may target non-streaming formats (`aiff`, `ape`): job
outputs are seekable files, so every muxer back-patch applies (exact WAV
sizes, FLAC seek tables, the MP4 `iTunSMPB` gapless atom, the Monkey's Audio
seek table and file MD5). `loudness:
"analyze"` selects the two-pass form: measure the source, apply the
exact gain to the ReplayGain reference (replacing `gain`), and write
measured `REPLAYGAIN_TRACK_*` tags describing the finished output. Three
outputs get a projection there instead of a measurement, because their
muxer takes its tags before the encode and cannot be patched after it:
Ogg-FLAC, WavPack, and Monkey's Audio. The job's `analysis` still carries
the measurement in every case; only the file carries the estimate.
Analyze jobs measure EBU R128 loudness (integrated LUFS, loudness
range, true peak, sample peak) without producing audio.

An album measurement is a library call rather than a job type:
`Engine.AnalyzeGroup` takes the members and returns the group's numbers
beside each member's own, from one decode per member. Each member is
metered at the width it will be DELIVERED at (`GroupMember.Channels`, read
off that member's own transcode plan), because a member measured inside a
wider envelope is not measuring its own fold: a mono member inside a stereo
one reads 3.01 dB hot, since the envelope duplicates it (ADR-0010). The
group's gates then run over the union of the members' blocks, which is what
makes one gain right for all of them. A member is handed open
(`GroupMember.Media`, the caller's to close) or opened on demand
(`GroupMember.Open`, the shape `ConcatSource.Open` has): the engine opens
such a member when the run reaches it and closes it before opening the
next, so a group of such members holds one descriptor at a time however
long the album, and the damage each member's read found comes back on that
member's
`AnalyzeResult.InputWarnings`, as it does for every `Analyze`. A silence
map and a tap are refused for a group, since both are properties of one
source's timeline.

Each field belongs to a specific set of job types, and a field on a type
that does not take it is a 400 rather than a field silently ignored at
run time: `srcs` and `titles` are merge-only, `cuts`, `skip` and `cue`
split-only (`cue` exclusive with the other two), the silence fields
analyze-only, `gain`
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

`split` cuts one `src` at `cuts` into N+1 pieces and writes the ones it
does not `skip`, one output each. Cut points are **source sample offsets,
strictly ascending, and interior**: `0` is implied before
the first and the source's end after the last, so N cuts make N+1 pieces
and piece *i* runs `[cuts[i-1], cuts[i])`. A leading `0`, a cut at or past
the end, a repeat, or a descending pair each ask for an empty piece or for
samples the source does not have, and are all a 400 at creation rather
than a job that dies on piece 7. Samples rather than seconds for the same
reason a span is: a cut declares which samples are this piece, so it is
content identity, and 245.32 s at 44100 floors to one sample short of the
boundary a CUE sheet's CD-frame arithmetic names exactly.

**`skip` lists pieces the split does not write**, as 0-based indices into
those N+1 pieces, strictly ascending. The outputs are the remaining pieces
in order, and a list naming every piece is a 400. It is how a sheet's data
track reaches the job (below); a client cutting by hand may name pieces of
its own.

**`cue` names a CUE sheet instead**, exclusive with `cuts` and `skip`. It
is a source reference like `src`, so the sheet can be uploaded
(`upload:<id>`) or sit in a library root beside its rip, and it resolves
through the same resolver. The sheet's track boundaries become this
split's cut points at creation: a first track starting at frame 0 is
dropped (a cut there would ask for an empty piece), so the usual sheet of
N audio tracks yields N-1 cuts and N pieces; a first track starting past
frame 0 is kept, and the audio before it (a pregap, or hidden track one
audio) becomes the first piece, so N tracks yield N cuts and N+1 pieces. A
data track (`TRACK 01 MODE1/2352`, which a mixed-mode disc's sheet lists
ahead of its audio and an Enhanced CD's after it) is a piece too, since
the boundaries partition the whole file, and it resolves into `skip`
rather than into a piece of noise named after a song: the audio before it
ends at its `INDEX 00`, the audio after it begins at its own `INDEX 01`
(the mode change puts data-mode sectors inside that pregap), and a data
track occupying none of the file (EAC lists it at frame 0 beside the audio
it ripped; an Enhanced CD's sits in a second session past the rip) is no
piece at all and adds no cut. Whether it lies past the file is decided
against the same length the cuts are held to, which no demuxer delivers
audio past. A cooked data track (`MODE1/2048`) occupying the file ahead of
audio is a 400, since only 2352-byte sectors keep the sheet's frames on
samples past it. The daemon
reads the sheet strictly: a line it cannot read is a 400 naming the line,
since a skipped line would be a silently wrong cut. A sheet that names no
FILE indexes `src`.

The daemon parses the sheet rather than making each client do it, and that
is the point of the field. A CD frame is 1/75 s, which no nanosecond clock
can represent, so the obvious conversion lands a sample short at every
boundary; every CD-family rate divides by 75 exactly (44100/75 = 588), so
frames convert to samples directly and exactly. Pushing that to each
client is pushing each client to rediscover the same off-by-one. Sheets
are decoded best-effort (BOM stripped, UTF-8 when valid, CP1252
otherwise), and a sheet indexing several files is refused, since its tracks
are already separate and there is nothing to cut, unless only one of them
holds audio: a data track given a FILE of its own beside the rip is how EAC
and XLD write a mixed-mode disc, and the audio file is the one cut.

The sheet does not reach the job. It is resolved into `cuts` and `skip`,
which is what the job carries, so `GET /jobs/{id}` shows the boundaries
the 201 accepted and an edit to the sheet afterward cannot change what
runs.
Sending `cue` and sending the samples it means produce the identical job.

The pair is exact, and that is the property worth having: **a split to a
lossless format at the source rate rejoins bit for bit through a merge**,
less any piece it skipped.
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
caller caching the map needs it to know when the map went stale, and
`/caps` advertises the current one as `dsp.silenceDetector`, so a cached
map whose `version` differs can be invalidated without re-running a job.
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

`outputs` is a list because a split has one entry per piece it writes, in
cut order; every other type has at most one. Its index is what `GET
/jobs/{id}/result/{n}` takes.

States: `queued`, `running`, then one of `done`, `failed`, `canceled`.
`analysis` peak and loudness fields are `null` for digital silence
(negative infinity does not survive JSON). Jobs queue beyond the
`jobSlots` worker count, and running jobs pause between chunks while
the live pool is saturated: interactive streams always win.

`GET /jobs` lists all jobs (`{"schemaVersion": 1, "jobs": [...]}`).
`DELETE /jobs/{id}` cancels the job if running and removes it (204).

`GET /jobs/{id}/events` is a server-sent event stream (`client.JobEvents`):
one `event: job` per state or progress update, each `data:` line a full
job document, ending after the terminal event (comment heartbeats every
15 s keep proxies from timing it out).

`GET /jobs/{id}/result/{n}` serves the job's nth output file
(`client.JobResult`; real `Content-Length`, full ranges, strong
per-output `ETag`, `Content-Disposition: attachment`): a transcode's or
a merge's audio, one piece of a split, or an analyze job's
`silence.json`. An index the job does not have is a 404. A queued or
running job answers 400, a failed one replays its error envelope.

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

## POST /roots/reload

Re-reads the daemon's configuration and reconciles its live library roots
to match, so a root added at runtime streams without a restart
(`client.ReloadRoots`). Empty body; returns

    {"schemaVersion":1,"added":["B"],"removed":[],"changed":[],"roots":["A","B"]}

`added`, `removed`, and `changed` report what moved (`changed` is a root
whose name stayed but whose path was re-pointed); `roots` is the full set
after the reload, in configuration order. All four are always arrays.

The reconcile scope is exactly the fields the resolver owns, the library
`roots` and `sourceMaxBytes`, so a reload loads byte-for-byte what a
restart would. Everything else (`addr`, cache, slots, TLS, ...) belongs to
subsystems built once at startup and still needs a restart. A new root is
opened, a dropped one closed, and a re-pointed one swapped; in-flight
streams already resolved from a removed or re-pointed root run to
completion, later requests for a removed root `404`. A bad config edit
(missing or unreadable file, unopenable path, invalid or duplicate root
name) is refused synchronously as `400 invalid-request` with nothing
changed, so the caller learns at once instead of after a `200` that did
nothing; a wired endpoint never answers `404`, so that status
unambiguously means the endpoint is not served here. To avoid a reload
racing a half-written file, the writer updates the config **atomically**
(write a temp file, then rename).

The endpoint exists only when a reload could actually do something: the
daemon has a config file and `WAXFLOW_ROOTS` is not pinning roots (that
env variable replaces file roots wholesale and is read once at startup, so
a reload could never reflect a file edit). Otherwise the route `404`s and
`delivery.rootsReload` is `false`, so a client detects support at
capability-probe time. An env-only or no-config deployment keeps serving
runtime-added roots by direct download instead. `client.ReloadRoots`
should be gated on `delivery.rootsReload` rather than on the status it
gets back: an unwired daemon's `404` and a proxy that lost the route are
not distinguishable from the client side.

## GET /metrics

Prometheus text: `waxflow_build_info`, `waxflow_sessions_active`,
`waxflow_sessions_total{kind}` (live, sync, hls),
`waxflow_direct_play_total`, `waxflow_remux_total`,
`waxflow_hls_segments_total`,
`waxflow_cache_{hits,misses}_total`, `waxflow_cache_{bytes,entries}`,
`waxflow_admission_rejects_total`, `waxflow_admission_in_use{pool}`,
`waxflow_session_degradations_total`, `waxflow_ttfb_seconds` histogram.
