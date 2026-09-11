# WMA Voice bitstream notes

ADR-0001 black-box analysis artifact. This file, `wma-voice-oracle-corpus.md`
beside it, and the generated tables named in section 17 are the **only** inputs
the session that writes the decoder consumes; that session does not open
FFmpeg, and none of these files contains code from it.

Scope: `wFormatTag` 0x000A, the codec Windows names **Windows Media Audio Voice
9**, mono, 16-bit, 8 to 22.05 kHz. Tag 0x000B is the same coded layer under a
second registered tag and `container/asf` already maps both to this codec.
Nothing in this format is shared with WMA v1/v2 (0x0160, 0x0161), WMA Pro
(0x0162) or WMA Lossless (0x0163) except the ASF container and the
`WAVEFORMATEX` wrapper. This is not a transform codec at all: it is a CELP
speech codec, closer in shape to AMR or SIPRO than to anything else in this
tree.

Like the rest of the WMA family this format has no published specification. The
layout below was recovered in this analysis pass and then checked by decoding:
a decoder was written from this description and run against the whole committed
corpus. Where a statement is a measurement it says *measured*; where it is a
deduction that no file in reach can confirm it says so.

## This is a CELP codec, and that changes what "the same" means

Two things follow from the codec's shape and both matter more here than they
did for WMA Pro.

**Everything is float and everything is recursive.** The excitation is built
from small integers, but from there on it runs through a long-term predictor
whose memory is the last few hundred output samples, then through an
all-pole synthesis filter, then through a postfilter with its own memories. A
rounding difference does not stay local: it feeds back. *Measured*: a decoder
written from this file, in `float32` for the signal path and `float64` where
this file says double, reproduced the reference decoder's output on all seven
committed cells to a worst-case absolute deviation of **6.5e-06** on a nominal
full scale of 1.0, with an RMS of 1.5e-07. That is about four bits worse than
the WMA Pro figure and the reason is the recursion, not the arithmetic.

**A decode that starts in the middle is a different decode.** There is no
overlap-add window to converge through: the adaptive codebook is a pitch-lag
copy of the decoder's own past output with a gain near 1, so a wrong history
decays only as fast as that gain lets it. Section 13 measures how fast, and it
is not one frame.

What is and is not allowed to vary:

| stage | type | may a conforming decoder differ? |
|---|---|---|
| every bitstream field | integer | no, exact |
| pitch lags, pulse positions, codebook indices | integer | no, exact |
| the comfort-noise codebook offset | integer | no, exact, and it depends on a frame counter (11) |
| dequantised LSFs and the LSP-to-LPC conversion | double | in the last bits |
| the excitation, the synthesis filter, the postfilter | float | yes, and it accumulates |
| the postfilter's transform normalisation | float | **no**, see 10.5: get it wrong and the output is wrong, not noisy |

### Notation

| notation | meaning |
|---|---|
| `x >> n` (signed x) | arithmetic shift, equal to `floor(x / 2^n)` |
| `x &~ m` | clear the bits of m, that is `x & ^m` |
| `trunc(a/b)` | integer division truncating **toward zero**, C and Go both |
| `clip(x, lo, hi)` | saturate to the closed range |
| `ceilLog2(x)` | smallest n with `2^n >= x`; **0 for x <= 1** |
| `int(f)` | a float truncated toward zero |
| `roundEven(f)` | round to nearest, ties to even |

All bit reading is **most significant bit first**, bytes in file order. The
little-endian fields are confined to the `WAVEFORMATEX` of section 1.

## Measurement basis

Reference studied: FFmpeg n9.0 at commit
`d32b387f2b0a484599d4587d651891f0c63c4238`, the commit pinned across this repo.
Every file fetched and studied, with its SHA-256 (the files are LF-only, so
CRLF normalisation is a no-op and these digests hold either way):

| file | SHA-256 | what it contributed |
|---|---|---|
| `libavcodec/wmavoice.c` | `f4e77847584383f3a3c145ace8b4bbe6eee13811362b0d9923d4f4cc450a4d37` | the whole coded layer, and one data table (17) |
| `libavcodec/wmavoice_data.h` | `47d570cf58e98f8015ac588e6bb8fbbbc99e04b89aa994c781de11e4d119c173` | every constant table |
| `libavcodec/celp_filters.c` | `eb589c90611e55a849910f7dc9e88c11af5cf5ce442a17e4b43324863d5259fc` | the two LP filters, section 9 |
| `libavcodec/celp_filters.h` | `6905507b368d4f958c5b6f12190c64959f1f8a20591d7d31195b616b1a8e455e` | nothing beyond declarations |
| `libavcodec/acelp_vectors.c` | `7e018c982d14baddceefa7aa575935ec261730e3ed14bc4c1799be41199ebe32` | the fixed-vector scatter and the weighted sum, sections 8 and 9 |
| `libavcodec/acelp_vectors.h` | `b4735c3f382ca950f8394bab7e625b0b6f1e1a17bfb1c984a32b21a395ac774e` | the pulse descriptor's fields |
| `libavcodec/acelp_filters.c` | `75c19bcf8345757c2127dab0cabafe134689621af3d0de7908943321d108bb40` | the fractional interpolator, the tilt filter, the order-2 section |
| `libavcodec/acelp_filters.h` | `171a03070086256412f87b6c50e32c44b6ec2212a3835d44e67f6d94cefbea80` | nothing beyond declarations |
| `libavcodec/lsp.c` | `231cc4bab36aa8d85a4031a80ff63019c396e3c29bbd92089bc797d165698781` | the LSP-to-LPC conversion, section 5.7 |
| `libavcodec/lsp.h` | `72b10ad9504af722381d650c5cfd580bdb6c92906782ae5d51f1a59bfd40f2ef` | the half-order bound |
| `libavcodec/sinewin_tablegen.h` | `a2defd94fb60f2c6b838e51219afedf6a0991f393fccae6dd9d205a60b5c78de` | the half-sample-offset sine, section 10.5 |
| `libavutil/tx.h` | `a1c5c309f493cc22f4637e67696581be676cd33fdcaa0661a01f5bb4345c900a` | the transform contracts |
| `libavutil/tx_template.c` | `b3c4becdffefd82321069775b80bb542e577fb6ad229ea6bda3570d98d76e130` | the transform definitions and the index-64 quirk, section 10.5 |

Only the first two are pinned by the generator (section 17): those are the two
this pass extracts data from. The rest were read, not extracted, and their
digests are recorded so a later reader can confirm which revision was studied.

The checkout was deleted at the end of this pass.

Everything marked *measured* was taken on 2026-09-10 in one of four ways.

1. **By decoding.** A decoder was written in this pass from the description
   below and run against all seven committed cells plus
   `container/asf/testdata/voice-mono.wma`. Every one agreed with the reference
   decoder (`ffmpeg -cpuflags 0`, scalar path) on the sample count exactly and
   on the samples to within 6.5e-06, once the reference's superframe-cache
   defect of section 3.3 is accounted for. Field statistics come from that
   decoder's instrumentation, so "*measured*: never occurs" means the field was
   parsed on every frame of every one of those files and never took that value.
2. **By resuming.** The same decoder was restarted at every media object
   boundary of four cells, 47 resume points in all, and each resumed decode was
   compared superframe by superframe with the linear decode. Section 13 reports
   the result.
3. **By probing the reference's transform library.** The four transforms the
   postfilter uses were called directly with known inputs and their outputs
   compared against closed forms, to pin the normalisation and sign of each.
   Section 10.5 states what came back.
4. **From the encoder's own enumeration.** The Windows Media Format SDK offers
   exactly seven formats for this codec, and all seven are in the corpus.
   Claims about "what the encoder can write" cover the whole envelope.

Every fixture was produced by **Windows' own encoder**. FFmpeg has no encoder
for this format, so nothing here is a round trip through the reference.

## 0. The envelope

What the only encoder offers, from the `IWMCodecInfo3` enumeration
(`wma-voice-oracle-corpus.md` section 2):

| axis | values offered | not offered |
|---|---|---|
| sample rate | 8000, 11025, 16000, 22050 | everything else |
| channels | 1 | everything else |
| bit depth | 16 | everything else |
| bit rate | 4000, 5000, 8000, 10000, 12000, 16000, 20000 | everything else |
| LSP order | 10 at 8000 and 11025, 16 at 16000 and 22050 | mixing them at one rate |
| postfilter | always on | a stream with it off |

The format allows more than that, and the gap is where the risk lives:

| the format allows | the encoder writes | consequence |
|---|---|---|
| any rate from 322 to 22097 Hz (2) | four rates | the rest of the pitch-bound arithmetic is untested |
| per-frame independent LSPs (5.4) | residual LSPs, always | **the independent path has no fixture at all** |
| a per-superframe statistics field (3.4) | never | untested, skipped by its declared length |
| the extended 8-bit pitch-adaptive window index (8.2) | never | untested |
| the "no adaptive pulses" branch of the window coder (8.2) | never | untested |
| a pitch-adaptive window range of 16 (8.2) | 24 only | untested |
| a DC removal filter (10.7) | never selected | untested |
| frame types 8, 9, 10 and 14 (4) | never | untested |
| an LSP set that needs stabilising (5.6) | never | untested |
| a superframe whose first bit is 0 (3.5) | never | **refused**, section 14 |

Read that as a warning, not a licence to implement the middle column. Each
untested row is called out again where it appears, and section 14 says which of
them become refusals.

## 1. The WAVEFORMATEX and the 46 extra bytes

`container/asf` hands the whole `WAVEFORMATEX` over as `Track.CodecConfig`: 18
fixed bytes up to and including `cbSize`, then the codec-specific extra bytes.
For this format `cbSize` is **exactly 46**, so the blob is 64 bytes and anything
else is a refusal. (The reference refuses any extradata size other than 46 by
name, and so should this decoder.)

The fixed part, with the offsets stated because one of them is easy to get
wrong:

| offset | width | field | use |
|---|---|---|---|
| 0 | 2 | `wFormatTag` | 0x000A or 0x000B selects this codec |
| 2 | 2 | `nChannels` | **not used**; this codec is mono by construction |
| 4 | 4 | `nSamplesPerSec` | sets the pitch bounds and every derived field width (2) |
| 8 | 4 | `nAvgBytesPerSec` | **not used** |
| 12 | 2 | `nBlockAlign` | the packet size in bytes, and the sole input to the spillover field width |
| 14 | 2 | `wBitsPerSample` | **not used**; the decoder emits float |
| 16 | 2 | `cbSize` | 46 |
| 18 | 46 | the extra bytes | sections 1.1 and 1.2 |

`wBitsPerSample` is at offset **14**, not 16. `wma-pro-bitstream.md` section 1
puts it at 16; that is a typo in the older note (the field it calls out as
unused is `cbSize`), harmless there because both are unused, and stated here so
the next reader does not copy it.

The extra bytes split three ways:

| extra offset | width | what |
|---|---|---|
| 0..17 | 18 | a WMA Pro `WAVEFORMATEX` extension block, present because this codec can in principle carry a WMA Pro payload (3.5). **Not read.** *Measured*: bytes 0..1 are 0x0010 (16 bits per sample), bytes 2..5 are 0x00000004 (`SPEAKER_FRONT_CENTER`), bytes 14..15 are a WMA Pro decode-flags word, and the rest is zero on all seven formats. |
| 18..21 | 4 | the flags word, **little-endian** (1.1) |
| 22..45 | 24 | the variable bit mode tree, 17 fields of 3 bits, MSB first, then padding (1.2) |

### 1.1 The flags word

Read `flags` as a little-endian 32-bit value at extra offset 18. Bit numbering
below is from the low bit of that value.

