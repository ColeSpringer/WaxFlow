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
  Revision 2026-08-18 (the HE-AAC stage), three alignment changes in one
  re-baseline: the search bound `odgMaxLag` grew 3000 -> 6000 (HE-AAC
  primes at 3010 and libfdk past 5000); a smallest-lag-within-5%
  tie-break, because the wider bound let periodic corpus items align one
  signal period late (that lag is genuinely equivalent over the scored
  overlap on periodic synthetic content); and the error window moved off
  the stream head (skip 4000, span 12000, was the first 4000), because a
  head-anchored window measures codec warm-up, and on sparse content it
  fit the one distorted onset it contained -- the reference encoder's
  own transient cell read -3.9 from a 22-sample misalignment while the
  audio was fine. Both sides of every gate are scored on the revised
  metric; the recorded per-cell movements are in this change's history,
  and every pre-existing gate still passes with margin.
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
| HE-AAC v1/v2 | vs ffmpeg on fdk-encoded fixtures: RMS < 2^-13 FS across explicit m4a (v1, v2, downsampled SBR) and implicit ADTS; seeks land sample-exact, the tail within the same gate for v1 and within 0.02 for v2 (the SBR noise and sinusoid phase free-runs from the stream head in every decoder, so a mid-stream join reproduces it differently forever; the parametric layer itself resynchronizes inside the 12-AU preroll); the realtime floor is under Performance floors, ratcheted at the encoder stage's bench pass |
| ALAC | bit-exact vs ffmpeg; >=100x realtime |
| Opus | all opus_testvectors 01-12 pass RFC 6716 section 6 (ported opus_compare, both decode rates, against the RFC 8251 regenerated references; the 2012 originals are stale for hybrid/transition vectors and fail even current libopus); Ogg bisection seek exact after 80 ms pre-roll; >=150x realtime |
| Vorbis | vs ffmpeg: RMS < 1e-4 FS, max < 1e-3 FS; >=80x realtime |
| APE (Monkey's Audio) | bit-exact against the samples the reference encoder was handed AND against ffmpeg's decode of the same file, across every compression level, 8/16/24-bit, mono and stereo, the silent and pseudo-stereo special frames, a non-standard rate, and a multi-frame stream with a short final frame; the committed fixtures re-check the same thing with no tools installed; every out-of-scope shape refused by name (32-bit, floating point, more than two channels, stream versions outside 3950 to 3990); frame-granular seek, exact at the frame it lands on; >=100x realtime at the default compression level. **Recorded deficit:** the 3950..3989 bitstream (a different entropy coder, and below 3980 a different header form and weight quantizer) ships UNFIXTURED. No encoder in circulation writes those versions -- the reference tool has written 3990 since 2003 and no old build is distributed any more -- and there is no APE conformance corpus, so nothing can generate one. What limits the exposure is the format itself: every frame carries a CRC over its own samples, so a mistake in those paths is a refusal rather than wrong audio. The header form IS tested, against headers built by hand in `codec/ape/ape_test.go`. |
| WMA (Windows Media Audio 1/2) | TWO differentials, against two independent encoders, because one of them is not enough. **FFmpeg corpus, 20 cells** spanning both versions, all three frame lengths, all three coefficient-book pairs, every reachable arm of the high-frequency ladder, and 8 to 48 kHz mono and stereo: vs ffmpeg's scalar decode (`-cpuflags 0`) RMS < 1e-7 FS, max < 1e-6 FS, sample count matching exactly. The measured worst case is 4.8e-8 / 2.4e-7, within 1.5x of ffmpeg disagreeing with ITSELF (its scalar and vectorised decodes differ by 2.5-2.9e-8 / 1.5-2.4e-7), so the gate sits at the oracle's own floor rather than at a round number. **Windows Media Foundation corpus, 8 cells** (`scripts/wmfenc`, Windows only, self-skipping): Microsoft's own encoder at 8 to 48 kHz, decoded by us and scored against ffmpeg's decode of the same bytes, so the encoder is independent of the oracle. That second corpus exists because the first one's encoder is measurably narrow: it emits a FLAT exponent curve (0 of 1216 exponent decodes had two different band values, on the corpus and on tone, low-passed, high-passed, two-tone and near-silent sources), never sets a noise-fill flag (0 of 2317 high bands), always codes both channels (0 one-sided mid/side blocks, even on digital silence), never sets a `flags2` bit past the first, and refuses bit rates below 24 kbit/s. Microsoft's does the opposite on nearly every count, and between them the two corpora reach the bit reservoir, variable block lengths, the three tabulated v2 exponent-band tables, non-flat exponent curves, LSP-coded exponents, noise-filled bands (283 of 604 at 16 kHz), exponent reuse, both the mid-only mid/side case and blocks with nothing coded. Seek: BIT-EXACT against a linear decode from the landing packet on every noise-off cell, and on the others an energy ratio within 5% with correlation >= 0.95 -- never convergence, because the generated noise index is never reset by the format and a resumed decode draws different noise forever (it is also periodic, so a landing on the period IS bit-exact, which pins that every coded channel draws exactly one value per coefficient). Block-align identity `nBlockAlign == floor(bitRate*frameLen/(rate*8))` on all 20 ffmpeg cells, decode-free. FuzzDecode over RUNS of packets AND configurations, driven through one decoder with no Reset between them so the cross-packet state machine is reachable at all: no panic, bounded production, every sample finite. Realtime: >=500x at 44.1 kHz stereo (measured 1226x) and >=500x on the worst shape the format allows, 48 kHz stereo with LSP exponents (measured 1057x, benchmarked directly because no ffmpeg-written file is LSP-coded). The floor stays at 500x rather than rising because the LSP shape is the binding one and 2x headroom for slower runners is what the other floors carry. **Chapters**: the Marker Object is the chapter store, its entries read as stored (the pre-roll in their presentation times, since the File Properties Object holding it may come after) and placed on the playback timeline once the header is walked, in start order; `chapters.wma`, written by ffmpeg's muxer from a chapter list, and the `ffprobe -show_chapters` differential pin the placement exactly on the marker's own 100 ns grid, an oracletest cell pins that the tag library places the same chapters, and the walk's stepping by description length rather than by the entry length field is pinned to the oracle by mutation (`TestEntryLengthIsIgnoredLikeFFprobe` overwrites every such field in the fixture and gets the same three chapters from ffprobe and from us). A count the object cannot back, a description running past the object (which ffprobe drops and this reader keeps, its time being intact), a name running past it, a second Marker Object, a marker past the end of the stream and a time no duration holds each warn once for the object and fail strict mode; a marker inside the pre-roll lands at the start with a note (ffprobe reports it at a negative time), the cap of 65536 entries counts every entry walked and cuts with a note, and a description past the tag cap is cut with a note. Refused by name: WMA Voice (Lossless and Pro have their own rows below and decode); rates above 50 kHz; more than two channels; a missing block align or byte rate; v1 with variable block lengths; stereo v1 with a bit reservoir; ASF Audio Spread error correction with a span above 1, whose payload interleaving nothing here undoes; and, at the container, any 0x0160/0x0161 header the decoder would refuse, so an out-of-scope file cannot probe as a playable track. A decode failure is latched: nothing in a WMA packet re-anchors a drifted reader, so a refused packet kills the decoder until Reset, and a resume is itself refused on the one shape whose block-length walk the bitstream never restates (variable block lengths with no reservoir). The hand-built streams have a differential of their own through a WMA-in-WAV wrap (WMA is RIFF codec 0x0161, and ffmpeg decodes the wrap bit-identically to the same stream in ASF), so variable block lengths, LSP exponents and the noise-times-reuse interaction are scored against an independent decoder even though no encoder writes them: `synthdiff_test.go`. **Recorded deficits.** (1) ROOT-CAUSED, oracle-side, carried in `msDeficits` with staleness teeth: the one block of the 22.05 kHz mono Microsoft cell that reuses an exponent curve at a DIFFERENT block length (256 reusing a 128) decodes about 1.4% off because ffmpeg reads the resampled curve one source bin behind inside the noise regions (per-bin fill and the band powers behind the gains), while this decoder indexes a reused curve the same way everywhere, as the notes describe. Pinned by spectral recovery on the cell (the shifted step reproduces the 0.95494 band gain and both boundary bin values exactly), reproduced on a deterministic hand-built stream (`TestNoiseReuseCharacterization`, which also notices if ffmpeg stops), and bounded by twelve noise-off reuse probes that matched exactly at every block-size ratio. Windows' own decoder could not arbitrate: it garbles hand-built streams its encoder would not write, and on real cells its noise system differs too much to resolve a 2% question. (2) The two LSP cells miss the gate by two to three times, at -112 dBFS, from per-bin float rounding order on the LSP path: measured through the wrap, ffmpeg's curve agrees with the exact `(p+q)^(-1/4)` to ~6e-7 relative with no structure in p+q (so there is no reference table to match), and the noise-off LSP differential floor (~9e-7 relative) is the same scale as these cells' misses. The curve is evaluated as two square roots and a reciprocal rather than `math.Pow`, which is float32-identical over 20M sampled values and 2.4x faster; that is what put the LSP path above the realtime floor. The inverse MDCT then went from a 2M-point complex DFT (half its input zero padding) to the N/4 factorization the CELT transform in this tree already uses, which is 8x less transform work for the same answer: decode roughly doubled at every cell AND the differential got tighter, since fewer operations accumulate less rounding. (3) Only ffmpeg writes v1: Media Foundation's `Wma8` subtype is v2, so v1 is covered by the narrow corpus alone. (4) The side-only mid/side case (channel 1 coded, channel 0 absent) appears in neither encoder's output and is held by a hand-built stream. Every claim above was checked by mutation rather than assumed: 36 mutations, 32 caught, and the four survivors are explained rather than counted -- three accumulator-zeroing writes that the window transitions make provably unobservable (each block's non-zero span ends exactly where the next one's store begins, so the region they clear is already zero) and the exponent seed, which cancels because the reconstruction divides by the maximum exponent. |
| WMA Lossless | **The gate is the source PCM, not another decoder.** A lossless decode returns the encoder's input sample for sample, so every committed cell is compared against a source regenerated in Go with no floating point anywhere: exact equality, no tolerance, no allowlist. A mismatch is a bug, never a deficit. FFmpeg is the secondary check and a real one, an independent implementation whose decode of every cell is byte-identical to the same source; its SIMD and scalar paths agree here (`-cpuflags 0`), which is what an all-integer decoder should do. **Corpus**: six committed cells (`codec/wmalossless/testdata/corpus/`, 400 KiB) and ten generated on Windows, spanning the WHOLE envelope Windows' encoder offers, which is all there is: 44.1 kHz stereo at 16 and 24 bits and 48/88.2/96 kHz stereo and 5.1 at 24 bits. Each cell is four equal segments of whole coded frames -- two triangles per channel, triangles plus quarter-scale noise, digital silence, and full-scale noise -- so every cell reaches the tonal path, the mixed path, the all-uncoded path, the raw PCM escape (64 of 65 subframes on full-scale noise) and a hard transient on a frame boundary. Four cells exist to reach one thing each and are named for it: `-dup` duplicates channel 0 into channel 1, the only way to reach the ONE-SIDED case of the two-channel lifting, where the decoder reconstructs a channel the encoder never coded; `-skip` is not a whole number of coded frames, so its last frame carries an END SKIP, which every other cell leaves zero; `-pad` carries 16-bit content in a 24-bit stream, which is the only thing that makes the encoder set PADDING ZEROES and so the only cell that exercises the output stage's shift; and the 5.1 cell carries a channel mask. **Seek**: sample-exact through the engine, and bit-exact through the codec. `tests.TestWMALosslessSeekSampleExact` runs fifty random targets per cell against a linear decode; `codec/wmalossless`'s own seek test starts a decode at every packet of every cell and requires exactly the tail of the continuous decode, sample for sample, because a seekable tile clears every filter. Two things make that work and both are measured: the container lands on the newest object the decoder can BEGIN at, read from the codec's own packet-header flag rather than assuming every object is one, and it snaps that object's presentation time onto the frame grid a resumed decode lands on, since ASF states times in milliseconds and the difference is 48 to 58 samples. A landing that reaches no tile emits nothing, which is legal and not damage; the per-cell counts are asserted so a decoder that landed one packet later each time cannot pass. **Fuzz**: `FuzzDecode` over RUNS of packets and configurations through one decoder with no Reset between them, which is the only way to reach the cross-packet state at all; no panic, bounded production, every sample inside the declared depth. 12.4M executions clean across the corpus-seeded and synthetic runs, and the ASF demuxer's own `FuzzDemux` carries a lossless seed so the container's 0x0163 arm is reachable from it (4.9M clean). **Realtime**: >=100x, measured 710x at 44.1 kHz stereo 16-bit and 228x on the binding shape, 48 kHz 5.1 at 24 bits, where six cascades run per sample. **Refused by name**: arithmetic coding, the LPC filter, a spliced packet, transmitted CDLMS coefficients, a non-aligned tiling, a CDLMS order above 256, a bit depth other than 16 or 24, more than eight channels, a subframe depth above 5, a packet longer than nBlockAlign, and, at the container, any 0x0163 header the decoder would refuse, carrying the container's public name. **Not** refused, because none of them is damage: a sequence-number jump, a packet that is entirely continuation (115 of 496 in the corpus), a packet SHORTER than nBlockAlign (container/asf assembles compressed payloads independently of it), a landing before the first seekable tile, a caller's own emit failure, and a decode that outruns the container's declared duration, which every file does by about a millisecond because the ASF Play Duration rounds down. Neither a refusal taken from the packet header nor a caller's emit failure latches the decoder, because neither has consumed a frame bit and latching would also cost the frames still waiting in the carry. **Recorded deficits, all of them unreachable rather than unmeasured.** (1) MCLMS is implemented from the notes and exercised by NO fixture: no encoder sets its flag, on stereo or 5.1, with correlated channels or without, so a stream that uses it decodes on a path nothing here verifies. It is implemented rather than refused because the description is complete and refusing would turn a valid file into an unplayable one. (2) The CDLMS cascade's ORDER is unverifiable for the same reason: every tile any encoder writes declares one filter per channel. What is pinned is that a second filter changes the output, that the declared order changes it, and a digest of a two-filter decode so the order cannot change by accident; which order is RIGHT has nothing to be right against. (3) The frame's start skip is applied as a head trim by inference: the reference reads the field and discards it, and no file carries one. The end skip beside it IS confirmed, by the `-skip` cell. (4) Mono, and any 16-bit stream above 44.1 kHz, are reachable in the format and producible nowhere. (5) One decode-flags word, 0x01a1, is all any encoder writes, so the frame-length adjustment, the length prefix and the absent-DRC path are structural only; the tile header and the tiling walk are covered at every subframe depth by hand-built frames instead. (6) The channel count comes from nChannels and the mask supplies only the layout, which is NARROWER than the notes describe: Windows also writes SPEAKER_ALL (0x80000000), whose population count is 1 whatever the stream carries, and overriding from it turned a stereo file into a mono track that probed playable and died mid-frame. (7) A mid-stream continuation count of zero with a carry still open is the one place the notes contradict themselves; the decoder decodes the carry, which keeps the benign case's audio, and treats a failure of that walk as the discontinuity it is rather than as drift. `docs/notes/wma-lossless-oracle-corpus.md` carries the measurements behind every number here. |
| WMA Pro (Windows Media Audio 9/10 Professional) | **A differential against FFmpeg on files it did not write, and cannot.** Windows' own encoder (`scripts/wmfenc/wmfenc.ps1 -Subtype Wma9`, Windows only, self-skipping) makes every fixture, and FFmpeg's scalar decode (`-cpuflags 0`) of the committed bytes scores this decoder, so the encoder and the oracle are independent. Gate: RMS <= 1e-7 FS and max <= 1e-6 FS on every gate cell, sample count exactly what FFmpeg produces, which on the power-of-two cells is exactly the source length. Measured worst case 5.1e-8 / 4.8e-7, at the oracle's own floor: FFmpeg's scalar and vectorised decodes of these files differ by 2.1e-8 to 4.4e-8 / 2.4e-7 to 4.2e-7, and Windows' own decoder agrees with FFmpeg to 1.8e-7 RMS on stereo and only to about 5e-5 on every 5.1 cell, which is why the bound is not written from the stereo figure. **Corpus**: six committed cells (`codec/wmapro/testdata/corpus/`, 356 KiB) and six generated on Windows, spanning 44.1 to 96 kHz, 16 and 24 bits, stereo and 5.1 with a channel mask and an LFE channel, both frame lengths (2048 and 4096), and bit rates from 64 to 768 kbit/s; the encoder offers no mono at any rate and no 16-bit above 48 kHz, so neither exists. Every count in the corpus note's coverage matrix, measured there with the analysis pass's own instrumented decoder, is pinned against this decoder's path counters (`TestCorpusReachesWhatTheNotesMeasured`), so the walk is known to take the same branches the same number of times and not merely to land on the same samples: both frame lengths, every subframe length, non-uniform tiling with per-channel presence bits, group sizes one through six, the stereo and the scaled two-channel matrices, the explicit rotation matrix (13 times), both scale-factor forms and the 14-bit escape, resampling without transmission, both run-level books, both vector escapes, the large-value escape, the run-level escape, per-channel step modifiers, the step escape, the start trim, and the zero continuation count that discards a carry. The tonal cell (one pure tone per channel, at a length that is not a multiple of the frame) exists to reach two rows nothing else does, pinned by count: the channel transform enabled **band by band** (24 times) and an **end trim**. **Alignment and polarity** are checked against the source PCM regenerated in Go with no floating point: the best-fit lag is zero on every non-periodic cell, and every channel of every cell correlates positively with the source (0.95 to 0.999, and 0.47 on the band-limited LFE channel), which is the test the notes ask for by name, since every sign bit in this codec means the opposite of the usual convention and a negated decode is bitstream-valid and sample-count exact. **Seek**: sample-exact through the engine (`tests.TestWMAProSeekSampleExact`, fifty random targets on four cells) and bit-exact through the codec, where a decode started at every packet of every cell must be exactly the tail of the continuous decode. Every media object is a start point at the cost of one frame of lead-in, and that is measured rather than assumed (65 resume points across thirteen files in the analysis pass); the container snaps an object's millisecond time onto the frame grid, and the per-cell landing counts are asserted so a decoder that dropped the landing packet's own frames could not pass. The seek test is what found the one resume defect the differential could never see: a decode resumed mid-stream appended the packet's continuation bits to an empty carry and parsed them as a frame. **Fuzz**: `FuzzDecode` over RUNS of packets and configurations through one decoder with no Reset between them; no panic, bounded production, every sample finite, the last because the per-band gain is ten to a transmitted exponent and a crafted step escape can name one no float32 holds (bounded at 10^20 and refused past it). 4.0M and 2.9M executions clean across the soaks before and after the review round; the ASF demuxer's `FuzzDemux` carries a Pro seed (30.9M clean), and it is the seed that found the one contradiction the review round's re-soak turned up: on a fuzzed stream whose first object sits off the frame grid, a seek before the start reported the snapped landing while a linear read reported the object's own time, so the snapped landing is now clamped to never claim a position later than both the target and the object's time (real files start at zero and are unaffected; the input is a committed regression seed). **Realtime**: >=100x, measured 941x at 44.1 kHz stereo 16-bit and 337x on the binding committed shape, 48 kHz 5.1 at 24 bits and 384 kbit/s (the review round's byte-wise bit carry and per-band gain table took those from 788x and 265x). **Refused by name**: the low-bit-rate tool, selected by a non-zero word at extra offset 16 (measured against Windows' own decoder, the only reference decoder decodes such streams 61% and 94% wrong, so there is no oracle for them; 32 kHz is the one rate at which Windows offers a single format and every 32 kHz stream is this shape, so the refusal cell is committed), its in-band payload, the channel transform section's leading bit, an unknown two-channel transform, a built-in decorrelation matrix for a group of more than six channels, frames with no length prefix, a sample rate above 384 kHz (the frame-length rule's top arm is open-ended, and the ceiling is where every subframe size still lays out a band, proved by `TestEveryValidConfigHasABandLayout` so that what probes as playable decodes), a frame length above 8192, a subframe depth above 5, a minimum subframe below 64, a depth other than 16 or 24, more than eight channels, a packet longer than nBlockAlign, and the structural conditions (a subframe length past the frame, a shift past the depth, a vector limit past the subframe, a run-level run past the subframe, the run-level escape's three run bits, a scale-factor run past the last band, a step escape that runs out of bits, a frame too short to hold its own length prefix, a frame whose walk reads past its declared length, a band exponent past the bound), each with a hand-built stream. **Not** refused, because none is damage: a sequence-number jump, a continuation count of zero over a pending carry, a count that claims more than the packet holds, a count that claims LESS than the carried frame needs (the frames that begin in the packet still decode), set bits in the header's two skipped positions, a negative quantisation step, a group of no channels, a subframe in which no channel transmits, a run-level block with no end-of-block symbol, a frame with more than one padding bit before its trailer, a packet shorter than nBlockAlign (whose tail is not carried, since the bits to the boundary are missing), and a caller's own emit failure. No refusal latches: every packet is a start point, so a refused packet, a dropped carry or a failed callback costs the frames not yet delivered and one frame of lead-in and nothing else, and a refusal taken from the packet alone costs not even that. **Recorded deficits, all unreachable rather than unmeasured.** (1) The built-in decorrelation matrices are implemented from the notes and selected by NO fixture: the encoder always transmits an explicit rotation instead, so the tables ship unused and a stream that selects one decodes on a path nothing here verifies. (2) The explicit vector coefficient limit is never transmitted, so the variant of the coefficient phase it enables (the zero-run switch disabled) is untested. (3) The post-processing transform field is never set; its matrix is skipped as the notes say, on a path nothing exercises. (4) The subwoofer cutoff is deliberately NOT applied: the reference zeroes the LFE spectrum above it before the loop that fills that spectrum, so the cutoff has no effect there, and no file codes LFE content up high, so a correct cutoff and an ineffective one cannot be told apart; applying it would diverge from the only thing that can score this decoder. (5) Which bits of the offset-16 word enable the low-bit-rate tool is unknown: only three values exist in the encoder's whole enumeration, so the refusal predicate is "non-zero" and `word & 0x0042` is recorded as an unconfirmed narrower candidate. (6) Mono, frame lengths other than 2048 and 4096, subframe depths other than 16, eight channels and unprefixed frames are reachable in the format and producible nowhere; the tiling walk and the refusals are covered at other depths by hand-built frames instead. (7) The channel count comes from nChannels and the mask supplies only the layout, narrower than the notes describe, for the reason WMA Lossless records above. `docs/notes/wma-pro-oracle-corpus.md` carries the measurements behind every number here. |
| WMA Voice (Windows Media Audio Voice 9) | **A differential against FFmpeg on files it did not write, and cannot, with the superframes FFmpeg gets WRONG skipped by name rather than tolerated.** Windows' own encoder (`scripts/wmfenc/wmfenc.ps1 -Subtype "{0000000A-0000-0010-8000-00AA00389B71}"`, Windows only, self-skipping) makes every fixture, and FFmpeg's scalar decode (`-cpuflags 0`) of the committed bytes scores this decoder, so the encoder and the oracle are independent. Gate: max <= 1e-5 FS and RMS <= 5e-7 on the three 8 kHz cells and the demuxer cell, whole file with nothing excluded, all four scored in the one test; on the other four, outside the named runs, a per-cell entry at about twice the cell's own measured figure (`staleDeficits`: 2e-6/1e-7, 1.2e-4/2e-6, 2e-5/4e-7 and 9e-5/1.6e-6 max/RMS), each a ceiling and a floor so a fix cannot land without tightening the entry it fixed, in the shape codec/wma's `msDeficits` has. Measured 8.0e-7 to 1.6e-6 max and 5.1e-8 to 6.5e-8 RMS on the first group and 9.2e-7 to 5.1e-5 max and 4.6e-8 to 9.3e-7 RMS on the second, against an oracle whose own scalar and vectorised paths differ by 6.6e-7 to 2.2e-6 max and 2.3e-8 to 4.2e-8 RMS. Sample count exactly the source's on every cell. **The skip is the unusual part and it is measured, not assumed.** A superframe may span two packets, so a decoder holds the tail of a packet as a bit-string carry; FFmpeg holds that carry in a 256-byte buffer through a helper that silently copies nothing when the copy does not fit, while recording the length it was asked for, so a carry above 2048 bits makes it decode the next superframe out of an earlier packet's bytes. The result is a plausible superframe of unrelated content, often very loud (4.66 full scale inside a digital-silence passage where Windows' decoder produces 0.19), whose filter memories poison the next two. Rebuilt with the buffer enlarged and nothing else changed, the reference agrees with a decoder written from the notes to 6.5e-6 on every cell. The runs are a **per-cell allowlist with the superframe indices in it** (`staleRuns`), and the superframes that START them are derived from this decoder's own carry length and checked against that list (`TestStaleCarryMatchesTheReferencesDefect`), so a change to the carry layer that moved them fails rather than skipping the wrong samples. Widening the tolerance to 4.66 instead would pass a decoder that was wrong everywhere. **The oracle has a second, much smaller defect, and that one is a VERSION of the oracle**: ffmpeg 7.0 and older narrow the denoise filter's energy index to float before truncating it to an int (upstream dropped the narrowing in `9876158e`, first released in 7.1; Ubuntu 24.04, which the CI differential job runs, ships 6.1.1). Measured with the reference instrumented, the narrowing moves the index exactly once across all eight fixtures, at superframe 31 of `voice-16000-16k`, where 28.999999561853929 rounds to 29.0 and one denoise coefficient of one frame comes from the next row of a table whose rows are 3.3% apart; the postfilter carries it into the next two superframes, where the two versions differ by 6.3e-5, 4.2e-6 and 3.0e-8, and everything else in every cell is bit-identical between them. Those three superframes are skipped by name (`narrowedRuns`) only on an oracle older than 7.1, so a current one scores them at the cell's own entry, and they are also scored ALONE there (`TestNarrowedRunsAreCleanOnACurrentOracle`: measured 6.0e-07 max and 1.0e-07 RMS, inside the clean bound), which is what stops the skip from hiding a decoder error. Every per-cell table's keys are pinned to the cell list (`TestSkipTablesNameRealCells`), since a key that stops naming a cell is silently no skip at all: measured by mutation, a typo in `narrowedRuns` leaves the teeth test passing with no subtest at all on a current oracle and turns CI's 6.1.1 red at the 6.2e-05 the skip exists for, which reads as a decoder regression. **Corpus**: seven committed cells (`codec/wmavoice/testdata/corpus/`, 76 KiB), **one per format Windows' encoder offers, which is the whole envelope**: mono 16-bit at 8 kHz (4, 5 and 8 kbit/s), 11.025 kHz (10), 16 kHz (12 and 16) and 22.05 kHz (20). There is no stereo format, nothing above 22.05 kHz and no other depth, so no cell here is a sample of a wider space of encoder settings; what the corpus cannot reach is the space of BITSTREAMS the format allows and this encoder never writes, which is the deficit list below. Every cell can be regenerated by a test run on Windows (`TestMicrosoftCorpus`), which checks that the encoder landed on the cell's shape before scoring the result, since MediaTranscoder renegotiates a format it cannot honour rather than refusing it. Each cell is eight segments of speech-like synthesis (voiced with a falling then a rising pitch sweep, unvoiced noise, digital silence twice, half-and-half, near-floor noise, and a loud voiced segment from a standing start), and the two silent segments are what found the reference's defect: they are where the encoder drops to about a hundred bits a superframe and a fixed-size packet then leaves kilobits of tail. Every count in the corpus note's coverage matrix, measured there with the analysis pass's own instrumented decoder, is pinned against this decoder's path counters (`TestCorpusReachesWhatTheNotesMeasured`): both LSP orders, both quantiser and default modes, thirteen of the seventeen frame types, both adaptive codebooks, all four quarter-sample phases of the Hamming codebook (the whole-sample copy and the three through its 16-tap sinc) and all eight reachable phases of the asymmetric one (its table's ninth, phase 0, is unreachable by the rounding that produces the phase), all three non-saturating arms of the pitch conversion ladder, all three gain predictor weights, both denoise tilt settings, spillover in both the zero and non-zero cases, superframe counts from 1 to 24, and the sample-count field on every cell. **Length and alignment**: a correct decode is exactly the source length on every cell, to the sample, with **no priming, no trim and no delay** anywhere, which is unusual in this tree and has a test of its own; the superframe's optional twelve-bit sample count is the whole mechanism, and the container plays no part (ASF's declared duration is 7 to 30 samples SHORT of the truth here, the opposite direction from WMA Pro, so a decoder that trusted it would truncate). The best-fit lag against the source's energy envelope is zero on every cell; the comparison is on the envelope because at 4 kbit/s a speech coder keeps the envelope and the pitch and throws the waveform away. **Seek**: sample-exact through the engine on every cell (`tests.TestWMAVoiceSeekSampleExact`, fifty random targets on each of the seven), by a run-up rather than by the codec. A resumed decode converges rather than lands exact, since the adaptive codebook is a pitch-lag copy of the decoder's own past output with a gain near 1 and a wrong history decays only as fast as that gain lets it, so the container backs every seek up by the codec's 64-superframe run-up (`wmavoice.SeekRunUp`) and the sample-exact pre-roll discard covers it; a run-up of zero fails that test on three cells. Measured over every media object of every cell, the median resume is exact after 4 to 12 superframes and the worst needs 56; the notes say that figure is not a bound, so the codec-level tests assert convergence rather than a pre-roll, each with teeth: a resume produces exactly the linear decode's tail in count, it converges wherever the file leaves room to (`TestResumeConvergesFromEveryObject`), and it stays a decode of the same audio in the meantime, under-shooting while the pitch memory rebuilds but never over-shooting. **The landing has to reach the decoder**, which is new in this tree: comfort-noise frames draw from a codebook at an offset that is a function of the frame index since the decoder started, so a resumed decode whose counter starts at zero draws different noise in every silent passage forever. `codec.Positioner` is the seam and `format.Media.SeekSample` calls it; removing that one call makes `tests.TestWMAVoiceSeekSampleExact` fail, and an off-grid landing rounds to the nearest superframe, which is what the container's fallback for a file whose first object is off the grid needs. **Fuzz**: `FuzzDecode` over RUNS of packets and configurations through one decoder with no Reset between them, which is the only way to reach the cross-packet carry, the pitch state, the gain predictor, the frame counter and the postfilter's four memories at all. The run's last byte chooses the packetisation (exact nBlockAlign, one byte over on every third packet, an empty packet after each, one byte short), and the whole run is decoded twice around a Reset and must come out the same samples both times, which is what pins Reset to restoring every field a decode reads before it writes; no panic, bounded production, every sample inside 2^20 full scale (which a NaN is not), the last because two recursions here can run away and the denoise filter takes base-10 logarithms of an LPC power spectrum and divides by its range. The soak found the first of those: a crafted LSF set whose nine values sit at the stabiliser's minimum spacing hands the postfilter's re-synthesis an LPC of order 10 with coefficients of 44, which float32 cannot hold stable, and the history it keeps across refused packets grew a thousandfold per half-frame until it overflowed (the input is a committed regression seed). The second is the adaptive codebook, a gain of up to 1.36 times a copy of the decoder's own past excitation, held there by a hand-built stream (`TestRunawaySynthesisIsRefused`). Both end at the same guard: a superframe with a sample past the bound is refused by name and every recursion's history dropped with it, so the next packet is a decode start and not the runaway's tail; the refused superframe still advances the frame counter, since it was decoded in full and only its output is refused. 9.9M executions clean before the finding, a 900 s soak that found it, and 158.4M clean after the guard, then 17.1M after the review round with every run decoded twice; the ASF demuxer's `FuzzDemux` carries a Voice seed so the container's 0x000A arm is reachable from it (37.6M and 36.3M clean). **Realtime**: >=200x, measured 1454x at 8 kHz and 507x on the binding shape, 22.05 kHz at 20 kbit/s, which has the most superframes per second and sixteen LSPs, so its postfilter runs the four transforms most often. **Refused by name**: extradata that is not exactly 46 bytes, by the cbSize field as well as by the bytes behind it (which is what a WMA v1 or v2 header retyped as Voice gets, as damage rather than as "unsupported"), a header declaring more than one channel (this format is mono, and a channel count is scope here as in the sibling codecs), a denoise strength of 12 or more, a sample rate outside 322 to 22097 Hz, a rate whose derived delta-pitch half-range is zero (every rate below about 8 kHz, which the notes require without following it to its floor), nBlockAlign of zero or above 2^22, a variable bit mode class holding more than three frame types, a superframe whose speech/music bit is clear (a WMA Pro payload inside a WMA Voice superframe, which the reference refuses by the same name and has never seen), a frame type symbol the stream's own tree does not map, a superframe declaring more than 480 samples, a superframe count of zero (the count is one more than the body yields, so zero describes no packet, and accepted it would drop the rest of the body without a word), a superframe whose synthesis diverges past 2^20 full scale (the only refusal that also drops the decoder's histories, since they are what diverged), a packet longer than nBlockAlign, and the structural conditions (a packet too short for its header, a count run that outruns the packet, a spillover longer than the packet holds, a superframe whose fields outrun the bits available, including at its first bit, which is damage and not the WMA Pro refusal a clear bit means). The second registered tag 0x000B is refused by its own name at the container and at the codec, because no reference decoder reads it: measured, ffmpeg 8.0.1 reports an unknown codec for a corpus file retagged to it. **Not** refused, because none is damage: a spillover of zero with a carry pending, a spillover of zero with none, a packet whose superframe count is 1 so its body decodes nothing, **a carry above 2048 bits** (it is the reference that cannot handle those), a statistics field being present, a sequence number that jumps, a packet shorter than nBlockAlign, an empty packet (which flushes the carry, as the reference's own end-of-stream call does), a pitch field that decodes to maxPitch or above, a window pulse whose position falls outside its block, a window second set that finds no free position, an LSF set that needs stabilising, any value in the flags word's unread bits, and a decode that outruns the container's declared duration, which every file does. No refusal latches: every media object is a decode start point, so a refused packet costs its own superframes and the next one resumes, though it does not come back exact for the reason the seek row gives. A hole is handled at two layers. An over-long or refused object drops the carry with it, so the packet after it is never spliced onto the head of a superframe whose tail went with the hole (`TestAHoleCostsOnlyItsPacket`: the lost superframes are exactly the object's own plus the one it carried, nothing after the hole is refused, and what follows converges as a resume). A media object the container drops with a warning reaches the decoder as a discontinuity on the next packet (`codec.Packet.Discont`, new here because this is the first codec in the container that carries bits across objects), on which the format layer restarts the decoder and tells it the landing (`asf.TestADecodeReadsPastAHoleAsAResume`: exact again from superframe 42 on the cell it is measured on, where the same file used to decode to one spliced superframe and no error). **Recorded deficits, all of them unreachable rather than unmeasured.** (1) The **independent per-frame LSP path** is implemented from the notes and selected by NO file: every packet of every cell sets the residual flag, so two codebook readers and a whole alternative frame layout ship on a path a hand-built stream proves decodes and nothing can prove decodes CORRECTLY. (2) **Frame types 8, 9, 10 and 14** are rows of a fixed table the encoder's trees give codewords and never emit. (3) The **pitch-adaptive window coder's other arms** are the highest-risk untested code in the codec: two cells reach the coder at all and both through one arm (range 24, positive pulse counts, no extended index, no concealment), so the 16-sample range, the extended 8-bit index, the zero-or-negative count branch and the concealment path are described but never decoded. The concealment's gain is an inference on top of that: a window-pulse frame reads no silence gain, so a concealed block comes out silent. (4) The **statistics field**, the **superframe count escape**, the **saturating pitch conversion arm**, **LSP stabilisation** and the **DC removal filter** are each implemented and each reached by nothing; the escape and the stabiliser have hand-built tests, the other three do not. (5) The denoise filter's **phase reference `h64`** is the reference's transform library writing past the outputs its DST-I declares, and reproducing it is a DECISION: the alternative reading is the only one with a defensible meaning and costs three orders of magnitude of headroom (6.4e-3 against 6.5e-6), which would turn this gate from the oracle's own floor into a named tolerance of about 1e-2 and hide real bugs. The same choice is made at index 64 of the LPC power spectrum, which reads bin 32's real part. (6) The denoise coefficient walk's **alternating sign** is described by the notes and confirmed by nothing: measured, inverting it (the specific mistake the notes warn about) moves this decoder's differential from 8.0e-7 to 2.7e-6 max and from 6.2e-8 to 1.0e-7 RMS on one cell and not at all on another, so it sits inside any bound the oracle's own floor allows. The reading here is the one the notes state and it is consistently NEARER the reference than either the inverted or the dropped form, which is evidence and not proof. (7) **Windows' own decoder is not a gate and cannot become one**: it differs from both FFmpeg and this decoder by 5 to 15 percent relative RMS everywhere, not concentrated anywhere, and it is not a delay (best-fit lag zero), not a level error (loud-superframe energy ratio 0.987 to 1.004) and not the cache defect. Nothing in reach can arbitrate, so it is a maintainer's sanity check with the expected figure written beside it. `docs/notes/wma-voice-oracle-corpus.md` carries the measurements behind every number here. |
| Musepack (SV7/SV8) | **Primary gate, libmpcdec's own float output.** The reference decoder is built from the pinned musepack_src_r475 tarball and read through `scripts/mpcdecraw`, since its own `mpcdec` tool writes truncated 16-bit WAV. Every tool-generated cell of both encoders and the FATE pair decode **bit-identical** on amd64 (max 0, 100% of samples exact); the gate is 1e-6 FS max and 1e-7 FS rms so another architecture's float rounding stays inside it. The cells span both stream versions, all four rates, mono and stereo, noise substitution (`--pns` on mppenc, `--quality 0` on mpcenc, which turns it on by itself), mid/side off, `--thumb`, `--braindead` and `--quality 10` (every quantiser path, the nibble-pair books included), one key frame per frame and the default 64, no seek table, no encoder info, and source lengths in all three SV7 tail regimes: a last frame of at most 671 samples, above 671, and exactly a frame. That regime decides the decay frame: the reference decodes one frame past the header's count exactly when the last frame holds more than 671 samples (its 481-sample synthesis delay pushes the tail into the next frame), and its output length follows the stream's own 11-bit last-frame count, not the header's; a mismatch between the two warns. **Repack gate**: every SV7 cell repacked losslessly by `mpc2sv8` decodes bit-identical to the original in float32, which pins the two frame readers, the two scalefactor state machines and the noise generator's draw order against each other with no rounding slack. **Secondary gate, ffmpeg** (`-cpuflags 0`; fixed-point synthesis, its own noise generator, neither the delay nor the tail trimmed, never a decay frame): noise-free cells max 4 LSB16 and rms 1.5 LSB16 over the overlap (measured 1.3 and 0.41). On noise-substituted cells ffmpeg's substituted noise carries **3.6 to 3.7 times the reference's power** (about 5.6 dB louder) while the shared content correlates at 1.0000; the two independent noises are separated from the content through the difference signal, and the gate is that separation (content correlation 1 within 2%, noise power ratio 3.3 to 4.0), so a release of ffmpeg that changes its noise level fails it and gets this text revisited. Recorded as ffmpeg's divergence, not a deficit: the reference is the format's own decoder and ours reproduces it bit for bit. **Tool-free**: the committed fixtures decode to exactly their source's sample count (the decay frame, the `mpccut` beginning-silence cut and the repacks included) and track the source, sine cells within 1e-3 FS rms (measured 3.4e-4) and noise cells above a per-cell correlation floor set from the measurement (a perceptual codec keeps little of white noise at quality 0, so one floor for all would say nothing); a coverage test asserts the set still reaches every quantiser path of both versions, both block shapes, all four rates, both channel counts, all three tail regimes, a nonzero beginning silence and a repack. **Seek**: bit-identical to a linear decode from the landing on both versions with noise substitution in play, through the codec and through the engine; the landing is the frame starting at least 512 samples before the target so the cold synthesis history refills inside the pre-roll, and the decoder state it needs (SV7 chains its scalefactor deltas across frames forever, both versions carry a noise generator no frame resets) is scanned from the stream, cached across seeks and through `container.Indexer` (the restore re-derives nine intervals from the stream before adopting a blob), and delivered in the first packet. An SV8 stream whose encoder info declares noise substitution off is not scanned: the declaration is trusted the way a LAME tag or an iTunSMPB atom is, both encoders' declarations agree with their streams on every fixture (`TestNoiseDeclarationMatchesUse`), and a stream that lied would resume with a different noise realisation, not wrong audio; an undeclared stream (no encoder info) is scanned. The reference's own seeks are approximate, so the linear decode is the oracle. **Length**: an SV7 stream states no position, so its length is confirmed by hopping every frame's 20-bit length field at open (19 ms per two hours in memory); the walk is the price of the probe-equals-read invariant and is taken at open rather than deferred. SV8 walks its packet headers to the end marker and derives the frame total from the sample count the way the reference decodes (`ceil((count + 481) / 1152)`), verified on every mpcenc cell by the padding rule in the last block. A truncated stream warns, shrinks to what its packets deliver and fails strict mode; a stream running past its declared length stays a tolerated disagreement. **Chapters**: the CT run is read from the two places the reference reads it, and the tag behind each chapter's gain and peak is the APEv2 header record without its preamble followed by the items, the shape the reference chapter editor writes and one of the two the specification allows (a tag not led by a header record, a start past the sample bound or past the end of the stream, and a run past the chapter cap each warn rather than pass silently); `chapters.mpc`, written by mpcchap through `make mpc-tools`, pins it, after a hand-built run had pinned bare items, which no writer produces, and every real title read as empty. The run is written in the editor's .ini order, so it can be unsorted; chapters are returned in start order (stable), which the API's span rule and the mp4 chapter track need and which the tag library does too. Refused by name: SV4 to SV6 (which share the magic and nothing else), more than two channels, a reserved rate index, a stream header failing its CRC or its version byte, an SV8 stream with no header before its first block, a header last-frame count above a frame, oversized frames, blocks, varints and header packets, a frame that does not consume its declared bits, a block with more than padding past its last frame; a decode failure is latched until Reset, since nothing in a packet re-anchors a drifted reader. The unknown-length SV8 shape (sample count 0) is hand-built only, since mpcenc always knows its input length; its last block's frame count is derived by parsing the block, so the stream delivers every frame less the synthesis delay as an exact length, and only a last block that fails to parse is read as a full block with an advisory length and a warning. **FuzzDecode** runs over RUNS of packets and configurations through one decoder with no Reset between them, **FuzzDemux** over both modes. >=**300x** realtime (measured 690x on the widest-quantiser cell, 1000x on noise, 1800x to 1900x on tone). |
| WavPack | bit-exact vs ffmpeg on the official WavPack test suite (every bit depth 8 to 32, the three stereo block modes, redundant-LSB and non-standard-rate files, all four speed modes) and on fixtures from both ffmpeg's encoder and the reference `wavpack` CLI; every out-of-scope shape refused by name (hybrid, float, DSD, more than two channels, and stream versions outside 4.02 to 4.16); the suite's pre-4.0 samples never reach the decoder at all, since they carry the source WAV header and resolve as RIFF; sample-exact seek; >=200x realtime |

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

The same pin covers the one *decoder* golden that hashes samples instead of
scoring them against a tolerance, the toolless committed-fixture digest in
`codec/wma` (`wantDigestArch`): arm64 contracts the IMDCT rotations and the
overlap-add, and `math.Pow` reaches a different `Exp`/`Log` there, which is an
ulp on some samples and a different hash. Everything else in the decoder table
is a differential with a tolerance and gates on every architecture.

**Where the baselines run.** The reference encoders these gates score against
are ffmpeg *build options*, not platform facts, and a given ffmpeg may omit
any of them. Ubuntu's build carries the lot, which is why `make
encoder-quality` is a Linux/CI target (the nightly job). It sets
`WAXFLOW_REQUIRE_*` for each oracle, so a baseline going missing there fails
the job rather than quietly dropping a gate.

The exception is **libfdk_aac**, which is non-free: no distribution ffmpeg
ships it, so `WAXFLOW_REQUIRE_FFMPEG` deliberately does not demand it and the
HE-AAC fresh-encode differentials gate on `WAXFLOW_REQUIRE_FDK=1` instead.
Without fdk they skip and the committed `codec/aac/testdata` fixtures carry the
HE-AAC differential, which is what runs in CI.

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
- The shift-off policy is the reference encoder's (nothing through 20-bit,
  one byte at 24, two at 32) and is pinned per depth, because losslessness
  cannot see it: any choice round-trips, but unshifted 24-bit residuals
  outgrow the Golomb kb cap and pay the 9-ones escape on loud material. A
  16-bit signal widened to 24 or 32 bits costs **exactly the raw low bytes**
  over its own 16-bit encode (an identity the tests assert per frame, not a
  bound), where it used to cost up to 14 bits a sample.
- **One deliberate divergence from the reference**: a 32-bit stereo frame
  never takes the uncompressed escape, because ffmpeg (through 8.0.1) does
  not implement the uncompressed stereo pair at 32 bits and fails the
  decode (32-bit mono works, as do 16/20/24 both ways). Full-range 32-bit
  stereo noise therefore stays compressed at a measured **17.5%** over its
  raw size (37.6 bits a sample; the CPE side channel codes a bit wider than
  the samples, which is most of the overhead). Every escape ffmpeg can read
  remains reachable -- 32-bit mono included, held by its own differential
  cell -- with its header carrying no shift, exactly as the reference
  writes it.
- >= **80x** realtime.

### APE (Monkey's Audio)
- There is no second encoder and no conformance corpus: ffmpeg decodes the
  format but does not write it, and no distribution packages the reference
  tool. `make ape-tools` builds it from the SHA-256-pinned SDK source, the
  same way the libopus tools are built, and the CI differential job runs it
  under `WAXFLOW_REQUIRE_MAC=1`.
- Two things are asserted per cell, and they fail differently. Against the
  **source samples**, which proves the decode. Against **ffmpeg's decode of
  the same file**, which proves the file is what we think it is and would
  catch a fixture generated from the wrong bytes.
- The committed fixtures hold signals `internal/testutil` rebuilds from a
  seed, so the bit-exact assertion also runs with no encoder, no ffmpeg and
  no network. Regenerating them (`go generate ./codec/ape`) is not a
  re-baselining: the same assertions hold afterward or the file is wrong.
- The committed set spans all five levels and all three depths, enforced by
  `TestFixturesCoverTheCascades`, because the level IS the filter cascade: a
  set that drifted to one level would leave four of the five chains resting
  entirely on a tool nobody has installed.
- **Encode**: `decode(encode(x)) == x` bit-exact at every written level
  (1000/2000/3000), depth (8/16/24) and channel count, with a final frame
  shorter than the rest, which is the only frame whose block count the file
  header states separately.
- Two independent readers take our files back: ffmpeg's decoder
  sample-for-sample with `ffprobe` agreeing on the shape, and the reference
  tool's own `mac -v` plus a `mac -d` decode compared against the source.
  The second is not redundant: `-v` recomputes the file MD5, which covers
  the frame data, the format header and the whole seek table, and no decoder
  here reads any of that.
- Our coded frames are the reference encoder's coded frames, **byte for
  byte**, at every depth, channel count and level
  (`TestAPEEncodeMatchesTheReferenceFrames`). Nothing in the format fixes an
  encoder's choices, so this is not required for correctness; what it buys
  is that a drift anywhere in the predictor, the filter's step quantizer,
  the range coder's carry handling or the mid/side matrix fails at the first
  byte it touches rather than as a fraction of a percent of size.
- Both per-frame optimizations (the silent frame and the pseudo-stereo
  frame, each of which codes no values at all) have a test asserting they
  **fire** on material that should trigger them. A stream that never takes
  one is still a correct stream, so nothing else would notice.
- The byte comparison is run against the special frames too, one silent
  channel at a time. That is the only place the reference's channel-order
  quirk shows: it reads the pair as (R, L) and keys LEFT_SILENCE off the
  SECOND channel, so a frame whose channel 0 is silent carries the RIGHT
  bit. Decoders act only when both bits are set, so a transposed pair is
  invisible to every round trip and to ffmpeg; only the reference's own
  bytes catch it, at byte 7.
- A .ape truncated exactly at a frame's first byte is **refused**, not read
  as a frame of no bytes. Only the last frame can produce one, since the
  seek table's synthetic end entry is the only one that can equal its
  predecessor, and a zero-length frame is impossible (a CRC word plus the
  coder's four-byte flush is nine bytes). Before it was refused the demuxer
  emitted a packet its own decoder would not parse, and the muxer inherited
  that through the same parse, so a transmux of a short file died mid-write.
- The **levels are not monotone in size**, and that is the format rather
  than a defect: on synthesized material the 64-tap cascade loses to the
  16-tap one by about half a percent at every length measured, and the
  reference encoder does the same thing on the same input. The gate asserts
  what holds everywhere measured -- both filtered levels beat the fast
  level, which runs no filter at all -- and the byte-identity above is the
  stronger claim anyway.
- **24-bit widened from 16 is large, and that is the format too.** Monkey's
  Audio has no wasted-bits mechanism (no field in a frame can say "the low
  byte is always zero"), and the neural cascade's int16 history saturates
  against residuals 256x its width, so a zero-shifted 16-bit signal costs
  about eight to ten extra bits a sample -- measured 1.9x the 16-bit file on
  tonal material and about 4x on quiet material, where FLAC, WavPack and
  (since the shift fix) ALAC pay only their raw low bytes or nothing. The
  cost is the reference encoder's own, byte for byte:
  `TestAPEEncodeMatchesTheReferenceOnWidenedInput` pins our frames to its on
  exactly this signal class, so a future "fix" that shrinks them has by
  definition stopped writing what Monkey's Audio writes. A caller who wants
  a small 24-bit file of 16-bit audio wants a different format, not a
  different encoder.
- A .ape asked for as APE takes the **transmux** rung: the frames move
  through byte for byte and only the header, the seek table and the file MD5
  are rebuilt, pinned on files from both encoders. The reference-encoded arm
  is the one that matters, because its frames share the word at each frame
  boundary where ours start on one; a muxer that could only lay down its own
  encoder's output fails on every .ape in existence, which is what it did
  until the packet learned to state its frame's byte length.
- Nothing narrows on the way in: a 32-bit integer source is refused by name
  rather than losing eight bits, which is the channel rule applied to depth.
  Every other lossless output here carries 32-bit through, so a quiet narrow
  would make this the one row that drops data without saying so.
- The two deepest levels (extra high, insane) **decode** and are not
  written, refused by name. Their cascades cost 256 and 1552 taps a sample
  in both directions, which puts the decode side into the offline regime for
  a fraction of a percent.
- >= **100x** realtime at the default level (251x measured on the noise
  worst case, 277x on a tone; the high level is about 137x).

### WavPack
- `decode(encode(x)) == x` bit-exact at every level, depth (8/16/24/32),
  and channel count, including the paths a plain fixture never reaches:
  the shift field for a source narrower than its container, the extension
  stream for one wider than the coder's range, dual-mono blocks coded
  false-stereo, and a stream that switches between the two mid-file.
- Two independent decoders read our files back: ffmpeg's, sample-for-sample
  with `ffprobe` agreeing on the shape, and **libwavpack's own** via
  `wvunpack -v` (its stream verification) plus a decode compared against the
  source. The second is not redundant. A decoder only reads the header
  fields it needs, and both ours and ffmpeg's ignore the block magnitude
  field; a 32-bit stream that stated the nominal depth there round-tripped
  through both of them perfectly and was refused outright by the reference.
  `wvunpack` is the only reader that checks everything we write, so it is
  the gate 32-bit output is held to.
- Levels are search breadth, not a fixed cascade: each level tries a
  superset of the previous one's candidate decorrelation chains and keeps
  the smallest per block. Candidates are scored by **coding them**, so the
  ladder is ordered **to the byte** across shapes and depths, not within a
  tolerance: a deeper level runs the shallower one's candidates and cannot
  come out larger. Scoring by a proxy for coded length instead cost up to
  4% on a sustained tone at the default level, which the old within-1%
  wording accommodated rather than caught.
- Each of the four per-block optimizations (false stereo, the joint-stereo
  matrix, the LSB shift, the 32-bit extension) has a test asserting its flag
  **fires** on material that should trigger it. Joint stereo shipped
  disabled on every block a stream ever contained, and round-trip, ffmpeg
  and `wvunpack -v` all passed throughout: a stream that never takes an
  optimization is still a correct stream.
- A digitally silent block costs the blocks after it **nothing**: silence is
  dual-mono, so it crosses into the false-stereo coding mode and back, and
  the two modes' carried state is held side by side rather than rebuilt.
  Rebuilding cost the following blocks up to 7%, and leading silence,
  trailing silence and inter-track gaps are ordinary things for a stream to
  contain.
- Size against **libwavpack** at the matching setting, on the real audio of
  the official suite (8 files, 8- to 32-bit, mono and stereo): **-2.0%** at
  fast, **-0.1%** at normal, **+0.5%** at high, **+1.0%** at very high.
  Parity at the default, ahead at the shallow end, and about a percent
  behind where the reference's own analysis goes deeper than our candidate
  pool reaches. Not a pinned gate (a lossless codec's gate is
  bit-exactness), recorded so a regression has a number to fail against.
- The synthetic corpus is kept for the tonal cases real music underweights,
  and is **not** the size reference: on pure partials the same encoder reads
  14-19% ahead of libwavpack, which says more about the signal than about
  either encoder.
- >= **50x** realtime at the default level (162x measured on the noise worst
  case, 296x on a tone; very high is about 61x). Scoring candidates by
  coding them is also what made this roughly twice as fast as scoring them
  by a log-magnitude proxy, since that proxy's table search was the hottest
  loop in the encoder.

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

### HE-AAC v1
- ODG-proxy at **48 and 64 kbps** on the shared corpus plus bright items
  whose upper octaves exercise the SBR band: corpus mean >= **libfdk_aac
  mean - 0.4** and no track > **0.7** below fdk, judged **at each
  bitrate separately** (a win at one bitrate must not offset a loss at
  another). Known deficits past the allowance are recorded per (track,
  bitrate) in the test's burn-down ledger, each with its own measured
  bound; the test fails when an entry has clearly closed, so the ledger
  is debt on record, never concealment. fdk is best-in-class (a harder
  bar than the LC gate's ffmpeg-native reference, hence the wider
  allowance) and non-free; the leg reaches it through an ffmpeg built
  with libfdk_aac or through `scripts/fdkenc` (a tiny CLI over the
  library's own API; build line in its source) named by
  `WAXFLOW_FDKENC`, self-skips without either, and escalates under
  `WAXFLOW_REQUIRE_FDK=1` (set it wherever a route is expected to
  exist; the stock-ffmpeg CI has none, so its runs judge the offline
  legs only).
  The ledger is empty: its one entry, the transient track at 48 kbps
  (-0.93, bound 1.3), closed with the core's deferred window decision
  (aac-enc-2). The cause was structural, not budgetary: the eight short
  windows start at block offset 448, and the switch fired one frame too
  late, so an attack in the first ~448 samples of a block after quiet
  frames was coded at full weight by the preceding LONG_START window's
  flat region, its quantization noise smeared over the long transform's
  ~70 ms support. The encoder now holds each AU one block past its
  window before deciding (emission timing only; the output mapping,
  delay, and the 3010 pin are unchanged), scans attacks per half-block
  against the previous sub-window under an ~18 ms refractory (rolls
  report every stroke, pulse trains at pitch rate report their onset),
  so every detected attack lands inside some frame's short windows.
  Re-judged 2026-08-18: mean -0.03 at 48k, **+0.20 above fdk** at 64k;
  the transient cell reads -0.08 at 48k and +0.70 at 64k.
- Offline legs, every run: ffmpeg's decode of our stream agrees with our
  own within the AAC RMS gate (cross-decoder conformance on our own
  bitstream); the QMF band-energy round trip holds the high band present
  and level-correct within 6 dB; and the ODG mean stays within **0.7** of
  our own AAC-LC at 64 kbps, a catastrophe net only, because an NMR-based
  proxy structurally punishes parametric replication (band-center sines,
  phase-random patches) that fdk pays for identically.
- The measured encode-decode delay is pinned at exactly
  **HEEncoderDelay (3010)** output samples under both decoders.
- >= **20x** realtime.

### HE-AAC v2
- ODG-proxy at **24 and 32 kbps** on the stereo corpus items plus two
  wide-image items (per-band alternating pans with drift, a shared bed
  under decorrelated noise: the shared corpus's stereo items are all
  R = k*L, which exercises none of the parameter layer, so without
  these the gate could not see a broken rotation, ICC sign, or
  decorrelator at all). The v1 gate's shape at v2's operating points:
  corpus mean >= **libfdk_aac aac_he_v2 mean - 0.4** and no track >
  **0.7** below fdk, judged at each bitrate separately, with the same
  per-(track, bitrate) burn-down ledger. The ledger is empty: its two
  entries, the v1 ledger's core-bound transient cell at v2's budgets
  (-1.30 at 24k, bound 1.6; -0.84 at 32k, bound 1.1; core-bound proven
  by the mono A/B and crossover experiments recorded in the test),
  closed together with the v1 entry under the core's deferred window
  decision (aac-enc-2), as the entries predicted. Re-judged 2026-08-18:
  mean -0.01 at 24k, **+0.10 above fdk** at 32k; the transient cell
  reads -0.16 at 24k and +0.23 at 32k, and bright-tonal and wide-pan
  still beat fdk at 32k.
- Offline legs, every run: the v2 mean stays within **0.4** of our own
  HE-AAC v1 at the same total bitrate (v2 exists because the
  mono-core-plus-parameters trade wins at low rates, so losing it means
  the downmix or parameter layer is broken, not "parametric is hard");
  the stereo image itself is gated codec-side, because the
  mono-downmixing proxy cannot see it: both channels within 3 dB and
  the image within 2 dB at three pan ratios, a hard pan lands the
  saturated 25 dB IID step, full and HALF-coherent anti-phase pairs
  reconstruct at level with the right correlation sign (the
  half-coherent bed is the shape where a blended rotation once targeted
  the zero vector and lost about 2 dB), and an uncorrelated pair comes
  back at level, decorrelated.
- The v2 delay is pinned at the same **HEEncoderDelay (3010)**: the PS
  front end is delay-compensated by construction and the pin tests
  prove it on both output channels under both decoders.
- >= **20x** realtime (measured 50-73x on the noise worst case across
  the stage's rounds: the mono core is cheaper than v1's stereo pair by
  more than the front end costs).

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
- Status: the encoder MEETS the ODG gate. At -q4 the corpus mean is **+0.54** vs
  libvorbis with every track at or above libvorbis (worst per-track +0.00): a
  peak-envelope floor plus a perceptual mask floor fixed tonal, block switching
  fixed transient pre-echo, and square-polar stereo coupling (per-channel type-1
  residues, coupling applied at the mapping layer) with demand-driven allocation
  carries stereo material at or above libvorbis. The +0.54 (re-measured
  after the shared attack detector moved to a previous-sub-window
  reference with an ~18 ms refractory, vorbis-enc-10) is smaller than the
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
AAC >=**150x** / HE-AAC >=**150x** / Opus >=**150x** / Vorbis >=80x /
WavPack >=**200x** / APE >=**100x** / WMA >=**500x** / WMA Lossless >=100x /
WMA Pro >=100x / WMA Voice >=**200x** / Musepack >=**300x**;
encode FLAC >=**150x** / ALAC >=80x / MP3 >=40x / AAC >=20x /
HE-AAC >=**20x** / Opus >=**30x** / WavPack >=**50x** / APE >=**100x**;
resampler HQ >=200x. The bolded
floors were ratcheted at the v1.0 bench pass against the post-FFT
measurements (decode FLAC 537-934x, AAC 260x, Opus 289-522x; encode FLAC
230-250x at level 5, Opus 55-67x), leaving 2x or more headroom for
slower CI runners; the HE-AAC pair was ratcheted at the encoder stage's
first bench pass (decode 357x, encode 39x on the noise worst case after
the review round's per-AU headers and gate legs landed).
Watched by the nightly `bench` job (benchstat against the previous
night's numbers, cache-carried). The AAC and Vorbis MDCTs were moved
onto the N/4 factorization after the WMA one was (each direction ran a
full N-point complex FFT, and the two inverses fed theirs a spectrum
zero-padded to N, so half of that transform was multiplying zeroes):
AAC decode 310x -> 482x, HE-AAC decode 363x -> 456x, Vorbis decode
232x -> 300x, with 2-16% on the encoders, whose cost is dominated by
the psychoacoustic model rather than the transform. The Vorbis decode
floor had no benchmark behind it at all until then, and neither codec
had a benchmark that would have shown the transform's cost; both
directions of both codecs are scored against their O(N^2) sums, which
is what makes a rewrite checkable independently of what it replaced. The WavPack encode floor is for the
default level, which is where the delivery path runs; the deeper levels
buy their size with search breadth and are the offline-job regime (very
high measures a bit over a third of the default's factor). It is left at
50x rather than ratcheted to the encoder's current 162x worst case,
because that speedup came from a change made for size, and a floor set
against it would be ratcheting on a number nothing was tuned for.
The APE decode floor is likewise for the default compression level, which
is what encoders have written by default since the format existed. There
the level is not search breadth but filter taps, and they are the decode
cost too: measured on pink noise the five levels run 430x / 267x / 149x /
62x / 15x, so the deepest one decodes a frame of its own 26-second length
in under two seconds and still plays back an order of magnitude faster
than realtime. A floor stated against the deepest level would be a floor
on a setting almost no file uses. The APE **encode** floor is for the same
default level and for the three levels the encoder writes; there is no search
over candidates here, so encode and decode cost track each other and the high
level measures about 137x.

The WMA decode floor is set at the 44.1 kHz stereo measurement (625x), not
at the 8 kHz mono one (6011x): the short-frame cell runs an order of
magnitude faster because the per-block overhead is tiny against a 512-sample
frame, and a floor stated against it would be a floor on a configuration
almost nothing is delivered at. There is no encode floor because WMA
encoding is a non-goal.

The Musepack decode floor is set against the widest-quantiser cell (mpcenc
`--quality 10` on noise, 688x), not the tone measurements (1800x to 1900x)
or the SV7 noise cell (1000x): the cost is the Huffman walk per sample, and
the quality-10 profile codes the most samples with the longest books. The
floor sits at 300x for the usual 2x headroom on slower runners. There is no
encode floor because Musepack encoding is a non-goal.

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
