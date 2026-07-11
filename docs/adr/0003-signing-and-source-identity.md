# ADR-0003: Signed playback URLs and source identity

Status: Accepted (2026-07-02)

## Context

Players (browser `<audio>`, AVPlayer, ExoPlayer, hls.js) cannot attach API
keys to media requests, and HLS multiplies requests across segments. Stream
and segment fetches therefore authenticate with short-lived signed URLs.
Signed URLs are bearer tokens exposed to the WAN; the format cannot change
casually, so it is pinned before the first line of
server code.

## Decision

**Signature.** `exp` (unix seconds) + `kid` (key id) + `sig` = base64url,
no padding, of HMAC-SHA256 over the canonical string:

    "waxflow-v1" "\n" method "\n" path "\n" canonicalQuery

- `canonicalQuery`: all query parameters except `sig`, sorted by key then
  value, URL-encoded per RFC 3986. Every playback-affecting parameter
  (`src`, `format`, `bitrate`, `bits`, `exp`, `kid`, ...) is inside the
  signature, so no part of a signed URL can be altered.
- **Method normalization: HEAD signs and verifies as GET**, so players'
  preflight HEAD requests cannot fail verification.
- Verification uses `crypto/subtle` constant-time comparison.
- `kid` selects from a rotation list; the active secret comes from config or
  is auto-generated and persisted under `dataDir` with mode 0600.
- Default TTL: `max(6h, 2 x source duration)`.

**Source identity.** The signed descriptor embeds the identity of the bytes
it was minted for: `size + mtimeNS`. On mismatch at request time the server
returns `410 source-changed` and the client re-mints. A stale URL can never
serve surprise content, and cache keys (ADR-0004) stay coherent with
signatures.

*Amended at M17 (resolver mode):* the original text said resolver mode
"adds PID and catalog sequence" to the identity. The PID needs no new
field (it is the `src` parameter, already inside the signature), and the
catalog sequence is deliberately excluded: identity pins bytes, and a
rename or catalog metadata edit changes no bytes. Folding the per-item
change sequence in would kill valid URLs on every rename, the exact event
the resolver's re-resolution makes transparent, while byte changes are
already caught because the identity is stat'd from whatever file the PID
currently resolves to.
Content hashing was rejected: hashing a 300 MB FLAC on first request
defeats the time-to-first-audio budget; `mtimeNS` granularity is the
documented residual risk, with `POST /cache/gc` as the escape hatch.

## Consequences

- The version prefix `waxflow-v1` makes future canonical-string changes an
  explicit new scheme rather than a silent break.
- Because `exp` is inside the signature, clock skew handling belongs to the
  verifier. Pinned with the streaming server: a fixed **60 second** leeway past `exp`
  (`sign.Leeway`), tolerant of unsynchronized self-hosted boxes without
  meaningfully extending a URL's life. Signature validity is checked
  before expiry, so a tampered expired URL reports `signature-invalid`,
  never `signature-expired`.
- The identity travels as the `id` query parameter (`<size>-<mtimeNS>`),
  inside the signature like everything else; signed URLs without it are
  refused as invalid.
- The `client` package ships a mint helper.
