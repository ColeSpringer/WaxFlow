# Deferred work

Known gaps that are understood, bounded, and deliberately not fixed in the
change that found them. Each entry says where it lives, what it is, why it
waits, and what found it. Things that need a sibling Wax repo go to
docs/upstream-requests.md instead.

## Four muxers carry a cache-key version with no golden bytes behind it

**Where:** `MuxerVersion` in `container/{ogg,wv,apen,adts}`, the
`goldens` target in the Makefile, ADR-0004's 2026-09-10 amendment.

**What:** the container term added to the cache key rests on a bump
being remembered, and what catches a forgotten one is a golden byte
comparison. `TestGoldenMuxOutputs` covers riff, aiff, flacn, mpa, mka
and mp4. The ogg, wv, apen and adts muxers have no golden, so a change
to what they write around unchanged packets reaches review with nothing
failing. Ogg is the one that stings: the granulepos fix (2026-07-29)
that motivated the term in the first place was an ogg muxer change, and
a repeat of it would still be caught by nothing. Their round-trip and
differential tests assert what the bytes mean, not what they are, so a
framing change that stays correct passes them.

**Why it is deferred rather than fixed:** four new golden fixtures with
their own regeneration story, and the two that matter are not
straightforward. Ogg's muxer batches packets into pages by a size and
duration target, so a golden pins the batching policy as much as the
framing and would fail on a deliberate retune; the wv and apen muxers
back-patch a header and write an APEv2 block whose content includes
nothing time-varying but whose layout has never been byte-pinned. Doing
it properly means deciding per muxer what the golden is allowed to
constrain, which is a larger piece of work than the term that exposed
the gap.
