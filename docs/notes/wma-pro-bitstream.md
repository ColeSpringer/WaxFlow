# WMA Pro bitstream notes

ADR-0001 black-box analysis artifact. This file, `wma-pro-oracle-corpus.md`
beside it, and the generated tables named in section 17 are the **only** inputs
the session that writes the decoder consumes; that session does not open
FFmpeg, and none of these files contains code from it.

Scope: `wFormatTag` 0x0162, the codec Windows names **Windows Media Audio 10
Professional**, two to eight channels at 16 or 24 bits. WMA v1/v2 (0x0160,
0x0161) is a different codec that shares the container, the `WAVEFORMATEX` and
a handful of concepts; `wma-bitstream.md` covers it, and the one thing that
genuinely carries over is the frame-length rule, whose Pro arms that file
declines to settle and this file settles in section 2. WMA Lossless (0x0163) is
different again and shares nothing but the container. XMA, the Xbox variant
that reuses this coded layer inside a different container and packet header, is
out of scope and refused by name (section 15).

Like the rest of the WMA family this format has no published specification.
The layout below was recovered in this analysis pass and then checked against
the black-box behaviour of real files. Where a statement is a measurement it
says *measured*; where it is a deduction that no file in reach can confirm it
says so.

## This is a float codec, and that changes the tolerance

Unlike WMA Lossless, nothing here is exact. The coded values are integers but
they are turned into transform coefficients by a base-10 exponential, run
through an MDCT, and windowed, all in floating point. Two conforming decoders
that differ only in the order of floating-point operations will differ in their
output, and the implementation session must not write an exact-equality
assertion anywhere.

What is and is not allowed to vary:

| stage | type | may a conforming decoder differ? |
|---|---|---|
| every bitstream field | integer | no, exact |
| scale factors, quantisation step, exponents | integer | no, exact |
| band edges, subwoofer cutoff, resample map | integer | no, exact |
| coded coefficient magnitudes | small integers held as floats | no, exact |
| the per-band gain `10^(exponent/20)` | float | yes, in the last bits |
| decorrelation matrix entries | float | yes, in the last bits |
| the MDCT | float | yes, and most of the drift lives here |
| the window and the overlap-add | float | yes, in the last bits |

*Measured*: a decoder written from this file alone, in `float64` throughout and
with an FFT-based inverse transform of its own construction, reproduced the
reference decoder's `float32` output on thirteen real files to a worst-case
**absolute** deviation of 4.2e-07 on a nominal full-scale of 1.0, that is about
22 bits of agreement. That is the right order for the implementation session's
tolerance: work in `float32` if you want to be closer, but do not expect or
demand bit-equality.

Two places where the type is not free:

- **The per-band gain must be computed in double and then narrowed**, because
  the exponent runs from about -25 to +115 (*measured*) and `10^(115/20)` is
  about 5.6e5, which a `float32` `pow` implementation will not agree on. The
  reference evaluates `10^(e/20)` in double and stores the result in a
  `float32`; a decoder that keeps it in double instead differs only in the last
  bits, which is inside the tolerance above.
- **Coded coefficient magnitudes are exact small integers** (0 to 15 from the
  vector books, 0 to 99 plus an escape from the single book, and arbitrary
  integers from the run-level escape). Hold them as integers or as floats, but
  a rounding difference there is a bug, not drift.

### Notation

| notation | meaning |
|---|---|
| `x >> n` (signed x) | arithmetic shift, equal to `floor(x / 2^n)` |
| `x >>u n` (unsigned x) | logical shift |
| `trunc(a/b)` | integer division truncating toward zero |
| `a & ~3` | clear the low two bits, that is round down to a multiple of 4 |
| `clip(x, lo, hi)` | saturate to the closed range |
| `floorLog2(x)` | largest n with `2^n <= x`; **0 for x = 0** |
| `ceilLog2(x)` | smallest n with `2^n >= x`; 0 for x <= 1 |
| `signed(n)` | read n bits and interpret them as two's complement |

`floorLog2(0) = 0` matters in exactly one place: a width computed as
`floorLog2(k)` where k can be 0, in which case **read no bits and use zero**,
never read one bit. Section 6.1 marks the one field that takes that path.

All bit reading is **most significant bit first**, bytes in file order. The
little-endian fields are confined to the `WAVEFORMATEX` of section 1.

All Huffman books are canonical, assigned in **table order**: walk the table
from index 0, and for each entry with a non-zero code length L give it the next
code of length L, where "next" means the top L bits of a 32-bit accumulator
that starts at 0 and advances by `2^(32-L)` after every entry. Entries with a
length of 0 are skipped entirely and consume no code space. Decoding is the
ordinary "shift a bit in, look for a code of this length" walk. Section 17 says
which generated table carries the lengths and which carries the symbols for
each book.

## Measurement basis

Reference studied: FFmpeg n9.0 at commit
`d32b387f2b0a484599d4587d651891f0c63c4238`, the commit pinned across this repo.
Every file fetched and studied, with its SHA-256 (the files are LF-only, so
CRLF normalisation is a no-op and these digests hold either way):

| file | SHA-256 | what it contributed |
|---|---|---|
| `libavcodec/wmaprodec.c` | `803547a38dea1294891c00402d6b3576a16053b0f00b395768c4983740c86553` | the whole coded layer |
| `libavcodec/wmaprodata.h` | `0ecb9a82befb7409d379e759c8bcf613c7958cd8cac698278a1427ab7b1ef29f` | every constant table |
| `libavcodec/wma.c` | `b97f11b2dc634c969650dacbae361098dcb5331df0e2164dee70c88962a9bd7e` | the escape-length rule and the run-level tail |
| `libavcodec/wma.h` | `7273921bafb32d258b722fe7749d3dafe779fc752fa3e527dd2113064dc56dc0` | book depth constants |
| `libavcodec/wma_common.c` | `ca2fd90adcd464ca3fe9d75051034a888108ed7fe31e1afb5e6c7ae1781239b7` | the frame-length rule, section 2 |
| `libavcodec/wma_common.h` | `77a8e21df2c9b569224c0d2166bb4a8711859902ebcb1152bae53a65337bf2cd` | nothing beyond a declaration |
| `libavcodec/vlc.c` | `8b44e56d86e0a781aac29aff5fe54e8aec82e923477358e439d619698806f82c` | the canonical code assignment above |
| `libavcodec/sinewin.c` | `515494ac041df33d492da0ed3e0cd99d7953db608106141c33a3f46bf44c6101` | nothing, a wrapper |
| `libavcodec/sinewin.h` | `b486c555871a4d098500ed0f389dff5f5cb07c17df8c70c8c155b5bd3f15f5f9` | window array sizes |
| `libavcodec/sinewin_tablegen.h` | `a2defd94fb60f2c6b838e51219afedf6a0991f393fccae6dd9d205a60b5c78de` | the window formula, section 13 |
| `libavcodec/kbdwin.c` | `a2009e3577b0e890eba200e542f6bc625aa9c3656c9f9667b3213421df4b475e` | nothing; this codec never uses a KBD window |
| `libavcodec/kbdwin.h` | `674fd93e97144f3388e8c21ff0b2f94c09233720763fb0cc6028e93180c8cc76` | nothing, as above |
| `libavutil/tx.h` | `a1c5c309f493cc22f4637e67696581be676cd33fdcaa0661a01f5bb4345c900a` | the transform's length convention |
| `libavutil/tx_template.c` | `b3c4becdffefd82321069775b80bb542e577fb6ad229ea6bda3570d98d76e130` | the exact inverse-MDCT definition, section 13 |
| `libavutil/float_dsp.c` | `349126b23468d502bfa65bb31710600a8e5eb614004c2bcfd5be8545a58fe3d8` | the overlap-add butterfly, section 13 |

The checkout was deleted at the end of this pass.

Everything marked *measured* was taken on 2026-09-10 in one of three ways.

1. **By decoding.** A decoder was written in this pass from the description
   below and run against thirteen distinct real files: the committed corpus and
   the wider generated set of the same family, twelve Windows-encoded cells
   after removing exact duplicates, plus one file encoded during this pass with
   a deliberately non-power-of-two length (100000 frames at 44.1 kHz stereo
   16-bit) to reach the end-trim field. Every one agreed with the
   reference decoder (`ffmpeg -cpuflags 0`, scalar path) on the sample count
   exactly and on the samples to within 4.2e-07. Field statistics quoted below
   come from that decoder's instrumentation, so "*measured*: never occurs"
   means the field was parsed on every subframe of every one of those files and
   never took that value.
2. **By resuming.** The same decoder was restarted at every packet boundary of
   every file, 65 resume points in all, and each resumed decode was compared
   frame by frame with the linear decode. Section 5 reports the result.
3. **From the encoder's own enumeration.** The scratchpad holds a dump of every
   format Windows' Media Format SDK offers for this codec, 189 rows with their
   full `WAVEFORMATEX` and extra bytes, taken through `IWMCodecInfo3`. Claims
   about "what the encoder can write" come from that dump and cover the whole
   envelope, not just the files on disk.

Twelve of the thirteen were produced by **Windows' own encoder** as part of
the corpus work; the thirteenth was produced by the same encoder during this
pass. FFmpeg has no
encoder for this format, so nothing here is a round trip through the reference
and every fixture is genuinely foreign to it.

## 0. The envelope

What the only encoder offers, from the 189-row enumeration:

| axis | values offered | not offered |
|---|---|---|
| sample rate | 32000, 44100, 48000, 88200, 96000 | everything else |
| channels | 2, 6, 8 | **1, 3, 4, 5, 7** |
| bit depth | 16, 24 | everything else |
| 32000 | 2ch/16-bit at 32 kbps and nothing else | every other shape at 32 kHz |
| 8 channels | 48000 and 96000 only | 8ch at 44100 or 88200 |
| 88200 | 24-bit only, 2ch CBR and 2ch/6ch VBR | 16-bit, 6ch CBR, 8ch |
| decode flags | 0x00e0 (183 rows), 0x0060 (6 rows) | everything else |
| word at extra offset 16 | 0x0000 (168 rows), 0xc042 (19), 0x20c6 (2) | everything else |
| extra bytes 6..13 | zero on all 189 rows | anything else |