| bits | field | meaning |
|---|---|---|
| 0 | postfilter enable | 1 runs the whole postfilter of section 10, 0 outputs the synthesis filter directly |
| 1 | (not read) | *measured*: 1 on formats 0, 1, 2 and 4, and 0 on 3, 5 and 6. No use is known. Ignore it. |
| 2..5 | denoise strength | 0 to 11, the row of the denoise power table (10.5). **12 to 15 is a refusal.** |
| 6 | denoise tilt correction | 1 applies the tilt filter to the denoise impulse response (10.5) |
| 7..10 | DC noise level | 0 to 15; **the DC removal filter runs only when this is greater than 8** (10.7) |
| 11 | (not read) | *measured*: 0 on all seven formats |
| 12 | LSP order | 0 selects 10 LSPs, 1 selects 16 |
| 13 | LSP quantiser mode | selects between the A and B inter-frame interpolation coefficient sets (5.4) |
| 14 | LSP default mode | selects between the two mean-LSF vectors (5.5) |
| 15 | (not read) | *measured*: 1 on all seven formats |
| 16..31 | (not read) | *measured*: a per-format pattern; bits 24..31 hold `2*formatIndex + 3` on the seven enumerated formats, which reads like a format serial number and not like anything a decoder needs |

Nothing in the flags word is a refusal except a denoise strength of 12 or more.
Bits the decoder does not read must be ignored rather than checked: three of
them are non-constant across the seven real formats.

### 1.2 The variable bit mode tree

The frame type of every frame is a Huffman code whose 22 codeword lengths are
fixed, but whose **assignment of frame types to codewords is carried in the
extradata**. That is what "variable bit mode" means here.

The fixed code is canonical with these lengths, in this order:

```
2, 2, 2, 4, 4, 4, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 14
```

Twenty-two codewords, and the Kraft sum is exactly 1, so the code is complete.
Assign codes in table order: walk from index 0 and give each entry the next
code of its length, where "next" means the top L bits of a 32-bit accumulator
that starts at 0 and advances by `2^(32-L)` after every entry. This is the same
convention `wma-pro-bitstream.md` states for its books. Decoding is the
ordinary "shift a bit in, look for a code of this length" walk. The result is a
**symbol index from 0 to 21**, not a frame type.

The extradata turns that symbol index into a frame type. Read 17 fields of 3
bits each, MSB first, starting at extra offset 22, one per frame type index `n`
from 0 to 16. Call the field `c`, the type's **class**. Maintain a per-class
counter starting at 0 and set

```
tree[3*c + count[c]] = n ;  count[c] += 1
```

over a 25-entry array initialised to "invalid". The symbol index from the VLC
then indexes `tree` directly, and the value there is the frame type index of
section 4.

The class is the codeword length: class `c` occupies symbol indices `3c`,
`3c+1`, `3c+2`, and those are exactly the three codewords of length `2(c+1)`.
So a class may hold at most three frame types. **A class that receives a fourth
is a refusal**: the reference allows four and lets the fourth overwrite the next
class's first slot, which is a defect, and no real extradata does it
(*measured*, on all seven formats: every class holds two or three types, and
the seventeen types fill classes 0 to 5 exactly). Symbol indices 17 to 21 are
never populated by any real extradata and decoding one is a refusal.

### 1.3 The seven observed extradata blobs, decoded

Every format Windows' encoder offers, decoded under section 1.1 and 1.2. This
is the cheapest check that the field map above is right: if an implementation
reproduces this table from the bytes in
`codec/wmavoice/testdata/corpus/MANIFEST.md`, its configuration parse is
correct.

| rate | bit rate | `nBlockAlign` | flags | postfilter | denoise | tilt | DC level | LSPs | q mode | def mode |
|---|---|---|---|---|---|---|---|---|---|---|
| 8000 | 4000 | 150 | 0x031980a7 | on | 9 | no | 1 | 10 | 0 | 0 |
| 8000 | 5000 | 187 | 0x0519809f | on | 7 | no | 1 | 10 | 0 | 0 |
| 8000 | 8000 | 300 | 0x071980cf | on | 3 | **yes** | 1 | 10 | 0 | 0 |
| 11025 | 10000 | 544 | 0x0959e115 | on | 5 | no | 2 | 10 | **1** | **1** |
| 16000 | 12000 | 450 | 0x0b1991a7 | on | 9 | no | 3 | **16** | 0 | 0 |
| 16000 | 16000 | 600 | 0x0d59918d | on | 3 | no | 3 | **16** | 0 | 0 |
| 22050 | 20000 | 1088 | 0x0f99f205 | on | 1 | no | 4 | **16** | **1** | **1** |

Two things to read off it. The postfilter is on everywhere, so a decoder that
skips it is wrong on every real file, not just on some. And the DC level never
exceeds 4, so the DC removal filter of section 10.7 never runs on anything this
tree can reach.

The variable bit mode trees, decoded the same way (class per frame type index
0..16, in order):

| rate / bit rate | classes |
|---|---|
| 8000 / 4000 | 0 0 0 1 1 1 3 2 4 4 5 2 2 3 5 3 4 |
| 8000 / 5000 | 1 1 0 0 0 1 2 2 4 4 5 2 3 3 5 3 4 |
| 8000 / 8000 | 0 1 3 4 4 4 5 5 2 2 3 1 0 0 3 1 2 |
| 11025 / 10000 | 1 1 3 4 4 4 5 5 2 2 1 0 0 0 3 2 3 |
| 16000 / 12000 | 0 1 3 4 4 4 5 5 2 0 0 1 2 1 3 2 3 |
| 16000 / 16000 | 0 1 3 4 4 4 5 5 2 2 1 1 0 0 3 2 3 |
| 22050 / 20000 | 0 1 3 4 4 4 5 5 2 2 1 0 0 1 3 2 3 |

The encoder gives the frame types it actually uses the short codewords, which
is why the trees differ per bit rate.

### 1.4 What to refuse before reading a bit of bitstream

- extra bytes that are not exactly 46, or a `cbSize` that is not 46
- `nBlockAlign` of zero, or above about four million (the reference's bound is
  `1 << 22`; anything of that order is fine, the point is that the field feeds
  a shift width)
- a denoise strength of 12 or more
- a sample rate outside 322 to 22097 Hz (section 2 derives both ends)
- a variable bit mode class holding more than three frame types

## 2. Sample rate, the pitch bounds and every width derived from them

Everything the frame layer needs is arithmetic on `nSamplesPerSec`. There are
no per-rate tables and no per-rate special cases.

```
minPitch  = ((rate << 8)      / 400 + 50) >> 8
maxPitch  = ((rate << 8) * 37 / 2000 + 50) >> 8
```

Both divisions are integer, and in the second the multiply by 37 happens
**before** the divide by 2000, so a 64-bit intermediate is wanted for the
highest rates. In real terms `minPitch` is about `rate/400` (a 400 Hz ceiling on
the fundamental) and `maxPitch` is about `rate*37/2000` (a 54 Hz floor), with a
`+50 >> 8` that rounds up by about 0.195 of a sample.

From those:

```
pitchRange       = maxPitch - minPitch            ; must be > 0
pitchBits        = ceilLog2(pitchRange)
historySamples   = maxPitch + 8
convTable[0]     = minPitch
convTable[1]     = (pitchRange * 25) >> 6
convTable[2]     = (pitchRange * 44) >> 6
convTable[3]     = maxPitch - 1
deltaPitchHalf   = (pitchRange >> 3) &~ 0xF       ; must be > 0
deltaPitchBits   = 1 + ceilLog2(deltaPitchHalf)
blockPitchRange  = convTable[2] + convTable[3] + 1
                   + 2 * (convTable[1] - 2 * minPitch)
blockPitchBits   = ceilLog2(blockPitchRange)
spilloverBits    = 3 + ceilLog2(nBlockAlign)
```

**The rate range.** `historySamples` is the excitation history the adaptive
codebook can reach back into, and the reference caps it at 416 samples. That
cap plus `minPitch >= 1` is where the supported rate range comes from:

```
minimum rate = 322 Hz     (below it, minPitch rounds to 0)
maximum rate = 22097 Hz   (above it, historySamples exceeds 416)
```

The 416 is the reference's buffer size, not a property of the format, but it is
also exactly what the highest real rate needs: at 22050 Hz `historySamples` is
**416 on the nose** (*measured*). Sizing the history buffer from `maxPitch + 8`
and refusing above 22097 Hz reproduces the reference's behaviour and costs
nothing, since no encoder writes a higher rate.

*Measured*, the four real rates:

| rate | minPitch | maxPitch | range | pitchBits | history | convTable | deltaPitchHalf | deltaPitchBits | blockPitchRange | blockPitchBits |
|---|---|---|---|---|---|---|---|---|---|---|
| 8000 | 20 | 148 | 128 | 7 | 156 | 20, 50, 88, 147 | 16 | 5 | 256 | 8 |
| 11025 | 27 | 204 | 177 | 8 | 212 | 27, 69, 121, 203 | 16 | 5 | 355 | 9 |
| 16000 | 40 | 296 | 256 | 8 | 304 | 40, 100, 176, 295 | 32 | 6 | 512 | 9 |
| 22050 | 55 | 408 | 353 | 9 | 416 | 55, 137, 242, 407 | 32 | 6 | 704 | 10 |

`spilloverBits` per cell is 11, 11, 12, 13, 12, 13 and 14 for the seven
`nBlockAlign` values of section 1.3. Note `nBlockAlign` 187 is odd and
`ceilLog2(187)` is 8, so `spilloverBits` is 11: nothing rounds the packet size
to a power of two.

## 3. Packets, superframes and frames

Three nested units, and the names matter because they do not line up with the
other codecs here.

- A **packet** is one ASF media object, exactly `nBlockAlign` bytes. It carries
  a header (3.1) and then a run of superframes.
- A **superframe** is the decoder's output unit: **480 samples**, three frames.
  A superframe may be shorter than 480 samples only by declaring so (3.4), and
  only the last superframe of a stream does. A superframe may **span two
  packets** (3.2).
