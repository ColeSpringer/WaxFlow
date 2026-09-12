# ADR-0004: Cache key format

Status: Accepted (2026-07-02)

## Context

The transcode cache is content-addressed: the same request must find the
same entry across restarts, and any change that alters output audio must
miss. A stale hit serves wrong audio silently, the worst failure mode a
transcoder has.

## Decision

    key = SHA-256(
        cacheSchemaVersion || sourceIdentity || canonicalOutputParams || nodeVersions
    )

- `cacheSchemaVersion`: bumps invalidate everything (layout changes).
- `sourceIdentity`: `ref + size + mtimeNS`, identical to the identity
  inside signed URLs (ADR-0003). In resolver mode the ref is the
  `pid:<ULID>` reference itself, so the PID keys entries with no extra
  field; the catalog sequence is deliberately excluded (amended with resolver mode,
  see ADR-0003): a rename changes no bytes, so it must not orphan cache
  entries, while replaced content misses because size+mtimeNS come from
  the file the PID currently resolves to.
- `canonicalOutputParams`: every parameter that shapes output bytes
  (format, bitrate/quality, bits, rate, channels, gain mode, segment
  duration for HLS) serialized in one canonical order.
- `nodeVersions`: the `Version()` constant of **every sample-affecting node
  in the chain**: the source codec's decoder (amended when the first
  decoder revision shipped and found decode versions defined per codec
  but never wired into the key), each encoder (bitstream/algorithm
  revision, psy-model revision, deterministic-mode flag) *and* each DSP
  node (resampler, dither, limiter, mix matrices). A resampler fix that
  changes output samples must never serve stale audio; conversely,
  improving the Opus encoder invalidates only entries whose *output* is
  Opus, and revising the Opus decoder only entries whose *source* is.
  Demuxer changes that alter emitted samples (trim fixes) remain covered
  only by a schema bump; they are rarer than codec revisions and the
  standing PR question below applies.

Layout (fixed alongside the key): `cacheDir/v1/<aa>/<hash>/meta.json` plus
`out.<ext>` (progressive) or `init.mp4 seg-*.m4s media.m3u8` (HLS variant).
Writes go to `*.tmp` with atomic rename; only completed progressive outputs
promote; HLS segments are individually complete and cache incrementally.
Probe results and frame indexes live under `cacheDir/idx/`, keyed by the
same source identity.

## Consequences

- Every encoder and DSP node carries a `Version()` from birth; forgetting to
  bump it on a sample-affecting change is the residual risk, so PR review
  treats "does this change output samples?" as a standing question, and the
  golden-stream tests catch unbumped changes by failing byte-comparison.
- Cache entries are never shared across `cacheSchemaVersion` bumps; no
  migration code, ever.
- **Known gap: there is no container or muxer term.** `nodeVersions` covers
  decoder, DSP nodes and encoder, so a change to how a muxer frames the same
  encoded packets alters output bytes with nothing in the key to notice, and
  every cached response keeps serving the old bytes until eviction. This is
  the demuxer gap named above, seen from the write side, and it is the same
  standing PR question.

  Found by the Ogg-Vorbis granulepos fix (2026-07-29), which changed only
  `container/ogg/muxmap.go`. The workaround was to borrow the nearest
  encoder term, `vorbis.EncoderVersion`, with a comment at the constant
  saying so. It is the narrowest lever available, not a precise one, and it
  errs in both directions:

  - It **over-invalidates**: Vorbis-in-Matroska shares the encoder term, so
    those entries drop even though the mka muxer did not change and their
    bytes are identical. The cost is one re-encode per entry, no
    correctness risk.
  - It **under-covers** in general: a change to a muxer that carries several
    codecs has no single encoder term to borrow, and nothing narrower than a
    schema bump would work.

  The fix is to add a container term to the tuple. It is deliberately not
  done yet, because adding an element to the joined version string
  invalidates *everything* on first deploy, which is the same practical cost
  as a `cacheSchemaVersion` bump; the value is precision for every muxer
  change after this one. Land it with the next schema bump rather than
  spending a full invalidation on its own.

## Amendment (2026-09-10): the container term, added

The known gap above is closed. `nodeVersions` gains a fourth element, so
the tuple is `[decoder, dsp..., encoder, muxer]`:

- Every progressive muxing container package carries a `MuxerVersion`
  constant, spelled `<name>-mux-<n>`: `adts`, `aiff`, `apen`, `flacn`,
  `mka`, `mp4`, `mpa`, `ogg`, `riff`, `wv`. All start at `-1` except
  `mpa`, which ships at `mpa-mux-2` because the same change that added
  the term also fixed what that muxer writes.
  `waxflow.muxerVersion` maps a resolved container name onto its
  package's constant, keyed on the same name `output.mux` branches on so
  the term cannot name a muxer other than the one that ran.
