# ADR-0001: Clean-room policy for codec references

Status: Accepted (2026-07-02)

## Context

WaxFlow implements six encoders and eight-plus decoders from scratch in pure
Go under an MIT license. The best existing implementations span every
license class: BSD/MIT/Apache/public-domain (libopus, libFLAC, Apple ALAC,
minimp3, stb_vorbis, mewkiz/flac, hajimehoshi/go-mp3) and LGPL/GPL (LAME,
Shine, ffmpeg, faad). Consulting the wrong source the wrong way contaminates
the codebase and voids the license promise.

## Decision

Two reference tiers, enforced per component:

- **Tier A: specs, papers, textbooks, and permissively licensed sources**
  (BSD/MIT/Apache/PD: libopus, libFLAC, Apple ALAC reference, minimp3,
  stb_vorbis, RFC appendix code, Bosi/Goldberg). These may be studied
  closely and ported, with attribution recorded in `THIRD-PARTY-NOTICES.md`.
- **Tier B: LGPL/GPL sources** (LAME, Shine and Go ports of it, ffmpeg,
  faad). These are **never open during implementation of the corresponding
  component**. They may only inform black-box artifacts produced in separate
  analysis passes (behavioral notes, parameter tables, and test vectors
  checked into `testdata/` or `docs/`) which implementation sessions then
  consume. No line-by-line porting, ever. Tier B binaries (ffmpeg, LAME) are
  permitted as *test oracles*: invoking a program is not copying it.

Process enforcement:

- Every PR affirms the clean-room checklist
  (`.github/pull_request_template.md`).
- `MAINTENANCE.md` carries the operational procedure and the per-codec
  reference ledger.
- Before the AAC-LC encoder is enabled in release builds, a good-faith
  patent-status review is recorded in `MAINTENANCE.md` (base MPEG-2/4 AAC-LC
  patents are widely believed expired; verify at time of shipping).

## Consequences

- AAC-LC gets the strictest treatment: no permissive reference encoder
  exists, so it is implemented from ISO/IEC 14496-3 and the textbook, with
  Tier-B behavioral notes only.
- Analysis passes and implementation passes are separate work sessions,
  which costs calendar time but makes provenance auditable.
- `THIRD-PARTY-NOTICES.md` is release-gated: the release checklist audits it
  against the reference ledger.
