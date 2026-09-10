# Deferred work

Known gaps that are understood, bounded, and deliberately not fixed in the
change that found them. Each entry says where it lives, what it is, why it
waits, and what found it. Things that need a sibling Wax repo go to
docs/upstream-requests.md instead.

## ALAC sample entries at 20 and 24 bits are unverified on Apple clients

**Where:** `container/mp4/mux.go` (`alacSampleEntry`), item 1 of the
manual checklist in docs/client-matrix.md.

**What:** the 'alac' entry's `samplesize` now carries the cookie's depth
(20, 24, or 32) where it carried a conventional 16, in a version-0
AudioSampleEntry. ffmpeg writes the same for MP4-brand files and its
output plays on Apple devices, and Apple's decoder takes the ALAC format
from the cookie, but Apple's is the one client family that decodes ALAC
and the one CI cannot drive, so this rests on ffmpeg parity until a
human runs the checklist.

**Why it is deferred rather than fixed:** it needs a Mac or iOS device.
The checklist item exists; record the client version and outcome there
and delete this entry when it has run.

**Found by:** the third-party review of the sample-entry fix (2026-09-06).

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

**Found by:** the third-party review of the cache-key muxer term
(2026-09-10).