- A **frame** is 160 samples, and carries one frame type, one LSP set (or a
  share of the superframe's), and one to eight blocks.
- A **block** is `160 / blocks` samples, so 160, 80, 40 or 20.

There is no bit reservoir and no overlap. A superframe's bits are contiguous
(possibly across a packet boundary) and its 480 output samples are complete
when it is decoded.

### 3.1 The packet header

At the top of every packet, MSB first:

| bits | field |
|---|---|
| 4 | packet sequence number; **skip it**, nothing reads it |
| 1 | residual LSP flag: 1 selects the residual LSP coding of section 5.4 for the superframes this packet governs, 0 selects per-frame independent LSPs |
| 6 | superframe count, repeated (see below) |
| `spilloverBits` | spillover bit count |

The superframe count is read as a run of 6-bit fields: read one, add it to the
running total, and read another whenever the value read was exactly 63. Stop on
the first value below 63. Before each 6-bit read, check that at least
`6 + spilloverBits` bits remain, and refuse if not. *Measured*: the escape never
occurs; every packet in the corpus carries a single 6-bit field, and the counts
range from 1 to 24.

The **spillover bit count** is the number of bits at the start of this packet's
payload that belong to the superframe the previous packet left unfinished.
*Measured*: values from 0 to 925, and zero is common (35 of the corpus's 101
packets).

**The residual LSP flag is read before the carried-over superframe is decoded**,
so a superframe that spans two packets is decoded under the **following**
packet's flag, not the one that was in force when its first bits were written.
Nothing in this tree can exercise a disagreement, because *measured* the flag is
1 on every packet of every file.

### 3.2 Taking superframes out of a packet

Let `N` be the superframe count from the header and `carry` the bits held over
from the previous packet.

1. If `carry` is non-empty, append the packet's first `spilloverBits` bits to
   it, decode one superframe from the result, discard whatever is left of
   `carry`, and continue at the bit position just past the spillover. `carry`
   is now empty.
2. If `carry` is empty and `spilloverBits` is non-zero, **skip** those bits.
   They are the tail of a superframe whose head this decoder never saw: at the
   start of a stream, after a seek, or after a refused packet.
3. Decode `N - 1` superframes back to back from the packet.
4. Whatever bits remain in the packet become the new `carry`.

That is the whole rule, and step 4 is the part that looks wrong the first time.
The header's count is one **more** than the number of superframes the decoder
takes out of the packet body, because the last one always becomes the carry,
even when the packet holds it complete with room to spare. It is then decoded at
the top of the next packet with a spillover of zero. At the end of the stream
there is no next packet, so **the decoder must be flushed**: a final call with
an empty packet decodes the carry and emits the last superframe. *Measured*:
without that flush every cell comes out exactly one superframe short.

The carry is a **bit** string, not a byte string, and its length is not a
multiple of 8. *Measured*: carries from 81 to 6422 bits.

Reading a superframe out of the packet body also leaves a sub-byte remainder.
The reference tracks it as a "skip this many bits next time" count because its
packet entry point is re-entered per superframe; an implementation that keeps
one bit reader per packet does not need it.

### 3.3 The reference's superframe cache is 2048 bits, and that is a defect

**This is the single most important thing in this file for the implementation
stage's gate, so it is stated here and measured in the corpus note.**

The reference holds the carry in a 256-byte buffer and copies into it through a
helper that **silently copies nothing** when the copy does not fit, while still
recording the length it was asked for. So when a packet's carry exceeds 2048
bits, the reference decodes the next superframe out of whatever bytes were left
in that buffer from an earlier packet. The output is a plausible superframe of
the wrong content, often very loud, and its filter memories poison what follows.

A correct decoder has no such limit: the carry is as long as the packet's tail,
which can be most of `nBlockAlign * 8`. Size the buffer from `nBlockAlign`.

*Measured*: carries above 2048 bits occur on four of the seven cells (never on
the three 8 kHz cells) and 18 times in total, always in low-bit-rate passages
where the superframes are 90 to 160 bits and a fixed-size packet leaves
kilobits of tail. `wma-voice-oracle-corpus.md` sections 6 and 7 give the exact
superframe indices, the size of the resulting error, and what a gate can
therefore assert.

### 3.4 The superframe

MSB first, in this order:

| bits | field |
|---|---|
| 1 | speech/music flag. **1 is speech, this codec. 0 is WMA Pro in disguise and is refused** (3.5). |
| 1 | sample count present |
| 12 | sample count, only when the previous bit is 1. Must be **at most 480**; more is a refusal. |
| ... | the residual LSP block, only when the packet header's residual LSP flag is set (5.4) |
| ... | three frames (section 4 onward), each preceded by an independent LSP block when the residual flag is clear (5.4) |
| 1 | statistics present |
| 4 | statistics length code `k`, only when the previous bit is 1 |
| ... | `10 * (k + 1)` bits, skipped unread |

**The sample count is what governs the last superframe's length**, and it is the
whole answer to the question the measurement set raised. There is no trim field,
no container-side trimming and no priming: a superframe emits exactly 480
samples unless it says otherwise, and *measured*, on all seven cells plus the
demuxer cell, **exactly one superframe carries the field, the last one, and the
value it carries makes the decode exactly as long as the encoder's source**:

| cell | superframes | last superframe's declared samples | total | source |
|---|---|---|---|---|
| `voice-8000-4k` | 67 | 183 | 31863 | 31863 |
| `voice-8000-5k` | 67 | 183 | 31863 | 31863 |
| `voice-8000-8k` | 67 | 183 | 31863 | 31863 |
| `voice-11025-10k` | 92 | 237 | 43917 | 43917 |
| `voice-16000-12k` | 133 | 367 | 63727 | 63727 |
| `voice-16000-16k` | 133 | 367 | 63727 | 63727 |
| `voice-22050-20k` | 183 | 473 | 87833 | 87833 |
| `voice-mono` (ASF cell) | 34 | 160 | 16000 | 16000 |

FFmpeg emits 63840 and 87840 on two of those cells, over-running the source by
113 and 7 samples. That is not a second length rule: it is the cache defect of
3.3 landing on the last superframe of those two files, so the reference reads
the sample-count field out of stale bytes. Do not model it.

The statistics field is *measured*: **never present**, on any superframe of any
file. Read the bit, skip the block by its declared length if it is set, and do
not refuse it.

### 3.5 The superframe whose first bit is 0

A clear speech/music bit says the superframe carries a **WMA Pro** payload
inside a WMA Voice superframe, which is why the extra bytes begin with a WMA Pro
extension block (section 1). The reference refuses it by that name and has
never seen one in the wild. WaxFlow refuses it by the same name (section 14).
Nothing in this tree can produce one.

## 4. Frame types

The frame type index from section 1.2 selects a row of this table. The table
itself is not in the extradata and not in the bitstream: it is fixed.

| type | blocks | log2 | adaptive codebook | fixed codebook | double pulses |
|---|---|---|---|---|---|
| 0 | 1 | 0 | none | silence | 0 |
| 1 | 2 | 1 | none | hardcoded | 0 |
| 2 | 2 | 1 | asymmetric | adaptive-window pulses | 0 |
| 3 | 2 | 1 | asymmetric | innovation pulses | 2 |
| 4 | 2 | 1 | asymmetric | innovation pulses | 5 |
| 5 | 4 | 2 | asymmetric | innovation pulses | 0 |
| 6 | 4 | 2 | asymmetric | innovation pulses | 2 |
| 7 | 4 | 2 | asymmetric | innovation pulses | 5 |
| 8 | 2 | 1 | Hamming | innovation pulses | 0 |
| 9 | 2 | 1 | Hamming | innovation pulses | 2 |
| 10 | 2 | 1 | Hamming | innovation pulses | 5 |
| 11 | 4 | 2 | Hamming | innovation pulses | 0 |
| 12 | 4 | 2 | Hamming | innovation pulses | 2 |
| 13 | 4 | 2 | Hamming | innovation pulses | 5 |
| 14 | 8 | 3 | Hamming | innovation pulses | 0 |
| 15 | 8 | 3 | Hamming | innovation pulses | 2 |
| 16 | 8 | 3 | Hamming | innovation pulses | 5 |

The block count divides 160, so blocks are 160, 80, 40 or 20 samples. "log2" is
the block count's base-2 logarithm and it appears directly in two field widths
(8.1) and one gain weight (7.3), so it is worth carrying rather than
recomputing.

The three adaptive-codebook kinds are:

- **none**: the block is comfort noise or a hardcoded codebook read, with no
  long-term prediction at all (7.1, 7.2, 11).
- **asymmetric**: one pitch value per frame, interpolated to a pitch per sample,
  with a 17-tap asymmetric sinc at nine fractional phases (6.1, 9).
- **Hamming**: one pitch value per block, quarter-sample resolution, with a
  4-tap Hamming-windowed sinc at eight fractional phases (6.2, 9).

*Measured* frame type usage across the corpus is in
`wma-voice-oracle-corpus.md` section 4. In short: types 0, 1, 13 and 16 carry
almost everything; the 8 kHz cells at 4 and 5 kbit/s additionally reach 2 to 7,
11, 12 and 15; and **types 8, 9, 10 and 14 are never used by any cell**.

## 5. LSP coding

Every frame needs ten or sixteen line spectral frequencies. They arrive either
once per superframe in residual form (5.4), which is what every real file does,
or once per frame in independent form (5.2, 5.3), which nothing in this tree
reaches.

### 5.1 The multi-stage dequantiser

All eight LSP codebooks are read the same way. A codebook is a flat run of
unsigned 8-bit entries. A dequantisation of `num` coefficients over `S` stages
takes `S` indices read from the bitstream, `S` sizes, `S` scale factors `mul`
and `S` offsets `base`, and a table pointer:

```
for m in 0 .. num-1:  out[m] = 0
offset = 0
for s in 0 .. S-1:
    row = offset + index[s] * num
    for m in 0 .. num-1:
        out[m] += base[s] + mul[s] * table[row + m]
    offset += size[s] * num
```

Note that `base[s]` is added once **per coefficient per stage**, not once per
coefficient, so a two-stage dequantisation adds two bases. Everything here is
`float64`; the reference is double throughout the LSP path and the values are
small differences of transcendentals, so single precision is not enough.

### 5.2 Ten independently coded coefficients

Read four indices, in this order and these widths: 8, 6, 5, 5 bits. Then
dequantise ten coefficients over four stages from `dqLSP10i`, with

```
sizes = 256, 64, 32, 32
mul   = 5.2187144800e-3, 1.4626986422e-3, 9.6179549166e-4, 1.1325736225e-3
base  = pi * -2.15522e-1, pi * -6.1646e-2, pi * -3.3486e-2, pi * -5.7408e-2
```

Twenty-four bits in total.

### 5.3 Sixteen independently coded coefficients

Read five indices: 8, 6, 7, 6, 7 bits. Then run **three** dequantisations into
three slices of the output, each with its own table and its own slice of the
constants:

```
sizes = 256, 64, 128, 64, 128
mul   = 3.3439586280e-3, 6.9908173703e-4, 3.3216608306e-3,
        1.0334960326e-3, 3.1899104283e-3
base  = pi * -1.27576e-1, pi * -2.4292e-2, pi * -1.28094e-1,
        pi * -3.2128e-2,  pi * -1.29816e-1

coefficients  0..4   : 2 stages, indices 0 and 1, table dqLSP16i1
coefficients  5..9   : 2 stages, indices 2 and 3, table dqLSP16i2
coefficients 10..15  : 1 stage,  index 4,          table dqLSP16i3
```

Each dequantisation uses the slice of `sizes`, `mul` and `base` that starts at
its first index. Thirty-four bits in total.

### 5.4 Residual coding across the superframe

When the packet header's residual LSP flag is set, the superframe carries one
LSP block that covers all three of its frames.

1. Form `prev[n] = lastSuperframeLSF[n] - meanLSF[n]` for every coefficient,
   where `lastSuperframeLSF` is the third frame's LSF set from the previous
   superframe (initialised as in 5.6) and `meanLSF` is from 5.5.
2. Read an independent set exactly as in 5.2 or 5.3 into `i_lsps`. This is the
   **third** frame's set.
3. Read a 5-bit interpolation index.
4. Read three more indices: 7, 6, 6 bits for ten coefficients, or 7, 7, 7 bits
   for sixteen.
5. Build the interpolated pair. With `L` the coefficient count and `t` the
   interpolation table selected by the LSP quantiser mode flag (`A` when clear,
   `B` when set):

   ```
   for n in 0 .. L-1:
       delta   = prev[n] - i_lsps[n]
       a1[n]     = t[index][0][n] * delta + i_lsps[n]
       a1[L + n] = t[index][1][n] * delta + i_lsps[n]
   ```

   The table entries are `float32`; widen them to `float64` for the multiply.
6. Dequantise the correction vector `a2`, which is `2L` long:

   ```
   ten coefficients:     a2[0..19]  = 3 stages, sizes 128, 64, 64, table dqLSP10r
                         mul  = 2.5807601174e-3, 1.2354460219e-3, 1.1763821673e-3
                         base = pi * -1.07448e-1, pi * -5.2706e-2, pi * -5.1634e-2

   sixteen coefficients: a2[0..9]   = 1 stage, size 128, index 0, table dqLSP16r1
                         a2[10..19] = 1 stage, size 128, index 1, table dqLSP16r2
                         a2[20..31] = 1 stage, size 128, index 2, table dqLSP16r3
                         mul  = 1.2232979501e-3, 1.4062241527e-3, 1.6114744851e-3
                         base = pi * -5.5830e-2, pi * -5.2908e-2, pi * -5.4776e-2
   ```

   For sixteen coefficients the three runs are 10, 10 and 12 long, which is 32,
   and each uses the `mul` and `base` at its own index.
7. Form the three frames' LSF sets:

   ```
   frame0[n] = meanLSF[n] + (a1[n]     - a2[2n])
   frame1[n] = meanLSF[n] + (a1[L + n] - a2[2n + 1])
   frame2[n] = meanLSF[n] + i_lsps[n]
   ```

   Note the interleave: `a2` is read as pairs, the even entry correcting the
   first interpolated frame and the odd entry the second.
8. Stabilise all three sets (5.6).

Total: 48 bits for ten coefficients, 60 for sixteen.

**When the flag is clear**, each of the three frames instead carries its own
independent set immediately before its frame data: read 5.2 or 5.3, add
`meanLSF`, stabilise. *Measured*: **this never happens on any file in this
tree**. Implement it from the description and carry it as a deficit.

### 5.5 The mean LSF vectors

Two vectors per coefficient count, selected by the LSP default mode flag. They
are `float64` and they ship as `meanLSF10[2][10]` and `meanLSF16[2][16]`
(section 17). They are ascending and lie in `(0, pi)`, which is what makes the
residual coding above a small correction to a plausible spectrum.

### 5.6 Stabilisation

Applied to every LSF set, in this order, in `float64`:

```
lsf[0] = max(lsf[0], 0.0015 * pi)
for n in 1 .. L-1:  lsf[n] = max(lsf[n], lsf[n-1] + 0.0125 * pi)
lsf[L-1] = min(lsf[L-1], 0.9985 * pi)
if any lsf[n] < lsf[n-1]:  sort the whole vector ascending
```

The sort is a single insertion sort over the whole vector, triggered by the
first inversion found, and it is only reachable because the third line can push
the last value back below the second to last.

*Measured*: on the whole corpus, **the first clamp never fires, the last clamp
never fires, and the sort never runs**. The spacing clamp is not instrumented
separately because it is a `max` per coefficient rather than a branch. Do not
skip stabilisation on the strength of that: it costs nothing and the encoder is
free to write an LSF set that needs it.

### 5.7 LSF to LSP to LPC

The values above are LSFs: angles in `(0, pi)`. Blocks and the postfilter need
LPC coefficients, and they get there in two steps.

**Interpolate, then take the cosine.** Every place that needs LPCs first forms a
weighted LSF set and then takes `cos` of each entry, in `float64`. Section 9
gives the weight for a block and section 10 the weights for the postfilter.

**LSP to LPC.** With `lsp[0..L-1]` the cosines and `H = L/2`:

```
poly(v, f, H):
    f[0] = 1
    f[1] = -2 * v[0]
    for i in 2 .. H:
        a = -2 * v[2*i - 2]
        f[i] = a * f[i-1] + 2 * f[i-2]
        for j in i-1 down to 2:
            f[j] += f[j-1] * a + f[j-2]
        f[1] += a

poly(lsp,       pa, H)        ; the even-indexed cosines
poly(lsp[1:],   qa, H)        ; the odd-indexed cosines

for h in H-1 down to 0:
    p = pa[h+1] + pa[h]
    q = qa[h+1] - qa[h]
    lpc[h]         = 0.5 * (p + q)
    lpc[2*H-1 - h] = 0.5 * (p - q)
```

`poly` is the standard product-of-second-order-sections expansion; the index
`2*i - 2` is what makes the second call read the odd-indexed cosines. `pa` and
`qa` are `float64` and hold `H+1` entries. The output `lpc` is `float32` and
holds `L` entries, and it is the **direct-form denominator** of the synthesis
filter: `A(z) = 1 + lpc[0] z^-1 + ... + lpc[L-1] z^-L`. There is no leading 1 in
the array and no sign flip; section 9 states the filter that consumes it.

## 6. Pitch

### 6.1 Per-frame pitch, for the asymmetric adaptive codebook

Read `pitchBits` bits and form

```
cur = minPitch + field
cur = min(cur, maxPitch - 1)
```

Then decide whether to carry the previous frame's pitch forward. Let `last` be
the pitch state described at the end of this section:

```
if previousFrameACB == none  or  20 * |cur - last| > cur + last:
    last = cur
```

That is, a jump of more than about five percent (or the first frame after a
comfort-noise frame) **resets** the interpolation origin so the frame does not
sweep from an unrelated pitch. The reset writes the state, so the interpolation
below then runs from `cur` to `cur`.

Pitch per block, with `B` the block count and `logB2 = log2(B) + 1`:

```
for n in 0 .. B-1:
    w = 2n + 1
    pitch[n] = (w * cur + (2B - w) * last + B) >> logB2
```

Pitch per sample is carried as a 16.16 slope:

```
pitchSlope = (cur - last) * 65536 / 160
```

with C-style truncation toward zero (the numerator can be negative). Section 9
uses it.

**The pitch state after the frame** depends on the frame's adaptive codebook
kind, and this is easy to get wrong because it is not always `cur`:

| adaptive codebook | pitch state after the frame |
|---|---|
| none | 0 |
| asymmetric | `cur` |
| Hamming | the **last block's** pitch, that is `pitch[B-1]` |

At the start of a stream the state is **40**, not zero, and the previous frame's
adaptive codebook kind is "none".

### 6.2 Per-block pitch, for the Hamming adaptive codebook

One field per block, and the value is on a piecewise scale that has to be
converted.

```
first block:  field = read(blockPitchBits)
other blocks: field = lastField - deltaPitchHalf + read(deltaPitchBits)

lastField = clip(field, deltaPitchHalf, blockPitchRange - deltaPitchHalf)
```

The clip is applied to the **state**, not to the value used, so a block can
decode a pitch outside the clip range while the next block's delta is still
taken from inside it.

Convert `field` to a quarter-sample pitch. With

```
t1 = (convTable[1] - convTable[0]) << 2
t2 = (convTable[2] - convTable[1]) << 1
t3 =  convTable[3] - convTable[2] + 1
```

the conversion is a four-arm ladder of decreasing resolution:

```
if field < t1:              pitchQ = (convTable[0] << 2) + field
else:
  field -= t1
  if field < t2:            pitchQ = (convTable[1] << 2) + (field << 1)
  else:
    field -= t2
    if field < t3:          pitchQ = (convTable[2] + field) << 2
    else:                   pitchQ =  convTable[3] << 2
```

So the low range is quarter-sample resolution, the middle half-sample, the top
whole-sample, and the last arm saturates. The block's integer pitch is
`pitchQ >> 2` and its fractional phase is `pitchQ & 3`.

*Measured*: the three resolution arms are all reached on every cell that uses
this codebook; **the saturating fourth arm is never reached**. Implement it and
carry it as a deficit.

For the asymmetric codebook, `pitchQ` is simply `pitch[n] << 2`, so the
fractional phase is always zero there and section 9 takes a different path
anyway. For a comfort-noise or hardcoded frame there is no pitch at all, and the
value the postfilter is handed for such a frame is a sentinel it must not use
(section 10.1).

## 7. Gains

### 7.1 The silence gain

A comfort-noise frame (fixed codebook "silence") reads **8 bits** once per
frame, immediately after the frame type, and looks the value up in
`gainSilence`. The table runs from 1.9e-05 to 0.393 and is ascending
(*measured*). The gain applies to every block of the frame.

### 7.2 The hardcoded gain

A hardcoded frame reads, **per block**, 8 bits of codebook offset and then 6
bits of gain index, and looks the gain up in `gainUniversal` (0 to 0.222,
ascending). Section 11 says what the offset indexes.

### 7.3 The adaptive and fixed codebook gains

Every other block reads **one 7-bit index** after its pulses and gets both gains
from it:

```
acbGain = gainACB[index]                                  ; 0.05 to 1.36
fcbGain = exp( dot(gainPredErr, gainCoeff) - 5.2409161640 + gainFCB[index] )
```

with

```
gainCoeff = 0.8169, -0.06545, 0.1726, 0.0185, -0.0359, 0.0458
```

and `gainPredErr` a six-entry `float32` history. The dot product is accumulated
in `float32`; `exp` is evaluated in `float64` and narrowed. `gainFCB` runs from
-5.298 to 1.621, so it is a log-domain term and the predictor is a log-domain
moving average: this is the AMR-style fixed-codebook gain predictor.

After the gains are formed, push the block's contribution into the history:

```
predErr = clip(gainFCB[index], -2.9957322736, 1.6094379124)   ; log(0.05), log(5.0)
weight  = 8 >> log2Blocks                                     ; 4, 2 or 1
shift gainPredErr right by weight (dropping the last weight entries)
fill gainPredErr[0 .. weight-1] with predErr
```

so a frame with fewer, longer blocks writes each block's error into more history
slots and the predictor's time constant stays the same in samples. *Measured*
weights: 1, 2 and 4 all occur; 8 cannot, because the only one-block frame type
is comfort noise and that path never reaches here.

**The history is cleared to zero by every comfort-noise and hardcoded block.**
That is not a reset at frame boundaries: it happens inside the block, before the
excitation is built, in exactly those two frame types.

## 8. Pulse coding

The fixed codebook contribution of a block is a sparse set of unit pulses,
scattered into a zero vector of the block's length, then scaled by `fcbGain`.

A pulse set is described by, per pulse, a position, a sign, and a "repeat" flag.
Scattering is:

```
for each pulse i:
    x = position[i] ; y = sign[i]
    loop:
        out[x] += y
        x += pitchLag
        if x >= size or not repeat[i]: break
```

`pitchLag` is the block's integer pitch. A repeating pulse therefore lays a
comb at the pitch period. Positions are always inside the block; a position at
or past the block length is malformed.

### 8.1 Innovation pulses

Five pulses, plus up to five more. With `w = 5 - log2Blocks` (so 4, 3 or 2 bits
per position) and `D` the frame type's double-pulse count:

```
for n in 0 .. 4:
    sign = read(1) ? +1 : -1
    p1   = read(w)
    pulse at 5*p1 + n with sign, no repeat
    if n < D:
        p2 = read(w)
        pulse at 5*p2 + n with (p1 < p2 ? -sign : +sign), no repeat
```

Every pulse in this scheme is on the residue class `n` modulo 5, which is what
keeps the positions inside the block: the largest is `5*(2^w - 1) + 4`, which is
`size - 1` for every block size. **No pulse repeats.**

Note the second pulse of a pair takes the opposite sign when it lands after the
first and the same sign when it lands before or on it. This is the standard
ACELP two-pulse trick and it is the easiest thing in this section to get
backwards.

### 8.2 Pitch-adaptive window pulses

Used only by frame type 2, which is a two-block frame. The coding is in two
parts: coordinates read once per frame, and two pulse sets read per block.

**The coordinates**, read once per frame right after the frame type:

```
b = read(6)
if b >= 54:
    extended = true
    b += (b - 54) * 3 + read(2)
```

so `b` is 0 to 53 directly, or 54 to 93 through the extension, and it indexes
`awStartOffsets` (94 entries, ascending from -11 to 159). *Measured*: **the
extension never occurs**; every real frame reads a plain 6-bit value below 54.

From the two blocks' pitches `p0` and `p1`:

```
range = min(p0, p1) > 32 ? 24 : 16
offset = awStartOffsets[b]
while offset < 0: offset += p0
nPulses[0]   = (p0 - 1 + 80 - offset) / p0            ; integer division
firstOff[0]  = offset - range/2
offset      += nPulses[0] * p0
nPulses[1]   = (p1 - 1 + 160 - offset) / p1
firstOff[1]  = offset - (160 + range) / 2

if awStartOffsets[b] < 80:
    while firstOff[1] - p1 + range > 0:  firstOff[1] -= p1
    if awStartOffsets[b] < 0:
        while firstOff[0] - p0 + range > 0:  firstOff[0] -= p0
```

`nPulses[k]` can be zero or negative, which selects a different coding for that
block below. *Measured*: `range` is **24 on every frame in the corpus**, and
`nPulses` is never zero or negative. Both arms of the 16 case and both zero-count
arms are therefore untested.

**The first pulse set**, per block. Read 12 bits, or 10 bits when the extension
was used and this is block 0.

When `nPulses[block] > 0`, the field packs several pulses, most significant
first, and the packing depends on `range`:

```
range 24: 3 pulses, 4 bits each: 1 sign bit (mask 8) and 3 index bits (mask 7)
range 16: 4 pulses, 3 bits each: 1 sign bit (mask 4) and 2 index bits (mask 3)

for n = count-1 down to 0, taking the low bits of the field and shifting right:
    sign     = (field & signMask) ? -1 : +1
    position = (field & indexMask) * count + n + firstOff[block]
    while position < 0: position += pitchLag
    if position < 80: emit the pulse, repeating
```

A pulse whose position is still at or past 80 after the wrap is **dropped**, not
clamped. All pulses from this set repeat at the pitch lag.

When `nPulses[block] <= 0`, the same 12-bit field is read as a pair of
non-repeating pulses instead:

```
v    = (field & 0x1FF) >> 1
sign = (field & 0x200) ? -1 : +1
if      v < 79:  d = 1 ; i = v + 1
else if v < 156: d = 3 ; i = v + 1 - 77
else if v < 231: d = 5 ; i = v + 1 - 152
else:            d = 7 ; i = v + 1 - 225
pulse at i - d with sign, no repeat
pulse at i     with ((field & 1) ? -sign : +sign), no repeat
```

so the pair is separated by 1, 3, 5 or 7 samples and the second pulse's sign is
independent. *Measured*: never reached.

**The second pulse set**, per block, adds exactly one more pulse at a position
chosen from those the first set did not already cover.

Build an 80-bit exclusion mask, held most significant bit first, with two words
of padding in front of it because the walk below starts at a negative index.
Start with all 80 bits set (available). Then, when `nPulses[block] > 0`, walk
`idx` from the block's pulse offset upward in steps of `pitchLag` and clear
`range` consecutive bits from each `idx`:

```
pulseOff = firstOff[block]
if nPulses[block] > 0:
    while pulseOff + range < 1:  pulseOff += pitchLag

if nPulses[0] > 0:
    if block == 0: span = 32
    else:          span = 8 ; if nPulses[1] > 0: pulseOff = nextOffCache
else:              span = 16
pulseStart = nPulses[block] > 0 ? pulseOff - span/2 : 0

if nPulses[block] > 0:
    for idx = pulseOff; idx < 80; idx += pitchLag:
        clear bits idx .. idx + range - 1
```

The clearing walk can start at a negative `idx` (as low as `1 - range`), which
is what the two words of padding are for: those bits are cleared and never read.

Then read the index of which free position to take. The width is `4` when
`nPulses[0] <= 0`, and otherwise `5 - 2*block` (5 bits for block 0, 3 for block
1). Walk positions from `pulseStart` upward, wrapping any negative position
forward by `pitchLag`, and count the free ones:

```
n = 0 ; start = 0
for p = pulseStart; n <= wanted; p += 1:
    idx = p
    while idx < 0: idx += pitchLag
    if idx >= 80:
        pick the highest set bit of the first non-empty mask word,
        as (16*word + 15) - floorLog2(maskWord)
        if every word is empty: the block is unrecoverable, see below
    if bit idx is set:
        clear it ; n += 1 ; start = idx
```

Emit one pulse at `start`, repeating, with a sign from one more bit: **a set bit
is -1 here**. Then set the cache the next block uses:

```
r = (80 - start) mod pitchLag
nextOffCache = r != 0 ? pitchLag - r : 0
```

**If every mask word is empty** the block cannot be coded. The reference
conceals it: fill the block with comfort noise at the frame's silence gain
(section 11), skip **8 bits** so the next block starts at the right offset, and
carry on. *Measured*: never happens. Reproduce the concealment rather than
refusing, because refusing would be a hard failure on a stream the reference
plays.

**Sign polarity, stated once because it is the easiest thing to get wrong.** In
the innovation pulse scheme of 8.1 a **set** sign bit is **+1**. In both
pitch-adaptive window schemes a **set** sign bit is **-1**. They are opposite.
Write a test that would fail on a negated pulse train in either scheme.

## 9. Excitation and the synthesis filter

Per block, in this order.

**1. The fixed codebook contribution.** Scatter the pulses of section 8 into a
zero vector `pulses` of the block's length (or read the comfort-noise or
hardcoded codebook instead, section 11).

