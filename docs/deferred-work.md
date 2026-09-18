# Deferred work

Known gaps that are understood, bounded, and deliberately not fixed in the
change that found them. Each entry says where it lives, what it is, why it
waits. Things that need a sibling Wax repo go to
docs/upstream-requests.md instead.

## A mid-stream DiscardPadding is trimmed but not re-timed

**Where:** `format/media.go` `fill`, `container/mka/read.go` `ReadPacket`,
`remux.go` `remuxTrailer`.

**What:** Matroska states its tail trim per block, and a block that is not the
last may carry one (mkvmerge writes one at each append seam). A read drops
those frames in place, so the delivered stream is right, but the raw timeline
still counts them. Three consequences follow. A seek whose landing cluster
lies after the padded block pre-rolls through no trim, so the position is
stamped at the target while the audio delivered there is the trim's worth
earlier than it should be; once the track is settled, the raw-end cap counts
from that same overstated position and ends the seeked read the same worth
short. And a remux keeps only the final packet's trim, so the mid-stream
frames play in the output.

**Why deferred:** an exact seek needs a demuxer raw timeline that excludes
discards, which changes what `raw` means for every `container.SettleLength`
caller, and no output container can express a mid-stream trim anyway (Ogg and
MP4 have no field for one; mka to mka could forward `container.Packet.Padding`
through the muxer's `emitBlock` later). Summing every trim into the trailer
instead would shorten the output's end by a mid-stream trim's worth while
those frames still play, which loses real audio; keeping the last one loses
none.

**Found by:** making the Matroska open lazy, 2026-09-18. Pinned by
`container/mka` `TestMidStreamDiscardPadding` and
`TestMidStreamDiscardPaddingRead`, which assert the limitation as it stands so
a fix is noticed rather than silently breaking them.

## A wrapped Media hides the walk that would confirm its length

**Where:** `waxflow.go` `confirmableLength`, against `waxflow.Slice` and
`waxflow.Concat`.

**What:** a run confirms a declared count by asking the media for its deferred
walk, and a wrapper answers that question only if it forwards `format.Walker`
by hand. A slice and a timeline deliberately do not (see `format.Walker`,
which says why: a consumer opens the member files one at a time). So a library
caller who slices an open-ended span of a Xing-tagged MP3 and transcodes it to
a destination the muxer cannot patch gets the old behaviour: the declared
count is projected unconfirmed.

**Why deferred:** the daemon does not reach it. Every spanned route measures
the source and bounds the run through `sliceMeasured`, and a bounded span is
exact by `SpanTrack`'s own arithmetic, so only an open-ended library slice is
left. Closing it means either forwarding `Walker` through a slice, which
contradicts what that interface documents and would tell the daemon's job gate
that a slice is a file it can measure, or teaching `Slice` to carry a walk of
its inner media, which is a second meaning for one method.

**Found by:** the third-party review of this batch, 2026-09-18.

## Ogg-FLAC does not verify its declared total

**Where:** `container/ogg/mapflac.go` `finalizeTrack`.

**What:** flacn and wv now verify their declared total against the payload at
open, in both directions, and report the result exact. Ogg-FLAC does not: a
nonzero STREAMINFO total is taken at its word and `lastGranule()` is only
consulted when the total is zero, so such a file reports a length nothing
checked and leaves `SamplesExact` false.

**Why deferred:** the branch has no fixture. Every Ogg-FLAC producer available
here writes STREAMINFO total 0 (ffmpeg 8.0.1 does, and so do all three
committed fixtures: `container/ogg/testdata/golden-flac.oga`,
`testdata/sine-s16.oga`, `testdata/noise-s24.oga`), so they already take the
granule route and already report the right length. That leaves nothing to pin
the granule against a nonzero total, and marking the length exact on an
unverified assumption is what would turn a one-off disagreement into a
truncation. It needs a real Ogg-FLAC from the reference `flac --ogg`, which
this environment has no build of.

**Found by:** the D6 half of the lazy-open batch, 2026-09-18, which gated the
change on that pin.
