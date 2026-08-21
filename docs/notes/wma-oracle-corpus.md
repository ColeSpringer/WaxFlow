# WMA oracle corpus and black-box behaviour

ADR-0001 black-box analysis artifact, the oracle half of the WMA analysis
pass. Everything here was produced by running the `ffmpeg` binary, which
ADR-0001 permits as a test oracle. It records what the corpus should be, what
the binary does, and what the binary **cannot** be asked, which is the part
that decides how much of a WMA decoder a differential can actually gate.

Measured with `ffmpeg version 9.0-full_build-www.gyan.dev`, libavcodec
63.1.100. Figures below are from that build on this machine; the identities
(block align, frame length, decode lag) are exact and portable, the noise
floors are not.

## 1. The circularity, stated plainly

ffmpeg is the only WMA encoder available here and the only WMA decoder
available here. A differential built from it asks "do we agree with ffmpeg",
not "do we decode WMA", and the two questions come apart exactly where ffmpeg
is wrong. Three things narrow the gap, in descending order of strength:

1. **A file from a non-ffmpeg encoder.** One real Windows Media Encoder file
   converts the claim into "we decode WMA". This is the single highest-value
   fixture this codec can acquire, and it is also the only way to reach the
   paths in section 4.
2. **Structural checks that do not involve ffmpeg**: the block-align identity
   below, the exponent-band partitions, the Huffman completeness already
   pinned in `codec/wma/tables_test.go`.
3. **The differential itself**, over the corpus in section 3.

## 2. Generating the corpus

The source signal matters less than the configuration, but two properties are
not optional. It has to be **broadband**, or the high bands are empty and
noise coding is never exercised, and it needs a **tonal** component, or
nothing makes the exponent curve peaky. A chirp plus deterministic noise
covers both, and stays generatable rather than committed:

```sh
SRC="aevalsrc='0.28*sin(2*PI*(200+1200*t)*t)+0.22*(2*random(1)-1)':s=$RATE:d=2"
```

**And for a stereo cell it has to be real stereo.** The obvious spelling,
`-ac 2` on a mono source, duplicates the channel: measured on the first
version of this corpus, `max|L-R|` was exactly 0 over every frame. That makes
the side channel identically zero, so mid/side is only ever exercised in its
degenerate form and the cells claiming to cover it cover nothing. It is the
same trap the WavPack fixtures hit. Note that `aevalsrc` will not rescue it
either: `random(1)` and `random(2)` index evaluator state, not seeds, and
return the *same* sequence, so per-channel noise cancels in the side channel
exactly as duplication does.

What works is decorrelating one broadband source against itself, which also
keeps the channels correlated enough at low frequencies that mid/side is
worth choosing:

```sh
FILT="asplit=2[l][r];[r]adelay=1S:all=1,volume=0.85[rd];[l][rd]amerge=inputs=2"

# one cell: version, rate, channels, kbit/s
ffmpeg -f lavfi -i "$SRC" ${CH:+-filter_complex "$FILT"} \
       -c:a wmav$VER -b:a ${KBPS}k "v${VER}_${RATE}_${CH}ch_${KBPS}k.wma"
```

(apply the filter for stereo cells only; mono cells take `$SRC` unchanged).
Measured on the corpus below, that gives `max|L-R|` between 0.58 and 1.00 on
every stereo cell. Check it, per cell, rather than trusting the graph:
decode to `f32le` and take the maximum absolute difference between the two
interleaved channels. A stereo fixture that is secretly mono is invisible
until something that depends on the side channel silently stops being tested.

Read the packet layout back without decoding anything:

```sh
ffprobe -show_entries packet=size,pts_time -of csv=p=0 cell.wma
ffprobe -show_streams -show_data -of default cell.wma | sed -n '/^extradata=/,/^extradata_size/p'
```

Decode for the differential (see section 6 on `-cpuflags`):

```sh
ffmpeg -i cell.wma -f f32le cell.raw            # trimmed, ffmpeg's own delay handling
ffmpeg -flags2 +skip_manual -i cell.wma -f f32le cell.untrimmed.raw
```

## 3. The corpus

Eighteen cells. Every cell is at or above ffmpeg's 24 kbit/s encoder floor
(section 4). The derived columns are what each cell exercises; they follow
the rules in `wma-bitstream.md` sections 2, 7 and 8, and this table is meant to
be transcribed into a fixture generator rather than re-derived.

