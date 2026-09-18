# Deferred work

## A mid-stream DiscardPadding is read as the last frame's, and a cut cannot cross one

**Where:** `container/mka/read.go` `ReadPacket` and `walk`, `cut.go` `PlanCut`.

**What:** the spec attaches a DiscardPadding to the end of the block that
carries it, so a trim larger than that block's last frame would have to reach
back into the frames before it. WaxFlow trims the last frame by as much as it
holds and warns once per file; the settled length still takes the whole of a
*final* trim, through the raw-end cap. ffmpeg ignores an oversized trim
outright, and applies a laced block's trim to every lace, so neither reader
spreads one and ffmpeg is an oracle for an unlaced block alone. Separately, a
cut of a source whose blocks carry a trim before the last is declined for every
destination, Matroska included: past the first inner trim every packet starts at
a grid position minus the trims so far, so any window edge past it straddles a
packet and `PacketGrid` cannot see it, because `Dur` is unchanged. The transcode
rung serves those cuts.

**Why deferred:** spreading a trim over a laced block's earlier frames needs
per-frame durations at block load, which for Vorbis advances inter-frame state
early; and a cut across one needs the walk to record trim positions rather than
a sum, so the grid snap can account for each of them. No producer writes a laced
padded block, and mkvmerge's seams are one frame, so neither is reachable
outside a hand-built file.

**Found by:** re-timing the Matroska packet timeline, 2026-09-18. Pinned by
`container/mka` `TestMidStreamDiscardPadding` (the oversized rows) and
`tests` `TestPlanCutDeclinesAMidStreamTrim`.
