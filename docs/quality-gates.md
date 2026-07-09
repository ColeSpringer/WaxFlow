# Quality gates

These are the CI-blocking numeric thresholds every codec must clear before
it ships. They are pinned before any codec exists, because the gates ARE
the schedule: if quality slips, the date slips. A gate is
never weakened to hit a date. Raising a gate is routine; lowering one
requires a superseding ADR.

## Reference corpus

A fixed 20-item corpus, SHA-256-pinned and fetched by `make verify-vectors`
(CI-cached, never committed):

| Class | Items | Purpose |
|---|---|---|
| Music (broadband) | 8 | steady-state psychoacoustics |
| Speech (clean + noisy) | 4 | SILK path, low-rate tuning |
| Transients (percussion, castanets, glockenspiel) | 4 | window switching, pre-echo |
| Tonal/pathological (pitch pipe, harpsichord, sine sweeps) | 4 | tonality estimation, ringing |

All items 44.1 or 48 kHz stereo source, >=16-bit. The corpus is versioned;
changing it re-baselines every gate in the same PR.

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
- Every bitstream decodes via libopus AND our decoder.
- opus_compare deficit vs libopus at matched bitrate, decoded by libopus,
  scored against the original, at **96, 128, 160 kbps** stereo:
  corpus mean deficit <= **2.0** points; no track > **5.0** points.
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
- At **24, 32, 48 kbps** (speech corpus, NB-WB): mean opus_compare deficit
  vs libopus <= **3.0** points; no item > **6.0** points.
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