The format allows far more than that, and the gap is where the risk lives:

| the format allows | the encoder writes | consequence |
|---|---|---|
| 1 to 8 channels | 2, 6, 8 | mono is reachable in the bitstream and unproducible here |
| frame lengths 512 to 8192 | 2048 and 4096 | the shorter and longer arms of section 2 are untested |
| 1, 2, 4, 8, 16 or 32 subframes per frame | 16, always | the other five subframe-depth encodings are untested |
| frames with no length prefix | always prefixed | the unprefixed frame layer is untested |
| a built-in decorrelation matrix per group size | never chosen | untested, section 9.3 |
| an explicitly transmitted count of vector-coded coefficients | never transmitted | untested, section 11.1 |
| a post-processing transform field in the frame header | never present | untested, section 7.3 |
| a low-bit-rate tool signalled in the extra bytes | at and below 96 kbps stereo | **refused**, section 1.2 |

Read that as a warning, not a licence to implement the middle column. Each
untested row is called out again where it appears, and section 15 says which
of them become refusals.

## 1. The WAVEFORMATEX and its extra bytes

`container/asf` hands the whole `WAVEFORMATEX` over as `Track.CodecConfig`: 18
fixed bytes up to and including `cbSize`, then the codec-specific extra bytes.
For this format `cbSize` is 18, so the blob is 36 bytes and anything shorter is
a refusal.

From the fixed part the decoder needs three fields and must ignore two:

| offset | width | field | use |
|---|---|---|---|
| 0 | 2 | `wFormatTag` | 0x0162 selects this codec |
| 2 | 2 | `nChannels` | only when the channel mask is zero |
| 4 | 4 | `nSamplesPerSec` | sets the frame length (2) and the band layout (5) |
| 8 | 4 | `nAvgBytesPerSec` | **not used** |
| 12 | 2 | `nBlockAlign` | the packet size in bytes, and the sole input to two field widths |
| 16 | 2 | `wBitsPerSample` | **not used**; the depth comes from the extra bytes |
| 18 | 2 | `cbSize` | 18 |

`nAvgBytesPerSec` is informational. *Measured*: it is a plausible byte rate on
every real file but it does not predict how much coded data the file actually
holds. On the 32 kHz corpus cell it reads 4000 bytes per second while the file
carries 10752 bytes of coded data for 1.536 seconds of audio, which is 7000
bytes per second. Nothing may be derived from it, and in particular a frame
count must never be estimated from it.

`nBlockAlign` is not a frame size and not a frame count. It is the size in
bytes of one container packet, every packet is exactly that size, and its
`floorLog2` sets the width of the packet header's continuation field and of
every frame's length prefix (section 7). Reading it wrong desynchronises every
packet in the file rather than producing slightly wrong audio. *Measured*
values across the enumeration run from 768 to 35666.

The extra bytes, at offsets relative to the start of the extra block (add 18
for an offset into the whole config blob), are little-endian:

| extra offset | width | field |
|---|---|---|
| 0 | 2 | valid bits per sample: 16 or 24 |
| 2 | 4 | channel mask, `WAVEFORMATEXTENSIBLE` bit assignments |
| 6 | 8 | unused; *measured* zero on all 189 enumerated formats and all 13 files |
| 14 | 2 | decode flags, section 1.1 |
| 16 | 2 | low-bit-rate tool selector, section 1.2 |

**Bit depth** comes from extra offset 0, not from `wBitsPerSample`. The two
agree on every measured file, but only the extra-bytes value drives anything.
It feeds two unrelated things that must both use it: the quantisation base of
section 12 and the transform's output normalisation of section 13. 16 and 24
are the only values in reach; the reference accepts 1 to 32 and this decoder
should refuse anything else by name (section 15).

**Channel mask** at extra offset 2 gives both the channel count and the channel
order. When it is non-zero it overrides `nChannels`: the number of channels is
its population count, and the channels appear in ascending mask-bit order,
which is the ordinary `WAVEFORMATEXTENSIBLE` order (front left, front right,
front centre, LFE, back left, back right, and so on). *Measured*: 0x0003 for
stereo and 0x003f for 5.1, matching `nChannels` in both cases. A zero mask
means fall back to `nChannels` with an unknown layout.

**The LFE index** is derived from the mask and is the one place the mask
changes decoding rather than labelling. If mask bit 3 is set, the LFE channel
is at index `popcount(mask & 0xF) - 1`, which is 3 for a 5.1 mask. If bit 3 is
clear there is no LFE channel. Section 12.3 says what the index is for, and
also why it is currently inert.

### 1.1 The decode-flags word

One 16-bit word at extra offset 14, and every bit of it that matters:

| bits | meaning |
|---|---|
| 0 | no effect |
| 1-2 | frame-length adjustment, section 2 |
| 3-5 | subframe depth: `maxSubframes = 2^((flags >> 3) & 7)`; more than 32 is a refusal |
| 6 | each frame opens with its own length in bits (section 7.3) |
| 7 | each frame carries an 8-bit dynamic-range gain |
| 8-15 | no effect |

*Measured*: only two values occur anywhere in the envelope.

| flags | bits 1-2 | maxSubframes | length prefix | DRC gain | where |
|---|---|---|---|---|---|
| 0x00e0 | 0 | 16 | yes | yes | every format at 64 kbps and above, 183 of 189 rows |
| 0x0060 | 0 | 16 | yes | no | the three 32 and 48 kbps stereo formats, 6 rows |

So in practice: the frame length is never adjusted, there are always sixteen
possible subframes per frame, every frame is always length-prefixed, and the
DRC gain is present except at the two lowest bit rates. Bit 8 and above are
unassigned in this codec and both observed words leave them clear.

The DRC gain, when present, is one byte per frame, read and discarded.
*Measured*: 0 on every frame of every file that carries it. A decoder reads it
to stay in sync and must not apply it; there is no gain curve here to apply it
with.

### 1.2 The low-bit-rate selector at extra offset 16, and why it is a refusal

The 16-bit word at extra offset 16 selects a coding tool that the reference
does not implement. **The reference never reads this word**; for this codec it
reads extra offsets 0, 2 and 14 and nothing else, so a stream that sets it is
decoded as if it were not set, silently, with no warning and no error.

*Measured*, over the encoder's 189 enumerated formats, the word takes exactly
three values:

| value | rows | which formats |
|---|---|---|
| 0x0000 | 168 | every format at 128 kbps and above, at every rate, depth and channel count |
| 0xc042 | 19 | 44100 and 48000 stereo at 48, 64, 80 and 96 kbps, plus the three lowest VBR quality levels at those rates |
| 0x20c6 | 2 | 32000 stereo 16-bit at 32 kbps, the only 32 kHz format the codec offers |

The boundary is a bit rate, not a rate or a channel count: **every stereo
format at or below 96 kbps carries a non-zero word and every format above it
carries zero.** Nothing at 6 or 8 channels carries a non-zero word, because the
codec offers no multichannel format below 128 kbps.

What the tool is, as far as the bitstream shows:

- Every subframe carries an optional variable-length payload immediately after
  the subframe's first bit (section 10.1). The reference reads its length and
  skips it.
- *Measured*: that payload is present **only** on the two files whose word at
  offset 16 is non-zero, and on those it is present on about half the
  subframes: 26 of 47 subframes on the 32 kHz cell, carrying 7848 bits, and 45
  of 61 on the 44.1 kHz 64 kbps cell, carrying 10030 bits. On the eleven files
  whose word is zero it never appears, not once in 622 subframes.
- *Measured*: on those same two files the coded spectrum stops early. The
  highest non-zero transform coefficient over the whole file sits at 50.0% of
  the subframe length on the 32 kHz cell and 52.3% on the 64 kbps cell, against
  72.7% to 100% on every other cell. At 32 kHz and 2048 coefficients that puts
  the coded top at 8 kHz with the Nyquist at 16 kHz.

So the payload is the parameter set for a tool that reconstructs the top half
of the spectrum from the coded bottom half, and a decoder that skips it
produces a low-passed and otherwise differently reconstructed signal. That is
what the reference does.

The independent check confirms it and goes further. Comparing the reference's
decode against **Windows' own decoder** on the same bytes, relative RMS error
is 6.2e-06 on a cell whose word is zero, 0.61 on the 64 kbps cell (0xc042) and
0.945 on the 32 kHz cell (0x20c6), and on the 32 kHz cell the disagreement is
not confined to the missing top: below 6 kHz the reference is still 91% wrong.
The split across ten cells is perfect, zero word to agreement and non-zero word
to disagreement.

**The refusal predicate is: the 16-bit word at extra offset 16 is non-zero.**

Use exactly that test. Justification, and the two ways it could be wrong:

- It does not over-refuse anything reachable. Every non-zero value the only
  encoder can write is a value the reference gets wrong, so there is no
  known-safe non-zero value to let through.
- A narrower predicate exists but has no evidence behind it. The two observed
  values share bits 1 and 6 (`word & 0x0042` is 0x42 in both), and one could
  guess that those are the enabling bits and the rest are parameters:
  0x20c6 also sets bits 2, 7 and 13, and 0xc042 also sets bits 14 and 15.
  **This decomposition is an inference and is not settled.** The reference
  gives no evidence either way because it never reads the word, and no file in
  reach carries a third value. Do not implement `word & 0x0042`; implement
  "non-zero" and record the narrower candidate as an open question.
- A wider predicate is not needed. The subframe payload flag could be used as a
  mid-stream refusal instead, and doing so as a belt-and-braces check is
  reasonable, but it fires one subframe late and the extra-bytes test catches
  the same streams before a bit is read.

Name the tool by what it does rather than by a product name. Windows calls the
codec "Windows Media Audio 10 Professional" and this is its low-bit-rate mode;
nothing in this note depends on that name being right.

### 1.3 What a decoder must refuse before reading a bit

