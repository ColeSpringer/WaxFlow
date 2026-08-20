# Deferred work

## Three demuxers peel trailing tags with three copies of the same walk

**Where:** `container/apen/demux.go` (`stripTrailers`),
`container/wv/demux.go` (`stripTrailers`), `container/flacn/demux.go`
(`stripTrailer`).

**What:** all three peel the same trailers off the end of a file (APEv2 first,
because "APETAGEX" spells TAG where the ID3v1 probe looks; then ID3v1; then an
appended ID3v2 tag), and all three restate the reasoning in their own comments.
They have already drifted: apen and flacn peel an appended ID3v2 tag and wv
does not, so the same tag on a .wv leaves twenty bytes of trailer inside the
block walk's extent, where it draws a "trailing bytes are not a block" warning
rather than being recognized.

**Why it is not a live bug:** an unpeeled trailer is tolerated damage in wv,
not lost audio, since a WavPack block carries its own length and the walk
simply stops. In apen and flacn the peel is confirmed (flacn re-checks the
frame checksum; apen cross-checks the descriptor's own byte count and warns
when the two disagree), so a false recognition is reported rather than silent.

**Why it is deferred rather than fixed:** flacn's peel is checksum-confirmed
and retried per trailer, wv's and apen's are one pass with different
minimum-remaining rules, and the shared helper would need all three shapes.
Extracting it means changing a shipped demuxer's behavior for no functional
gain in that demuxer, which wants its own change with its own tests rather
than riding along with a new codec.

**Found by:** the stage 6 third-party review round (2026-08-20).