**2. The adaptive codebook contribution**, written **in place** into the
excitation buffer at the block's position. The excitation buffer carries
`historySamples` of past excitation in front of the current superframe, plus the
480 samples of the superframe itself, so a lag of up to `maxPitch` always has
something to read.

For the **asymmetric** codebook the pitch varies within the block and the block
is covered in runs:

```
n = 0
while n < size:
    abs   = block * size + n
    pSh16 = (pitchState << 16) + pitchSlope * abs
    p     = (pSh16 + 0x6FFF) >> 16
    iSh16 = ((p << 16) - pSh16) * 8 + 0x58000
    frac  = iSh16 >> 16
    if pitchSlope != 0:
        next = pitchSlope > 0 ? (iSh16 &~ 0xFFFF) : ((iSh16 + 0x10000) &~ 0xFFFF)
        run  = clip( trunc( trunc((iSh16 - next) / pitchSlope) / 8 ), 1, size - n)
    else:
        run = size
    interpolate(exc[n .. n+run-1], from exc[n - p ...], asymmetric filter, frac)
    n += run
```

`pitchState` is the pitch carried in from the previous frame (6.1), not this
frame's. `frac` lands in 1 to 9 inclusive; zero cannot occur and nine is the
top. Both divisions truncate toward zero.

The **asymmetric filter** is `interpAsym`, 153 entries laid out as nine groups
of seventeen. Group `k` holds seventeen phases of tap `k`, and the first entry
of every group is zero. The interpolation is