- Fewer than 18 extra bytes.
- A bit depth other than 16 or 24.
- More than 8 channels, or zero channels.
- A subframe-depth field above 5, that is `maxSubframes` above 32.
- `samplesPerFrame / maxSubframes` below 64.
- A frame length above 8192 samples (section 2).
- `nBlockAlign` zero, or so large that `floorLog2(nBlockAlign) + 4` exceeds 25.
- A non-zero word at extra offset 16 (section 1.2).

## 2. Frame length

Every frame in a stream holds the same number of samples per channel. The rule
takes the sample rate and decode-flags bits 1-2, and it is the rule
`wma-bitstream.md` section 2 declines to finish:

```
frameLenBits = 9      rate <= 16000
               10     rate <= 22050
               11     rate <= 48000
               12     rate <= 96000
               13     otherwise

adjust = (flags >> 1) & 3
frameLenBits += 0     adjust == 0
                +1    adjust == 1
                -1    adjust == 2
                -2    adjust == 3

samplesPerFrame = 1 << frameLenBits
```

Three things to get right. The `rate <= 32000` arm that appears in the v1/v2
rule belongs to **version 1 only** and does not exist here; at 32 kHz this
codec uses 2048-sample frames. The adjustment applies **only** to this codec,
not to v1 or v2. And the adjustment is applied after the rate arm, so it can
push the result out of the legal range: a result above 13 is a refusal by name
(section 15), and a result below 6 cannot happen because the smallest rate arm
is 9 and the largest downward adjustment is 2.

For the whole envelope, with `adjust == 0` on every format the encoder offers:

| rate | frameLenBits | samplesPerFrame | minSubframeLen at 16 subframes |
|---|---|---|---|
| 32000 | 11 | 2048 | 128 |
| 44100 | 11 | 2048 | 128 |
| 48000 | 11 | 2048 | 128 |
| 88200 | 12 | 4096 | 256 |
| 96000 | 12 | 4096 | 256 |

*Measured* on all thirteen files: 2048 at 32, 44.1 and 48 kHz, 4096 at 88.2 and
96 kHz.

The cheapest way to confirm a frame-length implementation without decoding a
sample is not available here, unlike in v1/v2: `nBlockAlign` has no arithmetic
relation to the frame length in this codec (*measured*: `nBlockAlign` is a
container packet size chosen from the bit rate alone, and the same
`nBlockAlign` of 16384 appears at 48 kHz with 2048-sample frames and at 96 kHz
with 4096-sample frames).

## 3. Subframe sizes and the derived per-size tables

A frame is split into subframes. Every subframe length is
`samplesPerFrame >> k` for some `k` in `0 .. log2(maxSubframes)`, so with the
measured `maxSubframes` of 16 there are five possible sizes. Call `k` the
**size index**; it is `log2(samplesPerFrame / subframeLen)` and it indexes
every per-size table below.

Three tables are derived once at configuration time, from the sample rate and
the frame length. All three are integer arithmetic and must come out exactly.

### 3.1 Scale factor band edges

The 28 band edge frequencies in `bandEdgeFreqs` (section 17) are the classic
critical-band ladder, 100 Hz to 63875 Hz. For each size index `k`, with
`L = samplesPerFrame >> k`:

```
edges[0] = 0
n = 1
for f in bandEdgeFreqs, in order:          // 28 entries
    if edges[n-1] >= L: stop
    e = (trunc(L * 2 * f / rate) + 2) & ~3
    if e > edges[n-1]:
        edges[n] = e
        n = n + 1
    if e >= L: stop
edges[n-1] = L                              // overwrite the last edge written
numBands[k] = n - 1
```

Read the last two lines carefully. The final assignment **overwrites** the last
edge the loop wrote rather than appending, so the band whose edge the loop
placed at or past `L` is absorbed into the top band. The result is
`numBands[k]` bands with edges `edges[0..numBands[k]]`, `edges[0] = 0` and
`edges[numBands[k]] = L`. `numBands[k]` is at most 28 and a computed value of
zero or less is a refusal.

The multiplication `L * 2 * f` reaches about 4096 * 2 * 63875, which needs more
than 32 bits only if `L` exceeds 8192; it does not. The division truncates.
The `& ~3` makes every edge a multiple of 4, which is why the coefficient
vector books can work four at a time without straddling a band.

*Measured* band counts, which an implementation should assert on:

| rate | samplesPerFrame | bands at each size index k = 0..4 |
|---|---|---|
| 32000 | 2048 | 25, 25, 24, 20, 15 |
| 44100 | 2048 | 26, 26, 23, 19, 14 |
| 48000 | 2048 | 26, 26, 23, 18, 14 |
| 88200 | 4096 | 28, 28, 25, 21, 16 |
| 96000 | 4096 | 28, 28, 25, 20, 16 |

For a spot check on the arithmetic, the full-frame edges at 48 kHz are
0, 8, 16, 24, 36, 44, 52, 64, 80, 92, 108, 128, 148, 172, 196, 232, 268, 316,
376, 452, 548, 656, 812, 1024, 1324, 1764, 2048.

### 3.2 The scale factor resample map

Scale factors transmitted for one subframe size are reused at another, and the
map that says which band of the old layout feeds which band of the new one is
derived, not transmitted. For every pair of size indices, target `k` and source
`j`, and every band `b` of the target:

```
mid = ((edges[k][b] + edges[k][b+1] - 1) << k) >> 1
v = 0
while (edges[j][v+1] << j) < mid: v = v + 1
map[k][j][b] = v
```

The `<< k` and `<< j` lift both layouts onto the common frequency grid of a
full-frame subframe, so a band index at one size is comparable with one at
another. `mid` is the midpoint of the target band on that grid, computed as a
sum minus one and halved, which biases it down half a bin; that bias is part of
the definition and changing it moves band assignments.

### 3.3 The subwoofer cutoff

One value per size index, with `L = samplesPerFrame >> k`:

```
cutoff[k] = clip( trunc( (440*L + 3*(rate >> 1) - 1) / rate ), 4, L )
```

*Measured* at 48 kHz and 2048-sample frames: 20, 10, 6, 4, 4. Section 12.3 says
what it is for and why a decoder should compute it anyway.

## 4. Packet geometry

The coded stream is a sequence of **packets**, each exactly `nBlockAlign`
bytes, delivered by the container as one media object per packet. Frames do not
align with packets: a frame may start in one packet and finish in the next, and
a packet commonly holds several whole frames plus the head of another.
*Measured*: 1 to 10 whole frames start in a single packet across the corpus.

Two derived widths, both from `nBlockAlign` alone:

```
frameSizeBits = floorLog2(nBlockAlign) + 4
```

That single width is used twice: for the packet header's continuation count and
for every frame's length prefix. *Measured* values across the envelope run from
13 (`nBlockAlign` 768) to 19 (32768).

### 4.1 The packet header

Every packet opens with, in order:

| width | field |
|---|---|
| 4 | sequence number, increments modulo 16 |
| 2 | skipped, **not** reserved-zero |
| `frameSizeBits` | continuation count, in bits |

*Measured*: the sequence number increments by exactly 1 modulo 16 on every
packet of every file. The two skipped bits take the value 2 on almost every
packet and 0 on at least one, so a decoder that validates them as zero will
reject real files. Skip them.

The **continuation count** says how many bits at the start of this packet's
payload belong to the frame that was in progress at the end of the previous
packet. Its three shapes:

| value | meaning |
|---|---|
| 0 | no frame is continued; **discard whatever was carried over, unread** |
| 0 < n < remaining payload bits | the first n bits complete the carried frame |
| n >= remaining payload bits | the whole remaining payload belongs to the carried frame |

The zero case is not theoretical and a decoder that treats the bit stream as a
simple concatenation will desynchronise on real files. *Measured*: a
continuation count of zero arrives with a non-empty carry **16 times across 10
of the 13 files**, discarding as much as 23583 bits (2.9 KB) of packet tail
that the encoder padded and abandoned. Honour it: drop the carry and start
fresh at this packet's first frame.

The saturating case is rare and appears at end of stream. *Measured*: once,
in one file, where the final packet declares a continuation count of 65536 for
a payload of 65513 bits. Clamp to the payload and carry on. The carried frame
in that instance was complete within the clamped bits and decoded normally;
*measured* over every other continuation in every file, the carried frame's
own length prefix is **exactly** equal to the carry plus the continuation
count, so the field is redundant with the length prefix on a clean stream and
exists so a decoder can resynchronise without one.

Reference choice worth knowing: the reference attempts to decode the carried
frame even in the saturating case, when the frame may still be incomplete, and
relies on its bounded bit reader returning zeros past the end. Do not copy
that. Decode the carried frame when the accumulated bits reach its declared
length, and otherwise keep accumulating.

### 4.2 Reading frames out of a packet

After the header and the continuation bits, a packet holds whole frames back to
back. Take another frame while all three hold:

1. more than `frameSizeBits` bits remain in the packet, and
2. the next `frameSizeBits` bits, read as the frame's length prefix, are
   non-zero, and
3. that declared length is no greater than the bits remaining.

and stop as soon as a frame's trailer bit (section 7.3) says no more frames
follow in this packet. Whatever remains after the last frame taken is the carry
into the next packet, subject to that packet's continuation count.

*Measured*: the "declared length does not fit" stop fires 50 times across the
corpus, so it is the ordinary way a packet ends, not an error path. The
trailer bit and the fit test are both load-bearing and neither alone is enough.

### 4.3 Sequence discontinuity

A sequence number that is not one more than the previous one modulo 16 means
packets were lost or the stream was joined mid-file. The recovery is exactly
the seek path: **discard the carry, skip this packet's continuation bits
unread, and resume at the first frame that starts in this packet**, then
discard one frame of output (section 14.2). Do not refuse; do not attempt to
decode the partial frame.

*Measured*: no discontinuity occurs in any corpus file, so the recovery path
is exercised only by the resumed-decode measurement of section 5, which is
the same code path.

## 5. What a decode may start at, and what it costs

