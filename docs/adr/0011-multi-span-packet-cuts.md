# ADR-0011: Interior joins of a multi-span packet cut snap inward

Status: Accepted (2026-09-20)

## Context

The cut rung (`cut.go`) serves a span request by moving the source's own packets
into a new container: no decode, no encode, byte-identical payloads. A request
names sample ranges, and packets are the unit that can be moved, so every cut
point has to land on a packet boundary. The head of the first span and the tail
of the last are exact anyway, because the container's gapless fields express
their slop as a trim the player drops. An interior join has no such field: it is
a point inside a run of packets, and nothing portable states "discard part of
this one".

What the rung did was back every span's head off by the codec's decode pre-roll
and snap outward at both ends. For the first span that is right and load-bearing
(an Opus pre-skip of 3840 lands exactly on the 960 grid, so snapping alone would
drop all four priming packets and leave a cold decoder at output sample 0). For
an interior span it meant delivering the pre-roll as audio: **1024 samples on
AAC-LC, 3840 on Opus and 24576 on HE-AAC -- 21 ms, 80 ms and half a second of
the span the caller asked to remove, at every join**, plus the outward tail
snap on the span before it, with everything after the join shifted later by
the same amount. A WaxTap end-to-end report measured exactly that on Opus and
AAC and called it High. HE-AAC, whose pre-roll is the SBR decoder's convergence
and an order of magnitude larger, was never measured: its cuts must keep the
stream head, so the rung does not advertise them.

Exact interior trims do exist in one container. Matroska states `DiscardPadding`
per block, and a spike confirmed the shape works: an Opus stream remuxed to mka
with four full-packet paddings and one half-packet padding decodes in ffmpeg
sample-for-sample equal to the expected splice, with this tree's decoder
agreeing. The player matrix, read from the implementations rather than assumed:

| player | per-packet `DiscardPadding` |
|---|---|
| ffmpeg-based | honoured on any block |
| Chromium (WebM parser) | attached on any block; a wholly discarded buffer is dropped |
| Chromium (MSE frame processor) | drops duplicate **zero-duration** audio frames at one timestamp, so such blocks need a `BlockDuration` |
| ExoPlayer | honoured on any Opus block |
| Firefox (WebM demuxer) | honours exactly **one** per stream, and marks any later padded block's duration invalid, which rejects the file |

The Firefox row was then measured rather than left as a source reading
(playwright 1.62.0, Firefox 153.0, both cut shapes over HTTP): the default
file plays to `ended` with the removed span 85 dB down, and the spliced one
stops after 15,616 of 95,352 frames with a media error, its MSE
`SourceBuffer` raising an error on `appendBuffer`. Chromium plays both, MSE
included, and reports a duration equal to `plan.Samples` (the spliced file's
is 24 frames longer, which is the WebM `Duration` element rounding to the
millisecond). Firefox's duration for the default file is 312 frames long: it
does not take `CodecDelay` off the declared duration, which is cosmetic and
unrelated to this decision.

No other container has one. Ogg carries a single pre-skip and a single end
granule; a chained Ogg was tried and ffmpeg 8.0.1 mis-reports the result's
duration. A multi-entry MP4 edit list is honoured by ffmpeg and by Apple, but
player breadth for progressive m4a (ExoPlayer, Firefox) is unverified. A
negative `DiscardPadding` as a front trim is spottier still.

## Decision

**The default cut snaps every interior edge inward, with no pre-roll.** Span 0's
head and span n-1's tail are unchanged: they still back off and snap out, and
their slop becomes the synthesized `Delay` and `Padding`. Every interior head is
`snapUp(from)` and every interior tail is `snapDown(to)`. A span left holding no
whole packet declines (`CodeUnsupportedFormat`), and the transcode rung serves it
exactly.

Spans that TOUCH are the exception, and they have to be: `spans[i].To ==
spans[i+1].From` names one range in two pieces, so there is nothing between
them to remove and an off-grid boundary would cost the packet it sits in for
no reason. Such a pair shares one snapped boundary, the head's, so the packet
goes to the span that starts there and the range arrives whole. This is the
only place a span's own landing reaches past its own request, and only ever
into the span beside it.

The invariant changes from "a cut never delivers less than was asked for" to
**"a cut never delivers audio from outside the request, and each interior edge
lands within one packet inside it"**. `CutPlan.Landed` reports where each span
really fell, as it always has.

What the default cannot promise is a converged decoder at a join. A stream copy
does not reset the decoder there, so the first packet after a splice decodes
against the previous span's state for as long as that codec's memory runs. That
is the artefact every stream-copy tool has, and it is bounded: one packet on
AAC, a few on Opus, always inside the kept audio rather than outside it.

**`TranscodeOptions.SpliceTrims` buys exact splices where a container can carry
them.** With it set, an interior head's pre-roll packets are walked again and
each carries a full-duration `DiscardPadding` (decoded, so the decoder
converges; discarded, so nothing is heard), and an interior tail snaps out with
the slop stated as the last kept packet's own trim, which makes it exact. The
cut's track then trims inside its run, so `PlanRemux` declines every destination
but mka and webm and the request falls through to a re-encode; `PlanCut` logs
the decline naming the remedy. Interior *heads* stay inward-snapped even here,
because an exact head would need a trim from the FRONT of a packet and no
container states one.

The Matroska muxer now writes `BlockDuration` beside every `DiscardPadding`, for
the Chromium MSE row above.

## Alternatives rejected

- **Keep the pre-roll and document it.** It delivers audio the caller asked to
  remove. No amount of documentation makes that the right default.
- **Interstitial `DiscardPadding` by default.** Firefox rejects the file
  outright, and it narrows every cut to two containers.
- **Chained Ogg.** ffmpeg 8.0.1 mis-reports the duration of the result.
- **Multi-entry MP4 edit lists.** Honoured by ffmpeg and Apple; breadth on
  progressive m4a unverified, so not a default.
- **Negative `DiscardPadding` as a front trim.** Spottier support than the
  positive form this already declines to rely on.
- **Merging spans whose gap is under a grid.** A real gap, however small, is
  audio the caller asked to remove, and merging across it would deliver that
  audio while breaking `Landed`'s one-for-one correspondence with the request.
  The rung declines and rung 3 serves it. Spans that touch are not this case:
  there is no gap there to merge across.

## Consequences

- A request naming one range in two touching pieces used to decline outright,
  since the old outward snap overlapped the next span's pre-roll. It is served
  now, whole.
- `CutVersion` bumps to `cut-3`: the bytes and the `Landed` semantics both
  change for every cut of more than one span. `SpliceTrims` enters the plan
  cache key beside it.
- A multi-span cut is now up to one packet per interior edge shorter than the
  request. A caller who needs the exact span must read `Landed`, or ask for
  `SpliceTrims` on Matroska, or transcode.
- A span narrower than the packet grid now declines where it used to return a
  whole packet of audio from outside the request.
- `mka`'s `MuxerVersion` bumps to `mka-mux-3` for the `BlockDuration`.
- An mka-to-mka copy that forwards a source's own inner trims already produced a
  file Firefox rejects; `SpliceTrims` makes that reachable deliberately rather
  than only by inheritance.