```
out[m] = sum over k in 0..8 of
             in[m + k]     * f[17*k + frac]
           + in[m - k - 1] * f[17*(k+1) - frac]
```

that is, nine taps forward at phase `frac` and nine taps backward at phase
`17 - frac`, both read out of the same 17-entry group. `in` is the excitation
`p` samples earlier, and it reads up to eight samples **ahead** of the current
position, which is why the excitation buffer needs twelve samples of slack past
the superframe.

For the **Hamming** codebook the pitch is constant across the block. With
`frac = pitchQ & 3` and `p = pitchQ >> 2`:

```
frac == 0: exc[m] = exc[m - p] for m = 0 .. size-1, in ascending order,
           so a lag shorter than the block repeats
frac != 0: out[m] = sum over k in 0..7 of
                        in[m + k]     * g[4*k + frac]
                      + in[m - k - 1] * g[4*(k+1) - frac]
           with in the excitation p samples earlier and g = interpHamming
```

`interpHamming` is 32 entries, eight groups of four; group `k` holds four phases
of tap `k` and its first entry is zero. The forward-copy case must be a forward
loop: with `p < size` it is deliberately overlapping.

**3. Mix.** The block's excitation is

```
exc[m] = acbGain * exc[m] + fcbGain * pulses[m]
```

which overwrites the adaptive contribution in place.

**4. Synthesise.** The block's output is the excitation through the all-pole
filter whose denominator is the LPC set of section 5.7, formed from LSFs
interpolated across the frame:

```
weight = (block + 0.5) / blockCount
lsp[n] = cos( prevFrameLSF[n] + weight * (thisFrameLSF[n] - prevFrameLSF[n]) )
lpc    = lspToLPC(lsp)

synth[m] = exc[m] - sum over j in 1..L of  lpc[j-1] * synth[m - j]
```

`prevFrameLSF` is the previous frame's set, or for the first frame of a
superframe the previous superframe's third-frame set (initialised as below). The
synthesis buffer carries `L` samples of history in front of the superframe.

**What carries across a superframe.** At the end of every superframe, keep:

| state | what it becomes |
|---|---|
| excitation history | the last `historySamples` samples of the superframe's excitation, that is the 480-sample window's tail |
| synthesis history | the last `L` samples of the superframe's synthesis output |
| previous LSF set | the **third** frame's set |
| pitch state, previous adaptive codebook kind | as section 6.1 leaves them after the third frame |
| gain predictor history | as section 7.3 leaves it after the last block |
| frame counter | incremented by three (11) |
| the postfilter's four memories | as section 10 leaves them (10.2's zero-synthesis buffer is shifted down by 480, and the gain control memory, the DC filter memory and the denoise overlap cache simply persist) |

Nothing else survives a superframe boundary. In particular there is no
reservoir, no window tail and no lookahead.

**Filter state at the start of a stream, and after a seek:** the excitation
history, the synthesis history, the gain predictor history, the postfilter's
zero-synthesis history, its gain control memory, its DC filter memory and its
denoise overlap cache are all zero; the pitch state is 40; the previous adaptive
codebook kind is "none"; the frame counter is zero at a stream start and
`3 * superframeIndex` after a seek (11, 13); and the previous LSF set is
`pi * (n + 1) / (L + 1)` for `n` from 0, which is an evenly spaced set spanning
the band.

**A note on the shape of the synthesis filter.** The reference's scalar path
computes this recursion four samples at a time with the coefficients
pre-combined, which is arithmetically the same recursion in a different
association order. A straightforward per-sample loop differs from it in the last
bits, and because the filter is recursive that difference accumulates over the
block. It is inside the tolerance quoted at the top of this file; it is also
most of that tolerance.

## 10. The postfilter

The flags word's bit 0 selects it, and *measured* it is set on all seven real
formats. With it clear the frame's output is the synthesis filter's output
verbatim. With it set the frame runs the chain below **twice, over 80 samples
each time**, and each half uses its own LPC set:

| half | samples | LSF set used for the LPC |
|---|---|---|
| first | 0..79 | `cos(0.5 * (prevFrameLSF[n] + thisFrameLSF[n]))` |
| second | 80..159 | `cos(thisFrameLSF[n])` |

So the postfilter's LPCs are the frame-centre and frame-end sets, not the
per-block ones.

### 10.1 What runs, and what selects it

| stage | runs when |
|---|---|
| zero synthesis (10.2) | always |
| Kalman smoothing (10.3) | the frame's fixed codebook is pulses of either kind, that is not silence and not hardcoded |
| re-synthesis (10.4) | always |
| Wiener denoise (10.5) | the frame's fixed codebook is **not** silence |
| the denoise overlap cache (10.5) | always, even for a silence frame |
| adaptive gain control (10.6) | always |
| DC removal (10.7) | the flags word's DC level is **greater than 8** |

The pitch handed to Kalman smoothing is `pitch[0]`, the **first block's** pitch.
A comfort-noise or hardcoded frame has no pitch, and the reference guards
against reaching the smoother with the sentinel it uses for that. Since the
smoother only runs for the two pulse-coded fixed codebooks, and those always
have a pitch, the guard is unreachable; keep the equivalent assertion anyway.