- A remux plan carries `[RemuxVersion, muxer]`, and a progressive cut
  appends `CutVersion` to that. `RemuxVersion` keeps only what it always
  described best, the gapless trailer this rung synthesizes; a muxer
  change bumps the muxer's own term instead of borrowing it.
- Segmented plans are unchanged: their muxer *is* the segmenter, and
  `mp4.SegmenterVersion` already keys it. They do inherit the progressive
  term through the transcode plan they embed, and the term they inherit
  is the one their FORMAT's progressive container resolves to, not
  mp4's: an HLS FLAC plan carries `flacn.MuxerVersion` and an HLS Opus
  plan `ogg.MuxerVersion`, muxers that never run for a CMAF output,
  while `mp4.MuxerVersion` reaches an HLS key only for the aac, he-aac
  and alac rows, whose progressive container is MP4 too. So the
  inherited term over-invalidates for every segmented format and covers
  nothing that `mp4.SegmenterVersion` does not already cover. That is
  accepted rather than trimmed: the event is rare, the cost is one
  re-encode per entry, and stripping the last element of an inherited
  slice is a second version-assembly path that could disagree with the
  first. It is imprecise, not unsafe: the segmented form has no
  under-covered muxer.
- One `mp4.MuxerVersion` covers both `NewMuxer` (fragmented) and
  `NewProgressiveMuxer` (moov-first), because they share the sample
  entries and movie boxes a change here would be about; a revision to
  either bumps it.

`cacheSchemaVersion` is *not* bumped. The layout does not change and the
key already moves, so every cached progressive and HLS entry misses once
after deploy and regenerates on demand; the size-bounded GC ages the
orphans out. That one-time full invalidation is the cost the gap
paragraph above was waiting to spend, and it is spent here rather than
saved for a schema bump that may not come: the sample-entry fix
(2026-09-06) had already shipped bytes that cached progressive ALAC
transcodes were not regenerating.

The residual risk is the same standing PR question, moved: forgetting to
bump a `MuxerVersion` on a framing change.
`TestMuxerVersionNamesTheMuxerThatRuns` cannot see that; it checks which
term is used, not whether it was bumped.

What catches an unbumped change is a golden byte comparison, and all ten
packages now have one (see the `goldens` target). Within a case, the
golden constrains every byte: a paging retune that moves a page boundary
changes bytes and so has to bump `MuxerVersion` anyway, and the golden
failing is the reminder. Regeneration is `make goldens` plus a reviewed
diff, and the failure message names the version bump as well as the
command.

Coverage is per case, not per muxer, so the cases are chosen to sit on
the boundaries a retune would move: `ogg`'s Opus case packs packets that
land a page exactly on the byte target and one byte past it, so the
target is caught moving in either direction, and reaches the segment-table
and granule caps on their own exact boundaries. Where one version term
covers two writers it takes two cases: `mp4.MuxerVersion` covers the
fragmented muxer and `NewProgressiveMuxer`, which share little beyond the
sample entries, so each has its own golden. What no golden reaches is
still real: the optional tag, chapter and cover-art boxes several muxers
write are exercised by their own round-trip tests rather than pinned byte
for byte, so a change confined to those would reach review with nothing
failing.

The goldens are compared on every architecture CI runs: amd64 under the
race pass, arm64 on the macOS leg, and 386 through `make test-386`. That
is the evidence behind them rather than a determinism argument, and the
distinction matters, because `docs/quality-gates.md` records that
byte-exactness is a within-a-build property and that a float encoder can
legitimately differ in the last ulp across architectures. Two encoders
are known to, and both are pinned to one architecture and skipped
elsewhere (`codec/vorbis.goldenEncodeArch`, `codec/wma.wantDigestArch`).
An encoder pinned that way must not feed a committed golden, which is why
the `ogg` Vorbis case demuxes a committed libvorbis stream for its
CodecConfig and packets and re-muxes those: it exercises the whole
mapping without running our own encoder. The Opus case is canned packets;
the FLAC, AAC, ALAC, WavPack and Monkey's Audio encoders behind the rest
were already pinned this way before these goldens were added, and have
held. If one ever stops holding, CI says so on the leg that disagrees,
which is the point of comparing there.

The one output too large to commit is `apen`'s unknown-length layout,
whose seek-table reservation is 233 KB of zeros at this frame length. Its
golden is the SHA-256 of the stream, kept in a file like any other so
`make goldens` regenerates it.
