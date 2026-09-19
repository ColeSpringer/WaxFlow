# ADR-0010: A timeline is built at the width it is delivered at

Status: Accepted (2026-09-19)

## Context

ADR-0009 made a timeline's envelope the format no member loses information to
reach: the maximum rate, the maximum channel count, the wider domain. A member
narrower than the envelope is widened by placement (its positions at unity, the
rest zero-filled), which keeps its own levels: BS.1770 sums channel powers, so a
placed member measures inside the queue exactly what it measures alone.

That holds only while nothing folds afterwards. `dsp/mix` builds its matrix from
the layout pair alone and energy-normalizes each output row over every source
column, silent ones included. A member widened by placement therefore has
columns in the divisor that carry nothing, and a later fold of the assembled
timeline is that member's own fold times `sqrt(E_own / E_envelope)`:

| member | envelope | folded to | error vs the member's own fold |
|---|---|---|---|
| 5.1 | 7.1 | stereo | -0.969 dB |
| stereo | 5.1 | stereo | **-3.010 dB** (its own fold is a no-op) |
| stereo | 7.1 | stereo | -3.979 dB |
| 5.1 | 7.1 | mono | -0.792 dB |
| mono | 5.1 | stereo | **-6.021 dB** |

It is a scalar on the output, so integrated loudness, true peak and sample peak
all move by it together and nothing downstream can tell that anything happened.

The normalization itself is not the defect. It is the documented BS.775 choice,
`mix.Version` is a cache key, and no row built from a layout pair can know which
of its columns carry silence. The member has to be folded before it meets its
siblings.

WaxFlow's own products had the defect. The merge job concatenated and then
planned a transcode, so a lossy row's fold folded the envelope: a stereo member
of a mixed-width merge to opus was delivered 3 dB under the same file merged
alone. `/hls` over a mixed-width `tl=` to a lossy format did the same. Nothing
tested or documented it.

ADR-0009 rejected "capping the envelope's channels at 2" because it is
output-aware knowledge `ConcatTrack` does not have, and a caller-pinned
`ConcatOptions.Format` because of a plan-versions drift hole. Both judgements
stand; this supersedes neither, it supplies the missing knowledge from the only
party that holds it.

## Decision

### `ConcatOptions.Channels` sets the timeline's width

Zero keeps ADR-0009's envelope. Nonzero makes it the envelope's channel count,
and every member whose count differs is conformed to it by its own chain: a fold
for a wider member, a placement for a narrower one, which is exactly the
conversion `TranscodeOptions.Channels` applies to that member read alone. A
member already at the count runs no chain, as before.

The fold is the delivery fold, not a measurement one. It sits inside the
member's chain, so the true-peak limiter runs with it (a 5.1-to-stereo matrix
has `MaxGain` 1.707) and an integer envelope requantizes at the envelope's depth
with TPDF, which is the cost a resampled member already pays.

One count, not a per-member list. Every delivery has one width, so the only
shape a list could express that one count cannot is "fold member 0 but leave
member 1 wider", which nothing produces.

The width enters the cache key by name, `tlwidth-N-1`, in `crossfadeVersion`'s
shape and for the same reason: two timelines over the same members at different
widths are different audio out of the same code.

### The engine resolves the width: `Engine.TimelineChannels`

It plans the raw envelope and returns the delivered count **only when the defect
would occur**: the delivered count differs from the envelope's *and* some member
differs from the envelope's. Otherwise 0.

That is what keeps adoption free. A uniform 5.1 album to opus keeps folding once
in the encode chain (its own fold, limiter after gain, no requantization), keeps
its cache keys, and keeps its bytes; a uniform stereo queue with an explicit
`ch=6` keeps widening downstream. Only a queue where a member would otherwise be
placed and then folded takes the new path. When it does answer a fold it logs
one, because the run's chain no longer will.

### A channel conversion never crosses a mixed-width seam

Any plan or run that would convert the channel count of a timeline whose members
do not all share the envelope's count is refused, `CodeInvalidRequest`, naming
the remedy. It is the exact complement of the rule above, through the same
predicate, and it turns a silent 1-to-6 dB error into a named error for any
caller that does not adopt the option.

The guard runs before the seek and the confirm walk, keeping `TranscodeMedia`'s
"refused before anything reads the source" rule. It asks a yes-or-no question
about the samples, not about the member list, so it is answered through a
one-method optional interface, `format.MixedWidth`, which `Slice` forwards
(unlike `Composite`, this question has a right answer for a window) and every
wrapper that forwards `Walked` forwards too.

Widenings are refused as well as folds, and mono is the reason: a placement
commutes, but mono does not place, it duplicates. A mono member in a stereo
envelope sits on both fronts, and widening that to 5.1 leaves it there, where
the member's own conversion puts it on the center, 3 dB away.

A uniform-width composite converts freely: that fold is every member's own.

## Alternatives rejected

- **Changing `dsp/mix`'s normalization** so silent columns do not count. The
  matrix is built from a layout pair and cannot know which columns a particular
  stream leaves silent; making it know would mean a per-run matrix, a
  `mix.Version` bump, and a different answer for content that is momentarily
  silent in a channel it does carry.
- **A per-member `Channels` list.** Nothing delivers one (see above).
- **Capping the envelope at the delivered width inside `ConcatTrack`.**
  ADR-0009's reason still holds: the function does not have the output. The
  caller states the width instead, and the engine tells the caller what it is.
- **Leaving the downstream fold legal and documenting the error.** It is
  invisible in every measurement, which is precisely why it has to be an error.

## Consequences

- A timeline is built at the width it is delivered at. Every in-tree caller (the
  merge job's validation and run, the HLS variant plan and worker) resolves the
  width through `Engine.TimelineChannels`.
- An external caller who concatenated mixed-width members and then transcoded to
  a lossy format now gets a 400 where they used to get audio. That is a
  behavioural break on the public API, recorded in ADR-0002; the audio they used
  to get was wrong by up to 6 dB.
- A queue without the defect is untouched: same chain, same cache keys, same
  bytes. The predicate that decides is written once and read by both halves,
  which is what makes that guarantee checkable rather than hoped for.
- The crossfade bound is checked at the wider of the envelope's width and the
  widest member's, so a fold can never loosen a bound a caller already passed.
