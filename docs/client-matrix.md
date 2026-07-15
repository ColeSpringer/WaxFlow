# Client matrix

The tested playback facts behind the named delivery profiles in `GET
/caps` (`server/types.go`, `deliveryProfiles`). A profile is a claim
about a client family; this file is the evidence per cell. The two must
change together: `TestDeliveryProfilesAreHonest` pins the wire shape and
keeps every advertised format inside the build's real capabilities, and
this matrix records why a format is listed at all.

Cell status vocabulary:

- **automated**: exercised by `make client-e2e` (headless Chromium via
  Playwright, nightly in CI). The strongest basis: re-verified on every
  nightly run.
- **manual**: verified by a human following the checklist below; record
  the client version and date when run. Re-run per release that touches
  delivery (the release checklist in MAINTENANCE.md points here).
- **vendor-doc**: documented by the platform vendor (Apple HLS
  authoring specification, Android Media3 supported-formats list) and
  consistent with our validator runs, but not yet re-verified by hand
  on real hardware for this release.
- **no-decoder**: the client family ships no decoder for the codec;
  listing it would be aspiration, so the profile omits it.

## Matrix

Progressive is `GET /stream` (live chunked transcodes and direct play);
HLS is the CMAF/fMP4 variant surface.

| Cell | Chromium + hls.js | Safari / AVPlayer | Android Media3/ExoPlayer | mpv / VLC (libmpv, ffmpeg) |
|---|---|---|---|---|
| HLS opus | automated | vendor-doc (iOS/tvOS 17+, macOS 14+) | vendor-doc | manual |
| HLS aac | automated | vendor-doc (universal) | vendor-doc | manual |
| HLS flac | automated | vendor-doc (iOS 11+) | vendor-doc | manual |
| HLS alac | no-decoder | vendor-doc (iOS 11+) | no-decoder | manual |
| Progressive opus (Ogg) | automated | vendor-doc (Safari/iOS 18.4+) | vendor-doc | manual |
| Progressive mp3 | automated | vendor-doc | vendor-doc | manual |
| Progressive aac (fMP4) | automated | **manual, pending** | vendor-doc | manual |
| Progressive flac | automated | vendor-doc (Safari 11+) | vendor-doc | manual |
| Progressive alac (fMP4) | no-decoder | **manual, pending** | no-decoder | manual |
| Progressive wav | automated | vendor-doc | vendor-doc | manual |
| Direct play (format=auto) | automated | vendor-doc | vendor-doc | manual |
| Timeline (`tl=`, gapless queue) | automated | vendor-doc | vendor-doc | manual |
| Span (`from=`/`to=`, virtual track) | automated | vendor-doc | vendor-doc | manual |

The timeline cell is the multi-source surface (`POST /hls/timeline` then a
master signed against the `tl` digest). It is HLS, so nothing about it is
new to a client: it is one media playlist, one init segment, and one edit
list, with no `EXT-X-DISCONTINUITY`, which is exactly what makes it
deliverable at all. The automated cell mints a three-track queue, requires
the player to see one stream of the whole queue's length, and then seeks
across a track boundary and requires playback to carry on through it. The
other columns inherit their HLS rows: a client that plays HLS opus plays a
timeline of opus, because it is the same presentation.

The span cell is the virtual-track surface (`from=`/`to=` on the HLS mint):
one offset range of one file served as its own presentation. It inherits
the other columns for the same reason a timeline does, since a span is
also just one playlist, one init, and one edit list. The automated cell
mints a span of a **44.1 kHz** fixture as 48 kHz opus, which is the CUE-rip
case and forces the resampler, and requires the player to see the span's
own duration rather than the file's. That assertion is the cell: a playlist
built from the file's track plays perfectly while being the wrong stream,
so it is checked directly rather than through a proxy. Whether the samples
are the right ones is the engine suite's job
(`TestSpanDeliversItsOwnSamples`, `TestSpanPrerollMatchesContinuous`); a
browser cannot see that and should not be asked to.

The **manual, pending** cells are the "Safari progressive playback of a
live transcode" question: our live transcodes are chunked with no
Content-Length and `Accept-Ranges: none` (a `Range: bytes=0-` request
gets a plain 200), and AVPlayer's tolerance for that varies by version.
Until the checklist below has been run on current Apple hardware, the
`apple-native` profile recommends `delivery: "hls"`, which is Apple's
own guaranteed path. If the checklist passes reliably, flip the
recommendation deliberately (profile table + this file + the pinned
test).

## Manual checklist: Safari and AVPlayer (needs a Mac / iOS device)

Run against a daemon reachable from the device, per release that
touches delivery. Record client versions and outcomes here.

1. HLS, each of `format=opus|aac|flac|alac`: mint via `POST /sign
   {"path":"/hls/master.m3u8","params":{"src":...,"format":...}}`, play
   in Safari and in AVPlayer (device or simulator): start, seek far
   ahead, seek back, play to the end. `mediastreamvalidator` details
   live in docs/hls-validation.md.
2. Progressive Ogg-Opus (Safari 18.4+): `GET /stream?...format=opus` in
   an `<audio>` tag and as a bare URL; must start and survive a `t=`
   seek issued as a fresh URL.
3. **Progressive live transcode**: `format=aac` and `format=alac`
   (fMP4), also `mp3` and `flac`; confirm playback starts promptly
   (TTFA), plays to the end, and pausing/resuming does not stall. This
   is the cell that decides the `apple-native` recommendation.
4. Direct play: `format=auto` over an untouched `.m4a`/`.mp3`/`.flac`
   source; confirm full seek (byte ranges work on completed/direct
   responses).

Outcome log (append per run):

- (none yet; first run pending Apple hardware)

## Manual checklist: Android Media3/ExoPlayer

Media3's demo app plays a URL directly (Media3 supports fMP4 HLS with
AAC/FLAC/Opus; Android has no ALAC decoder).

1. HLS `format=opus|aac|flac`: master URL in the demo app; start, seek,
   finish.
2. Progressive `format=opus|mp3|aac|flac|wav` and `format=auto`.

Outcome log (append per run):

- (none yet)

## Manual checklist: mpv / VLC

```sh
mpv "$(waxflow sign --src lib/track.flac --format opus)"
mpv "http://.../hls/master.m3u8?...."   # any rung set, minted via POST /sign
```

Every output format and both surfaces are expected to play (ffmpeg
demuxes all of them; our formats are verified against ffmpeg in the
differential CI job). Spot-check per release.

Outcome log (append per run):

- (none yet)
