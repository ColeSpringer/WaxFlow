# MAINTENANCE

Operational procedures that gate releases. The policy behind them is
ADR-0001 (clean-room) and docs/quality-gates.md.

## Clean-room procedure

Reference tiers are defined in [ADR-0001](docs/adr/0001-clean-room-policy.md).
Operationally:

1. **Tier A work** (specs, papers, BSD/MIT/Apache/PD sources): study and
   port freely. Record every ported source in `THIRD-PARTY-NOTICES.md` in
   the same PR.
2. **Tier B work** (LGPL/GPL: LAME, Shine and its Go ports, ffmpeg, faad):
   never open while implementing the corresponding component. Behavioral
   analysis happens in separate, dedicated passes whose only outputs are
   black-box artifacts (behavioral notes and parameter tables under
   `docs/notes/`, test vectors under `testdata/`) which implementation
   sessions then consume. No line-by-line porting, ever.
3. Tier B *binaries* are permitted as test oracles (differential CI jobs).
4. Every PR affirms the checklist in `.github/pull_request_template.md`.

### Reference ledger

| Component | Tier A references used | Tier B behavioral artifacts |
|---|---|---|
| codec/flac (decoder) | RFC 9639 (spec); IETF flac-test-files suite (test vectors, SHA-256-pinned); libFLAC behavioral fact only: unequal STREAMINFO block bounds mark pre-1.0 variable-blocksize streams (libFLAC is BSD/Tier A regardless; no source consulted) | none |
| container/flacn | RFC 9639 (spec) | none |
| container/ogg | RFC 3533 (spec); Xiph Ogg-FLAC mapping 1.0 (spec) | none |
| codec/mp3 (decoder) | ISO 11172-3 / 13818-3 (spec); PDMP3 via hajimehoshi/go-mp3 (Apache-2.0, pipeline structure + tables); minimp3 (CC0, LSF scalefactor/intensity/band-edge handling) | none |
| codec/mp3 (encoder) | ISO 11172-3 / 13818-3 (spec, quantization and Huffman); the forward Huffman tables and the polyphase analysis window are derived in code from the decoder's already-attributed decode trees and synthesis window (no new source); textbook filterbank/MDCT theory | Shine reached only as an `ffmpeg -c:a libshine` quality oracle (never opened; the ODG-proxy parity gate) |
| container/mpa | ISO 11172-3 (spec); Xing/Info/VBRI and LAME-tag layout (documented interchange formats) | none |

## AAC patent-status review

**Open. Must be completed before the AAC-LC encoder is enabled in release
builds.** Record here a good-faith review of the base MPEG-2/4 AAC-LC
patent status at time of shipping (the base patents are widely believed
expired; verify then, not now).

## Listening-test protocol

The nightly encoder-quality harness (`make encoder-quality`, the
`encoder-quality` job in `nightly.yml`) is the objective gate: it encodes
the corpus with our encoder and the reference baseline (Shine for MP3),
scores both with the ODG-proxy (`internal/testutil/odg.go`, a bark-band
noise-to-mask ratio), and fails when our corpus mean falls below the
baseline or any track drops more than the per-codec allowance. The HTML
report is uploaded as a CI artifact.

Objective scores are a proxy, so a subjective ABX pass gates a release
when a codec's quality changes:

1. **Material.** Use the same corpus classes the gate names (broadband
   music, speech, transients, tonal). Prefer the pinned real-audio
   vectors once they land; the synthesized corpus is the interim stand-in.
2. **Preparation.** Encode each item with the release build and decode it
   back. Level-match decoded and reference to within 0.1 dB and align
   them sample-exact (the gapless trims already do this for our streams).
3. **Procedure.** Blind ABX (reference vs coded, order randomized) with at
   least 12 trials per item per listener, two listeners minimum. Record
   the identification rate; anything a listener cannot distinguish from
   the reference passes that item.
4. **Decision.** A release is clear when no item is reliably
   distinguished (identification rate not significantly above chance) at
   the target bit rate. A regression that the objective gate misses but a
   listener catches blocks the release and re-baselines the metric.

## Release checklist (grows over time)

- [ ] `make check` green (fmt, vet, race tests, depcheck)
- [ ] `THIRD-PARTY-NOTICES.md` audited against the reference ledger
- [ ] Tag `vX.Y.Z` pushed -> `release.yml` publishes binaries + SHA256SUMS +
      multi-arch image to ghcr.io
- [ ] Container smoke: `docker run` + HEALTHCHECK healthy
