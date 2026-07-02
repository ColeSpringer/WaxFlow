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
- `sourceIdentity`: `path + size + mtimeNS` (+ PID and catalog sequence in
  resolver mode), identical to the identity inside signed URLs (ADR-0003).
- `canonicalOutputParams`: every parameter that shapes output bytes
  (format, bitrate/quality, bits, rate, channels, gain mode, segment
  duration for HLS) serialized in one canonical order.
- `nodeVersions`: the `Version()` constant of **every sample-affecting node
  in the chain**: each encoder (bitstream/algorithm revision, psy-model
  revision, deterministic-mode flag) *and* each DSP node (resampler, dither,
  limiter, mix matrices). A resampler fix that changes output samples must
  never serve stale audio; conversely, improving the Opus encoder
  invalidates only Opus entries, not the whole cache.

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
