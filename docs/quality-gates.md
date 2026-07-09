# Quality gates

These are the CI-blocking numeric thresholds every codec must clear before
it ships. They are pinned before any codec exists, because the gates ARE
the schedule: if quality slips, the date slips. A gate is
never weakened to hit a date. Raising a gate is routine; lowering one
requires a superseding ADR.

## Reference corpus

A fixed 20-item corpus, SHA-256-pinned and fetched by `make verify-vectors`
(CI-cached, never committed): the first 20 reference clips (a bias-free
fixed prefix) of the 30-sample Hydrogenaudio 2011 public multiformat
listening test, which mixed 15 known-difficult samples from prior HA tests
with 15 organizer-selected ones spanning music, speech, transient, and
tonal material: clips hand-picked for codec evaluation and hosted by Xiph
with upstream checksums since 2011 (`internal/testutil/vectors.go`,
`opus/corpus/`).

All items 48 kHz stereo 16-bit, 7-30 s. The corpus is versioned; changing
it re-baselines every gate in the same PR.

## Metrics

- **Validity**: the reference decoder (and ours) accepts every produced
  stream; decoded sample count matches the gapless invariant
  (`output_samples == source_samples_after_trim`) wherever the format
  signals it (capability matrix).
- **Differential RMS / max-abs**: full-scale-relative error vs the ffmpeg
  float decode of the same stream.
- **ODG-proxy**: PEAQ-anchored objective difference grade implemented in
  `internal/testutil` (0 = imperceptible, -4 = very annoying). Gates
  compare *deltas between encoders on the identical metric version*, so a
  metric revision re-baselines both sides in one PR, never silently.
- **opus_compare**: the RFC 6716 section 6 comparison tool ported into
  `internal/testutil`; quality score Q <= 100, vectors pass at Q >= 0 (the
  tool's own pass bar, weighted error <= 0.277). The decoder currently
  scores 96-100 across all vectors at both rates.
- **Realtime factor**: single-core, portable (non-SIMD) build, measured on
  the CI baseline machine class; recorded by `make bench`.

## Decoder gates

| Decoder | Gate |
|---|---|
| FLAC | bit-exact on the full IETF/Xiph suite; sample-exact seek; >=100x realtime |
| MP3 | vs ffmpeg: RMS < 1e-4 FS, max < 1e-3 FS; LAME gapless sample-count invariant; sample-exact seek at 100 random offsets in VBR; >=150x realtime |
| AAC-LC | vs ffmpeg: RMS < 2^-13 FS; iTunes (iTunSMPB) gapless invariant; edit-list seek exact; >=80x realtime |
| ALAC | bit-exact vs ffmpeg; >=100x realtime |
| Opus | all opus_testvectors 01-12 pass RFC 6716 section 6 (ported opus_compare, both decode rates, against the RFC 8251 regenerated references; the 2012 originals are stale for hybrid/transition vectors and fail even current libopus); Ogg bisection seek exact after 80 ms pre-roll; >=60x realtime |
| Vorbis | vs ffmpeg: RMS < 1e-4 FS, max < 1e-3 FS; >=80x realtime |

## Encoder gates

Every encoder, always: validity (above) plus golden-stream byte-exactness in
deterministic mode.

### FLAC
- `decode(encode(x)) == x` bit-exact on all suites, levels 0-8.
- `flac -t` accepts every output; streamable-subset compliant.
- Size at level 5: corpus total <= **1.05x** `flac -5`; no track > **1.08x**.
- >= **60x** realtime at level 5.

### MP3 baseline, CBR
- LAME-tag gapless round-trip; decodes in ffmpeg, LAME, browser matrix.
- ODG-proxy at 128 kbps CBR: corpus mean >= **Shine mean** (parity); no
  track > **0.25** below Shine.
- >= **40x** realtime.

### ALAC
- `decode(encode(x)) == x` bit-exact; ffmpeg demuxes and decodes our fMP4.
- Size: corpus total <= **1.05x** ffmpeg's ALAC encoder.
- >= **80x** realtime.

### Opus phase 1: CELT/music
- Every bitstream decodes via libopus AND our decoder; the harness carries
  the range coder's final state per packet, so the reference decoder
  cross-checks every packet (`opus_demo` hard-fails on a mismatch).
- opus_compare vs libopus at matched CBR and complexity 10, both decoded by
  the reference decoder (`opus_demo`, sample-exact by construction, no
  cross-correlation alignment), scored against the original, on the pinned
  20-track corpus at **96, 128, 160 kbps** stereo. The gate unit is the
  **internal weighted-error ratio** (ours / libopus), because Q-point deltas
  do not compare across error depths (ADR-0008; the original 2.0/5.0-point
  budgets translate at the metric's calibration to ratios 1.20/1.51).
- Gate: geometric-mean error ratio <= **1.20** per bitrate; no track >
  **2.6**. The per-track bound admits the documented phase-1 gap (no
  tonality analyser, worth up to 2.5x per track in libopus's own A/B), and
  phase 2 tightens it to **1.5** when the analyser lands (ADR-0008).
- The pitch pre-filter's per-frame decisions (on, period, gain, tapset)
  agree with libopus on >= **90%** of frames on a pitched fixture.
- >= **15x** realtime portable (>= 30x with the SIMD build).

### AAC-LC
- ODG-proxy at 128 kbps: corpus mean >= **ffmpeg-aac mean - 0.2**; no track
  > **0.5** below ffmpeg-aac. (The gate is ffmpeg's encoder, a realistic bar, not Apple's.)
- Plays in AVFoundation and ExoPlayer (client matrix).
- >= **20x** realtime.

### MP3 quality phase, VBR + joint stereo
- ODG-proxy at 128 kbps: corpus mean >= **Shine mean + 0.3** (measurably
  better); no track below Shine - 0.1.
- LAME comparison reported in the nightly artifact (informational,
  non-blocking).

### Opus phase 2: SILK + hybrid
- At **24, 32, 48 kbps** (speech corpus, NB-WB): mean opus_compare
  weighted-error ratio vs libopus <= **1.35**; no item > **2.0** (the
  3.0/6.0-point budgets translated per ADR-0008).
- The tonality analyser (analysis.c) lands here and is wired into CELT
  (`max_pitch_ratio`, `leak_boost`, tonality VBR boost); the phase-1
  per-track bound tightens from 2.6 to **1.5**.
- Speech/music mode decision agrees with libopus on >= **90%** of corpus
  windows (report-only below 95%, blocking below 90%).
- Non-negotiable for v1.0: CELT-only is sequencing, not scope fallback.

## Service targets (recorded per release once streaming and HLS exist)

- TTFA p95: < **300 ms** warm cache, < **800 ms** cold.
- HLS seek-to-segment p95: < **1 s**.

## Performance floors (ratchets, may only rise)

Portable build, per core: decode FLAC >=100x / MP3 >=150x / AAC >=80x /
Opus >=60x / Vorbis >=80x; encode FLAC >=60x / ALAC >=80x / MP3 >=40x /
AAC >=20x / Opus >=15x; resampler HQ >=200x. Enforced by `make bench` +
benchstat regression thresholds in nightly CI.

## Reporting

The nightly encoder-quality harness (stood up with the first lossy
encoder) publishes an HTML report per run: per-track metrics vs references,
deltas against the previous run, and ABX-ready clip pairs. The human
listening protocol lives in `MAINTENANCE.md`.
