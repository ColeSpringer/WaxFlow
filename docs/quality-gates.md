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
- **Realtime factor**: single-core, measured on the CI baseline machine
  class; recorded by `make bench`.

## Decoder gates

| Decoder | Gate |
|---|---|
| FLAC | bit-exact on the full IETF/Xiph suite; sample-exact seek; >=300x realtime |
| MP3 | vs ffmpeg: RMS < 1e-4 FS, max < 1e-3 FS; LAME gapless sample-count invariant; sample-exact seek at 100 random offsets in VBR; >=150x realtime |
| AAC-LC | vs ffmpeg: RMS < 2^-13 FS; iTunes (iTunSMPB) gapless invariant; edit-list seek exact; >=150x realtime |
| ALAC | bit-exact vs ffmpeg; >=100x realtime |
| Opus | all opus_testvectors 01-12 pass RFC 6716 section 6 (ported opus_compare, both decode rates, against the RFC 8251 regenerated references; the 2012 originals are stale for hybrid/transition vectors and fail even current libopus); Ogg bisection seek exact after 80 ms pre-roll; >=150x realtime |
| Vorbis | vs ffmpeg: RMS < 1e-4 FS, max < 1e-3 FS; >=80x realtime |

## Loudness meter

Conformance is the gate, not agreement with another implementation.
`TestEBUTech3342Vectors` measures the four EBU Tech 3342 loudness-range
cases and checks them against the document's stated results at its own
+-1 LU tolerance; all four land on their specified value exactly
(10 / 5 / 20 / 15 LU). The BS.1770 integrated anchors (a 0 dBFS 997 Hz
sine in one channel of a stereo meter reading -3.01 LKFS, the same tone
at -18 dBFS in both reading -18.0) are pinned at 44.1, 48 and 96 kHz.

`TestFFmpegDifferential` compares integrated, range and true peak against
ffmpeg's `ebur128` filter on synthesized signals, at 0.15 LU / 0.5 LU /
0.3 dB. Those are empirical bounds over those signals, not properties of
either meter, and the range one in particular should not be read as a
promise: **real material can separate the two by more, and both remain
conformant.** An independent test of WaxTap v3.0 measured a 0.58 LU
difference on real music and attributed it to WaxFlow; the vectors above
say the meter is right, and the mechanism is structural.

Two differences, both read out of ffmpeg's `libavfilter/f_ebur128.c`
rather than assumed:

- ffmpeg bins every short-term loudness into a fixed histogram at
  1/100 LU (`HIST_GRAIN 100`), flooring each value onto that grid, and
  floors the relative-gate position onto the same grid so the gate can
  admit blocks an exact comparison excludes. WaxFlow sorts the exact
  float64 powers.
- The percentile rank differs by one. ffmpeg walks bins until the
  cumulative count reaches `round(f*n)`; WaxFlow indexes the sorted
  array at `round(f*(n-1))`, the libebur128 convention.

Both are invisible where the distribution is smooth through the 10th and
95th percentiles and grow where it is steep, which is why every synthetic
signal tried agrees to under 0.25 LU while real music, with distinct loud
and quiet passages, can separate by half an LU. Tech 3342 specifies the
quantity to +-1 LU; neither ranking is more correct than the other.