*Measured, and this is the fact the container's seek behaviour rests on*: a
decode resumed at **any** packet, discarding the carry and starting at the
first frame that begins in that packet, produces output identical to a linear
decode from the start of the file, for every frame after the first, with a
maximum absolute difference of **exactly zero**. This was checked at all 65
packet boundaries of all thirteen files, comparing frame by frame against the
linear decode of the same file.

The first frame after a resume is wrong, by up to 1.6 in absolute value on a
full scale of 1.0, because it overlap-adds against a transform tail the decoder
does not have. One frame is exactly enough: the second frame overlaps against
the first frame's own tail, which is correct regardless of history.

Why one frame suffices, structurally: the only state that crosses a frame
boundary is the overlap-add tail and the previous subframe length that selects
the window. Scale factor reuse is reset at the start of every frame (section
10), quantisation is per subframe, and channel transforms are per subframe.

So: **every media object in the container is a decode start point, the lead-in
is exactly one frame, and the container does not need a key-frame flag to seek
correctly.** The corpus files mark every packet as a key frame, which is
consistent, but a decoder must not depend on the flag.

## 6. Subframe layout

The frame's samples are divided into subframes independently per channel, but
the divisions are coded jointly so that channels sharing a division share its
bits.

### 6.1 The subframe length field

One field per step of the tiling walk, giving a right-shift applied to the
frame length:

```
if frontier == samplesPerFrame - minSubframeLen:
    length = minSubframeLen                     // no bits read
else if maxSubframes is 4 or 16:
    read 1 bit
    if 0: shift = 0
    else: shift = 1 + read(shiftFieldWidth - 1)
else:
    shift = read(shiftFieldWidth)

length = samplesPerFrame >> shift
```

where

```
shiftFieldWidth = floorLog2( log2(maxSubframes) ) + 1
```

and `log2(maxSubframes)` is the raw 3-bit field from the decode flags, so
`floorLog2(0) = 0` gives width 1 in the `maxSubframes == 1` case, which is
never reached because the first shortcut always fires there.

| maxSubframes | shiftFieldWidth | encoding | reachable shifts |
|---|---|---|---|
| 1 | 1 | never read | 0 |
| 2 | 1 | raw | 0..1 |
| 4 | 2 | 1-bit prefix | 0..2 |
| 8 | 2 | raw | 0..3 |
| 16 | 3 | 1-bit prefix | 0..4 |
| 32 | 3 | raw | 0..7, of which 6 and 7 are illegal |

At `maxSubframes == 32` the raw field can express a shift of 6 or 7, which
would give a subframe shorter than the minimum. Refuse: a length below
`minSubframeLen` or above `samplesPerFrame` is a broken frame.

*Measured*: only the `maxSubframes == 16` row occurs, and subframe lengths of
128, 256, 512, 1024, 2048 at 2048-sample frames and 256 through 4096 at
4096-sample frames all appear.

### 6.2 The tiling walk

Think of each channel filling its frame left to right. At any moment there is a
**frontier**: the smallest number of samples any channel has placed. Each step
of the walk codes one subframe length, which is given to every channel that
sits at the frontier and elects to take it. The step's length is coded once, so
channels that agree on a division pay for it once.

```
placed[c] = 0 for every channel
atFrontier = channelCount
frontier = 0
uniform = (maxSubframes == 1) ? 1 : read(1)

repeat
    for each channel c in ascending index:
        if placed[c] == frontier:
            if uniform or atFrontier == 1 or frontier == samplesPerFrame - minSubframeLen:
                takes[c] = 1                      // no bit read
            else:
                takes[c] = read(1)
        else:
            takes[c] = 0

    L = subframe length field (6.1), decoded with this step's frontier
    frontier = frontier + L

    for each channel c in ascending index:
        if takes[c]:
            append L to c's subframe list
            placed[c] = placed[c] + L
            refuse if placed[c] > samplesPerFrame
        else if placed[c] <= frontier:
            if placed[c] < frontier:
                atFrontier = 0
                frontier = placed[c]
            atFrontier = atFrontier + 1
until frontier >= samplesPerFrame
```

Four details that a paraphrase would lose, all of them load-bearing because
they decide whether a presence bit is read:

- `uniform` is one bit at the head of the tiling and, when set, means every
  channel takes every step. *Measured*: set on every stereo file and both
  values occur on the multichannel files, so a real 5.1 file does tile its
  channels differently from each other.
- The presence bit is skipped when only one channel is at the frontier,
  because that channel must take the step.
- The presence bit is skipped at the last possible frontier, because only one
  length is possible there and every channel at the frontier must take it.
- `atFrontier` counts only channels that did **not** take the step, and it is
  reset only when the frontier moves backwards. When no non-taker sits at or
  below the new frontier it keeps the value it had at the previous step. That
  carry-over is real and a decoder that recomputes the count from scratch each
  step will read the wrong number of bits.

After the walk, each channel's subframe offsets are the running sums of its
own lengths, and each channel's lengths sum to `samplesPerFrame`. A channel
with more than 32 subframes is a broken frame.

*Measured*: 1, 2, 5, 6 and 8 subframes per channel per frame all occur; a
frame of one full-length subframe is the commonest shape and a frame of eight
is the next.

### 6.3 Which channels share a subframe

Subframes are decoded in a fixed order derived from the tiling, and every
subframe of every channel is visited exactly once. At each step:

1. Let `offset` be the smallest sample count any channel has already decoded in
   this frame, and let `L` be the next subframe length of the **lowest-indexed**
   channel achieving that minimum. Ties on `offset` with a different `L` do not
   change `L`.
2. The **participating channels** are those whose decoded count equals `offset`
   **and** whose next subframe length equals `L`, in ascending channel index.
3. The frame is complete when, after crediting the participating channels with
   `L` samples each, every channel has `samplesPerFrame` samples.

So channels with the same division share a subframe and are coded together;
channels with a different division at the same offset get their own subframe at
a later step. *Measured*: on the 5.1 files, subframes with 1, 5 and 6
participating channels all occur.

## 7. Frame layout

### 7.1 The frame header

In order:

| present when | width | field |
|---|---|---|
| decode-flags bit 6 | `frameSizeBits` | frame length in bits, including this field and the trailer bit |
| always | variable | the tiling of section 6.2 |
| channelCount > 1 | 1 | post-processing transform present |
| the bit above is set | 1 | matrix present |
| the bit above is set | `4 * channelCount^2` | skipped |
| decode-flags bit 7 | 8 | dynamic range gain, read and discarded |
| always | 1 | trim fields present |
| the bit above is set | 1 | start trim present |
| the bit above is set | `floorLog2(2 * samplesPerFrame)` | start trim, in samples |
| the trim-present bit | 1 | end trim present |
| the bit above is set | `floorLog2(2 * samplesPerFrame)` | end trim, in samples |

The trim field width is `frameLenBits + 1`, so 12 bits at 2048-sample frames
and 13 at 4096.

When the trim-present bit is clear, both trims are zero for this frame. When
it is set but an individual trim's own bit is clear, **that trim keeps whatever
value it had**, which in a decoder that clears the trims after applying them
means zero. Clear them after use.

*Measured*: the post-processing transform bit is **clear on every frame of
every file**, so the skipped matrix is entirely untested. Read the bits, skip
what they say to skip, and do not refuse; but record it as a deficit, because a
stream that sets it will silently take a path nothing here can check.

### 7.2 Subframes

The frame body is the sequence of subframes described in section 6.3, back to
back with no alignment and no separator, until every channel has
`samplesPerFrame` samples.

### 7.3 The frame tail and the trailer bit

After the last subframe, a frame ends with one padding bit and then a **trailer
bit**: 1 means another frame follows in this packet, 0 means this packet's
frames are finished.

With the length prefix present, the frame occupies exactly `len` bits counting
the prefix itself and the trailer bit, so the trailer bit is at bit `len - 1`
from the frame's start. Seek there rather than assuming the padding is one bit.

*Measured, on every frame of every one of the thirteen files*: the bits actually
consumed by the header and subframes are exactly `len - 2`, so there is exactly
one padding bit and then the trailer bit, with no variable padding anywhere.
The reference treats any other gap as a fatal error and refuses the frame;
that strictness is a **reference choice**, and its own comment says it is not
sure the condition is always an error. A decoder should seek to `len - 1` and
read the trailer bit there, and may warn rather than refuse on a wider gap.

With no length prefix (never seen in this envelope) the frame ends at the first
1 bit found while scanning forward, which is the trailer bit; the format's
padding in that case is a run of zero bits.

## 8. The subframe header

For the current subframe, with participating channels from section 6.3, length
`L`, size index `k`, band count `numBands[k]` and band edges from section 3.1:

| width | field |
|---|---|
| 1 | extended payload present |
| variable | the extended payload, section 8.1 |
| 1 | **must be 0**, refuse otherwise |
| variable | the channel group section, present when the stream has more than one channel, section 9 |
| 1 per participating channel | this channel transmits coefficients |
| variable | the quantisation section, present when any channel transmits, section 12 |
| variable | the coefficient blocks, one per transmitting channel, section 11 |

Derived for the subframe and needed below:

```
escapeWidth = floorLog2(L - 1) + 1
```

so 11 bits at `L = 2048` and 7 at `L = 128`.

### 8.1 The extended payload

When the leading bit is set:

```
n = read(2)
if n == 0:
    w = read(4)
    n = (w == 0 ? 0 : read(w)) + 1
skip n bits
```

so `n` is 1, 2 or 3 directly, or an escape giving 1 to 32768. A payload that
runs past the end of the frame is a broken frame.

`w == 0` is the one place `floorLog2(0)`-style zero-width reading matters in
this codec: read no bits, take the value 0, and the payload is one bit long.

This is the carrier for the low-bit-rate tool of section 1.2. *Measured*: the
bit is set only on streams whose extra-bytes word at offset 16 is non-zero, and
never on any other stream. Since such streams are refused at configuration
time, a decoder that reaches this field with the bit set has a stream it should
not have accepted; treat a set bit as a second line of defence and refuse by
name rather than skipping.

