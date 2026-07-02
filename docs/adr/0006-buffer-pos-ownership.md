# ADR-0006: Buffer.Pos ownership

Status: Accepted (2026-07-02)

## Context

`audio.Buffer.Pos` carries the position (int64 sample count) of a buffer's
first frame, and `Discont` marks seek/splice points. Gapless playback,
sample-exact seeking, segment alignment, and priming-discard all depend on
these fields being trustworthy. If two stages both think they own `Pos`,
positions drift and every consumer compensates differently, the classic
timestamp bug family. This is one written invariant, decided once.

## Decision

- **`format.Media` stamps `Pos`.** It is the only component that knows the
  landed sample after a seek (sync-point landing plus decode-and-discard
  pre-roll). It stamps every emitted buffer's `Pos` in source timeline
  samples and sets `Discont` on the first buffer after a seek or splice.
- **Decoders never touch `Pos`.** A `codec.Decoder` maps packets to PCM; it
  has no knowledge of edit lists, container timelines, or where a seek
  landed. Decoder-emitted buffers carry no position authority.
- **Rate-changing nodes rescale.** The resampler converts `Pos` between
  timelines (`out.Pos = in.Pos x outRate / inRate` with its group delay
  fully compensated, primed and trimmed in-node). All other DSP nodes
  preserve `Pos` and `Discont` untouched.
- **Positions are int64 sample counts at a stated rate.** Float seconds
  never appear inside the pipeline; HTTP `t=` seconds converts at the
  boundary, once.
- Downstream consumers (segmenter, gapless trimming, priming discard) read
  `Pos`/`Discont` in-band and never infer discontinuities on their own.

## Consequences

- Seek correctness is testable at one interface: land `format.Media` at a
  target, assert the first buffer's `Pos` equals it, sample-exact.
- Any node that changes the number or rate of samples must state its
  position policy in its doc comment; PR review checks new nodes against
  this ADR.
- `Buffer.Pos` is meaningful only within one stage's timeline; comparing
  positions across a rate change without rescaling is a bug by definition.
