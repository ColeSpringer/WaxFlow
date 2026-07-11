# HLS validation

How WaxFlow's HLS output is verified, from always-on CI to the manual
Apple tooling pass that needs a Mac. The layers below run in order of
cost; everything automated lives in the test suite, and this file exists
mostly for the parts that cannot be automated on Linux.

## Automated (CI)

- **Structure and timing**: the engine suite parses every emitted
  segment (`styp`, `moof`/`tfdt`/`trun`, `mdat`) and asserts decode
  times, per-segment sample counts, and boundary alignment against the
  plan (`waxflow_segments_test.go`).
- **Round trip**: FLAC segments decode back to the source bit-for-bit;
  Opus segments decode through our conformance-tested decoder.
- **Determinism**: golden init headers and segments are committed
  (`testdata/golden/hls/`, regenerate via `make goldens`), and the
  server e2e suite wipes cached segments and asserts regeneration is
  byte-identical.
- **Worker restarts**: a mid-stream restart (seek) must reproduce the
  continuous run's decode timeline exactly; FLAC restarts are
  byte-identical, Opus restarts are primed (~100 ms) and
  timing-identical.
- **ffprobe/ffmpeg differential** (gated, `WAXFLOW_REQUIRE_FFMPEG=1` in
  the differential job): init+segments concatenations probe as the right
  codec and decode fully; each single segment after the init header is
  independently decodable; FLAC survives ffmpeg's fMP4 demuxer
  bit-exact.
- **hls.js in a real browser** (gated, nightly): `make client-e2e`
  drives every browser cell of the client matrix (HLS through hls.js
  plus progressive and direct play through `<audio>`) with Playwright
  against a live daemon and asserts each cell progresses without player
  errors. Requires Node and Playwright (`npx playwright install
  chromium`); see docs/client-matrix.md for the cells.

## Apple mediastreamvalidator (manual, per release)

Apple's `mediastreamvalidator` and HLS report tooling only run on macOS.
Run this checklist against a daemon reachable from the Mac before
tagging a release that touches HLS:

1. Mint a master URL for each rung shape:
   - Opus ladder: `POST /sign {"path":"/hls/master.m3u8","params":{"src":"...","format":"opus","bitrates":"64,96,160"}}`
   - Lossless rungs: `format=flac` and `format=alac`.
2. `mediastreamvalidator "<absolute master URL>"` and then
   `hlsreport validation_data.json`.
3. Must-pass items (anything else is a regression to file):
   - Playlist syntax valid, `EXT-X-VERSION:7` accepted, VOD playlist
     ends with `EXT-X-ENDLIST`.
   - `EXT-X-MAP` init segment fetches and parses (no "failed to parse
     segment as fMP4" errors).
   - Measured segment durations within tolerance of `EXTINF` (our
     EXTINF values are exact by construction; a violation means a
     muxer/plan bug, not a tolerance issue).
   - `BANDWIDTH` at or above the measured peak per variant (ours is a
     deliberate upper bound: CBR rate or PCM wire rate plus overhead).
   - `CODECS` strings accepted (`Opus`, `fLaC`, `alac`).
   - No "different target durations" or media-sequence warnings across
     variants.
4. Known-acceptable notes:
   - Audio-only streams warn about missing video attributes; ignore.
   - The final segment is short (the tail remainder); that is legal and
     expected.
   - EXTINF values are decode durations (what validators measure per
     segment), so per-segment checks agree exactly, but for Opus their
     sum exceeds the edit-list presentation duration by the encoder
     delay plus the flushed tail padding (at most ~22 ms). Every
     priming-carrying HLS stream (AAC included) has this property;
     clipping EXTINF to presentation instead would make the per-segment
     measurements disagree.
5. Play each master in Safari and in AVPlayer (an iOS device or
   simulator): start, seek far ahead, seek back, play to the end. The
   M19 client matrix records the outcomes per format and feeds the
   `/caps` delivery profiles; ALAC/FLAC-in-fMP4 support notes belong
   there, not here.

## Latency target

Seek-to-segment (a request that restarts the variant worker) p95 stays
under 1 s; the e2e suite measures a restart-heavy fetch pattern and the
`waxflow_ttfb_seconds` histogram tracks it in production.