One related asymmetry is deliberate and documented in place: the
integrated relative gate compares strictly greater (BS.1770-4's formula)
where the range gate compares greater-or-equal (libebur128's and
ffmpeg's, at both of theirs). It cannot change a reading -- separating
the two takes a block power exactly equal to a mean divided by ten in
float64 -- and unifying them would spend a `loudness.Version` bump,
invalidating every externally stored measurement, to move nothing.

## True-peak limiter

The gate is a property, not a number: **for any legal `GainDB`, the true peak
of the PCM the limiter emits is at or below `gain.DefaultCeilingDB`, as
measured at 4x per BS.1770-4.** The 4x qualifier is load-bearing in both
directions: 4x under-reads true inter-sample peaks against an 8x or 16x
detector, so an unqualified claim would state a property neither detector in
this repo can verify.

The property is structural. The gain is a min-hold over the look-ahead window
smoothed by a non-negative kernel of unit mass whose support fits inside that
window, so every tap contributing to the gain at sample `n` was already
constrained by the peak at `n`. See the `gain.Limiter` type doc for the
derivation; it is three lines.

Two assertions back it, at two tolerances, because they answer different
questions (`dsp/gain/limiter_ceiling_test.go`):

- **Internal**, and the real gate: re-run the limiter's own 4x interpolator
  over the limiter's output. That is exactly the quantity the construction
  bounds. Its tolerance is `ceilEpsilon(look)`, which is not slop but a
  measured law: an interpolated point reconstructs the gain-*modulated* signal,
  so what leaks through is the gain's curvature across the interpolator's 16
  taps, and it falls as 1/look². Worst measured excess is 0.013 dB at 8 kHz and
  0.0002 dB at 48 kHz. The same assertion is what checks the sample clamp stays
  inert, since a firing clamp flat-tops the waveform and a flat top is what an
  interpolating detector reads as an over-ceiling peak.
- **External**, loose on purpose: `dsp/loudness`'s independently designed 4x
  detector (12 taps at Kaiser beta 6 against the limiter's 16 at 3.67). The two
  disagree by ~0.04 dB on broadband transients, so its bound is 0.20 dB. It
  does not need to be tight; the defect it exists to catch was 1.80 dB.

`FuzzLimiterCeiling` is what backs the word "any" over random crest, gain, rate
and chunking. Two hand-built fixtures cannot establish a universal, and the
sentence above is a universal.

`tests.TestTranscodeGainTruePeakCeiling` mirrors the harness the WaxTap v3.0
report used to attribute its F2 finding here (`Engine.Transcode` with a
positive `GainDB`, then `Engine.Analyze`), and logs the loudness shortfall per
row so the cost of holding the ceiling is a recorded number rather than a
guess: it saturates around 1.2 LU at 27 dB crest, less on real music.

Scope: the guarantee is on the PCM the limiter emits, which for `wav` and
`flac` is the delivered file (dither contributes below it). For the lossy
formats it is the encoder's input, and a decoder can reconstruct a true peak a
few tenths of a dB above what the encoder was handed. `docs/api.md` and
`/caps`'s `truePeakCeilingDb` both carry that caveat.

## Encoder gates

Every encoder, always: validity (above) plus golden-stream byte-exactness in
deterministic mode.

Byte-exactness holds *within* a build, not across architectures. Go lets a
compiler contract `a*b+c` to a single rounding (arm64 does, amd64 does not),
and the math package's per-arch implementations need not agree in the last
ulp; either moves a float encoder's spectral values by an ulp, which is enough
to flip a quantizer decision and pick different codewords. The stream stays
valid and the quality gates still hold, so golden hashes are pinned to one
architecture and skip elsewhere (`codec/vorbis.goldenEncodeArch`). What the
ADR-0004 cache key rests on is a build reproducing its own bytes, which the
deterministic-mode tests cover on every platform.

**Where the baselines run.** The reference encoders these gates score against
are ffmpeg *build options*, not platform facts, and a given ffmpeg may omit
any of them. Ubuntu's build carries the lot, which is why `make
encoder-quality` is a Linux/CI target (the nightly job). It sets
`WAXFLOW_REQUIRE_*` for each oracle, so a baseline going missing there fails
the job rather than quietly dropping a gate.

On macOS, Homebrew's ffmpeg has **no libshine at all** and no way to get one:
there is no `shine` formula, `ffmpeg-full` omits it too, and Homebrew dropped
`--with-*` build options years ago. So the MP3 baseline gate cannot run
locally on a Mac, and `make check` does not need it to: the gate self-skips
with a message naming what is missing. Run it in CI, or in a Linux container.
Homebrew's ffmpeg also omits **libvorbis**, which the Vorbis differential and
quality gates use; `brew install ffmpeg-full` supplies that one (it is
keg-only, so point the oracles at it explicitly or put it ahead on PATH).

### FLAC
- `decode(encode(x)) == x` bit-exact on all suites, levels 0-8.
- `flac -t` accepts every output; streamable-subset compliant.
- Size at level 5: corpus total <= **1.05x** `flac -5`; no track > **1.08x**.
- >= **150x** realtime at level 5.

### MP3 baseline, CBR
- LAME-tag gapless round-trip; decodes in ffmpeg, LAME, browser matrix.
- ODG-proxy at 128 kbps CBR: corpus mean >= **Shine mean** (parity); no
  track > **0.25** below Shine.
- >= **40x** realtime.

### ALAC
- `decode(encode(x)) == x` bit-exact; ffmpeg demuxes and decodes our fMP4.
- Size: corpus total <= **1.05x** ffmpeg's ALAC encoder.
- >= **80x** realtime.

### Opus: CELT/music
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
  **1.5** (ADR-0008). The original bound was 2.6, admitting the documented
  analyser-less gap; the tonality analyser's CELT hooks and the fix for the
  encoder's last-band scratch clobber closed it, and the measured corpus
  now sits at mean parity or better (128/160k means below 1.0, worst track
  1.22 at 96k).
- The pitch pre-filter's per-frame decisions (on, period, gain, tapset)
  agree with libopus on >= **90%** of frames on a pitched fixture.
- >= **30x** realtime (ratcheted from 15x at the v1.0 bench pass; measured
  55-68x once the CELT MDCT moved onto `dsp/fft`).

### AAC-LC
- ODG-proxy at 128 kbps: corpus mean >= **ffmpeg-aac mean - 0.2**; no track
  > **0.5** below ffmpeg-aac. (The gate is ffmpeg's encoder, a realistic bar, not Apple's.)
- Plays in AVFoundation and ExoPlayer (client matrix).
- >= **20x** realtime.

### MP3 quality, VBR + joint stereo
- ODG-proxy at 128 kbps: corpus mean >= **Shine mean + 0.3** (measurably
  better); no track below Shine - 0.1.
- LAME comparison reported in the nightly artifact (informational,
  non-blocking).

### Opus: SILK + hybrid
- At **24, 32, 48 kbps** (speech corpus, NB-WB): mean opus_compare
  weighted-error ratio vs libopus <= **1.35**; no item > **2.0** (the
  3.0/6.0-point budgets translated per ADR-0008).
- The tonality analyser (analysis.c) lands here and is wired into CELT
  (`max_pitch_ratio`, `leak_boost`, tonality VBR boost); the CELT/music
  per-track bound tightens from 2.6 to **1.5**.
- Speech/music mode decision agrees with libopus on >= **90%** of corpus
  windows (report-only below 95%, blocking below 90%).
- Non-negotiable for v1.0: CELT-only is sequencing, not scope fallback.

### Vorbis
- `decode(encode(x))` runs through both our decoder and libvorbis (via ffmpeg);
  the gapless sample-count invariant (`SamplesExact`) holds; golden-stream
  byte-exactness in deterministic mode.
- ODG-proxy at libvorbis -q4 (~128 kbps): corpus mean >= **libvorbis mean -
  0.2**; no track > **0.5** below libvorbis. libvorbis (the reference) is a
  clean-room oracle, never opened while implementing our encoder.
- The ODG proxy shapes its absolute-mask floor by the threshold of hearing (both
  the top-octave and deep-bass limbs), so error the ear can barely hear at a
  moderate playback level is not scored as fully audible; without it the proxy
  over-penalizes an encoder that drops a defensible top-octave rolloff. The rise
  is **capped at 15 dB** (was 40): the proxy has no absolute SPL anchor, so the
  ATH's magnitude is really a playback-level assumption, and a 40 dB rise assumed
  a playback so loud the top octave's floor reached the signal peak, leaving the
  metric blind to a whole-octave rolloff (it scored 0). 15 dB keeps a moderate,
  defensible allowance that still scores gross HF removal and a sub-bass deficit.
- High-q and real-audio validation is a separate, gated harness (not the q4
  synthetic gate, which favors us and cannot see the high-q size/quality
  tradeoff): `TestVorbisRealAudioQuality`/`TestVorbisRealAudioDiag` in `tests/`
  (set `WAXFLOW_REAL_AUDIO_DIR`) sweep q4/q6/q8 on real lossless clips and break
  the deficit down per bark band.
- Self-generated clean-room codebooks trade size for provenance: streams may be
  somewhat larger than libvorbis at equal quality; the gate is quality
  (ODG-proxy), not byte-competitiveness.
- >= **40x** realtime (matching MP3; a long-block-plus-psy pipeline).
- Decode with libvorbis, not ffmpeg's *native* Vorbis decoder: the native
  decoder is experimental (trac.ffmpeg.org/10571) and mis-decodes some legal
  coupled streams, so the tests pin `-c:a libvorbis`. The specific defect is
  known and narrow: its **vectorized** inverse coupling branches on
  `magnitude >= 0` where its own C fallback and the spec branch on
  `magnitude > 0`, so a line stored as a zero magnitude with a nonzero angle
  comes back with the angle channel negated. Running ffmpeg with `-cpuflags 0`
  decodes those streams correctly, which is how the two paths were separated.
  We no longer emit that representation (see `coupleForward`), so ffmpeg's
  native decoder now agrees with libvorbis on our output.
- Coupled stereo has its own gate, `TestVorbisCoupledStereo` in `tests/`, because
  the ODG corpus above cannot see coupling defects: its material is mono,
  broadband or near-dual-mono, which keeps the angle residue small. The gate runs
  decorrelated, anti-phase and opposite-direction-sweep stereo, at both q6 and q1
  (q1 is where allocation lands on the single-pass noise and coarse classes, the
  ones with no refinement pass to walk a bad magnitude back), through four
  decoder legs: ours, libvorbis, ffmpeg-native, and ffmpeg-native under
  `-cpuflags 0`. It scores each leg per channel against its own source channel,
  since a coupling defect lands on one channel and leaves the other exact, and it
  scores the legs **against each other**, which is what actually pins F1: that
  defect was correct decoders reading different audio out of the same bytes, not
  a stream anyone failed to decode. The two ffmpeg legs differ only in SIMD
  dispatch, so their agreement is what proves the vectorized inverse coupling was
  reached and is happy; without that leg the gate would go green on a runner
  whose ffmpeg never leaves the C fallback.
- Status: the encoder MEETS the ODG gate. At -q4 the corpus mean is **+0.52** vs
  libvorbis with every track at or above libvorbis (worst per-track +0.00): a
  peak-envelope floor plus a perceptual mask floor fixed tonal, block switching
  fixed transient pre-echo, and square-polar stereo coupling (per-channel type-1
  residues, coupling applied at the mapping layer) with demand-driven allocation
  carries stereo material at or above libvorbis. The +0.52 is smaller than the
  ~+1.0 reported when the proxy's ATH cap was 40 dB: tightening the cap to 15 dB
  (see the ODG-proxy note above) removed an over-generous sub-bass and top-octave
  discount that had inflated the margin, so this is the honest gain from the
  encoder alone. The earlier real-audio figures (q4 ~+0.8, q6 ~+0.7, q8 ~+0.4)
  were measured under the 40 dB proxy and should be re-run under the 15 dB cap;
  they will come in lower but keep the same ordering (the gate is comparative, so
  the reweighting hits our encoder and libvorbis alike).
- Size: residue is coded by multi-dimensional product-lattice VQ books whose
  codeword lengths are trained offline (`books_gen.go` via `go generate`), and it
  is allocated by masking on a graduated precision ladder (noise 1/8, coarse
  1/16, then quarter-step refinement passes to 1/64, 1/256, 1/1024), so a
  partition takes the cheapest rung whose quantization noise clears its masking
  demand rather than overshooting a coarse-to-fine gap. Coupled stereo splits
  into per-channel magnitude and angle (the angle skips wherever it is zero), and
  steady broadband noise caps at the noise book while a transient's broadband
  attack stays fine (gated on temporal steadiness). On real clips the encoder
  runs ~2.8x libvorbis at q4 down to ~1.7x at q8 (the deficit narrows as -q rises
  because libvorbis grows faster). Streams still exceed libvorbis (the clean-room
  books and the peak-floor scheme carry more residue precision), which the plan
  accepts for a VBR codec; the gate remains quality, not size.
- The refinement books are mid-tread (they include a zero point): the decoder
  picks its stereo decouple branch from the sign of the magnitude channel, so a
  coupled magnitude of exactly zero must reconstruct as exactly zero, and on the
  anti-phase lines where the angle exceeds the magnitude the sign is preserved so
  a small magnitude cannot round into the wrong sign and invert the angle channel.
  That sign-preserving nudge runs on the **last** cascade pass, not pass 0: a
  pass-0 nudge to a same-sign coarse point lands a residual outside the refinement
  books' range, which they cannot walk back, wrecking accuracy (~890x worse error)
  on every anti-phase coupled magnitude line while a finer class is meant to be
  most precise. Applied on the last pass, and only where the cascade so far is
  still zero, it costs at most one final-step of accuracy and keeps the cascade
  convergent.

## Service targets (recorded per release once streaming and HLS exist)

- TTFA p95: < **300 ms** warm cache, < **800 ms** cold.
- HLS seek-to-segment p95: < **1 s**.

## Performance floors (ratchets, may only rise)

Portable build, per core: decode FLAC >=**300x** / MP3 >=150x /
AAC >=**150x** / Opus >=**150x** / Vorbis >=80x; encode FLAC >=**150x** /
ALAC >=80x / MP3 >=40x / AAC >=20x / Opus >=**30x**; resampler HQ
>=200x. The bolded floors were ratcheted at the v1.0 bench pass against
the post-FFT measurements (decode FLAC 537-934x, AAC 260x, Opus
289-522x; encode FLAC 230-250x at level 5, Opus 55-67x), leaving 2x or
more headroom for slower CI runners. Watched by the nightly `bench` job
(benchstat against the previous night's numbers, cache-carried).

The floors are triaged from that job's numbers, not asserted by the default
suite: a shared runner measures its own scheduling noise as much as the codec,
so wall-clock assertions there fail on the runner rather than on a regression.
The realtime-factor tests report their numbers always and gate only under
`WAXFLOW_PERF=1`, for a dedicated perf run on a baseline machine, the same
switch `server`'s TTFA percentiles use.

Floor notes recorded at the v1.0 bench pass:

- The AAC encode floor stays at 20x: the synthetic noise worst case
  measures 19-20x across every box since the encoder was first benched
  (real audio measures 49-68x), so the ratchet candidate, raised at that
  first bench and again after the FFT, is closed as declined; a
  noise-dominated track still encodes at ~5x the 2x-realtime delivery
  pace, and the two-loop is the cost of the quality gate. The recorded
  DP-sectioning idea remains a post-1.0 quality candidate, not debt.
- Vorbis decode still has no default-suite benchmark, so its performance is
  observed via the differential job's corpus decodes and the 80x floor stands
  on the original decoder measurements. The reason recorded here, that WaxFlow
  had no Vorbis encoder to self-generate fixtures, expired when the encoder
  landed: `codec/vorbis` now self-generates its own encode benchmarks
  (`BenchmarkEncodeSine`/`BenchmarkEncodeNoise`), and a decode benchmark can be
  fed the same way. Adding one is now an open task rather than something the
  tree cannot express.
- The per-frame allocation scratch passes recorded during the Opus work
  (Opus decode ~19-31 allocs/packet, encoder equivalents) are closed as
  declined at v1.0: immaterial at 55-500x realtime; reopen only if a
  profile on real hardware says otherwise.

## Reporting

The nightly encoder-quality harness (stood up with the first lossy
encoder) publishes an HTML report per run: per-track metrics vs references,
deltas against the previous run, and ABX-ready clip pairs. The human
listening protocol lives in `MAINTENANCE.md`.
