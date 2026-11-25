# ADR-0014: Priming a segmented run

Status: Accepted (2026-11-25)

## Context

An HLS variant worker runs from the segment it starts at to the end of the
stream, and a seek restarts it at any segment. Segment n from a restarted
worker must be the segment a continuous run produces: its packets at the
same decode positions, and its audio joining segment n-1 without a seam.

A run that starts mid-stream starts every stateful part of the pipeline
cold. The encoder carries cross-frame state (psychoacoustic and prefilter
history). The resampler holds an FIR window, which a restart fills with
zeros. A dynamics node (a limiter, a compressor) holds an envelope that
decays exponentially and so converges only asymptotically; it declares a
`dsp.Settler` horizon, the pre-roll after which its quantized output on a
restart is bit-equal to a continuous run's.

A span (a window onto a longer stream, `format.Headroomer`) has real audio
before its own sample 0. A file has none.

## Decision

### Two windows: one for the encoder, one for the chain

`primeStarts` returns two starts on the output timeline for a run whose
first kept sample is `p0`.

- The encoder's window is `primeSeconds` (0.1 s) rounded up to whole encoder
  frames. The chain and the encoder both see it, and the packets it
  produces are discarded. It settles the encoder's cross-frame state and
  the finite-memory nodes: once the resampler's window holds real audio,
  their output on a restart equals a continuous run's.
- The chain's window is the encoder's plus the chain's horizon, rounded up
  to whole frames. Only the chain sees the part ahead of the encoder's
  window; its output is dropped before the encoder. The horizon assumes the
  finite-memory nodes have already converged, so the two windows add. A
  chain with no horizon (no gain, no dynamics) primes the encoder's window
  alone.

The split is a matter of cost. Every sample fed to the encoder costs a
decode, the DSP and an encode, and a horizon is orders of magnitude longer
than the encoder needs. Feeding the long window to the chain alone keeps
the encoder's share of a restart at the short window.

The chain's window is never capped. A short pre-roll is the defect the
horizon exists to prevent: a restarted worker would emit segments that
differ from a continuous run's.

### Starts are frame multiples, clamped at the stream top

`p0` is a whole number of frames and both windows round up to whole
frames, so both starts are whole frames too: the priming packets are
discarded on an exact packet boundary and the kept stream's decode
timeline is unshifted. Neither start reaches past the stream top. The
encoder's never goes below 0, and the chain's never below the floor, which
is rounded toward zero to a whole frame so the start stays one. A run from
the top of a file primes neither.

### A span primes its chain from before sample 0, never its encoder

The floor is 0 for a file and negative for a span (`headroomFloor`).
Feeding the chain from before a span's sample 0 makes that sample hold the
audio a continuous run of the whole source delivers there, since the
resampler's window then holds real samples. The encoder's window stops at 0
all the same: a span is served as its own stream with its own init
segment, so its sample 0 is where its encoder starts, as a whole track's
is, and an encoder primed from a negative position would number its first
frame below zero, which no encoder here can express.

`headroomFloor` holds back `headroomMargin` source samples, so the seek's
floor and one-sample back-off never ask for a sample before the source's
start.

### Positions live on the output timeline

The run computes every start on the output timeline and converts to the
source once, at the seek, landing at or before the chain's start. The
first chunk's `Pos` says how far before, and the run drops that slop
and the chain's pre-roll up to `pEnc` in one skip. This is why a horizon is
a duration (`dsp.Settler`): the resampler's window is in input samples and
a dynamics node's in output samples, and the run converts once.

## Alternatives rejected

**One window through the encoder.** Feeding the horizon through the encoder
too is simpler, and encodes the whole horizon on every restart only to
discard it.

**A capped horizon.** It bounds a restart's cost and breaks the property
the horizon exists for.

## Consequences

- A restarted worker's segments sit at the continuous run's decode
  positions on every codec; a FLAC restart is byte-identical.
- A chain with a dynamics node costs its horizon in decode and DSP on every
  restart, and no encode.
- A run's length counts from `pEnc`, where the encoder started.