### 10.2 Zero synthesis

Run the block's LPC denominator as an **all-zero** filter over the synthesis
output, into a buffer that persists across superframes (it needs
`historySamples` of history in front of the current superframe, and it is
shifted down by 480 at the end of every superframe exactly as the excitation
history is):

```
zero[m] = synth[m] + sum over j in 1..L of  lpc[j-1] * synth[m - j]
```

This recovers an excitation-like signal from the synthesis output. Note it reads
`synth` history, not `zero` history: it is FIR, not recursive.

### 10.3 Kalman smoothing

Search the zero-synthesis history for the best-matching earlier segment and
blend toward it.

```
lo = max(minPitch, pitch - 3)
hi = min(maxPitch, pitch + 3)
best = none ; bestDot = 0
for lag in lo .. hi:
    d = dot(zero[0..79], zero[-lag .. -lag+79])
    if d > bestDot: bestDot = d ; best = lag
if best is none or bestDot <= 0: no smoothing, use zero[] unchanged
den = dot(zero[-best..], zero[-best..])
if den <= 0: no smoothing
f = den <= bestDot ? 0.625 : den / (den + 0.6 * bestDot)
out[m] = zero[m - best] + f * (zero[m] - zero[m - best])
```

`f` lands in 0.625 to 1.0, so the blend is at most 37.5 percent toward the
history. Every dot product accumulates in `float32`. *Measured*: the search
succeeds on almost every eligible frame; it fails 0 to 10 times per cell,
always in near-silent passages where the history is all zeros.

### 10.4 Re-synthesis

Run the same LPC denominator as an all-pole filter (the recursion of section 9)
over the smoother's output, or over the zero-synthesis output when the smoother
did not run, into a **separate** buffer that keeps `L` samples of its own
history from the previous half-frame.

The history is saved **before** the denoise filter runs, so what carries over is
the clean re-synthesis, not the denoised signal.

### 10.5 The Wiener denoise filter

The heart of the postfilter, and the part where a normalisation error does not
look like noise, it looks like a broken decoder. Everything below is
`float32` unless it says otherwise.

**Step 1: tilt the LPCs.** Build a 128-entry vector: 1.0, then the `L` LPC
coefficients, then zeros. Compute

```
tilt(a, n) = ( a[0] + dot(a[0..n-2], a[1..n-1]) ) / ( 1 + dot(a, a, n) )
```

on the LPC array, and apply a one-tap tilt filter to the first `L + 2` entries
of the vector with coefficient `0.7 * tilt(lpc, L)`:

```
tiltFilter(mem, t, s, n):
    newMem = s[n-1]
    for i = n-1 down to 1:  s[i] -= t * s[i-1]
    s[0] -= t * mem
    mem = newMem
```

with `mem` starting at 0 here.

**Step 2: the LPC power spectrum.** Take the forward 128-point real DFT
(defined below) of the tilted vector. Read 65 magnitudes out of it, in this
order, taking base-10 logarithms and tracking the running minimum and maximum
across all 65:

```
g[64] = log10( X[32].re * X[32].re )                      ; see the note below
g[n]  = log10( X[n].re^2 + X[n].im^2 )   for n = 1..63
g[0]  = log10( X[0].re * X[0].re )
```

**The `g[64]` line is not a typo and it is not the Nyquist bin.** The reference
reads the array position that the Nyquist bin would occupy under a packed
half-spectrum layout it no longer uses, which under the layout it does use is
the **real part of bin 32**. Nothing in reach can say what Microsoft's decoder
reads there. Matching the reference means reading bin 32's real part; see the
subsection at the end of 10.5.

Let `range = max - min` over the 65 values.

**Step 3: gains and phases per point.** With

```
irange   = 64 / range
gainMul  = range * (hardcoded frame ? 5/13 : 5/14.7)
angleMul = gainMul * (8 * ln(10) / pi)          ; evaluated in float64
```

for each `n` in 0..64:

```
i     = max(0, roundEven( (max - g[n]) * irange - 1 ))
pwr   = denoisePower[strength][i]
g[n]  = angleMul * pwr                                    ; g[] becomes a phase
x     = ( float64(pwr * gainMul) - 0.0295 ) * 70.570526123
x     = max(x, 0)
j     = int(x)
mag[n] = j > 127 ? energyTable[127] * pow(1.0331663, j - 127) : energyTable[j]
```

`roundEven` is round-half-to-even on the whole expression including the `-1`.
`pwr * gainMul` is a `float32` product, widened before the subtraction.
`70.570526123` is `1 / log10(1.0331663)`, which is why the fallback above index
127 continues the same geometric series.

**Step 4: the Hilbert-style phase shift.** Take the 64-point DCT-I of `g[0..63]`
and then the 64-point DST-I of that result. Call the outcome `h[0..63]`. The
value `h[64]` is discussed at the end of this section.

**Step 5: build the coefficient spectrum.** With a phase table `cosT`/`sinT` of
511 entries each (defined below) and

```
idx(v) = 255 + clip(int(v), -255, 255)
```

fill a 130-float interleaved half-spectrum `C`:

```
C[0]   = mag[0] * cosT[idx(h64)]
last   = mag[64] * cosT[idx(h64 - 2*h[63])]
for n = 63 down to 1, alternating:
    n odd :  a = idx(-h64 - 2*h[n-1])
    n even:  a = idx( h64 - 2*h[n-1])
    C[2n+1] = mag[n] * sinT[a]
    C[2n]   = mag[n] * cosT[a]
C[64]  = last
C[128] = C[129] = 0
```

The alternation starts at `n = 63` with the negated form and flips every step.
The walk must run **downward**, because `mag[n]` for the higher `n` still has to
be readable when the lower `n` write into indices `2n` and `2n+1`; the reference
does the whole thing in place in one array for that reason, and doing it in two
is equivalent.

Three indices are worth calling out. `C[1]` is never written: it is the
imaginary part of the DC bin, and the inverse transform of step 6 overwrites it
from `C[128]` before reading anything, so its value cannot matter. `C[128]` and
`C[129]` are the Nyquist bin and are **zero**, which is why the DC bin's
imaginary part ends up zero too. And `C[64]` is written last, after the `n = 32`
step already wrote there, so the 65th magnitude lands on bin 32's real part;
that is the same array-position confusion as `g[64]` in step 2, seen from the
other side, and it is reproduced here because reproducing it is what matches the
reference.

**Step 6: back to the time domain.** Take the inverse 128-point real DFT of `C`.
Zero everything from index `remainder` on, where

```
remainder = min(127 - size, size - 1) = 47   for the 80-sample halves
```

If the flags word's tilt correction bit is set, zero `coeffs[remainder - 1]` as
well and apply the tilt filter of step 1 to the first `remainder` entries with
coefficient `-1.8 * tilt(coeffs, remainder - 1)` and `mem` starting at 0.

Finally normalise to a fixed energy:

```
sq = (1/64) * sqrt( 1 / dot(coeffs, coeffs, remainder) )
coeffs[0..remainder-1] *= sq
```

so the impulse response has an L2 norm of exactly 1/64, and the transform pair
below supplies the other factor of 128.

The reference builds the spectrum in a buffer it seeds from the previous frame's
finished impulse response, which sounds like state and is not: every index that
is read back was written earlier in the same call, except the three called out
above, and none of those three can affect the output. Starting from a zeroed
buffer each call matches.

**Step 7: filter.** Zero the re-synthesis buffer from `size` to 127, take the
forward 128-point real DFT of it and of the 128-entry impulse response, multiply
them as complex numbers bin by bin (bins 1 to 64 as complex products; bin 0's
real and imaginary parts are multiplied separately, and its imaginary part is
zero on both sides so that product is zero), and take the inverse transform.

**Step 8: overlap-add.** The result is 128 samples long but the frame only wants
`size` of them. Merge and carry:

```
if cacheLen > 0:
    lim = min(cacheLen, size)
    out[0..lim-1] += cache[0..lim-1]
    cacheLen -= lim
    move cache down by size
if this is not a silence frame:
    lim = min(remainder, cacheLen)
    cache[0..lim-1] += out[size .. size+lim-1]
    if lim < remainder:
        cache[lim..remainder-1] = out[size+lim .. size+remainder-1]
        cacheLen = remainder
```

The cache is up to 160 entries and is zeroed at a decode start. A silence frame
runs the merge but contributes nothing new, which is how the tail of the last
denoised frame decays into a silent passage instead of clicking off.

#### The four transforms, and exactly what they compute

These were probed directly against the reference's transform library, with the
scale values the decoder configures. An implementation on this tree's `dsp/fft`
must reproduce these definitions, not merely "a DFT of the right size".

**Forward real DFT, 128 points, scale 1.0.** Unnormalised, `e^{-i}` kernel,
output as 65 complex values packed into 130 floats at `(2k, 2k+1)`:

```
X[k] = sum over t in 0..127 of  x[t] * exp(-2*pi*i*k*t/128)     k = 0..64
X[0].im = X[64].im = 0
```

**Inverse real DFT, 128 points, scale 1.0.** Unnormalised, so the round trip
multiplies by 128:

```
y[t] = X[0].re + (-1)^t * X[64].re
       + 2 * sum over k in 1..63 of ( X[k].re*cos(2*pi*k*t/128)
                                    - X[k].im*sin(2*pi*k*t/128) )
```

**DCT-I, 64 points, scale 1/64:**

```
y[k] = (1/64) * ( x[0] + (-1)^k * x[63]
                  + 2 * sum over t in 1..62 of x[t]*cos(pi*t*k/63) )   k = 0..63
```

**DST-I, 64 points, scale 1/64:**

```
y[k] = (2/64) * sum over t in 0..63 of  x[t] * sin(pi*(t+1)*(k+1)/65)  k = 0..63
```

All four were checked numerically against these closed forms to seven digits.

**The phase table.** 511 entries each, built from a half-sample-offset sine.
With `m = idx - 255` in -255..255:

```
cosT[idx] = cos( (|m| + 0.5) * pi / 512 )
sinT[idx] = sign(m) * sin( (|m| + 0.5) * pi / 512 )     ; sign(0) = +1
```

so the pair traverses a phase in `(-pi/2, pi/2)` and `idx` 255 is very nearly
zero phase. (The reference builds it as a quarter sine window mirrored twice,
which is the same thing; the closed form above is what it comes out to.)

#### The phase reference `h64`, which cannot be settled

Steps 4 and 5 above use a value called `h64`, and it is the one place in this
whole file where the reference and any independent implementation must disagree
unless the implementation goes out of its way.

The obvious reading is that `h64` is `g[64]`, the 65th phase from step 3,
untouched by the DCT-I and DST-I of step 4 because those transform 64 points and
`g[64]` is the 65th. That is what an implementation written from this
description will naturally do, and it is the only reading with a defensible
meaning.

It is not what the reference computes. The reference's DST-I writes past its 64
outputs into the caller's buffer, and what lands at index 64 is an internal of
the transform library: the imaginary part of bin 64 of the **unscaled 65-point
complex sub-FFT** its 130-point real DFT is built on. In closed form, with `s`
the DST-I input and `T` its 130-point antisymmetric extension
(`T[0] = T[65] = 0`, `T[i] = -s[i-1]` and `T[130-i] = s[i-1]` for `i` in 1..64):

```
h64 = sum over n in 0..64 of ( T[2n]*sin(2*pi*n/65) + T[2n+1]*cos(2*pi*n/65) )
```

*Measured*, on the whole corpus: the two readings differ by about 1 to 3 in a
quantity that is then truncated to an integer and used as a phase index, so they
shift every bin's phase by a step or two of `pi/512`.

