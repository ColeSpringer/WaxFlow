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
- The actively licensed parts of the Via/Fraunhofer AAC pool concern
  the later extensions: SBR/HE-AAC, PS, ELD, xHE/USAC. All are
  explicitly out of WaxFlow's scope (the decoder rejects or decodes
  only the LC base layer; the encoder produces LC only).
- Action if scope ever grows toward SBR/PS/xHE: redo this review
  first; those toolsets remain licensed.

## Listening-test protocol

The nightly encoder-quality harness (`make encoder-quality`, the
`encoder-quality` job in `nightly.yml`) is the objective gate: it encodes
the corpus with our encoder and the reference baseline (Shine for MP3,
ffmpeg's native aac for AAC-LC, libopus via the reference tools for
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

- [ ] `make check` green (fmt, vet, functional + race passes, the cli /
      resolver / oracletest module suites, depcheck)
- [ ] `THIRD-PARTY-NOTICES.md` audited against the reference ledger
- [ ] Root `go.mod` require block still empty (the v1.0 structural
      guarantee; new dependencies belong in the cli, resolver, or
      oracletest modules)
- [ ] `make soak` on a quiet box: streaming soak clean (no goroutine or
      heap growth), TTFA p95 targets met; update the README performance
      section if the numbers moved
- [ ] Fuzz soak run for a release that touched any parser
      (`make fuzz FUZZTIME=160m`, see the fuzzing posture above)
- [ ] Client matrix re-run for a release that touched delivery:
      `make client-e2e` (automated browser cells) plus the manual
      checklists in docs/client-matrix.md (Apple, ExoPlayer, mpv);
      update the /caps profiles if any cell changed
- [ ] `resolver/go.mod`: bump the waxbin pseudo-version (WaxBin publishes no
      tags, so `@latest` resolves off its default branch). Never ship a
      resolver behind the WaxBin whose catalog it reads: it opens read-only
      and never migrates, so an older catalog is fine but a newer one is
      refused outright (`catalog schema vN is newer than this build
      supports`), failing `resolver.Open` at startup. Ahead is inert and safe
- [ ] Tag `vX.Y.Z` pushed -> `release.yml` publishes binaries + SHA256SUMS +
      multi-arch image to ghcr.io
- [ ] Container smoke: `docker run` + HEALTHCHECK healthy