## 9. Channel groups and decorrelation

Present whenever the **stream** has more than one channel, even when only one
channel participates in this subframe.

The section opens with one bit that **must be 0**. The reference refuses a set
bit as an unknown channel transform and so should this decoder. *Measured*:
clear on every subframe of every file.

Then groups are formed, repeatedly, while ungrouped participating channels
remain and the group count is below the participating channel count.

### 9.1 Forming a group

- If **more than two** participating channels are still ungrouped: read one bit
  per still-ungrouped participating channel, in ascending channel index; a set
  bit puts that channel in this group. A group of zero channels is possible
  and harmless.
- Otherwise (two or fewer ungrouped): every still-ungrouped participating
  channel joins this group and **no bits are read**.

*Measured*: groups of 1 through 6 channels all occur on the 5.1 files.

### 9.2 The transform for a group of exactly two

```
read 1 bit
  0 -> transform on, with the fixed matrix below
  1 -> read 1 more bit
         0 -> no transform for this group
         1 -> refuse, unknown transform type
```

The fixed matrix depends on the **stream's** channel count, not the group's:

| stream channels | matrix, rows are output channels |
|---|---|
| exactly 2 | `[ 1, -1 ; 1, 1 ]` |
| more than 2 | `[ c, -c ; c, c ]` with `c = 0.70703125` |

`0.70703125` is `181/256` exactly, a truncated `cos(pi/4)`, and the compensation
constant of section 9.5 is `181/128`. Use the exact rationals, not a computed
cosine.

*Measured*: the stereo form occurs 382 times, the scaled form 13 times, and the
explicit "no transform" answer 99 times.

### 9.3 The transform for a group of more than two

```
read 1 bit
  0 -> no transform for this group
  1 -> transform on, read 1 more bit
         1 -> an explicit rotation matrix follows, section 9.4
         0 -> the built-in matrix for this group size
```

The built-in matrices are a single concatenated table, one square matrix per
group size from 1 to 6, at offsets 0, 1, 5, 14, 30 and 55 into it, 91 floats in
all. Sizes 7 and 8 have **no** built-in matrix. The reference logs a complaint
and leaves whatever matrix the group last held, which is undefined output;
a decoder must **refuse** a built-in matrix request for a group of more than
six channels.

*Measured*: the built-in matrices are **never selected** in any file, at any
group size. They are in the generated tables and completely untested.

A group of one channel, or of zero, reads nothing here and has no transform.

### 9.4 The explicit rotation matrix

For a group of `n` channels:

```
for i in 0 .. n*(n-1)/2 - 1:  angle[i] = read(6)
for i in 0 .. n-1:            M[i][i] = read(1) ? +1.0 : -1.0     // all other entries 0

p = 0
for i in 1 .. n-1:
    for x in 0 .. i-1:
        a = angle[p + x]
        if a < 32:  s = sinTable[a];       c = sinTable[32 - a]
        else:       s = sinTable[64 - a];  c = -sinTable[a - 32]
        for y in 0 .. i:
            v1 = M[x][y]
            v2 = M[i][y]
            M[x][y] = v1*s - v2*c
            M[i][y] = v1*c + v2*s
    p = p + i
```

`sinTable[m] = sin(m * pi / 64)` for `m` in 0..32, 33 entries. The two arms
extend it to a quarter turn: `a` in 0..31 gives a sine and cosine both taken
from the first quadrant, `a` in 32..63 gives a positive sine and a negative
cosine. `a == 32` falls in the second arm and gives `s = sinTable[32] = 1`,
`c = -sinTable[0] = 0`.

The inner loop over `y` runs to `i` inclusive, one past `i-1`, and the angle is
constant across it: the angle index depends on `x` only. Both the row order and
the fact that `M[x][y]` is overwritten before `M[i][y]` reads it matter, so
read `v1` and `v2` first.

*Measured*: an explicit matrix is transmitted 24 times across 4 files, so this
path is genuinely exercised.

### 9.5 Enabling the transform per band

Any group whose transform is on then reads:

```
read 1 bit
  1 -> every band uses the transform
  0 -> read 1 bit per band, numBands[k] bits in ascending band order
```

*Measured*: per-band bits occur, 25 times in one file, so the path is
exercised, but on the committed corpus every transform-on group enables every
band. The implementation session should keep a fixture that reaches the
per-band form.

### 9.6 Applying the transform

For each group whose transform is on, for each band `b` of the subframe, with
`lo = edges[k][b]` and `hi = min(edges[k][b+1], L)`:

- **If the band is enabled**, for each coefficient index `y` in `[lo, hi)`,
  gather the group's channels' coefficients at `y` into a vector `d` in group
  order, then write back `out[m] = sum over j of d[j] * M[m][j]` for each `m`.
  The matrix is row-major with the output channel as the row. All channels are
  updated from the gathered vector, so gather first and write second.
- **If the band is not enabled and the stream has exactly two channels**,
  multiply both of the group's channels' coefficients in `[lo, hi)` by
  `181/128`. This compensates the unnormalised stereo matrix of section 9.2 so
  that enabled and disabled bands come out at the same level. It applies only
  when the stream is stereo; a two-channel group inside a wider stream uses the
  already-scaled matrix and needs no compensation.
- **If the band is not enabled and the stream has more than two channels**,
  nothing happens.

This runs after all channels' coefficients are decoded and **before**
dequantisation.

## 10. Scale factors

Scale factors are per band, per channel, and are integers. They are read in
the quantisation section (section 12), after the quantisation step and before
any coefficients, once per participating channel in ascending channel index.

Each channel carries three pieces of state that live for the duration of one
frame and are reset at the start of every frame:

- **saved** -- the last set of scale factors this channel actually transmitted
  in this frame, together with the size index they were transmitted at.
- **reuse** -- false until the channel has transmitted once in this frame.
- **step** -- the scale factor resolution, set only on a first transmission.

Reset at the start of every frame means: **scale factors never cross a frame
boundary**, which is exactly why one frame of lead-in is enough after a seek.

For each participating channel:

```
work[0 .. numBands[k]-1] = 0

if reuse:
    m = map[k][savedSizeIndex][.]                 // section 3.2
    for b in 0 .. numBands[k]-1:
        work[b] = saved[ m[b] ]

transmitted = (this is the channel's first subframe in this frame) or read(1)

if transmitted:
    if not reuse:
        step = read(2) + 1                        // 1, 2, 3 or 4
        v = trunc(45 / step)                      // 45, 22, 15, 11
        for b in 0 .. numBands[k]-1:
            v = v + scaleDelta()                  // Huffman, symbol already offset by -60
            work[b] = v
    else:
        apply run-level differences to work, below
    saved = work
    savedSizeIndex = k
    reuse = true

scaleFactors for this subframe = work
maxScaleFactor = max over the numBands[k] entries of work
```

Two things that are easy to get wrong. **`saved` is updated only on a
transmission**, so a subframe that resamples without transmitting uses the
resampled values but the next resample still starts from the last transmitted
set, at that set's size index. And **the first subframe of a channel in a frame
always transmits**, with no bit read. The condition is the channel's subframe
INDEX within the frame, not `reuse`: a channel whose leading subframes coded
no coefficients reaches a later subframe with `reuse` still false and READS
the bit there all the same, and a clear bit then leaves every band at zero.
(An earlier draft of this paragraph said the bit was skipped "because `reuse`
is false", which conflates the two; the addendum at the end says how that was
found.)

*Measured*: `step` takes 1, 2 and 3; 4 never occurs. A resample without a
transmission occurs 23 times across 4 files. Scale factor values run from 5 to
77 over the corpus, but nothing constrains them to be positive and a decoder
must not assume it.

### 10.1 The DPCM form

The first transmission in a frame codes the bands as a running sum. The
starting value is `trunc(45 / step)` and each band adds one Huffman symbol from
the scale-delta book. **The book's symbols already carry a bias of -60**, so a
symbol as generated is `rawSymbol - 60` and the deltas straddle zero. Section
17 says which generated table this is; the offset must be baked into the table
or applied at lookup, once, not twice.

### 10.2 The run-level difference form

Later transmissions in the same frame code differences against the resampled
values:

```
b = 0
while b < numBands[k]:
    s = scaleRunLevel()                    // Huffman
    if s == 0:                             // escape
        code = read(14)
        val  = code >> 6
        sign = (code & 1) - 1
        skip = (code & 0x3f) >> 1
    else if s == 1:                        // end of differences
        stop
    else:
        skip = scaleRunLevelRuns[s]
        val  = scaleRunLevelLevels[s]
        sign = read(1) - 1
    b = b + skip
    refuse if b >= numBands[k]
    work[b] = work[b] + ((val ^ sign) - sign)
    b = b + 1
```

`sign` is 0 for a set bit and -1 for a clear one, and `(val ^ sign) - sign` is
`+val` or `-val`. Note the polarity: **a set sign bit means positive.** The
same polarity governs every sign bit in this codec (section 11.4).

The 14-bit escape packs three fields into one word: the level in the top 8
bits, a 5-bit run in bits 1..5, and the sign in bit 0. *Measured*: the escape
fires often, 930 times across the corpus, so it is not a corner.

After the loop, `b` may fall short of `numBands[k]`; the remaining bands keep
their resampled values. A `skip` that pushes `b` to or past `numBands[k]` is a
broken frame.

## 11. Coefficient coding

One block per participating channel that set its transmit bit, in ascending
channel index, decoded after the whole quantisation section. A channel that did
not set its bit has all `L` coefficients zero and reads nothing.

The block has two phases: a vector phase that codes four coefficients at a
time from the low end up, and a run-level tail that codes sparse coefficients
above it. The switch between them is not a flag; it is a rule.