| ver | rate | ch | kbit/s | frameLen | blockAlign | noise | highFreq | coefPair | what it is for |
|---|---|---|---|---|---|---|---|---|---|
| 1 | 8000 | 1 | 24 | 512 | 192 | off | 1.00 | 2 | shortest frame, v1 |
| 1 | 16000 | 2 | 24 | 512 | 96 | on | 0.50 | 2 | short frame, stereo, v1 |
| 1 | 22050 | 1 | 32 | 1024 | 185 | off | 1.00 | 2 | mid frame, v1 |
| 1 | 32000 | 1 | 32 | 1024 | 128 | on | 0.75 | 1 | v1 default rate arm, high bps |
| 1 | 32000 | 2 | 32 | 1024 | 128 | on | 0.50 | 1 | **v1 at 32 kHz frames 1024 where v2 frames 2048** |
| 1 | 44100 | 1 | 32 | 2048 | 185 | off | 1.00 | 1 | coef pair 1 |
| 1 | 44100 | 2 | 64 | 2048 | 371 | off | 1.00 | 2 | ordinary v1 stereo |
| 1 | 48000 | 2 | 24 | 2048 | 128 | on | 0.50 | 0 | coef pair 0, v1 default rate arm |
| 1 | 48000 | 2 | 64 | 2048 | 341 | on | 0.60 | 1 | v1 default rate arm, mid bps |
| 2 | 8000 | 1 | 24 | 512 | 192 | off | 1.00 | 2 | shortest frame, v2 |
| 2 | 11025 | 1 | 24 | 512 | 139 | on | 0.70 | 2 | the 11025 arm |
| 2 | 16000 | 1 | 24 | 512 | 96 | on | 0.50 | 2 | the 16000 arm |
| 2 | 22050 | 2 | 24 | 1024 | 139 | on | 0.70 | 2 | mid frame, stereo, noise on |
| 2 | 32000 | 2 | 32 | 2048 | 256 | on | 0.70 | 1 | **v2 at 32 kHz normalises to the 22050 arm** |
| 2 | 44100 | 1 | 32 | 2048 | 185 | off | 1.00 | 1 | coef pair 1, noise off |
| 2 | 44100 | 2 | 24 | 2048 | 139 | on | 0.40 | 0 | coef pair 0, noise on |
| 2 | 44100 | 2 | 128 | 2048 | 743 | off | 1.00 | 2 | ordinary v2 stereo |
| 2 | 48000 | 2 | 128 | 2048 | 682 | off | 1.00 | 2 | highest rate |

Coverage: both versions; all three frame lengths (512 x5, 1024 x4, 2048 x9);
all three coefficient pairs (0 x2, 1 x6, 2 x10); noise coding on in 10 cells
and off in 8; **every reachable arm of the high-frequency ladder**, which is
what the last two v1 rows are for; and 10 stereo cells, so mid/side and v1's
stereo byte alignment are both reached with a side channel that is not zero.

