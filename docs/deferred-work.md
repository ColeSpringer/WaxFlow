# Deferred work

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

## A header walk runs to its end after a cancel

**Where:** `format.Walker.Walk` and `container.Walker.Walk` (fourteen
implementations) and the window readers under them (`srcwin`,
`mpegframes`).

**What:** `waxflow.MeasureLength` stops a decode count when its context
ends, and a caller left waiting on another's canceled measure measures
again, but a header walk (an MP3's frame index, a Matroska cluster walk, an
ASF frame index) runs to its end first. Sources are local files, so that is
one read of the file's headers at disk speed: 6 ms for a 15-minute Xing MP3.

**Why deferred:** a judgment call, open to override. A context there reaches
every Walk and the readers beneath it, for a wait the size of one header read.

**Found by:** the third-party review of the open-span hold, 2026-11-01.
