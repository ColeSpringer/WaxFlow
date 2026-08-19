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
| codec/mp3 (encoder) | ISO 11172-3 / 13818-3 (spec: quantization, Huffman, scalefactor/compress/preflag layout, the informative two-loop encoder structure); the forward Huffman tables and the polyphase analysis window are derived in code from the decoder's already-attributed decode trees and synthesis window (no new source); dsp/psy (own, spec-derived) drives the noise shaping; textbook filterbank/MDCT theory | Shine and LAME reached only as `ffmpeg -c:a libshine` / `-c:a libmp3lame` binary quality oracles (never opened; the ODG-proxy gate names Shine, LAME is informational) |
| container/mpa | ISO 11172-3 (spec); Xing/Info/VBRI and LAME-tag layout (documented interchange formats) | none |
| dsp/psy | ISO 11172-3 Annex D model 2 and ISO 13818-7 Annex B (spec, informative psychoacoustic model); Terhardt ATH approximation and the bark scale (published formulas) | none |
| codec/aac (encoder) | ISO 14496-3 (spec, incl. the informative encoder annex's two-loop structure); Bosi/Goldberg (textbook); forward Huffman tables, band boundaries, and windows derived in code from the decoder's already-attributed tables (no new source) | ffmpeg's native AAC encoder reached only as a binary quality oracle (never opened; the ODG-proxy gate) |
| codec/aac (SBR/PS decode) | ISO 14496-3 4.6.18 and 8.6.4 (spec); normative parameter tables recorded per ADR-0001's parameter provision (see THIRD-PARTY-NOTICES) | ffmpeg's AAC decoder as the differential oracle and as a source of behavioral facts (validity bounds, header state-machine rules, the QMF restart modulation confirmed tap-for-tap against an instrumented build); table restatements as parameter cross-checks: faad2 and FFmpeg for the SBR tables, FFmpeg alone for the PS tables; no decoder logic taken |
| codec/aac (SBR encode) | ISO 14496-3 4.6.18 (spec, band tables and payload syntax shared with the decode entry above); the encoder-side 64-band QMF analysis, extraction heuristics, and grid logic written from the spec's decoder semantics (what the adjuster does with each field decides what to put in it); forward Huffman tables derived in code from the decoder's trees (no new source) | libfdk_aac reached only as a binary quality oracle where the nightly's ffmpeg carries it (never opened, bespoke license); ffmpeg's decoder as the cross-decoder conformance oracle on our own streams |
| codec/wavpack (decoder) | libwavpack 5.9.0 (BSD-3-Clause): the entropy decoder, decorrelation passes and their weight macros, metadata sub-block handlers, the exp2 mantissa table, and the block sync predicate ported faithfully because a lossless decoder has to be bit-exact (see THIRD-PARTY-NOTICES); the official WavPack test suite (test vectors, SHA-256-pinned) | ffmpeg's WavPack decoder and the reference `wavpack`/`wvunpack` binaries as differential oracles and fixture generators |
| container/wv | the WavPack block layout as documented in the reference headers (Tier A) | none |
| container/adts (muxer) | ISO 14496-3 1.A (spec); the write-side inverse of the demuxer's header parser | none |
| container/mp4 (esds writer) | ISO 14496-1 section 7.2.6 descriptors (spec); the write-side inverse of the demuxer's parser | none |

## AAC patent-status review

**Recorded 2026-07-10, when the AAC-LC encoder was enabled in release
builds.** This is a good-faith engineering review, not legal
advice.

- WaxFlow implements only the AAC-LC toolset: window switching, TNS,
  M/S stereo, Huffman coding, the two-loop quantizer. Every one of
  these tools is present in MPEG-2 AAC (ISO/IEC 13818-7, published
  1997; essential filings 1997 and earlier), whose base patents, on
  20-year terms, expired by the late 2010s. Commonly cited expiry
  surveys place the last base AAC-LC-relevant patents' expiry in the
  early 2020s across major jurisdictions; all predate this review by
  several years.
- Public precedent: Red Hat's legal review cleared an LC-only encoder
  and decoder ("fdk-aac-free") for Fedora in 2017, and distributions
  have shipped LC codecs since. ffmpeg has shipped a native AAC-LC
  encoder in default builds for years.
- **SBR/PS review, recorded 2026-08-15** (the redo this section's
  self-trigger demanded before HE-AAC decode ships; same class of
  good-faith analysis that cleared LC):
  - Classic SBR's essential patents are expired. The core Coding
    Technologies/Dolby family (priority 1997-06-10): US7283955 expired
    2021-01-05. The 2002-era refinement family (EP1408484, adaptive
    noise floor) is expired. The last known straggler, US8935156
    ("Enhancing performance of SBR"), expired 2026-03-09, five months
    before this entry.
  - PS's essential patents are expired. The canonical Philips family
    (Breebaart/Schuijers, priority 2002-07): US7542896 lapsed
    fee-related, term end 2025-05-26 at the latest. The v2 spec froze
    in 2004; nothing essential to it outlives 2026 by more than
    administrative term adjustment.
  - The live mines are **enhanced SBR**: Dolby holds active patents to
    2039 (US11810590, US11810591, US11810592, US11862185, US11289106;
    priority 2018) covering harmonic transposition, pre-flattening, and
    eSBR signaling. Post-spec filings cannot be essential to the
    2003/2004 codec. Keep-out rule: implement classic spectral-patching
    SBR per ISO/IEC 14496-3 only, never eSBR, and skip its extension
    payloads. The keep-out is executable, not aspirational:
    `codec/aac/sbr_esbr_test.go` proves an eSBR extension id is skipped
    by length with output identical to the classic-only decode.
  - Context: Via LA still licenses the pool list, as it did for AAC-LC
    years after LC essentials expired (Red Hat cleared LC in 2017;
    ffmpeg ships it by default; no enforcement since).
- The HE-AAC v1 and v2 decoders (SBR, PS, downsampled SBR, ADTS
  implicit signalling; 2026-08) ship under that review, and so does the
  HE-AAC v1 *encoder* (2026-08): it emits classic spectral-patching SBR
  only, no eSBR tool exists to signal, and the expired families above
  cover the encode direction of the same technology. xHE/USAC or a PS
  (v2) encoder would trigger a redo.

## Listening-test protocol

The nightly encoder-quality harness (`make encoder-quality`, the
`encoder-quality` job in `nightly.yml`) is the objective gate: it encodes
the corpus with our encoder and the reference baseline (Shine for MP3,
ffmpeg's native aac for AAC-LC, libfdk_aac for HE-AAC where the nightly's
ffmpeg carries it, libopus via the reference tools for
Opus), scores both with the ODG-proxy (`internal/testutil/odg.go`, a
bark-band noise-to-mask ratio) or opus_compare, and fails when our corpus
mean falls below the baseline or any track drops more than the per-codec
allowance. The HTML reports are uploaded as CI artifacts.

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

### A deliberately audible node needs a different protocol

The ABX protocol above rests on one assumption: the node under test is
trying to be transparent, so "the listener cannot distinguish it from the
reference" is a pass. Every codec and every resampler here is like that.

`dynamics=voice` is not. It **must** be distinguishable from the
reference; that is the entire feature, and a `voice` preset that passed
an ABX would be a broken one. Running the protocol above against it would
either fail it for working or, worse, pass it for doing nothing.

So a dynamics preset gets a **subjective sign-off** rather than an ABX,
and it is a release gate for exactly the same reason the ABX is: the
objective tests (`TestCompressorReducesRange` and friends) prove the
curve does arithmetic, not that the arithmetic sounds right.

1. **Material.** Spoken word, and specifically the case the preset exists
   for: a wide-range reading with quiet passages, not studio-levelled
   broadcast speech that has nothing left to compress.
2. **Preparation.** Serve the same source twice, `dynamics=off` and
   `dynamics=voice`, with the same `gain=` in both, since the preset acts
   on the post-gain signal and comparing across different levels compares
   the wrong thing.
3. **Procedure.** Sighted, not blind. Listen at low volume, which is the
   condition the preset exists for. The questions are: are the quiet
   passages now intelligible, does the loud material stay unpumped, and
   is the noise floor between phrases still unobtrusive (makeup gain
   raises it too).
4. **Decision.** One named listener signs off per preset per change to
   its curve. A `CompressorVersion` bump is the marker that this is owed
   again: the constant exists to invalidate caches, but it is also the
   flag that the curve moved and nobody has heard it yet.

A `LimiterVersion` bump does not carry the same marker by default, and the
`limiter-3` rebuild is the precedent. It replaced the one-pole attack with a
min-hold plus a smoothed envelope, so the resulting gain is smoother than the
old one, more conservative everywhere, and provably at or below the ceiling
where the old one was not. Every audible difference is in the direction of
less distortion, which makes it a defect fix rather than a voicing change. The
distinction that matters: a design that reaches the ceiling by *notching* the
gain at individual peaks would introduce a new artifact class and would owe the
listen, whatever its measurements said. Record the reasoning when bumping, so
the next bump is not decided by precedent alone.

## Fuzzing posture

Every parser (demuxers, packet decoders, probe, the HLS descriptor, the
signature verifier) carries a native `Fuzz*` target; findings become
committed regression corpus entries under `testdata/fuzz/`. The layout
is OSS-Fuzz-compatible (native Go fuzzing, no external fixtures needed
to build the targets), so onboarding to OSS-Fuzz needs only the
standard `compile_native_go_fuzzer` build script listing the targets.
Budgets: CI smoke 45 s/target, nightly 20 m/target, and a release soak
via `make fuzz FUZZTIME=160m` (about 80 hours of aggregate fuzzing
across the ~30 targets; run it on a spare box, not CI).

## Release checklist (grows over time)

- [ ] `make check` green (fmt, vet, functional + race passes, the module
      suites `test-cli` / `test-oracle` / `test-example`, depcheck)
- [ ] `THIRD-PARTY-NOTICES.md` audited against the reference ledger
- [ ] Root `go.mod` require block still empty (the v1.0 structural
      guarantee; new dependencies belong in the cli or oracletest
      modules)
- [ ] **No module here requires a module that requires waxflow.** This is
      what keeps waxflow a leaf and the empty require block above
      structural rather than aspirational. `resolver/` was the only one
      that ever did (it required waxbin), and it was dropped 2026-07-17;
      its last commit is `8bc7751` ("Update itemPIDs function to include
      empty userPID in query"), so recovering that code is `git show
      8bc7751:resolver/resolver.go`, not archaeology. Adding such a
      module reopens the cycle risk this closed; the existing convention
      that a new `go.mod` entry needs justification is where that gets
      caught
- [ ] `make soak` on a quiet box: streaming soak clean (no goroutine or
      heap growth), TTFA p95 targets met; update the README performance
      section if the numbers moved
- [ ] Fuzz soak run for a release that touched any parser
      (`make fuzz FUZZTIME=160m`, see the fuzzing posture above)
- [ ] Client matrix re-run for a release that touched delivery:
      `make client-e2e` (automated browser cells) plus the manual
      checklists in docs/client-matrix.md (Apple, ExoPlayer, mpv);
      update the /caps profiles if any cell changed
- [ ] Tag `vX.Y.Z` pushed -> `release.yml` publishes binaries + SHA256SUMS +
      multi-arch image to ghcr.io
- [ ] Container smoke: `docker run` + HEALTHCHECK healthy
