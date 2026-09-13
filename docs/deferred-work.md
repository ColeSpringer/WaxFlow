# Deferred work

Known gaps that are understood, bounded, and deliberately not fixed in the
change that found them. Each entry says where it lives, what it is, why it
waits. Things that need a sibling Wax repo go to
docs/upstream-requests.md instead.

## QuickTime channel layouts ('chan') in MP4/MOV

**Where:** `container/mp4/stsdpcm.go`, `setPCM`.

**What:** the 'chan' box states a track's channel order, and it is not read.
Every track is described with `audio.DefaultLayout`, so a multichannel file
whose 'chan' box states an order other than the WAVE one plays with its
channels in the wrong places. Mono and stereo have only one order and are
unaffected; above two channels the entry raises a Note naming the assumption,
which probe prints.

**Why deferred:** the mapping is a table (AudioChannelLayoutTag and its
descriptions) against a layout vocabulary that is WAVE's, and nothing in the
tree yet reads a multichannel MP4 PCM track: no encoder writes one, no fixture
holds one, and the ffmpeg differential that would score the mapping needs a
file that ffmpeg itself writes a 'chan' box into. Guessing a layout is worse
than defaulting to one and saying so.

## A frame-walked payload's damage does not reach a probe

**Where:** `format/format.go`, `buildInfo`; `container/internal/mpegframes`.

**What:** `Info.Warnings` is snapshotted once, at open. A frame index is built
lazily, so damage past the head of the run is reached by the call that walks to
it: `waxflow probe` on an MP3 whose middle is damaged reports a clean file, and
`--strict` accepts it, while the same file fails at the packet that reaches the
gap. Leading junk and a damaged head are reported, since those are read at
open. This is the bare MP3 reader's behaviour and now the WAV and AIFF-C
wrappers' too.

**Why deferred:** the two honest fixes are both larger than the finding.
Walking the payload at open costs every probe the full scan the lazy index
exists to avoid, on the one format where that scan is the expensive thing.
Re-reading warnings after the fact means a dynamic accessor on `format.Media`
and a rule for when consumers must ask again, which is API rather than a patch.
`container.Warner` is already the demuxer-side seam for it; what is missing is
the `Media`-side one.

## Opening a frame-walked payload reads a full window

**Where:** `container/riff/demux.go` and `container/aiff/demux.go`, `startMPEG`;
`container/internal/srcwin`, `load`.

**What:** the walk's first byte access rounds up to `srcwin.Chunk`, so opening
an MP3-in-WAV reads 128 KiB where the same file's PCM sibling reads about 44
bytes of chunk headers. Measured on a 2,142,058-byte file: 5 reads and 131,130
bytes against 4 reads and 44. `Begin` needs the first frame header and at most
one frame (1441 bytes at the largest) for the metadata-frame scan, so a caller
that opens and never reads a packet — a probe, the daemon's track memo — pays
about 85x the bytes it uses.

**Why deferred:** the round-up is what makes a linear read cost one window
rather than one per frame, it is pinned by `srcwin`'s own allocation test, and
five demuxers walk through it. A request-sized first load with read-ahead
starting at the first `Frame` call is the shape of the fix, and it belongs to
`srcwin` and its whole caller set rather than to the two wrappers that noticed.
`container/mpa` has always paid the same cost for every bare MP3.

## An ADPCM WAV is classified as expensive to measure

**Where:** `server/timeline.go`, `timelineNeedsJob`.

**What:** the gate reads `container.Indexer` as "measuring this member costs a
full scan", and Go's method sets made that per-format where a WAV carrying MP3
frames needs it per-file. `Track.SamplesExact` covers the byte-linear payloads,
which state their length from their own byte count; an ADPCM WAV with no `fact`
chunk states a capacity rather than an adopted truncation, so it is not marked
exact and becomes a background job it does not need.

**Why deferred:** the fix is to widen what `SamplesExact` means for a block
codec, and its current meaning there is a decision with tests and stated
reasons behind it (whether the declared count was believed, which is the one
thing about such a track a caller cannot derive). Re-opening that belongs with
the block codecs rather than with this wrapper. The mis-classification costs a
job where a handler would do, which is the conservative direction.