| reading of `h64` | agreement with the reference's scalar decode, worst cell, whole file, with the superframe-cache defect of 3.3 taken out of the reference |
|---|---|
| the natural one (`g[64]` survives the transform) | max **6.4e-03**, RMS **5.7e-04** |
| the reference's (the closed form above) | max **6.5e-06**, RMS **1.5e-07** |

Both are audibly identical; the difference is at about -65 dB. **The
implementation session must choose with its eyes open.** Reproducing the closed
form is matching a transform library's buffer overrun, not the format, and it
should be commented as such if it is done. Not reproducing it costs three orders
of magnitude of differential headroom and turns the codec's gate from "the
oracle's own floor" into "a named tolerance of about 1e-02 peak". This file
recommends reproducing it, with the comment, because a loose gate hides real
bugs and there is no third implementation to appeal to.

### 10.6 Adaptive gain control

Restore the pre-postfilter energy, with a one-pole smoother whose memory
persists across half-frames:

```
speechEnergy    = sum |synth[m]|            over the 80 samples
postEnergy      = sum |denoised[m]|         over the 80 samples
g = postEnergy == 0 ? 0 : (1 - alpha) * speechEnergy / postEnergy
for m in 0..79:
    mem     = alpha * mem + g
    out[m]  = denoised[m] * mem
```

with `alpha = 0.99`. The sums accumulate in `float32`; the expression for `g` is
evaluated in `float64` and narrowed (`1 - alpha` in particular, where `alpha` is
the `float32` nearest 0.99). `mem` is zero at a decode start and converges
toward `speechEnergy / postEnergy` over about a hundred samples, so the control
is deliberately slow: it tracks the level, not the envelope.

### 10.7 DC removal

Only when the flags word's DC level exceeds 8. A second-order section applied in
place over the 80 samples, with memory that persists:

```
t      = gain * in[m] - pole0 * mem0 - pole1 * mem1
out[m] = t + zero0 * mem0 + zero1 * mem1
mem1   = mem0 ; mem0 = t

zero0 = -1.99997        zero1 = 1.0
pole0 = -1.9330735188   pole1 = 0.93589198496
gain  = 0.93980580475
```

A high-pass with its zero pair essentially on the unit circle at DC. *Measured*:
the DC level is 1 to 4 on all seven real formats, so **this filter never runs on
anything this tree can reach**. Implement it and carry it as a deficit.

## 11. Silence, comfort noise, and the generator that drives them

Two frame types build their excitation from a fixed 1000-entry codebook rather
than from pulses, and one of them chooses where to read from with a generator.

**Hardcoded frames** (type 1) read the offset from the bitstream: 8 bits per
block, giving 0 to 255, plus 6 bits of gain (7.2). The block's excitation is

```
exc[m] = stdCodebook[offset + m] * gain
```

The gain predictor history is cleared to zero first (7.3).

**Silence frames** (type 0) have one block of 160 samples, one 8-bit gain for
the frame (7.1), and **no offset in the bitstream at all**. The offset comes from
a generator:

```
x = 1877 * blockIndex + frameCounter
if x >= 0xFFFF:  x -= 0xFFFF
y = x mod 9
z = ( (x * 49995) / (5*y + 6) )  and 0xFFFF          ; integer divide, then truncate to 16 bits
offset = z mod (1000 - blockSize)
```

`blockIndex` is the index of the block within its frame, which is always 0 for a
silence frame but is not for the concealment path of section 8.2. `blockSize` is
160 for a silence frame. The `and 0xFFFF` is load-bearing: the numerator reaches
about 3.3e9 and the truncation to sixteen bits is what makes the sequence look
random. (The reference computes the division through a reciprocal table; that
form was checked against this one for every `x` in 0..65534 and they agree
exactly, so use the plain division.)

`frameCounter` counts **frames decoded since the decoder was started**, from 0,
incremented after every frame including silence and hardcoded frames, and
wrapping by subtracting 0xFFFF when it reaches 0xFFFF.

**This is the pseudo-random generator the analysis was asked to look for, and
three things follow from it.**