```
book = read(1)                             // selects which run-level book pair the tail uses
vecLimit = L                               // unless explicitly transmitted, section 12.1
cur = 0 ; zeros = 0 ; runLevelMode = false
zeroThreshold = L >> 8

while (explicitVecLimit or not runLevelMode) and cur + 3 < vecLimit:
    decode four magnitudes, section 11.1
    for each magnitude m, in order:
        if m != 0:
            sign = read(1)                 // 1 = positive
            coef[cur] = sign ? +m : -m
            zeros = 0
        else:
            coef[cur] = 0
            zeros = zeros + 1
            if zeros > zeroThreshold: runLevelMode = true
        cur = cur + 1

if cur < L:
    zero coef[cur .. L-1]
    run-level tail from index cur, section 11.3
```

Three details:

- `runLevelMode` is tested only at the top of the loop, so the group of four
  that trips it is always completed.
- `zeroThreshold` is `L >> 8`, not `L / 128`. At `L = 2048` that is 8, so nine
  zeros in a row switch to the tail; at any `L` below 256 it is 0, so a single
  zero switches.
- When an explicit vector limit was transmitted, the zero-run rule is disabled
  entirely and the vector phase runs to the limit.

*Measured*: the tail is reached on 1535 of the corpus's coefficient blocks and
skipped on 130 (the blocks where the vector phase covers the whole subframe).

### 11.1 The four-magnitude vector

```
s4 = vec4()                                // symbol already offset by -1
if s4 >= 0:
    magnitudes = the four nibbles of s4, most significant first
else:                                      // escape
    for each half, in order:
        s2 = vec2()                        // symbol already offset by -1
        if s2 >= 0:
            magnitudes = (s2 >> 4, s2 & 0xF)
        else:                              // escape
            for each of the two:
                v = vec1()                 // no offset
                if v == 100:               // the book's last symbol
                    v = v + largeValue()
                magnitude = v
```

The four-element book has 127 entries and its symbols are 16-bit words offset
by -1, so the entry whose raw symbol is 0 becomes -1 and is the escape; every
other entry's four nibbles are four magnitudes in 0..15, **high nibble first**.
The two-element book has 137 entries with the same -1 offset and the same
escape convention, and its non-escape symbols hold two magnitudes, **high
nibble first**. The single book has 101 entries, no offset, and its last symbol
(100) means "100 plus an escape".

*Measured*: the four-element escape fires 194809 times and the two-element
escape 99339 times across the corpus, so both are hot paths; the single
book's large-value escape fires 1747 times.

### 11.2 The large-value escape

Used by the single vector book and by the run-level escape:

```
n = 8
if read(1): n = n + 8
              if read(1): n = n + 8
                            if read(1): n = n + 7
return read(n)
```

so widths of 8, 16, 24 or 31 bits, unsigned. Note the last step adds 7, not 8.

### 11.3 The run-level tail

```
i = cur
mask = L - 1
while i < L:
    s = coefBook[book]()
    if s > 1:
        i = i + coefRuns[s]
        sign = read(1)
        coef[i & mask] = sign ? +coefLevels[s] : -coefLevels[s]
    else if s == 1:
        stop                                // end of block
    else:
        level = largeValue()
        if read(1):
            if read(1):
                if read(1): refuse, broken escape
                else: i = i + read(escapeWidth) + 4
            else: i = i + read(2) + 1
        // a clear first bit means a run of 0
        sign = read(1)
        coef[i & mask] = sign ? +level : -level
    i = i + 1
```

Two run-level books exist and the 1-bit selector at the head of the block picks
one; both are indexed by the same symbol and each has its own run table and
level table. Symbol 0 is the escape and symbol 1 is end-of-block in both, and
their run and level entries at those two indices are placeholders.

The index is masked with `L - 1` on every write. That masking is a
**reference choice** that turns an overrun into a wrap rather than a crash; the
format's own answer is that an index at or past `L` is a broken frame. The
reference also notices an overrun after the loop and reports it while keeping
the wrapped samples. *Measured*: no overrun occurs in any corpus file. A
decoder may refuse on overrun or may mask and warn, but it must not write out
of bounds.

End of block may be omitted: the loop simply runs out at `i == L`.

### 11.4 Sign polarity, stated once because it is the easiest thing to get wrong

Every sign bit in this codec, in coefficient vectors, in the run-level tail, in
the run-level escape and in the scale factor differences, means **1 is positive
and 0 is negative**. That is the opposite of the usual convention.

Getting it backwards produces a bitstream-valid decode whose every sample is
negated, which reads as a deviation of exactly twice the peak and is otherwise
silent. It was the only structural error in this pass's own decoder and it cost
a full decode to find. Test for it explicitly.

The decorrelation matrix's diagonal sign bit follows the same polarity: a set
bit is `+1.0`.

## 12. Quantisation

Present in a subframe only if at least one participating channel set its
transmit bit. In order:

| width | field |
|---|---|
| 1 | explicit vector coefficient limit, section 12.1 |
| variable | per-channel limits, if the bit above is set |
| 6 | quantisation step delta, signed |
| variable | the step escape, section 12.2 |
| 3 | per-channel modifier width, only when more than one channel participates |
| variable | per-channel modifiers |
| variable | the scale factors of section 10 |

### 12.1 The explicit vector coefficient limit

When the bit is set, each participating channel reads
`floorLog2(trunc((L + 3) / 4)) + 1` bits and the value is shifted left by 2, so
the limit is a multiple of 4 up to `L`. A limit above `L` is a broken frame.
When the bit is clear every channel's limit is `L`.

*Measured*: **never transmitted**, on any subframe of any file. Section 11's
behaviour under an explicit limit (the zero-run rule disabled) is therefore
entirely untested. Implement it from this description and record it as a
deficit.

### 12.2 The quantisation step

```
base  = (90 * bitsPerSample) >> 4          // 90 at 16 bits, 135 at 24
delta = signed(6)                          // -32 .. 31
step  = base + delta

if delta == -32 or delta == 31:
    acc = 0
    loop:
        v = read(5)
        if v == 31: acc = acc + 31; continue
        else: break
    step = step + (delta == 31 ? +(acc + v) : -(acc + v))
```

The escape fires at either extreme of the 6-bit field and extends in 5-bit
chunks, each full chunk of 31 adding 31 and continuing, the first non-full
chunk terminating and adding its own value. The sign of the extension follows
which extreme was hit.

The reference guards the escape loop against running off the end of the frame
and, if it does, silently uses the last value read; that is a **reference
choice** and a decoder should treat exhaustion here as a broken frame.

*Measured*: the escape fires 9 times across 5 files, so it must be
implemented. The final step values run from 1 to 144 over the corpus and the
raw 6-bit delta uses most of its range.

A negative step is legal, produces a gain below 1, and the reference logs it
and continues. Do not refuse it.

### 12.3 Per-channel modifiers

If **exactly one** channel participates, its step is the subframe's step and
nothing is read. Otherwise:

```
modWidth = read(3)
for each participating channel in ascending index:
    stepFor[c] = step
    if read(1):
        stepFor[c] = stepFor[c] + (modWidth ? read(modWidth) + 1 : 1)
```

so the modifier is always at least 1 when its bit is set, and a `modWidth` of
zero means a fixed increment of 1 with no value bits.

*Measured*: `modWidth` takes 0 through 6; a modifier is applied 747 times
across the corpus.

## 13. Dequantisation and the transform

Per participating channel, in ascending channel index, after the channel
transform of section 9.6.

### 13.1 The per-band gain

For each band `b` of the subframe, with `lo = edges[k][b]`,
`hi = min(edges[k][b+1], L)`, this channel's scale factors and step from
section 10:

```
exponent = stepFor[c] - (maxScaleFactor - scaleFactors[b]) * step
gain     = 10 ^ (exponent / 20)
for y in [lo, hi):  spectrum[y] = coef[y] * gain
```

`exponent / 20` is a division of an integer by 20 in **floating point**, not an
integer division; `exponent` can be negative. The exponent is a level in
decibels relative to the loudest band, so the loudest band has
`exponent == stepFor[c]` and quieter bands are attenuated by
`(maxScaleFactor - scaleFactors[b]) * step` decibels.

*Measured*: the exponent runs from -25 to +115 across the corpus, so gains from
about 0.056 to 5.6e5 all occur.

The union of the bands covers `[0, L)` exactly, because the last band edge is
`L`, so every coefficient gets a gain and nothing needs pre-zeroing.

### 13.2 The subwoofer cutoff is inert, and that is a reference bug

The format appears to intend that the LFE channel's spectrum above
`cutoff[k]` (section 3.3) be zeroed. In the reference, the zeroing is written
to the dequantised spectrum **before** the per-band gain loop fills it, so the
gain loop overwrites every zeroed value and the cutoff has no effect whatever.

*Measured*: on the two 5.1 files, the LFE channel never has a non-zero
dequantised coefficient at or above the cutoff, on any subframe, so the bug is
invisible on real material and the two behaviours cannot be told apart here.

What a decoder should do: **compute the cutoff and do not apply it.** Applying
it would diverge from the reference on any stream that does code LFE content up
high, and there is no oracle here to say which answer is right. Record it as a
named deficit with the measurement above, so that a future file which does code
LFE content above the cutoff has somewhere to land.

### 13.3 The inverse transform

Per channel, per subframe, a `L`-point inverse MDCT taking `L` dequantised
coefficients and producing `L` time samples. `L` is the subframe length, the
transform's window is `2L` long, and the output is the non-redundant half.

The definition, stated three ways so that a sign or factor-of-two error cannot
hide:

**As a reversed DCT-IV.** With

```
C[m] = sum over k in 0..L-1 of  X[k] * cos( pi * (2k+1) * (2m+1) / (4L) )
```

the output is

```
h[i] = scale * C[L-1-i]        for i in 0 .. L-1
```

**Explicitly.**

```
h[i]       =  scale * sum_k X[k] * cos( pi*(2k+1)*(2L - 2i - 1) / (4L) )   i in 0 .. L/2-1
h[L/2 + i] = -scale * sum_k X[k] * cos( pi*(2k+1)*(3L + 2i + 1) / (4L) )   i in 0 .. L/2-1
```

**Against the textbook inverse MDCT.** If

