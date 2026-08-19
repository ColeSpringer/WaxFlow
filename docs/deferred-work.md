# Deferred work

## Muxer patch offsets assume the writer starts at zero

**Where:** `container/wv/mux.go`, `container/flacn/mux.go`,
`container/riff/mux.go`, `container/aiff/mux.go`.

**What:** each of the four back-patches a header field by seeking to an
absolute offset and then restoring the write position with
`Seek(m.off, io.SeekStart)`, where `m.off` is a count of bytes the muxer has
written. The two are only the same number when the destination started at
offset zero. wv is the clearest instance (it patches the total-samples field
at absolute offset 11), but the convention, and the assumption, are shared
verbatim by all four.

**Why it is not a live bug:** every caller hands the muxer a freshly created
or truncated file positioned at zero, so the absolute and relative offsets
coincide. Nothing in the engine, the CLI, or the job runner writes a muxed
stream into the middle of a larger file, and no API exposes a way to.

**Why it is deferred rather than fixed:** the fix is to record the writer's
starting offset at Begin and make every patch and restore relative to it.
Doing that in wv alone would leave one muxer following a different convention
from its three siblings, which is a worse state to be in than the shared
assumption: the next person reading riff's patch path would have no way to
tell whether the difference was deliberate. It wants one pass across all four,
with a test that muxes into a writer already positioned past zero, which is a
test helper none of the four currently has.

**Found by:** the stage 5 third-party review round (2026-08-19).

## WavPack blocks carry no checksum sub-block

**Where:** `codec/wavpack/encode.go`.

**What:** libwavpack sets `HAS_CHECKSUM` in its block flags and appends an
`ID_BLOCK_CHECKSUM` sub-block covering the block's own bytes; we do not. Our
blocks carry the header CRC over the decoded samples, which is what
`wvunpack -v` verifies and what our decoder checks, so this is an optional
robustness feature rather than a conformance gap: the reference verifies every
stream we write.

**Why it is deferred:** it detects corruption of the coded bytes that the
sample CRC would only catch after a decode, which is a real but small
improvement, and it changes every block's bytes (so it costs an encoder
version bump). Worth doing alongside the next change that bumps the version
anyway, rather than on its own.

**Found by:** the stage 5 third-party review round (2026-08-19), while
checking the stream-version claim. The encoder now states its own
`writtenStreamVersion` rather than the decoder's `MaxStreamVersion`, so
raising what we can read no longer changes what we claim to have written.