1. It is not random and it is not seeded from anything the encoder controls. It
   is a deterministic function of the frame index, so two decoders that agree
   on the frame index produce **identical** comfort noise. It is part of the
   format in the sense that Microsoft's decoder and the reference agree on it
   (*measured*: my decoder, which implements exactly the above, matches the
   reference's comfort noise to the same 6.5e-06 as everything else).
2. It makes the decoder's output depend on **where the decode started**. Restart
   at a media object boundary and the frame counter restarts at 0, so every
   silence frame after that point produces different noise, forever. Section 13
   measures it: with the counter left at 0 a resumed decode never becomes exact;
   with the counter set from the resume position it becomes exact within a
   handful of superframes. A decoder that wants a seek to be reproducible must
   therefore be able to **set its frame counter from the resume position**,
   which is `3 * superframeIndex` because a superframe is three frames.
3. Nothing about it is a level or a gain, so it is not the explanation for
   anything loud. The loudness the measurement set found in silent passages is
   the reference's packet-cache defect of section 3.3, not this.

## 12. Output level and delay

The decoder emits one channel of `float32` at a nominal full scale of 1.0.

**There is no decoder delay and no priming.** The first sample of the first
superframe is the first sample of the source, the total is exactly the source
length (3.4), and *measured* a best-fit lag search against Windows' own decoder
returns 0 on every cell. That is unusual for this tree and it is worth stating
plainly: no trim, no discard, no pre-roll at the start of a stream. After a
seek is a different matter (13).

**Nothing clips.** *Measured*: no sample of a correct decode of any cell exceeds
full scale, with a maximum absolute value of 0.876. The reference does exceed
it, up to 4.66, but only inside the superframes its cache defect corrupts.
Converting to integer PCM is still the usual multiply by `2^(depth-1)` with a
clip, and the clip is WaxFlow's convention rather than the format's.

## 13. Seeking, and what a resumed decode owes

*Measured*, with the analysis decoder restarted at every media object boundary
of four cells (47 resume points) and compared superframe by superframe against a
linear decode of the same file.

**Every media object is a decode start point.** There is no key-frame flag and
none is needed: discard the carry, skip the packet's spillover bits unread, and
begin at the first superframe that starts in the packet.

**The output grid.** A resumed decode's first output sample is the start of the
first superframe that begins in that packet, which is not the object's
presentation time: ASF states times in milliseconds and the superframe grid is
480 samples. The landing must be snapped to the grid, and the position the
decode actually produced is the one to report, never the one that was asked for.

**Convergence, with the frame counter restored** (that is, set to
`3 * superframeIndex` at the resume, per section 11):

| superframes until the resumed decode is exact and stays exact | resume points |
|---|---|
| 0 to 2 | 18 |
| 3 | 14 |
| 4 to 8 | 11 |
| 9 to 11 | 3 |
| 22 | 1 |

Relative RMS error against the linear decode, by superframe after the resume,
over all 47 points:

| superframe | median | 90th percentile | worst |
|---|---|---|---|
| +0 | 0.77 | 1.00 | 1.02 |
| +1 | 0.11 | 0.99 | 1.01 |
| +2 | 6.0e-05 | 0.98 | 1.04 |
| +3 | 3.2e-06 | 0.97 | 1.09 |
| +4 | 1.6e-06 | 0.37 | 1.00 |
| +5 | 3.6e-07 | 0.055 | 0.96 |
| +7 | 0 | 0.054 | 0.95 |

Read that as two populations. A resume into a transient, a silent passage or an
unvoiced passage converges to exact in two or three superframes. A resume into
**sustained voiced speech** does not: the adaptive codebook is a pitch-lag copy
of the decoder's own past with a gain near 1, so a wrong history persists, and
the worst point measured needed 22 superframes (0.48 s at 22050 Hz) to become
exact.

So the practical statement is:

- **Discard at least three superframes (1440 samples) after any resume.** That
  gets the median resume exact and the rest audibly right.
- **Do not assert bit-exactness after a fixed pre-roll.** Twenty-two
  superframes is the measured worst case on this corpus and it is not a bound.
- **Without the frame counter restored, a resumed decode is never exact**, not
  at any pre-roll, because comfort-noise frames keep drawing different noise.
  *Measured*: with the counter left at zero, 14 to 18 of the superframes in a
  resumed decode differ from the linear decode indefinitely; with it restored,
  2 to 4 do.

## 14. Everything the decoder must not try

Each of these is a shape the format allows and this tree cannot verify. Refuse
by name, with the reason, rather than producing plausible wrong audio.

### 14.1 A superframe whose speech/music bit is clear

Section 3.5. It is a WMA Pro payload inside a WMA Voice superframe. The
reference refuses it by that name and has never seen one; WaxFlow refuses it by
the same name. Do not try to hand the payload to `codec/wmapro`: the extra
bytes' WMA Pro block is 18 bytes where that decoder wants 18 **plus** a
`WAVEFORMATEX`, the packet layer is this one and not that one, and nothing in
reach could check the result.

### 14.2 Extradata that is not exactly 46 bytes

Section 1. Refuse at configuration time, before reading a bit.

### 14.3 A denoise strength of 12 or more

Section 1.1. The table has twelve rows; there is no row 12.

### 14.4 A sample rate outside 322 to 22097 Hz

Section 2. Below the floor the minimum pitch rounds to zero and the pitch fields
have no range; above the ceiling the excitation history the adaptive codebook
needs exceeds what the format's own history window can hold.

### 14.5 A variable bit mode class with more than three frame types

Section 1.2. The reference lets a fourth entry overwrite the next class's first
slot. Refuse instead.

### 14.6 A frame type VLC symbol that the tree does not map

Section 1.2. Symbols 17 to 21 are unmapped by every real extradata, and an
unmapped symbol means the tree and the stream disagree.

### 14.7 A superframe declaring more than 480 samples

Section 3.4. The field is 12 bits and the superframe is three 160-sample frames.

### 14.8 The structural refusals

- a packet shorter than its own header
- a superframe count field run that outruns the packet
- a superframe whose fields outrun the bits available to it (the reference
  resets its whole filter state and drops the superframe when this happens;
  refuse the packet instead and let the container resynchronise)
- a carry longer than the packet that produced it

### 14.9 What is **not** on this list

Do not refuse any of these; all are normal:

- a spillover count of zero with a non-empty carry (*measured*, 35 of 101 packets)
- a spillover count of zero with an empty carry
- a packet whose superframe count is 1, so the packet body decodes nothing and
  the whole packet becomes carry (*measured*)
- a carry above 2048 bits (*measured*, 18 times; it is the reference that
  cannot handle these, section 3.3)
- a statistics field being present
- a pitch field that decodes to `maxPitch` or above, which the clamp handles
- a pitch-adaptive window pulse whose position falls outside the block, which is
  dropped
- a pitch-adaptive window second set that finds no free position, which is
  concealed (8.2)
- an LSF set that needs stabilising
- flags-word bits 1, 11, 15 and 16 to 31 taking any value
- a decode that outruns or falls short of the container's declared duration
  (*measured*: ASF's declared duration is 7 to 30 samples **short** of the true
  length on every cell)

## 15. Implementability checklist

Each item is something the session that writes the decoder must be able to do
with this file, the corpus note and the generated tables alone. The section that
answers it is in brackets.

- [ ] Parse `WAVEFORMATEX` plus 46 extra bytes into the flags, the LSP order and
      the two LSP mode bits; deliberately ignore `nChannels`,
      `nAvgBytesPerSec`, `wBitsPerSample` and flags bits 1, 11 and 15 up. (1)
- [ ] Reproduce the seven-row table of section 1.3 from the committed corpus's
      extradata, as a test. (1.3)
- [ ] Build the variable bit mode tree and the fixed 22-codeword canonical VLC,
      and refuse a class with four entries. (1.2)
- [ ] Compute the pitch bounds and all seven derived widths from the rate, with
      no per-rate arm, and reproduce the four-row table of section 2. (2)
- [ ] Parse a packet header including the 63-escape run, and carry a **bit**
      string across packets whose length is not a multiple of 8. (3.1, 3.2)
- [ ] Get the superframe accounting right: `N - 1` from the body, the last one
      always carried, and a **flush at end of stream** that decodes the carry.
      Assert the superframe counts of section 3.4. (3.2)
- [ ] Size the carry buffer from `nBlockAlign`, not from 256 bytes, and do not
      reproduce the reference's truncation. (3.3)
- [ ] Read the optional 12-bit sample count and honour it, and assert that the
      total equals the source length on every committed cell. (3.4)
- [ ] Refuse a clear speech/music bit by name. (3.5, 14.1)
- [ ] Implement all seventeen frame type rows, including the four no cell
      reaches. (4)
- [ ] Implement the multi-stage LSP dequantiser once and drive all eight
      codebooks through it, in `float64`, with the base added per stage. (5.1)
- [ ] Implement both the residual and the independent LSP paths, and record the
      independent one as a deficit. (5.4)
- [ ] Implement stabilisation including the sort, and record that nothing
      reaches it. (5.6)
- [ ] Implement the LSP-to-LPC expansion and check it against a known LSF set
      before trusting anything downstream. (5.7)
- [ ] Implement both pitch codings, including the five-percent reset rule, the
      per-block interpolation, the clip that applies to the state and not the
      value, and all four arms of the conversion ladder. (6)
- [ ] Implement the gain predictor with its per-frame-type weight and its
      clearing by comfort-noise and hardcoded blocks. (7.3)
- [ ] Implement both pulse schemes, and **get the two opposite sign polarities
      right**; write a test that would fail on a negated pulse train in either.
      (8.1, 8.2)
- [ ] Implement the pitch-adaptive window exclusion mask with its two words of
      front padding, and the concealment path. (8.2)
- [ ] Implement both interpolation filters with the 17-and-4 group layouts and
      the forward/backward phase pairing. (9)
- [ ] Implement the per-sample pitch run-splitting arithmetic exactly, including
      both truncating divisions. (9)
- [ ] Implement the postfilter chain in order, with the two 80-sample halves and
      their two different LPC sets. (10)
- [ ] Implement the four transforms to the definitions in 10.5 and **check each
      against its closed form numerically** before trusting the postfilter.
- [ ] Decide on `h64` (10.5), write the decision and its reason in a comment,
      and set the gate from the matching row of that table.
- [ ] Implement the denoise overlap cache including the silence-frame merge. (10.5)
- [ ] Implement the comfort-noise generator exactly, including the truncation to
      sixteen bits, and expose the frame counter so a seek can set it. (11)
- [ ] Assert that the output has no delay and needs no trim at the start of a
      stream. (12)
- [ ] Seek to any media object, discard the carry, skip the spillover, set the
      frame counter from the landing, and pre-roll at least three superframes.
      (13)
- [ ] Refuse, by name and before reading a bitstream: extradata of the wrong
      size, a denoise strength above 11, a rate outside 322 to 22097 Hz, an
      absurd `nBlockAlign`, and an overfull variable bit mode class. (1.4, 14)
- [ ] Refuse, by name and mid-stream: a clear speech/music bit, an unmapped
      frame type symbol, a sample count above 480, and every structural
      condition in 14.8.
- [ ] Do **not** refuse anything in 14.9.
- [ ] Compare against the oracle with a **tolerance**, per the table in the
      corpus note's section 7, and skip the superframes the corpus note names.

Five things this file cannot settle, each with what to do instead:

1. **`h64` in the denoise filter** (10.5). Two readings, both stated, both
   measured; pick one, comment it, and size the gate from it.
2. **What `g[64]` should read** (10.5, step 2). The reference reads bin 32's
   real part where the Nyquist bin belongs. Reproduce it, because the gate is
   against the reference and nothing else can arbitrate.
3. **The independent per-frame LSP path** (5.4). No file in reach uses it.
   Implement it from the description and carry it as a deficit.
4. **Every row of the "the format allows" table in section 0 that no file
   reaches**: rates other than the four, the extended window index, the
   zero-count window branch, a window range of 16, the DC removal filter, frame
   types 8, 9, 10 and 14, the saturating pitch conversion arm, and LSP
   stabilisation. Implement each from the description here and record it as a
   deficit rather than refusing.
5. **Why Microsoft's decoder differs from the reference by 5 to 15 percent
   relative RMS everywhere** (corpus note, section 7). Measured and
   characterised, not explained. It is not a delay, not a level error, and not
   the cache defect. Nothing here can arbitrate, so the gate is against the
   reference and Windows is a sanity check only.

Anything not on this list that the implementation session finds it needs is a
gap in this file, and closing it means another analysis pass, not a peek at the
reference.

## 16. Where this file is weakest

Stated plainly so the implementation session knows where to expect trouble.

- **Section 8.2 is the least verified part of this file.** The pitch-adaptive
  window scheme is reached by exactly two cells and only through one of its
  arms: window range 24, positive pulse counts, no extended index, no
  concealment. The description of the other arms is a transcription of
  behaviour into prose that nothing decoded.
- **The postfilter's step 5 loop** (10.5) is index-heavy and the note's
  description of the alternating sign is the only statement of it. A decoder
  that gets the alternation backwards will still produce plausible speech.
- **The independent LSP path** (5.4) is described but never executed.
- **`h64`** is discussed at length above and remains a choice, not a fact.

## 17. The tables, and what is handed over

The tables are extracted from the reference's data header under a
`wmavoicetablesgen` build tag and emitted as five Go files, all in
`package wmavoice`. One array comes from the decoder source rather than the data
header because that is where it lives upstream. **Do not transcribe table
contents into this note**; what follows is the shape of each one so the
implementation session knows what it is holding.

| generated file | Go name | shape | what it is |
|---|---|---|---|
| `tables_lsp.go` | `dqLSP10i` | 3840 uint8 | the 10-coefficient independent codebook, 4 stages of 256, 64, 32, 32 vectors of 10 (5.2) |
| `tables_lsp.go` | `dqLSP10r` | 5120 uint8 | the 10-coefficient residual codebook, 3 stages of 128, 64, 64 vectors of 20 (5.4) |
| `tables_lsp.go` | `dqLSP16i1/2/3` | 1600, 960, 768 uint8 | the 16-coefficient independent codebooks for coefficients 0..4, 5..9 and 10..15 (5.3) |
| `tables_lsp.go` | `dqLSP16r1/2/3` | 1280, 1280, 1536 uint8 | the 16-coefficient residual codebooks for entries 0..9, 10..19 and 20..31 (5.4) |
| `tables_lsp.go` | `lsp10InterpA/B` | [32][2][10] float32 | the inter-frame interpolation weights, selected by the quantiser mode flag (5.4) |
| `tables_lsp.go` | `lsp16InterpA/B` | [32][2][16] float32 | the same for sixteen coefficients |
| `tables_lsp.go` | `meanLSF10`, `meanLSF16` | [2][10], [2][16] float64 | the mean LSF vectors, selected by the default mode flag (5.5) |
| `tables_gain.go` | `gainSilence` | 256 float32 | the comfort-noise gain, 8-bit index (7.1) |
| `tables_gain.go` | `gainUniversal` | 64 float32 | the hardcoded-block gain, 6-bit index (7.2) |
| `tables_gain.go` | `gainACB` | 128 float32 | the adaptive codebook gain, 7-bit index (7.3) |
| `tables_gain.go` | `gainFCB` | 128 float32 | the log-domain fixed codebook gain term, same index (7.3) |
| `tables_pulse.go` | `stdCodebook` | 1000 float32 | the comfort-noise and hardcoded codebook (11) |
| `tables_pulse.go` | `awStartOffsets` | 94 int16 | the pitch-adaptive window start offsets (8.2); **the one array taken from the decoder source** |
| `tables_interp.go` | `interpAsym` | 153 float32 | the asymmetric interpolation filter, 9 groups of 17 (9) |
| `tables_interp.go` | `interpHamming` | 32 float32 | the Hamming interpolation filter, 8 groups of 4 (9) |
| `tables_denoise.go` | `energyTable` | 128 float32 | the denoise magnitude table, `1.071575641632 * 1.0331663^(n-127)` (10.5) |
| `tables_denoise.go` | `denoisePower` | [12][64] float32 | the denoise power table, `((y + 6.9)/64)^(0.025*(x+1))` (10.5) |

The last two are smooth analytic functions and their closed forms are given
above, but they ship tabulated rather than computed so that the postfilter
agrees with the reference to the last bits of the table. That matters here more
than it would elsewhere, because both feed integer index lookups.

Everything else a decoder needs is computed, not tabulated:

| quantity | rule |
|---|---|
| the pitch bounds and every derived width | section 2, from the rate and `nBlockAlign` |
| the frame type descriptor table | section 4, seventeen fixed rows |
| the frame type codeword lengths | section 1.2, the fixed 22-entry list |
| the LSF dequantiser scales and offsets | sections 5.2 to 5.4, thirty constants |
| the gain predictor coefficients | section 7.3, six constants |
| the postfilter phase table | section 10.5, `cos`/`sin` of `(|m|+0.5)*pi/512` |
| the four transforms | section 10.5, closed forms |
| the DC removal section's coefficients | section 10.7, five constants |
| the comfort-noise generator | section 11, arithmetic |

## Addenda from the implementation pass (2026-09-11)

Corrections found while the decoder was written and reviewed. The body above
is left as the analysis pass wrote it, with these on top.

- **Tag 0x000B.** Section 0 and the table in section 1 say the tag is the same
  coded layer and that `container/asf` maps both tags to this codec. The
  mapping meant was the one before the decoder existed, under which both tags
  were refused by name. *Measured* since: ffmpeg 8.0.1 does not map 0x000B at
  all (ffprobe reports an unknown codec for a corpus file retagged to it, and
  ffmpeg finds no decoder), so no reference decoder could check a decode of
  one. Both the container and the codec refuse it by name, and "the same
  coded layer" stands as mmreg.h's naming rather than as anything verified.
- **The spillover width list** in section 2 read 12 for `nBlockAlign` 187,
  against its own next sentence; corrected in place to 11.
- **Interpolator taps and phases.** Section 4's "17-tap asymmetric sinc at
  nine fractional phases" and "4-tap Hamming-windowed sinc at eight
  fractional phases" read the two tables transposed. By the indexing of
  sections 9 and 6.2, the asymmetric interpolator is 18 taps (nine each way)
  at a phase taken from a 17-wide symmetric axis, and the rounding of
  section 9 (`0x6FFF`, then `0x58000`) confines that phase to 1..8: phase 0
  is unreachable by arithmetic, so the corpus note's "eight of the nine
  phases" is the whole reachable set rather than a coverage gap. The Hamming
  interpolator is 16 taps (eight each way) at the three fractional
  quarter-sample phases, phase 0 being a whole-sample copy.
- **Section 3.2's flush.** The decoder treats an empty packet as the flush
  the reference's end-of-stream call is, at any point in the stream, and a
  superframe count of zero as damage: the count is one more than the body
  yields, so zero describes no packet, and accepted it drops the body without
  a word. A count that claims a superframe the packet does not hold is damage
  when the walk runs out of payload at the superframe's first bit, and the
  WMA Pro refusal of section 3.5 otherwise, since padding read as a superframe
  looks exactly like one.
