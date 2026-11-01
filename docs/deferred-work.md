# Deferred work

## A split piece that runs short is written as if whole

**Where:** `cli/split.go` `writePiece`; the daemon's split runner
(`internal/jobs/runner.go`), whose last span is `ToEnd` too.

**What:** a piece whose source delivers fewer samples than its span, but not
none, is written short and the split reports success. `Slice` holds a
bounded window to its length, but the last piece is open-ended. Both
splitters refuse an empty piece. The one known cause was a WMA v1/v2 landing
taken from ASF's stamps (a split of `sine-s16.wma` at 20000 wrote a second
piece 33 samples short); that landing is now counted from the frames and
exact, but nothing below the splitters would notice the next cause.

**Why deferred:** `TranscodeResult.Samples` counts output frames, so a check
against the span misfires on any resampled output (Opus at 48 kHz from a
44.1 kHz source); zero is the one count that is wrong at every rate. A
correct check counts the source frames the slice delivered, which means
wrapping the slice (hiding its `LengthConfirmer` from the engine) or holding
an open-ended `Slice` to an exact total; both reach past the splitters, and
the landing fix removed the known cause.

**Found by:** the review of the measure export, 2026-10-29.

## m3u8 cannot say that a variant's audio is a rendition

**Where:** `m3u8/parse.go` `ParseMaster` and `MasterVariant`; felt by any
host that follows a third-party master's variant for its audio, and by
`internal/hls`'s client.

**What:** a STREAM-INF with an `AUDIO` group takes its audio from an
`EXT-X-MEDIA` rendition, and its own URI is often video alone. `ParseMaster`
reads past both tags, so such a variant is listed like one that carries its
audio, and a client following it gets no audio track. `ParseMedia` now
refuses the tags its type cannot describe; `ParseMaster` has no such rule.

**Why deferred:** refusing every master with an `AUDIO` attribute would
refuse nearly every video master, which a host listing variants can still
use. Saying it means renditions in the types (a group on `MasterVariant`,
the `EXT-X-MEDIA` list on `MasterPlaylist`) and the writer to round-trip
them, while nothing this repo writes has one.

**Found by:** the third-party review's point that `ParseMedia` read past
`EXT-X-BYTERANGE` and `EXT-X-KEY`, while fixing it, 2026-10-30.