```
y[n] = sum_k X[k] * cos( pi/L * (n + 1/2 + L/2) * (k + 1/2) )    n in 0 .. 2L-1
```

is the ordinary `2L`-point inverse MDCT, then

```
h[j] = -scale * y[L/2 + j]      j in 0 .. L-1
```

**Note the minus sign.** The half-output is the *negated* middle `L` samples of
the textbook inverse transform, while the corresponding forward transform is
the textbook forward MDCT with no sign flip. An implementer wiring in an
existing MDCT routine must check this; the overlap-add butterfly of section
13.5 carries a compensating minus sign, so the pair is consistent and only the
pair is. All three forms above were checked numerically against each other in
this pass.

**The scale.**

```
scale = (2 / L) * 2^-(bitsPerSample - 1)
```

with `bitsPerSample` from the extra bytes, so `2/2048 * 2^-15` for a 16-bit
2048-sample subframe. The second factor is what normalises the output to a
nominal full scale of 1.0, and it is why the quantisation base of section 12.2
also scales with the depth: the two nearly cancel, and a 16-bit and a 24-bit
stream of the same material come out at the same level. Getting only one of
the two right produces output about 48 dB wrong, which is not subtle.

### 13.4 The output buffer

Each channel keeps a rolling buffer of `1.5 * samplesPerFrame` samples. A
subframe whose sample offset within the frame is `off` writes its `L` transform
outputs at buffer positions `samplesPerFrame/2 + off` through
`samplesPerFrame/2 + off + L - 1`.

At the end of a frame, the frame's output is buffer positions
`0 .. samplesPerFrame-1`, and then positions
`samplesPerFrame .. 1.5*samplesPerFrame - 1` are copied down to positions
`0 .. samplesPerFrame/2 - 1` for the next frame. Nothing needs zeroing: the
subframes of the next frame cover every position from `samplesPerFrame/2`
upward.

At the start of a stream, and after any seek, the whole buffer is zero.

### 13.5 Windowing and overlap-add

Each channel remembers the length of its previous subframe, initialised to
`samplesPerFrame` at the start of a stream and after a seek. For the current
subframe of length `L`:

```
overlap = min(previousLength, L)
centre  = samplesPerFrame/2 + off          // the subframe's own start in the buffer
region  = buffer[centre - overlap/2 .. centre + overlap/2 - 1]      // `overlap` samples
w[i]    = sin( pi * (i + 0.5) / (2 * overlap) )      for i in 0 .. overlap-1

for r in 0 .. overlap/2 - 1:
    a = region[r]                          // read both before writing either
    b = region[overlap-1-r]
    region[r]             = a * w[overlap-1-r] - b * w[r]
    region[overlap-1-r]   = a * w[r]        + b * w[overlap-1-r]

previousLength = L
```

That is the whole rule, and it handles the window transition between subframes
of different sizes without a special case: the overlap is the shorter of the
two lengths, it is always centred exactly on the boundary between them, and the
window is the sine window of that length. There is no long-start or long-stop
window shape in this codec.

The window array is a **half** window of `overlap` entries, so the full window
it belongs to is `2 * overlap` long. The butterfly uses `w[r]` for the first
half and `w[overlap-1-r]` for the second, which is where the rising and falling
halves come from. The minus sign on the first output line is the one that pairs
with the transform's sign in section 13.3.

Window lengths a decoder must be able to produce are every subframe length from
`minSubframeLen` to `samplesPerFrame`, so 128 through 2048 or 256 through 4096
in this envelope. Compute them; they are a one-line formula and no table is
needed.

*Measured*: overlap lengths of 128, 256, 512, 1024, 2048 and 4096 all occur.

A subframe in which no channel transmits coefficients still runs the window and
overlap-add, with all-zero transform output, and must not be skipped.

## 14. Output

### 14.1 Deinterleaving and depth

The decoder produces one array of floats per channel, in ascending channel
index, which is ascending channel-mask bit order when the mask is non-zero
(section 1). The container is responsible for naming the channels; the codec
only orders them.

The float output is already normalised to a nominal full scale of 1.0 by the
transform's `2^-(bitsPerSample-1)` factor, at both depths. Conversion to
integer PCM is the usual multiply by `2^(depth-1)` and clip to
`[-2^(depth-1), 2^(depth-1) - 1]`, and it is WaxFlow's convention rather than
the format's. Nothing in the codec clips, and *measured* values do exceed 1.0
in absolute value on real files, so the clip is not optional.

### 14.2 Decoder delay and the trim fields

**The first frame produced after any decode start must be discarded**, whether
the start is the beginning of the file, a seek, or a resynchronisation after a
sequence break. It is lead-in: it overlap-adds against an all-zero tail.

*Measured*, on all thirteen files: the first frame of the file carries a start
trim of exactly `samplesPerFrame` and an end trim of 0, which says the same
thing, and no other frame carries a start trim. So the two rules agree at the
start of a file and neither double-counts. After a seek there is no trim to
rely on and the discard rule is the only thing keeping the output correct.

Trims are applied to the frames that are output: a start trim drops that many
samples from the front of its frame and an end trim drops that many from the
back. A start trim equal to or greater than the frame length drops the whole
frame. *Measured*: an end trim appears only on the last frame of a file and
only when the source length is not a multiple of `samplesPerFrame`.

So the output length is

```
samplesPerFrame * (framesDecoded - 1) - (start and end trims on the frames kept)
```

*Measured* on the twelve files whose source length is a multiple of
`samplesPerFrame`: that comes out **exactly equal to the source length**, with
no head trim and no tail overhang, on every one.

One caveat the implementation session's tests must respect. *Measured* on the
one file encoded during this pass with a source of 100000 frames at 2048
samples per frame: the encoder wrote 50 frames and an end trim of 864 on the
last, giving 99488 output samples, which is **512 samples short of the
source**. Both this pass's decoder and the reference agree on 99488, so the
loss is in the encoder or its input stage and not in the decode. Do not assert
that a decode of an arbitrary-length source recovers the source length; assert
it only for the committed corpus, whose lengths are powers of two.

### 14.3 End of stream

There is no flush. The last frame's samples are complete when the frame is
decoded, and the trailing half-buffer that a frame leaves behind is lead-out
for a frame that never arrives. The reference does emit that half-buffer at end
of stream for the Xbox variant and not for this codec; do not emit it.

## 15. Everything the decoder must not try

Each of these is a shape the format allows and this tree cannot verify.
Refuse by name, with the reason, rather than producing plausible wrong audio.

### 15.1 XMA

The Xbox variants reuse this coded layer with a different container, a
different packet header (a frame count instead of a sequence number, plus a
packet-skip field), a fixed set of decode flags and a hardwired 512-sample
frame. They are out of scope. A `wFormatTag` that is not 0x0162 never reaches
this decoder in the first place.

### 15.2 A non-zero word at extra offset 16

Section 1.2. Refuse at configuration time, before reading a bit of bitstream,
because the reference decodes such streams silently wrong in the core band as
well as at the top and there is no oracle for them here.

### 15.3 A set extended-payload bit in a subframe

Section 8.1. This is the same tool as 15.2 seen from inside the bitstream, and
reaching it means the configuration-time test was bypassed. Refuse by name.

### 15.4 The channel transform section's leading bit set

Section 9. The reference calls it an unknown channel transform and refuses;
nothing here can say what it means. *Measured*: clear everywhere.

### 15.5 A two-channel group's second transform bit set

Section 9.2, the `1` then `1` path. An unknown transform type. Refuse.

### 15.6 A built-in decorrelation matrix for a group of more than six channels

Section 9.3. No such matrix exists in the table and the reference leaves the
group's previous matrix in place, which is undefined output. An 8-channel
stream can ask for this. Refuse by name rather than silently reusing a stale
matrix.

### 15.7 A frame length above 8192 samples

Section 2. Reachable as a sample rate above 96000 with the flags'
frame-length adjustment set to +1. Refuse; nothing here can produce or check
it.

### 15.8 The structural refusals

All of these are broken-frame conditions rather than unimplemented features:

- A subframe length below `minSubframeLen` or above `samplesPerFrame`.
- More than 32 subframes on one channel, or a channel whose subframes overrun
  the frame.
- A band count computed as zero or less.
- A reserved subframe bit that is set (section 8).
- A vector coefficient limit above the subframe length.
- A run-level escape whose three run bits are all set.
- A scale factor run that skips past the last band.
- An extended payload longer than the rest of the frame.
- The quantisation step escape running out of bits.

### 15.9 What is **not** on this list

Do not refuse any of these; all are normal:

- A continuation count of zero that discards a pending carry (*measured*, 16
  times).
- A continuation count that exceeds the packet payload (*measured*, once).
- The two skipped bits in the packet header being non-zero (*measured*).
- A negative quantisation step, or a negative band exponent.
- A channel group of zero channels.
- A subframe in which no channel transmits coefficients.
- A run-level block with no end-of-block symbol.
- A frame whose declared length leaves more than one padding bit, which the
  reference treats as fatal and this decoder should not.
- A decode that outruns or falls short of the container's declared duration.

## 16. Implementability checklist

Each item is something the session that writes the decoder must be able to do
with this file, the corpus note and the generated tables alone. The section
that answers it is in brackets.

- [ ] Parse `WAVEFORMATEX` plus 18 extra bytes into depth, channel mask,
      channel count and order, LFE index, `nBlockAlign` and the decode flags;
      deliberately ignore `nAvgBytesPerSec` and `wBitsPerSample`. (1)
- [ ] Refuse a non-zero word at extra offset 16, by that exact predicate, with
      a named error. (1.2, 15.2)
- [ ] Compute `frameLenBits` and `samplesPerFrame` including the flags'
      adjustment, and refuse a result above 13. (2)
- [ ] Compute `frameSizeBits = floorLog2(nBlockAlign) + 4` and use it for both
      the continuation count and every frame's length prefix. (4)
- [ ] Build the band edge table for every subframe size, exactly, and assert
      the measured band counts for at least one rate. (3.1)
