# Deferred work

Known gaps that are understood, bounded, and deliberately not fixed in the
change that found them. Each entry says where it lives, what it is, why it
waits. Things that need a sibling Wax repo go to
docs/upstream-requests.md instead.

## A transcode still projects an unconfirmed frame count

**Where:** `waxflow.go` `TranscodeMedia`, the `projected` guard.

**What:** an advisory source length projects nothing into the output's
headers, so a muxer that cannot back-patch writes a streaming placeholder
rather than a number the encoder then misses. A count the headers merely
state and nothing has confirmed still projects: a Xing-tagged MP3 cut short
declares 22050 samples, the chain produces fewer, and a WAV written to a
writer that cannot seek fails at End with `CodeInternal`.

**Why deferred:** the guard cannot widen to "not `SamplesExact`" without
taking FLAC and WAV with it, and those leave the flag false because their
totals can lie rather than because they are estimates: dropping their
projection would cost every streamed WAV its exact sizes to avoid a mismatch
that does not happen. Telling the two apart needs the walk, which is the
whole file, and `format.Open` does not run one. A caller that cares can
`Walk()` the media first, which settles the track and makes the projection
honest.

**Found by:** the third-party review of the WaxTap batch, 2026-09-17.

## A Matroska Opus or Vorbis open still walks the clusters

**Where:** `container/mka/demux.go` `finalizeTrack`, reached from
`format.Open` and `format.OpenDemuxer` (and so from the mint's
`timelineNeedsJob`).

**What:** a tolerant `format.Probe` now opens these tracks with
`DemuxerOptions.DeferWalk` and reads headers. Every other entry point still
pays the cluster walk at open, so playing, remuxing, cutting or minting one
reads the whole file before the first sample.

**Why deferred:** the tail trim is `DiscardPadding` on whichever block
carries it, and `format.Media` needs the exact total before its first read to
place the raw-end cap. Making the open lazy needs either a per-packet tail
trim (the padding applied to its own block as the read reaches it) or a
`Track.Padding` the header cannot state. The mint's gate now memoizes the
track it opened, so at least nothing pays for the walk twice.

**Found by:** the probe request in WaxTap's own `docs/upstream-requests.md`
(2026-09-17), while making a tolerant probe header-only. This repo's file of
that name is the other direction (what WaxFlow wants from its siblings) and
carries nothing about it.