Verified end to end: all 18 encode without complaint, all 18 demux through
`container/asf` with **zero warnings** and the right rate and channel count,
every stereo cell decodes to genuinely different channels (`max|L-R|` 0.58 to
1.00), and **every cell's packet size equals `floor(bitRate * frameLen /
(rate * 8))` computed from the frame-length rule**. That identity is a
decode-free check on frame length, including the v1/v2 split at 32 kHz, and it
is the first thing a decoder's tests should assert.

These cells are generated, not committed, and the generator belongs beside the
tests that consume it rather than here: this table is the specification for
it, with every derived column already computed so nothing has to be re-derived
by hand.

They do not replace the five `.wma` files already in
`container/asf/testdata`, which have a different job: they pin container
shapes (a fragmented layout, a tagged file, a mono 8 kHz file) for the
demuxer, and they predate this corpus. Do not promote them into a decoder
gate. All three of their configurations are 44.1 kHz or 8 kHz at one bit rate,
and **all four of their stereo files are false stereo** (`max|L-R|` measured
as exactly 0), which is harmless for a demuxer that never looks at a sample
and useless for anything that does.

## 4. What an ffmpeg corpus cannot reach

This is the section that matters most.

**`flags2` is 1 in every file either encoder writes.** Measured across a
sweep of both versions x seven rates x mono/stereo x three bit rates, 84
cells: v1 extradata is always `00 00 01 00` and v2 is always
`00 00 00 00 01 00 00 00 00 00`. So the corpus reaches VLC-coded exponents
only, and never:

- **LSP-coded exponents** (bit 0 clear). Half of section 6 of the bitstream
  notes, plus the `lspCodebook` table and the different `noiseMult`, is
  unexercised.
- **The bit reservoir** (bit 1). Every packet is exactly one frame, so the
  whole superframe header, the carry buffer and the cross-packet frame are
  unexercised.
- **Variable block lengths** (bit 2). One block size per frame, so the block
  selectors, exponent reuse and the asymmetric window transitions are
  unexercised. This one takes a second thing with it: the tabulated v2
  exponent bands apply only when `frameLenBits - 7 - k < 3`, which needs
  `k >= 1`, so **`expBands22050`, `expBands32000` and `expBands44100` are
  unreachable from any ffmpeg-produced file**. Three of the shipped tables are
  gated entirely on a file the corpus cannot contain.

**The encoder refuses any bit rate below 24000 bit/s**, absolutely, not per
channel and not scaled by rate ("bitrate too low: got 23000, need 24000 or
higher"). Real WMA at 8 to 16 kHz lives at 5 to 20 kbit/s, so four arms of the
high-frequency ladder in `wma-bitstream.md` section 8 sit under the floor and
cannot be reached: 8 kHz `x0.5` and `x0.65` (24 kbit/s at 8 kHz is already 3
bits per sample, which lands in the noise-off arm and cannot leave it), 16 kHz
`x0.3`, and 22.05 kHz `x0.6`. Every **other** arm is reachable and the corpus
covers it, including the two default-arm cases (`x0.75` and `x0.60`) that only
appear on v1 at 32 and 48 kHz.

**The encoder also refuses** rates above 48 kHz ("sample rate is too high")
and more than two channels, while the *decoder* accepts up to 50 kHz. A
stream between 48 and 50 kHz is decodable and not producible here.

Consequence: the differential gates the VLC-exponent,
single-block-size, no-reservoir subset, which is most real WMA but not all of
it. The other paths need either a real-world file or a hand-built stream, and
whichever it is, the gate text must say which paths the differential does not
cover rather than implying the corpus is complete.

## 5. Delay, length and trimming

Measured by matched filter (normalised cross-correlation, peak lag) between
the source PCM and the decode, on 8-second noise at every frame length and
both versions:

| cell | frameLen | raw decode lag |
|---|---|---|
| v1 8 kHz 24k | 512 | +512 |
| v2 8 kHz 24k | 512 | +512 |
| v2 11.025 kHz 24k | 512 | +512 |
| v2 16 kHz 24k | 512 | +512 |
| v1 22.05 kHz 32k | 1024 | +1024 |
| v1 44.1 kHz 32k | 2048 | +2048 |
| v2 32 kHz 32k | 2048 | +2048 |
| v2 44.1 kHz 128k | 2048 | +2048 |
| v2 48 kHz 128k | 2048 | +2048 |

Uniformly one frame. ffmpeg declares `2 * frameLen` of decoder delay and trims
that, so **its default output starts one `frameLen` into the source**; the
`-flags2 +skip_manual` decode above is the untrimmed one. Both numbers are
round-trip through ffmpeg's own encoder and cannot be split between encoder
lookahead and decoder delay without a file from a different encoder.

Recipe for re-measuring: decode both signals to `f32le`, correlate
`src[i]` against `dec[i+lag]` over a window that stays inside both rasters for
every lag tried, and take the peak. Keep the window well away from the ends;
a window that runs off an edge scores a spurious 1.0 on a handful of terms.

Lengths, from the demuxer side: the declared length and the packets' capacity
`packets * frameLen` **do not agree, in either direction**. Measured across
the corpus, declared minus capacity runs from -32 samples (48 kHz) to +60
(44.1 kHz), and it is exactly 0 on the cells where one frame is a whole number
of milliseconds (8 kHz and 16 kHz at 512, 32 kHz at 1024 and 2048) and nonzero
everywhere else.

That pattern places the rounding in ffmpeg's **muxer**, which accumulates
millisecond-truncated packet times, and not in `container/asf`, which reads
the stated play duration in 100-nanosecond ticks and only the pre-roll in
milliseconds. Do not repeat the appealing explanation that the container
"states a millisecond duration": it is wrong about the demuxer, and it also
cannot produce a residue larger than a millisecond or a negative one, both of
which are measured here.

What matters downstream is only this: the declared length is neither the source
length nor the coded capacity, which is why `SamplesExact` is false, why a
decode must not be trimmed to it, and why a decode that overruns or falls
short of it is not damage.

## 6. Two things about the oracle itself

**`-cpuflags 0` changes the answer, `-flags +bitexact` does not.** Scalar and
vectorised decodes of the same file differ, uniformly across the corpus, by
about 2-3e-8 RMS and 1.2-1.8e-7 max full-scale (-152 and -136 dBFS). That is
float32 rounding in the windowing and the transform, not an algorithmic split.

Two consequences. Pin `-cpuflags 0` for anything that stores a reference
answer, so the stored answer does not depend on the host's CPU; the cost is
speed and about -136 dBFS of accuracy, which is far below any useful gate. And
treat 3e-8 RMS / 2e-7 max as the **floor** for a tolerance gate: the plan's
opening bounds of 1e-3 RMS and 1e-2 max sit four to five decades above it, so
there is a lot of room to tighten empirically, and a gate that has to be
loosened past those is reporting a real defect.

**ffmpeg's `-ss` is not the oracle for a cold mid-file decode.** On short
files its seek output is bit-identical to its linear decode, because it
re-decodes enough history. It answers a different question than "what does a
decoder that starts at this packet produce".

## 7. What a seek actually costs, and why

The sharpest measurement in this pass, and it needs its method stated because
section 6 says `-ss` answers a different question on short files. Exactly what
was run, and what makes it valid here:

```sh
ffmpeg -f lavfi -i "anoisesrc=sample_rate=$RATE:duration=30:seed=5:amplitude=0.4" \
       -ac 1 -c:a wmav2 -b:a ${KBPS}k long.wma
