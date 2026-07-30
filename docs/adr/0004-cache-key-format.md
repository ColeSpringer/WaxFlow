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
