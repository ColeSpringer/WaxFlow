# Deferred work

Known gaps that are understood, bounded, and deliberately not fixed in the
change that found them. Each entry says where it lives, what it is, why it
waits, and what found it. Things that need a sibling Wax repo go to
docs/upstream-requests.md instead.

## The cache key has no muxer term, so the sample-entry fix reaches cached progressive transcodes only on eviction

**Where:** ADR-0004 (the "known gap" paragraph), `TranscodePlan.Versions`
in `waxflow.go`, `RemuxVersion` in `remux.go`, `mp4.SegmenterVersion`.

**What:** the fMP4 sample-entry fix (2026-09-06) changed what the mp4
muxers write in the init header and the progressive moov: the codec's
depth in `samplesize`, dOps agreeing with the entry on rate. Segmented
output regenerates through `mp4.SegmenterVersion` (mp4-seg-4) and remuxes
through `RemuxVersion` (remux-4), but a cached progressive *transcode* of
ALAC has no version term that changed, so it keeps its old bytes (a
16-bit entry over a 24-bit cookie) until the entry is evicted.

**Why it is deferred rather than fixed:** ADR-0004 already names the
missing container term and the reason it waits: adding an element to the
joined version string invalidates every entry on first deploy, the same
practical cost as a schema bump, so it lands with the next schema bump
rather than spending a full invalidation on its own. Borrowing the ALAC
encoder's version instead would re-encode every cached ALAC output for a
field every reader (our demuxer, ffmpeg, Apple, waxlabel since v1.4.2)
ignores in favour of the cookie.

**Found by:** the third-party review of the sample-entry fix (2026-09-06).

## 12- and 20-bit FLAC mints an HLS stream Chromium cannot play

**Where:** `container/mp4/seg.go` (`flacSampleEntry`), the `hls-js` note
in `server/types.go`, `TestMSERuleNamesTheUnplayableFLACDepths`.

**What:** Chromium's MP4 stream parser accepts only 8, 16, 24, and 32 as
an audio sample size, and its FLAC mapping requires the field to equal
STREAMINFO's, so a FLAC track at 12 or 20 bits is unplayable over Media
Source Extensions however the header is written. WaxFlow writes the
header the FLAC-in-ISOBMFF spec requires and mints the stream anyway: the
playlist, init header, and every segment answer 200, and the browser
refuses the first append. The `hls-js` caps profile, docs/client-matrix.md,
and docs/hls-validation.md tell a caller with such a source to ask for
`bits=16` or `bits=24` (24 widens 20-bit losslessly).

**Why it is deferred rather than fixed:** there is no cheap correct fix.
Widening at the transcode rung would not cover the remux rung, which
copies a 20-bit source's packets unchanged, so it would need a remux
eligibility rule too. Refusing the mint would break the clients that do
play those depths (native FLAC and every other family). The honest fix is
a consumer-aware depth policy (a mint that knows it is for hls-js widens),
which is a design change with its own API surface, and no real library
has asked for it: 20-bit FLAC exists, 12-bit barely. Revisit when one does.

**Found by:** the third-party review of the sample-entry fix (2026-09-06).

## ALAC sample entries at 20 and 24 bits are unverified on Apple clients

**Where:** `container/mp4/mux.go` (`alacSampleEntry`), item 1 of the
manual checklist in docs/client-matrix.md.

**What:** the 'alac' entry's `samplesize` now carries the cookie's depth
(20, 24, or 32) where it carried a conventional 16, in a version-0
AudioSampleEntry. ffmpeg writes the same for MP4-brand files and its
output plays on Apple devices, and Apple's decoder takes the ALAC format
from the cookie, but Apple's is the one client family that decodes ALAC
and the one CI cannot drive, so this rests on ffmpeg parity until a
human runs the checklist.

**Why it is deferred rather than fixed:** it needs a Mac or iOS device.
The checklist item exists; record the client version and outcome there
and delete this entry when it has run.

**Found by:** the third-party review of the sample-entry fix (2026-09-06).

## The browser e2e names an init-header refusal only as a 30 s timeout

**Where:** `scripts/client-e2e.mjs` (`runCell`).

**What:** a cell waits up to 30 s for `currentTime` to pass 2 s and only
then reads the player's health, so an init segment the browser refuses
outright (the pre-fix 24-bit FLAC header) fails as
`page.waitForFunction: Timeout 30000ms exceeded` rather than as the
hls.js fatal that was available within a second. The cell still fails,
which is its job; it was proved on a daemon built from the pre-fix tree.

**Why it is deferred rather than fixed:** racing the progress wait
against a fatal-error watch is a small harness change that wants its own
verification run against a deliberately broken daemon, and this change
already carried one.

**Found by:** proving the `hls:flac24` cell had teeth (2026-09-06).