- [ ] Build the scale factor resample map and the subwoofer cutoffs. (3.2, 3.3)
- [ ] Parse a packet header, skip the two non-reserved bits, and handle all
      three shapes of the continuation count including the zero case that
      discards a carry and the saturating case at end of stream. (4.1)
- [ ] Carry a bit string, not a byte string, across packets. (4.1)
- [ ] Take frames from a packet under all three stop conditions, and use the
      trailer bit as well as the fit test. (4.2, 7.3)
- [ ] Decode the tiling walk including the uniform bit, both bit-skipping
      shortcuts, and the frontier counter's carry-over across steps. (6.2)
- [ ] Order subframes by frontier and length and pick the participating
      channels correctly, including the tie rule. (6.3)
- [ ] Read the frame header in order, including the post-processing transform
      field that is never set, the DRC byte, and the trim fields. (7.1)
- [ ] Seek to `len - 1` for the trailer bit rather than assuming one padding
      bit, and do not refuse a wider gap. (7.3)
- [ ] Form channel groups, decode all three transform selections, build the
      explicit rotation matrix from the 6-bit angles and the sign diagonal, and
      apply per-band enables. (9)
- [ ] Apply the transform gathered-then-scattered, with the `181/128`
      compensation on disabled bands of a stereo stream only. (9.6)
- [ ] Decode scale factors in both forms, maintain the per-frame reuse state
      and the last-transmitted size index, and resample through the map. (10)
- [ ] Bake the scale-delta book's -60 offset in exactly once. (10.1)
- [ ] Decode coefficient vectors through all three books and both escapes, with
      the `L >> 8` zero-run switch. (11.1, 11)
- [ ] Decode the run-level tail with both books, the three-level run escape and
      the large-value escape. (11.2, 11.3)
- [ ] Get the sign polarity right: **a set bit is positive**, everywhere. Write
      a test that would fail on a globally negated decode. (11.4)
- [ ] Decode the quantisation step including its 5-bit chunked escape at both
      extremes, and the per-channel modifiers. (12.2, 12.3)
- [ ] Apply the per-band gain with the exponent divided by 20 in floating
      point. (13.1)
- [ ] Implement the inverse transform with the right sign and the right scale,
      and check it against all three statements in 13.3 before trusting it.
- [ ] Implement the rolling output buffer and the half-frame carry. (13.4)
- [ ] Implement the window transition as `min(previous, current)` centred on
      the boundary, with the butterfly's minus sign. (13.5)
- [ ] Discard the first frame after **any** decode start, and apply the
      bitstream's trims on top. (14.2)
- [ ] Seek to any packet, discard the carry, skip the continuation bits and
      resume; assert that every frame after the first matches a linear decode
      exactly. (5)
- [ ] Refuse, by name and before reading a bitstream: fewer than 18 extra
      bytes, a depth other than 16 or 24, more than 8 channels, a subframe
      depth above 5, a minimum subframe below 64 samples, an absurd
      `nBlockAlign`, and a non-zero word at offset 16. (1.3)
- [ ] Refuse, by name and mid-stream: the channel transform bit, an unknown
      two-channel transform, a built-in matrix for more than six channels, a
      set reserved subframe bit, a set extended-payload bit, and every
      structural condition in 15.8.
- [ ] Do **not** refuse anything in 15.9.
- [ ] Compare against the oracle with a **tolerance**, not for equality, and
      size it from the 4.2e-07 figure in the float section.

Three items this file cannot settle, each with what to do instead:

1. **Which bits of the offset-16 word actually enable the tool.** Refuse on
   non-zero; record `word & 0x0042` as an unconfirmed narrower candidate.
   Closing this needs either a third observed value or a decoder for the tool.
2. **Whether the subwoofer cutoff should be applied.** Compute it, do not apply
   it, record a named deficit. Closing this needs a file whose LFE channel
   codes content above the cutoff, which no encoder in reach produces.
3. **Every row of the "the format allows" table in section 0 that no file
   reaches**: mono, frame lengths other than 2048 and 4096, subframe depths
   other than 16, unprefixed frames, built-in decorrelation matrices, an
   explicit vector coefficient limit, and the post-processing transform.
   Implement each from the description here and record it as a deficit rather
   than refusing, except where section 15 says otherwise. None of them can be
   tested in this tree.

Anything not on this list that the implementation session finds it needs is a
gap in this file, and closing it means another analysis pass, not a peek at the
reference.

## 17. The tables, and what is handed over

Unlike WMA Lossless, this codec is table-heavy. A sibling agent extracts the
tables from the reference's data header under a `wmaprotablesgen` build tag and
emits four Go files. **Do not transcribe table contents into this note**; what
follows is the shape of each one so the implementation session knows what it is
holding.

| generated file | Go name | shape | what it is |
|---|---|---|---|
| `tables_bands.go` | `bandEdgeFreqs` | 28 uint16 | the critical-band edge frequencies of section 3.1, ascending, 100 to 63875 Hz |
| `tables_scale.go` | `scaleDeltaLens`, `scaleDeltaSyms` | 121 each | the DPCM scale factor book of section 10.1; **symbols carry a -60 bias** |
| `tables_scale.go` | `scaleRunLevelLens`, `scaleRunLevelSyms` | 120 each | the scale factor difference book of section 10.2 |
| `tables_scale.go` | `scaleRunLevelRuns`, `scaleRunLevelLevels` | 120 each | run and level per symbol; indices 0 and 1 are the escape and terminator placeholders |
| `tables_coef.go` | `coefLens`, `coefSyms` | two books, 272 and 244 entries | the run-level coefficient books of section 11.3, selected by the block's 1-bit flag |
| `tables_coef.go` | `coefRuns`, `coefLevels` | two books, 272 and 244 entries | run (integer) and level (float) per symbol; indices 0 and 1 are placeholders |
| `tables_coef.go` | `vec4Lens`, `vec4Syms` | 127 each | the four-magnitude book of section 11.1; **symbols carry a -1 bias**, so the raw-zero entry becomes the escape |
| `tables_coef.go` | `vec2Lens`, `vec2Syms` | 137 each | the two-magnitude book, same -1 bias and escape convention |
| `tables_coef.go` | `vec1Lens`, `vec1Syms` | 101 each | the single-magnitude book, no bias; symbol 100 is the large-value escape |
| `tables_decorr.go` | `defaultDecorrelation` | 91 float32 | the built-in matrices of section 9.3, concatenated, sizes 1 to 6 at offsets 0, 1, 5, 14, 30, 55 |

Everything else a decoder needs is computed, not tabulated:

| quantity | rule |
|---|---|
| frame length | section 2, from the rate and decode-flags bits 1-2 |
| `frameSizeBits` | `floorLog2(nBlockAlign) + 4` |
| subframe shift field width | `floorLog2(log2(maxSubframes)) + 1` |
| trim field width | `frameLenBits + 1` |
| run-level escape run width | `floorLog2(subframeLen - 1) + 1` |
| vector-limit field width | `floorLog2(trunc((subframeLen + 3)/4)) + 1` |
| band edges, resample map, subwoofer cutoffs | section 3, from the rate and frame length |
| the sine window at every subframe length | `sin(pi*(i + 0.5)/(2*overlap))` |
| the rotation sine table | `sin(m*pi/64)`, 33 entries, `m` in 0..32 |
| the per-band gain | `10^(exponent/20)` |
| the transform scale | `(2/L) * 2^-(bitsPerSample-1)` |

The three constants that are neither table nor formula, and must be written as
exact rationals rather than computed: `181/256` (0.70703125, the scaled stereo
matrix entry), `181/128` (1.4140625, the disabled-band compensation), and
`45` (the scale factor DPCM starting numerator, divided by the step with
truncation).


## Addendum from the implementation pass, 2026-09-10

Three places where writing the decoder found this file needed an answer it did
not give, or gave in two ways. Nothing above is rewritten except one sentence
in section 10: these are the resolutions, with their reasons, so the next
reader meets the disagreement and the decision in the same place.

**Which subframe reads the scale-factor presence bit (section 10).** The
pseudo-code says `transmitted = (this is the channel's first subframe in this
frame) or read(1)`, and the prose beside it said the first subframe skips the
bit "because `reuse` is false there". Those agree whenever a channel's first
subframe transmits coefficients, which is every subframe of every
single-subframe frame, and disagree whenever a channel's leading subframes are
silent: then `reuse` is still false at the first transmitting subframe, the
prose skips the bit, and the pseudo-code reads it. The corpus settled it for
the pseudo-code. Under the prose reading the two multi-subframe frames of the
44.1 kHz stereo cell that open with three silent subframes decoded their
first transmitting subframe with a scale-factor resolution of 4, which this
file says never occurs, and every subframe after it was wrong; under the
pseudo-code reading every cell matches FFmpeg at the oracle's floor. The prose
is corrected in place. Worth recording because the misparse was invisible to
the one oracle-free check the frame layer offers: the walk still consumed
exactly `len - 2` bits, since a prefix code resynchronises within a few
symbols and the last subframe's tail runs to the same end either way.

**Continuation bits with no carry (sections 4.1 and 5).** Section 5 says a
resumed decode discards the carry and starts at the first frame that begins in
the packet, which is right, and the packet header procedure in 4.1 does not say
what to do with a non-zero continuation count when there is no carry to append
it to. The answer is the same as for a sequence break: the bits are the tail of
a frame whose head this decoder never saw, and they are skipped. That covers
three cases that all look alike from inside the decoder: a decode start, a
resume after a seek, and the packet after a refused one. Appending them to an
empty carry and decoding the result as a frame is what a resumed decode did
before the codec-level seek test caught it, and it failed on every cell within
two packets.

**The channel mask (section 1) is trusted more narrowly than described.** As
`wma-lossless-bitstream.md` records for the same reason: Windows also writes
`SPEAKER_ALL` (0x80000000), which names no speaker position and whose
population count is 1 whatever the stream carries. So the count comes from
`nChannels`, and the mask supplies only the layout, and only when it is
positional and covers exactly that count. Every real file is unaffected.