ffmpeg -i long.wma        -f f32le full.raw      # linear decode
ffmpeg -ss 20 -i long.wma -f f32le seek.raw      # seeked decode
# align by length: seek.raw is the tail of full.raw, so compare
# full.raw[len(full)-len(seek):] against seek.raw sample for sample
```

**Thirty seconds with a 20-second seek is the point.** Repeat it on a
two-second file and both streams come back bit-identical, because ffmpeg
re-decodes from close enough to the start that the noise history matches; that
is the section 6 caveat, and it is why the short-file result proves nothing.
At 20 seconds the re-decode cannot reach back far enough and the divergence
appears. Any re-measurement has to keep the seek far from the head for the
same reason.

| stream | noise coding | result |
|---|---|---|
| 44.1 kHz mono 32 kbit/s | off | **bit-identical** |
| 11.025 kHz mono 24 kbit/s | on | RMS 7.9e-3 (-42 dBFS), max 3.2e-2 (-30 dBFS) |

And on the noise-on stream the difference **does not settle**: the first,
middle and last eleven thousand samples of the seeked region all read -42
dBFS. It is steady state, not a transient.

That is the noise index of `wma-bitstream.md` section 8, isolated. Everything
else in the decoder resynchronises inside one packet, which is why the
noise-off stream comes back exact; the noise index never resynchronises,
because it is never reset, so a decode that starts anywhere but the beginning
draws different noise for the rest of the file.

How a seek gate should be written, given that:

- **On a noise-off cell, demand bit-exactness** against the linear decode from
  the landing sample. It is a strong gate and it is achievable, and the
  corpus has eight noise-off cells to run it on.
- **On a noise-on cell, never demand convergence.** Compare energies or bound
  the difference against a level measured on the cell, or compare against a
  reference that seeks the same way. A gate written as "converges after N
  samples" will pass by accident on noise-off cells and be quietly disabled on
  the others.
- The -42 dBFS figure is this cell's, on this content. It scales with how much
  of the spectrum is noise-filled, so re-measure per cell rather than pinning
  it as a constant.

## 8. Refusals worth pinning

The decoder side, for the named-error tests the implementation owes:

- Sample rate above 50 kHz, more than two channels, and a missing block align
  are all refusals before any bitstream is read.
- A `.wma` truncated mid-packet fails on the incomplete packet and yields a
  short stream ("Error submitting packet to decoder: Invalid data found"),
  rather than emitting garbage. `container/asf` already refuses a truncated
  file in strict mode, so the two agree.
- WMA Pro, Lossless and Voice are separate `wFormatTag` values and separate
  codecs. `container/asf`'s `asfCodecID` table already names them, and the
  decoder's refusals should reuse those names rather than inventing new text.
