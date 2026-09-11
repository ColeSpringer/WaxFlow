# WMA Lossless bitstream notes

ADR-0001 black-box analysis artifact. This file and
`wma-lossless-oracle-corpus.md` beside it are the **only** inputs the session
that writes the decoder consumes; that session does not open FFmpeg, and
neither file contains code from it.

Scope: `wFormatTag` 0x0163, Windows Media Audio 9.2 Lossless, one to eight
channels at 16 or 24 bits. WMA v1/v2 (0x0160, 0x0161) is a different codec that
shares only the container and the `WAVEFORMATEX`; `wma-bitstream.md` covers it
and nothing in that file carries over except the container plumbing. WMA Pro
(0x0162) and Voice are different again.

Like WMA v1/v2, this format has no published specification. Microsoft never
released one and every independent decoder descends from the same
reverse-engineering effort, so the layout below was recovered in this analysis
pass and then checked against the black-box behaviour of real files. Where a
statement is a measurement rather than a structural fact, it says so.

## The whole decode path is integer arithmetic

There is no floating point anywhere in this codec: no transform, no window, no
gain in decibels, no normalisation. Every field is an integer, every predictor
is a fixed-point integer filter, and the output is the reconstructed integer
sample. A conforming decoder is bit-exact against the encoder's input by
construction, and any deviation is a bug rather than a tolerance.

That is the strongest single fact in this file. It means the implementation
session can and should assert exact equality against a source PCM file, with no
epsilon anywhere, and it means the arithmetic details in sections 5 to 9 are
load-bearing to the last shift. Two shifts that differ only in rounding
direction are two different codecs here.

Every shift below is stated with its signedness, because the two kinds do not
agree on negative values:

| notation | meaning |
|---|---|
| `x >> n` (signed x) | arithmetic shift, equal to `floor(x / 2^n)`, rounds toward negative infinity |
| `x >>u n` (unsigned x) | logical shift, equal to `floor(x / 2^n)` on a non-negative value |
| `trunc(a/b)` | division truncating toward zero, so `trunc(-3/2) = -1` while `-3 >> 1 = -2` |
| `sign(x)` | 1, 0 or -1 |
| `clip(x, lo, hi)` | saturate to the closed range |
| `floorLog2(x)` | largest n with `2^n <= x`; **0 for x = 0** |
| `ceilLog2(x)` | smallest n with `2^n >= x`; 0 for x <= 1 |

`floorLog2(0) = 0` and `ceilLog2(1) = 0` are not conveniences. Several field
widths are computed from them, and **a width of zero means read no bits and use
zero**, not read one bit. A scaling field of 0 takes exactly that path in three
places, sections 6.1, 7.1 and 8.2, so a decoder that reads a bit there
desynchronises the rest of the subframe.

All bit reading is **most significant bit first**, bytes in file order. There
is no little-endian bit packing anywhere in the coded layer; the little-endian
fields are confined to the `WAVEFORMATEX` of section 1.

## Measurement basis

Everything marked *measured* below was taken on 2026-09-10 against files
produced by the only encoder for this format, Windows' own Media Format SDK
codec, driven by `scripts/wmfenc/wmfll.ps1`, and decoded by a reference build
pinned to the same commit `codec/wma/tablesgen_test.go` carries (FFmpeg n9.0,
`d32b387f2b0a484599d4587d651891f0c63c4238`; `libavcodec/wmalosslessdec.c` at
that commit has SHA-256
`37af74c8eb8216b394b7e61b1861b4bef051b9e18a7e5875eae58febf58b1454`).

Fifteen files were built to span the encoder's whole envelope: 44.1 kHz stereo
at 16 and at 24 bits, 48 kHz stereo at 24 bits, 48 kHz 5.1 at 24 bits, and 96
kHz stereo at 24 bits, and within those, mono-duplicated material, independent
stereo, digital silence, full-scale white noise, impulsive attacks, a fast
chirp, correlated and uncorrelated multichannel, and a noise-to-tone
transition. Every one of the fifteen decoded **bit-identical to the encoder's
input PCM**, at the exact source length, with no head trim and no tail
overhang.

That encoder offers eight formats and no more: 44.1 kHz stereo at 16 and 24
bits, and 48, 88.2 and 96 kHz stereo and 5.1 at 24 bits. There is **no mono
format**, no 16-bit format above 44.1 kHz and no 5.1 format at 44.1 kHz, so a
mono stream is reachable in the format and not producible anywhere in this
tree. `wma-lossless-oracle-corpus.md` section 5 lists the same envelope from
the corpus side.

The envelope those files actually occupy is much narrower than the format:

| field | what the only encoder writes |
|---|---|
| `cbSize` and the extra bytes | 18, always |
| decode flags | 0x01a1, on every file at every rate, depth and channel count |
| `nAvgBytesPerSec` | 144000, a placeholder, not a bit rate |
| subframes per frame | one, spanning the whole frame |
| tile-aligned flag | 0, even though the tiling that follows is aligned |
| arithmetic coding | never |
| AC filter | always on, order 1, scaling 6 |
| inter-channel decorrelation | on or off, per seekable tile |
| MCLMS | never, not even on 5.1 |
| CDLMS | one filter per channel, order 16 at 16 bits and 8 at 24 bits, scaling 12, initial coefficients never transmitted |
| moving-average scaling | 5 |
| quantisation step | 1 |
| raw PCM tile | only for material that does not compress |
| padding zeroes | 0, or 8 for 16-bit content carried in a 24-bit stream |
| LPC | never, although the flag that enables it is set |
| transient flag | never set |
| DRC gain | present in every frame, always 0 |
| start skip | never present |
| end skip | last frame only |

Read that table as a warning, not as a licence to implement only the middle
column. It says which parts of sections 5 to 9 can be verified against a real
file and which cannot, and every unverifiable part is called out where it
appears.

## 1. The WAVEFORMATEX and its extra bytes

`container/asf` hands the whole `WAVEFORMATEX` over as `Track.CodecConfig`: 18
fixed bytes up to and including `cbSize`, then the codec-specific extra bytes.
For this format `cbSize` is 18, so the blob is 36 bytes and anything shorter is
a refusal.

From the fixed part the decoder needs exactly one field:

| offset | field | use |
|---|---|---|
| 0 | `wFormatTag` | 0x0163 selects this codec |
| 2 | `nChannels` | see the channel-mask note below |
| 4 | `nSamplesPerSec` | sets the frame length, section 2 |
| 8 | `nAvgBytesPerSec` | **not used**, and measured to be a placeholder |
| 12 | `nBlockAlign` | the packet size in bytes, and the one input to every header field width |
| 16 | `wBitsPerSample` | **not used**; the depth comes from the extra bytes |
| 18 | `cbSize` | 18 |

`nAvgBytesPerSec` reads 144000 on every measured file whatever the real rate,
so nothing may be derived from it. This is the opposite of WMA v1/v2, where the
declared byte rate drives the coefficient books and the noise ladder. Here it
is decoration.

`nBlockAlign` is not a frame size. It is the size of one container packet, and
it is the sole input to the width of two header fields (section 2), so reading
it wrong desynchronises every packet in the file rather than producing slightly
wrong audio.

The extra bytes, at offsets relative to the start of the extra block (add 18
for an offset into the whole config blob), are little-endian:

| extra offset | width | field |
|---|---|---|
| 0 | 2 | valid bits per sample: 16 or 24, and nothing else |
| 2 | 4 | channel mask, `WAVEFORMATEXTENSIBLE` bit assignments |
| 6 | 8 | reserved, measured zero on every file |
| 14 | 2 | decode flags |
| 16 | 2 | reserved, measured zero on every file |

**Bit depth** comes from extra offset 0, not from `wBitsPerSample`. The two
agree on every measured file, but only the extra-bytes value is read by any
derived decoder, so that is the definition. 16 and 24 are the only values;
anything else is a refusal by name (section 10).

**Channel mask** at extra offset 2 gives both the channel count and the channel
order. When it is non-zero it overrides `nChannels`: the number of channels is
its population count and the channels appear in ascending mask-bit order, which
is the ordinary `WAVEFORMATEXTENSIBLE` order (front left, front right, front
centre, LFE, back left, back right, and so on). Measured: 0x0003 for stereo and
0x003f for 5.1, matching `nChannels` in both cases. A zero mask means fall back
to `nChannels` with an unknown layout.

The LFE index, when mask bit 3 is set, is the count of set bits among bits 0 to
3, minus one, which is 3 for a 5.1 mask. The reference computes it and never
uses it; it is stated here only so that an implementation does not go looking
for a use.

### 1.1 The decode-flags word

One 16-bit word, and every bit of it that matters:

| bits | name | effect |
|---|---|---|
| 0 | unassigned here | read as part of the word, no effect on decoding |
| 1-2 | frame-length adjustment | see section 2 |
| 3-5 | subframe depth | `maxSubframes = 2^((flags >> 3) & 7)`; more than 32 is a refusal |
| 6 | length prefix | each frame opens with its own length in bits, section 2 |
| 7 | dynamic range compression | each frame carries an 8-bit gain, read and discarded |
| 8 | LPC enabled | each coded subframe carries an LPC flag, section 10 |
| 9-15 | unassigned here | no effect |

Measured value 0x01a1 on every file: bit 0 set, subframe depth 4 so
`maxSubframes = 16`, **no length prefix**, DRC present, LPC enabled. The
absence of the length prefix is the single most consequential measurement in
this file, because it is what forces the deferred packet model of section 2.4.

Bit 8 does two unrelated things. It gates the per-subframe LPC flag, and it
selects which window of the CDLMS update history the update-speed rescale
touches (section 7.4). Both real values of the bit are described below, but
only the set case is measured.

### 1.2 What a decoder must refuse before reading a bit

- `nBlockAlign` zero, negative, or absurd. The reference accepts up to 2^21.
- A bit depth other than 16 or 24.
- More than 8 channels.
- A subframe depth field above 5, that is `maxSubframes` above 32.
- Fewer than 18 extra bytes.

None of these is a judgement call. Each is a shape for which no layout is
defined, and each has to be named here because `container/asf` validates none
of them.

## 2. Frame and packet geometry

### 2.1 Frame length

```
frameLenBits = 9                      rate <= 16000
               10                     rate <= 22050
               11                     rate <= 48000
               12                     rate <= 96000
               13                     otherwise

then, from decode-flags bits 1-2:
   value 1  ->  frameLenBits += 1
   value 2  ->  frameLenBits -= 1
   value 3  ->  frameLenBits -= 2
   value 0  ->  unchanged

samplesPerFrame = 1 << frameLenBits
```

The adjustment field is measured to be 0 on every real file, so the second half
of the rule is structural only. `samplesPerFrame` must not exceed 16384.

Measured: 2048 at 44.1 and 48 kHz, 4096 at 88.2 and 96 kHz, which is what the
rule gives and what `wma-lossless-oracle-corpus.md` section 6 measures
independently from decoded frame counts. Every frame in a stream is the same
length; there is no variable frame length and no equivalent of WMA v1/v2's
block-size ladder.

### 2.2 The two derived widths

```
frameSizeBits = floorLog2(nBlockAlign) + 4
minSubframeLen = samplesPerFrame / maxSubframes
```

`frameSizeBits` is the width of the packet header's continuation-count field
and of the optional per-frame length prefix. Measured 17 for `nBlockAlign`
13375 and for 12288, both of which floor to 13.

Note the `+ 4` rather than `+ 3`: the field is one bit wider than the packet
needs, which is what lets it express a count larger than the packet (section
2.3).

### 2.3 The packet header

`container/asf` delivers one media object per `ReadPacket`, exactly
`nBlockAlign` bytes, and marks every one a sync point. That object is one WMA
packet. It opens with, most significant bit first from byte 0:

| bits | field |
|---|---|
| 4 | packet sequence number |
| 1 | seekable frame in packet, section 4 |
| 1 | spliced packet |
| `frameSizeBits` | bits belonging to the previous frame |

Everything after those `6 + frameSizeBits` bits is payload. Call the payload
length in bits `P = 8 * nBlockAlign - 6 - frameSizeBits`, and call the
continuation count `N`.

The **sequence number** increments modulo 16 from packet to packet. A value
other than the expected one means one or more packets were lost. Measured to
increment cleanly across every packet of every file.

The reference's recovery from a sequence jump is **not** the same as its
recovery from a seek, and the difference is worth naming because it is the
weaker of the two. On a jump it discards the carry and resumes at the first
frame that begins in that packet, but it does **not** wait for a seekable tile,
so it keeps predicting from filter state that belongs to samples it never saw
and emits wrong audio until the next seekable tile arrives. On a seek it clears
the filter orders as well, which forces the wait of section 4. Waiting in both
cases is the sounder choice and costs at most the distance to the next seekable
tile, measured at about 0.4 seconds.

The **spliced-packet** bit is measured to be 0 on every packet of every file.
The reference warns and continues (section 10).

The **continuation count** N is the number of payload bits, counted from the
first payload bit, that belong to the frame which began in an earlier packet.
It takes exactly three shapes, all three measured:

| shape | meaning | measured count |
|---|---|---|
| `N == 0` | nothing is carried; a frame begins at payload bit 0 | 16 |
| `0 < N < P` | the first N payload bits finish the frame in progress; a new frame begins at payload bit N | 365 |
| `N >= P` | the entire payload continues the frame in progress and no frame begins here | 115 |

In the third shape the encoder writes `N = 8 * nBlockAlign`, the packet size in
bits, which is larger than P by the header width. That is why the field is a
bit wider than the packet: the "whole packet is continuation" case is expressed
by overshooting, and a decoder **must clamp N to P** rather than treat the
overshoot as damage. It is common: 115 of 496 packets across the corpus, and
111 of 182 packets in one 5.1 file, where a single frame spans two to three
packets.

`N == 0` is the first packet of every file, which is where nothing can be
carried, and it also occurred once mid-stream where a frame happened to end
exactly on a packet boundary. Mid-stream it means the stream is asserting that
nothing is in progress, so a carry that is still open there is stale and must
be **discarded unread** rather than decoded from. The reference drops it the
same way, by resetting its buffer at the end of the packet.

A frame is therefore not bounded by a packet in either direction. It may span
arbitrarily many, and the carry buffer has to be a bit string, not a byte
string: the join is bit-continuous with no padding and no realignment, so the
bit offset inside the first carried byte has to be remembered alongside the
bytes or the frame resumes off by up to seven bits.

### 2.4 The rule for joining a split frame

This is the part that decides the decoder's entire state design, so it is
stated twice: once as the model, once as the procedure.

**The model.** The frames form one continuous bit stream. Each packet carries a
contiguous slice of it, and the packet header says where inside the packet the
slice's first frame boundary falls. Nothing is aligned, nothing is padded, and
no frame carries its own length unless decode-flags bit 6 is set, which no real
encoder sets.

**The consequence.** Without a length prefix a frame's length is known only
after its last subframe has been parsed, and parsing needs every bit. So the
frames that begin in packet k cannot be decoded until packet k+1 has supplied
the bits that finish the last of them. A decoder handed one packet per call
therefore emits the frames of packet k on the call that delivers packet k+1.
That is a one-packet output latency and not a sample delay: no sample is
dropped, no sample is duplicated, and the frames come out in order.

**The procedure**, for a decoder that is handed one whole packet per call:

1. Parse the header. Clamp `N` to `P`.
2. If a carry is open, append the first `min(N, P)` payload bits to it.
3. If `N >= P`, the packet is finished. Emit nothing.
4. Otherwise the carry, if there was one, now holds every frame that began in
   the previous packet. Decode frames from it one after another until a frame's
   trailer bit reads 0 (section 2.5). That bit falls exactly at the end of the
   carry on a well-formed stream.
5. Start a fresh carry from payload bit `N` of this packet and keep it until
   the next call.
6. At end of stream, drain: decode frames from the carry until a trailer bit of
   0 or until the carry is exhausted. Without this every frame that begins in
   the last packet is lost, which on the corpus is between one and five frames
   depending on the packet size, so the drain is worth up to a fifth of a
   second of audio and is not optional.

What must be kept across a packet boundary is therefore: the carry bits and
their bit offset, the sequence number, and the whole filter state of sections 6
to 8. Filter state is **not** re-stated at a packet boundary. It is re-stated
only at a seekable tile (section 4), which is a property of a subframe and has
nothing to do with where packets fall.

The model also explains a measurement from the corpus side that otherwise looks
like muxer damage: `wma-lossless-oracle-corpus.md` section 6 finds that a
packet's presentation time is not a multiple of the frame length. It cannot be.
A packet's payload usually opens in the middle of a frame and the frames it
begins are emitted a packet later, so **no arithmetic turns a packet index or a
packet timestamp into the index of the first sample that packet produces**. The
only way to know where a decode has landed is to count what comes out, which is
the same conclusion that note's section 7 reaches from the outside.

### 2.5 Frame layout

A frame, read from the carry, is:

| order | field |
|---|---|
| 1 | frame length in bits, `frameSizeBits` wide, **only** when decode-flags bit 6 is set |
| 2 | the tile header, section 3 |
| 3 | DRC gain, 8 bits, only when decode-flags bit 7 is set; read and discarded |
| 4 | 1 bit: skip fields present |
| 5 | if present: 1 bit, then when set a start skip of `frameLenBits + 1` bits |
| 6 | if present: 1 bit, then when set an end skip of `frameLenBits + 1` bits |
| 7 | subframes, section 3, until the frame is full |
| 8 | **only when a length prefix is present**: one bit the reference skips without interpreting |
| 9 | the trailer bit |

Field 8 does not exist without a length prefix: the trailer bit then follows
the last subframe immediately. Since no real file carries a prefix, the
immediate form is the measured one.

A frame always produces `samplesPerFrame` samples per channel, less the end
skip.

**The skip fields.** The end skip is the number of samples to drop from the end
of this frame. Measured and confirmed by arithmetic on five files: the last
frame of `st16` carries 820, and 65 frames of 2048 less 820 is 132300, which is
exactly three seconds at 44.1 kHz; the 96 kHz file carries 512 on its last
frame, and 47 frames of 4096 less 512 is exactly two seconds. The start skip is
by symmetry the number of samples to drop from the head, but that is an
**inference**: the reference reads it and throws it away, and no measured file
carries one, so nothing confirms it. Neither field appeared on any frame but
the last across the whole corpus.

There is no decoder delay, no lead-in and no lookahead. An untrimmed decode of
a measured file begins on the source's first sample and ends, after the end
skip, on its last. That is the whole gapless story, and it is the opposite of
WMA v1/v2's situation in `wma-bitstream.md` section 11.

**The length prefix**, when present, counts the whole frame in bits including
itself, the unnamed bit and the trailer bit. The reference requires it to equal
exactly two more than the bits consumed through the last subframe, then skips
one bit and reads the trailer, so on a conforming stream there is exactly one
uninterpreted bit between the subframes and the trailer. The robust way to
consume it is to position at `frameStart + len - 1` and read the trailer there;
the arithmetic way is to skip one bit. They agree on everything the reference
accepts. No measured file carries a length prefix at all.

**The trailer bit** is 1 while more frames begin in the same packet and 0 on
the last frame that begins there. It is not "more frames in the stream": after
a trailer of 0 there are usually more payload bits, and those are the head of
the next frame, carried forward by step 5 of section 2.4.

## 3. Subframe layout

A frame's samples are split into subframes, called tiles in the flag names
below. The split may in principle differ between channels, and the tile header
codes it for all channels at once.

### 3.1 The tile header

```
1 bit    tile aligned
```

Let `fixedLayout` be true when `maxSubframes == 1` or the tile-aligned bit is
set. Then repeat the following until the running minimum reaches
`samplesPerFrame`. Track per channel a running sample count, starting at zero;
let `minLen` be the smallest of those counts and `atMin` be how many channels
sit at it, starting at the channel count.

1. For each channel whose count equals `minLen`: the channel takes this
   subframe unconditionally when `fixedLayout` is true, or `atMin == 1`, or
   `minLen == samplesPerFrame - minSubframeLen`; otherwise read **1 bit**
   saying whether it does. A channel whose count is above `minLen` does not
   take it and reads nothing. If no channel takes the subframe the frame is
   malformed.
2. Read the subframe length: if `minLen == samplesPerFrame - minSubframeLen`,
   the length is `minSubframeLen` and **no bits are read**; otherwise read
   `floorLog2(maxSubframes - 1) + 1` bits as a ratio r and the length is
   `minSubframeLen * (r + 1)`. A length below `minSubframeLen` or above
   `samplesPerFrame` is malformed.
3. Append the length to each channel that took it, and to that channel's
   running count; a count above `samplesPerFrame` is malformed, and more than
   32 subframes on one channel is malformed.
4. Recompute `minLen` and `atMin` over all channels.

The frame closes when every channel's count reaches `samplesPerFrame` exactly.
The per-channel subframe offsets are the running sums of that channel's
lengths.

Measured: `maxSubframes` is 16 and the ratio field is 4 bits wide, the
tile-aligned bit is **0** on every frame of every file, and the encoder then
writes a per-channel bit of 1 for every channel and a ratio of 15, producing
one subframe per channel spanning the whole frame. So the aligned case is what
is coded and the aligned flag is what is not set, and a decoder that only
handles `fixedLayout` would fail on every real file at step 1.

### 3.2 Subframe grouping, and the one place the reference cannot be followed

Subframes are decoded in the order the tiling produces them: the next subframe
is the one belonging to the channel with the fewest decoded samples, and it
covers every channel whose decoded count and next subframe length both match.
Call that set the group.

The reference then reads the whole subframe body, including the per-channel
coded flags of section 3.3 and the residual layer of section 5, **for every
channel in the stream**, at the group's subframe length, and writes output only
for the channels in the group. Those two are the same set only when the tiling
is aligned. On a stream where the tiling differs between channels they
disagree, and the reference's reads run off the intended layout.

Since the format admits a non-aligned tiling and the only reference cannot
decode one, a decoder should **refuse a frame whose per-channel subframe lists
are not identical**, by name, rather than guess. That refusal costs nothing
measurable: no encoder that exists produces such a frame.

### 3.3 The subframe body

In order, from the first bit of the subframe:

| order | field | width |
|---|---|---|
| 1 | seekable tile | 1 |
| 2 | when seekable: the filter definitions of section 3.4 | varies |
| 3 | raw PCM tile | 1 |
| 4 | when not raw PCM: coded flag, one per channel of the stream | 1 each |
| 5 | when not raw PCM and decode-flags bit 8 is set: LPC flag, and when set the LPC definition of section 10.2 | 1 + varies |
| 6 | padding present | 1 |
| 7 | when present: padding zeroes | 5 |
| 8 | the sample layer, section 5 | varies |

A subframe that is neither seekable nor a raw PCM tile, arriving before any
seekable tile has ever been seen, is undecodable: no filter has an order. That
is the seek condition of section 4, not damage.

In the coded path, `paddingZeroes` above the bit depth is malformed. In the raw
PCM path, `bitsPerSample - paddingZeroes` must be at least 1.

### 3.4 The filter definitions, present only on a seekable tile

A seekable tile first **clears all filter state**: AC-filter coefficients and
history, LPC coefficients, MCLMS coefficients and history, the CDLMS
coefficients, sample history and update history of every filter, and the
moving-average accumulator of every channel. Then, in order:

| order | field | width | value |
|---|---|---|---|
| 1 | arithmetic coding | 1 | refuse when set, section 10.1 |
| 2 | AC filter present | 1 | |
| 3 | inter-channel decorrelation | 1 | |
| 4 | MCLMS present | 1 | |
| 5 | AC filter definition, when present | | section 8.2 |
| 6 | MCLMS definition, when present | | section 6.1 |
| 7 | CDLMS definition, always | | section 7.1 |
| 8 | moving-average scaling | 3 | 0 to 7 |
| 9 | quantisation step | 8 | value + 1, so 1 to 256 |

and then **resets the ring positions and the transient state** of every filter
that the CDLMS definition just declared.

There is a wrinkle in the clearing order worth naming, because it is the kind
of thing that bites once and is then invisible. The reference clears the CDLMS
buffers for however many filters the *previous* seekable tile declared, since
the clear runs before the new count is read. If a later seekable tile declares
more filters than an earlier one, the newly added filters start from stale
state rather than from zero. Every measured file declares one filter per
channel for its whole length, so the two behaviours never differ there. A fresh
implementation should clear everything it is about to use; that is what the
reference means, and it matches the reference on every stream anyone has.

**Quantisation step above 1 is a lossy mode.** It multiplies every
reconstructed sample at the end of the chain (section 9). Measured to be 1 on
every file, which is what "lossless" means here, but the field exists and the
multiply has to be implemented or a stream that uses it decodes far too quiet.

## 4. The seekable-frame marker and what a seek may land on

Two different flags carry the word "seekable" and they are not the same thing.

**The subframe flag** (section 3.3 field 1) is the real one. A seekable tile is
where the filter definitions live and where all filter state is cleared, so it
is the only place a decoder that has no history can begin. A raw PCM tile is
the other such place, because it needs no history at all.

So: after a discontinuity, a decoder must discard frames until one whose
**first subframe** is a seekable tile or a raw PCM tile. The reference detects
the condition by noticing that no CDLMS filter has an order yet, drops the
whole frame, and treats the rest of the packet as lost.

**The packet-header flag** (section 2.3, the bit immediately after the sequence
number) is the index. The reference reads it and throws it away, so its meaning
cannot be read out of the reference at all. It was measured instead, by
attributing every decoded frame to the packet its first bit fell in across all
fifteen files:

> The flag is set on packet k exactly when a frame that **begins** in packet k
> carries a seekable tile or a raw PCM tile. Agreement 381 of 381 packets, with
> no exceptions in any file.

That makes a seek cheap and exact: scan packets forward from the landing point
to the first one whose flag is set, then start decoding at that packet's first
frame boundary, which is payload bit `N` of that packet. Measured seek-point
density, one flagged packet every one to seven packets on stereo material and
every 23 to 29 packets on the 5.1 file, which is roughly one every 0.4 seconds
of audio in both cases; the encoder emits a seekable tile on a time interval,
not a packet interval.

Because the reference ignores the flag, a decoder should treat a set flag as a
hint and still confirm by finding the tile, and should treat a clear flag as
authoritative only for skipping. Nothing is lost by scanning: a packet whose
flag is clear provably begins no decodable frame on every file measured.

What a seek must discard, in the shape of section 2.4: the carry, the first `N`
bits of the landing packet, and all filter state. Nothing survives, which is
the whole point of a seekable tile.

WaxFlow's demuxer marks every media object a sync point. For this codec that is
too generous by a factor of two to thirty: a landing on an unflagged packet
produces no output until the next flagged one. That is not damage and must not
be reported as such, exactly as `wma-bitstream.md` section 4 says of the
all-continuation superframe.

## 5. The entropy layer

Everything in this section is per channel, and the channels are coded one after
another: the whole of channel 0's sample layer, then the whole of channel 1's.
A channel whose coded flag is clear reads nothing and its subframe is set to
zeros.

### 5.1 The raw PCM escape

When the raw PCM bit of section 3.3 is set, there is no entropy coding, no
prediction, and no filter state update. Let `w = bitsPerSample -
paddingZeroes`, which must be at least 1. Read `channels * subframeLen` fields
of `w` bits each, each a two's-complement signed value, **channel-major**: all
of channel 0's samples, then all of channel 1's. Every channel of the stream is
present, whatever the coded flags say, because the coded flags are not read on
this path.

Those values are the subframe's samples. Sections 6, 7 and 8 do not run, the
quantisation step is not applied, and the output stage of section 9 shifts them
left by `paddingZeroes` exactly as it does a coded subframe.

Measured trigger: full-scale white noise, where 64 of 65 subframes were raw PCM
tiles. It is not a rare corner: a decoder that omits it silences any material
that does not compress.

The escape leaves a real question open. A raw PCM tile does not feed its
samples into the CDLMS or AC-filter history, so a following coded subframe that
is not itself seekable would predict from stale state. Measured across 1010
tile transitions in the whole corpus: **a raw PCM tile is never followed by a
coded tile that is not seekable**, so the encoder always re-establishes state
after a raw run and the question never arises. Whether that is a rule of the
format or a habit of one encoder is **not determined**. A decoder should follow
the reference and leave the history alone, and should not treat the combination
as damage if it ever appears.

### 5.2 The transient fields

Every coded channel's sample layer opens with:

```
1 bit    transient
if set:  floorLog2(subframeLen) bits    transient position
```

The reference reads both and **never uses either value** for anything. The bits
must still be consumed or everything after them desynchronises, but no
description of what they mean can be recovered, from the reference or from a
measurement: the flag is 0 on every subframe of every measured file, including
the impulsive and attack material built specifically to provoke it.

So this is a hole, and it is an honest one. If a real-world file sets the bit,
a decoder that follows this section stays in sync and produces whatever the
reference produces, which is the only behaviour anyone can check.

### 5.3 The seekable-tile seeds

Still per coded channel, and only when the subframe is a seekable tile:

```
aveMean0 = read bitsPerSample bits, unsigned
aveSum   = aveMean0 * 2^(movaveScaling + 1)

first sample = read (bitsPerSample + d) bits, two's complement signed
               where d = 1 when inter-channel decorrelation is on, else 0
```

The first sample is a literal, and the residual loop below starts at index 1
rather than 0. The extra bit under inter-channel decorrelation is there because
the decorrelated value needs one more bit of range than the sample does.

Both fields use the full `bitsPerSample`, **not** reduced by `paddingZeroes`.
On a 24-bit stream carrying 16-bit content that wastes eight bits per seekable
tile per channel; it is the format, not a misreading, and the measured files
confirm it by decoding exactly.

`aveSum` is the state of the adaptive Golomb parameter. Everything in 5.4 is
built on it.

### 5.4 The adaptive Golomb residual coder

For every remaining sample index of the subframe, in order:

**Step 1, the quotient.** Read a unary run: count the 1 bits until a 0 bit,
consuming the 0. Call the count `q`.

**Step 2, the escape.** When `q` reaches 32, read a 5-bit width field `w` and
then `w + 1` further bits as an unsigned value, and add that value to `q`. The
escape therefore codes `32 + extra`. Measured: escapes occur on ordinary
material, with `w + 1` running from 1 to 21 bits.

A run longer than 32 ones is not something any encoder writes. The reference
counts the run without a bound and then applies the escape to whatever it
counted, which means a hostile stream of 1 bits spins until the buffer is
exhausted. A decoder should stop the run at 32 and treat a longer one as
malformed. That is a reader's choice, not a format constant, and it diverges
only on streams no encoder produces.

**Step 3, the Golomb parameter.** From the current state:

```
aveMean = (aveSum + 2^movaveScaling) >>u (movaveScaling + 1)
```

which is `aveSum / 2^(movaveScaling+1)` rounded to nearest, half up.

**Step 4, the remainder.**

```
if aveMean <= 1:
    u = q
else:
    k = ceilLog2(aveMean)
    r = read k bits, unsigned
    u = q * 2^k + r
```

`u` is the unsigned, zigzag-interleaved residual.

**Step 5, the state update, which uses `u` before the zigzag is undone.**

```
aveSum = aveSum + u - (aveSum >>u movaveScaling)
```

That is an exponential moving average with time constant `2^movaveScaling`,
tracking the **magnitude** of the interleaved value. Doing it after the zigzag,
on the signed residual, is the single easiest way to get this wrong: it would
track something that averages to zero and the Golomb parameter would collapse.

`aveSum` is an unsigned 32-bit quantity in the reference and is not saturated.
A crafted stream can wrap it. A decoder should keep the same width so it wraps
the same way, or bound the residual, but it must not silently promote to 64
bits and diverge.

**Step 6, undo the zigzag.**

```
residual = (u >>u 1) XOR (-(u AND 1))
```

that is, `u/2` for even `u` and `-(u+1)/2` for odd `u`, the ordinary
unsigned-to-signed interleave.

The result is the subframe's residual sample at that index. Sections 6 to 8
then turn residuals into samples.

### 5.5 Bits per sample and the quantisation step

`bitsPerSample` from section 1 sets the width of the seekable-tile seeds, the
width of a raw PCM field before padding, and the clipping range of every filter
history in sections 6 and 7, which is `[-2^(bitsPerSample-1),
2^(bitsPerSample-1)-1]`.

`paddingZeroes`, read per subframe, is the count of low-order zero bits the
samples are known to have. It narrows a raw PCM field and it is the final left
shift of section 9. It is **not** applied to the seeds of 5.3 and it does not
narrow anything in the coded path. Measured 8 on almost every subframe of a
24-bit stream carrying 16-bit content, with a single subframe at 0 in three of
those files, and 0 on every subframe of genuine 24-bit content. It is a
per-subframe decision and not a stream property, so a decoder must read it
every time rather than latch it at the first seekable tile.

`quantStep`, read per seekable tile, multiplies every sample at the very end of
the chain (section 9). Measured 1 everywhere.

## 6. The MCLMS filter

A single sign-sign LMS predictor across all channels at once, running over the
whole subframe sample by sample. It runs after the per-channel CDLMS cascade
and before the inter-channel decorrelation, and only when its flag is set.

**It is never set by the only encoder that exists**, on stereo or on 5.1, with
correlated or uncorrelated channels. Everything in this section is therefore
structural, recovered from the reference and unverified against any file. It is
described completely so that a decoder can implement it, but an implementation
should expect its first real test to be a file from somewhere else.

### 6.1 The definition

| order | field | width | value |
|---|---|---|---|
| 1 | order | 4 | `2 * (value + 1)`, so 2 to 32 |
| 2 | scaling | 4 | 0 to 15 |
| 3 | coefficients transmitted | 1 | |
| 4 | when transmitted: field width | `ceilLog2(scaling + 1)` | `value + 2` |
| 5 | when transmitted: `order * channels^2` coefficient fields | width from 4 | |
| 6 | when transmitted: the current-sample coefficients, `channels*(channels-1)/2` fields | width from 4 | |

Field 4 is read as zero bits when `ceilLog2(scaling + 1)` is 0, that is when
scaling is 0.

The field-5 coefficients form, for each channel `c`, a block of
`order*channels` values indexed by history position; block `c` starts at offset
`c * order * channels`.

The field-6 coefficients are the strictly lower triangle, read row by row: for
channel `c` from 1 upward, `c` values for channels 0 through `c-1`. The
coefficient for the pair `(c, j)` sits at index `c * channels + j`.

When the transmitted bit is clear, every coefficient starts at zero and the
filter adapts from nothing.

**The stored coefficient width.** The reference holds these in signed 16-bit
slots, so a field wider than 15 bits is reduced modulo 2^16 and reinterpreted
as two's complement. The width can reach 17. Whether that wrap is intended or
is an artefact of the reference cannot be settled here; it cannot be settled by
measurement either, because the transmitted bit is never set.

### 6.2 The prediction, once per sample index i

For each channel `c` in ascending order:

```
if channel c is not coded:  pred[c] = 0, and nothing is added
otherwise:
    pred[c] = sum over h in [0, order*channels) of
                  history[recent + h] * coeff[c*order*channels + h]
            + sum over j in [0, c) of
                  residual[j][i] * curCoeff[c*channels + j]
    pred[c] = (pred[c] + (2^scaling >> 1)) >> scaling
    residual[c][i] = residual[c][i] + pred[c]
```

Both sums accumulate in 32 bits and wrap there. The bias `2^scaling >> 1` is
`2^(scaling-1)` for scaling at least 1 and 0 for scaling 0. The final shift is
arithmetic.

The second sum is what makes this a *multichannel* predictor: channel `c`
predicts partly from the **already reconstructed** samples of channels below it
at the same index, which is why the channel loop must run in ascending order
and must not be reordered.

From here on `residual[c][i]` means the value in the array, which after the
line above is the reconstructed sample and before it is the residual. Section
6.3 uses the reconstructed value throughout.

### 6.3 The update, once per sample index i, after the prediction

The prediction error of channel `c` is the reconstructed value minus the
prediction, which is the residual as it stood before 6.2 added anything:

```
err[c] = residual[c][i] - pred[c]
```

Then for each channel `c`:

```
if err[c] > 0:
    coeff[c*order*channels + h] += update[recent + h]   for every h
    curCoeff[c*channels + j]    += sign(residual[j][i]) for every j < c
if err[c] < 0:
    the same two, subtracting
if err[c] == 0:
    nothing
```

Then the histories advance, **from the highest channel index down to the
lowest**:

```
for c from channels-1 down to 0:
    recent = recent - 1
    history[recent] = clip(residual[c][i], -2^(bitsPerSample-1), 2^(bitsPerSample-1) - 1)
    update[recent]  = sign(residual[c][i])
```

so that after the loop `history[recent]` is channel 0's newest sample and the
window `history[recent .. recent + order*channels - 1]` holds the last `order`
sample instants, most recent first, channel 0 first within an instant.

`recent` starts at `order * channels` and walks down. When it reaches 0, copy
`history[0 .. order*channels-1]` up to `history[order*channels ..]` and the
same for `update`, then set `recent` back to `order * channels`. That keeps the
window contiguous; a modular ring would need a different indexing and would not
match.

Both `history` and `update` are cleared, and `recent` re-seeded, at every
seekable tile.

## 7. The CDLMS filters

Per channel, a **cascade** of up to eight sign-sign LMS predictors. This is the
workhorse of the codec: it is on in every measured file and it is the only
predictor besides the order-1 AC filter that any real file uses.

### 7.1 The definition

One bit first, shared by all channels, saying whether initial coefficients are
transmitted. Then per channel, in channel order:

| order | field | width | value |
|---|---|---|---|
| 1 | filter count | 3 | `value + 1`, so 1 to 8 |
| 2 | one order per filter | 7 each | `8 * (value + 1)`, so 8 to 1024; above 256 is malformed |
| 3 | one scaling per filter | 4 each | 0 to 15 |
| 4 | when transmitted, per filter: the coefficient block below | | |

The coefficient block, per filter:

```
count  = read ceilLog2(order) bits, + 1        so 1 to order
width  = read ceilLog2(scaling + 1) bits, + 2  read as 0 bits when that is 0
then   count coefficient fields of `width` bits each
```

Coefficients beyond `count` stay zero. When the shared bit is clear no
coefficients are read and all of them start at zero.

For an order that is not a power of two the `count` field can express a value
above the order, since its width is the ceiling of the logarithm. The reference
does not check it. A decoder should bound `count` to the order rather than
write past the coefficient array.

Measured: one filter per channel, order 16 at 16 bits and order 8 at 24 bits,
scaling 12, and the shared transmitted bit **clear on every seekable tile of
every file**.

**The transmitted-coefficient scaling is undetermined and should be refused.**
The reference forms the stored value by placing the `width`-bit field in the
top of a 32-bit word and shifting right by `30 - scaling` without sign
propagation, then keeping the low 16 bits as a signed value:

```
stored = signed16( (raw * 2^(32 - width)) >>u (30 - scaling)  mod 2^16 )
```

The reading that makes the field a coefficient instead is to sign-extend first
and then rescale:

```
stored = signExtend(raw, width) * 2^(scaling + 2 - width)
```

where a negative exponent means an arithmetic right shift. The two agree
exactly when `scaling == 14`, because only then does the field's sign bit land
on bit 15 where the 16-bit truncation can act as the sign. For every other
scaling the first reading returns a positive value where the second returns a
negative one. Since no encoder sets the transmitted bit, neither reading can be
confirmed, and in a lossless codec a wrong initial coefficient is not a small
error. A decoder should refuse a stream that transmits CDLMS coefficients, by
name, and say that the scaling is undetermined.

### 7.2 The cascade

For a coded channel, run the filters **from the highest index down to index
0**, and run each one over the **whole subframe** before starting the next.
That ordering is not cosmetic: each filter consumes what the one after it
produced.

For filter `f` and sample index `i`:

```
residue = residual[i]
dot     = sum over h in [0, order) of coeff[h] * history[recent + h]
pred    = (2^scaling >> 1) + dot                 accumulated and wrapped in 32 bits
sample  = residue + (pred >> scaling)            arithmetic shift
```

and, interleaved with the dot product, the coefficient update:

```
coeff[h] += sign(residue) * update[recent + h]   for every h
```

The dot product must use the coefficients **as they were before this sample's
update**. The reference reads each coefficient and then writes it in the same
pass, which has that effect; computing the whole dot product first and then
applying the whole update has the same effect and is the clearer way to say it.

Then `sample` replaces `residual[i]` and the history advances by 7.3. The next
filter down sees the updated array.

The dot product runs over exactly `order` taps. The reference's 16-bit path
pads the length up to a multiple of 16, which is a defect and is named in
section 10.5.

### 7.3 The history advance, once per sample per filter

```
if recent > 0:  recent = recent - 1
else:
    copy history[0 .. order-1] to history[order .. 2*order-1]
    copy update [0 .. order-1] to update [order .. 2*order-1]
    recent = order - 1

history[recent] = clip(sample, -2^(bitsPerSample-1), 2^(bitsPerSample-1) - 1)
update[recent]  = sign(sample) * updateSpeed

update[recent + (order >>u 4)] = update[recent + (order >>u 4)] >> 2
update[recent + (order >>u 3)] = update[recent + (order >>u 3)] >> 1

every entry of `update` from index recent + order upward is zero
```

`recent` starts at `order` at a seekable tile, so the first window is entirely
zeros and the first advance moves it to `order - 1`.

The two shifts are the update-magnitude decay, and they are arithmetic shifts,
so a magnitude of 1 with a negative sign stays at -1 rather than reaching 0. An
update therefore enters the window at `±updateSpeed`, is quartered once it is
`order/16` samples old, and halved again once it is `order/8` samples old,
settling at about an eighth of its initial magnitude for the rest of its life
in the window.

For `order == 8` the first of those two indices is `recent` itself, so the
newest update is quartered in the same step that writes it. That reads like a
bug and is not one: the 24-bit measured files all use order 8 and decode
bit-exactly, so it is the format.

The sample history is held at the natural width for the bit depth, 16-bit slots
at 16 bits and 32-bit slots at 24 bits. The clip above already restricts the
values to that range, so the width is a storage detail and not a second
truncation.

### 7.4 The update speed

`updateSpeed` is per channel, not per filter, and it is **not** cleared at a
seekable tile. It takes two values:

- 16 on a subframe that is a seekable tile,
- 8 on a subframe that is not.

The switch happens per channel, between that channel's residual decode and its
CDLMS cascade, and switching **rescales the stored update history** of every
filter of that channel:

- switching to 16 from anything else: every entry of the window is doubled;
- switching to 8 from anything else: every entry of the window is **divided by
  two truncating toward zero**, which is not the same as an arithmetic shift
  and differs on every negative odd value;
- switching to the value already in force: nothing happens.

The window that is rescaled is `update[recent .. recent + order - 1]` when
decode-flags bit 8 is set, and `update[0 .. order - 1]` when it is clear. Bit 8
is set on every measured file, so the first form is the measured one and the
second is structural only.

`updateSpeed` starts at neither value, so the first seekable tile of a stream
always takes the rescale branch, harmlessly, over a window that tile has just
zeroed. After that it persists: it is not cleared by a seekable tile and not
cleared by a seek, so its value at any point is a function of whether the last
subframe was seekable and nothing else.

## 8. Channel transforms and the AC filter

Three stages run after the per-channel CDLMS cascade, in this order, each gated
by its own flag from the seekable tile:

1. MCLMS, section 6, when its flag is set.
2. Inter-channel decorrelation, 8.1, when its flag is set.
3. The AC filter, 8.2, when its flag is set.

and then the quantisation step of section 9. The order is fixed and reordering
any two of them produces different samples.

### 8.1 Inter-channel decorrelation

A reversible two-channel lifting step, and nothing else. There is no rotation
and no matrix.

It runs only when the stream has **exactly two channels** and at least one of
them is coded. For every sample index `i` of the subframe, in order, the two
assignments in this order:

```
a[i] = a[i] - (b[i] >> 1)
b[i] = b[i] + a[i]
```

with the shift arithmetic and the second line using the value the first line
just wrote. `a` is channel 0 and `b` is channel 1.

Two consequences worth stating because they decide what near-mono material
sounds like:

- **Only channel 0 coded.** Channel 1 is zeros, so `a` is unchanged and `b`
  becomes a copy of `a`. Both outputs are the coded channel. This is the common
  case for mono-in-stereo material and is measured on two files.
- **Neither channel coded.** The transform does not run at all and both
  channels stay silent. Measured on the digital-silence file, which decodes to
  exact silence.

A stream that does not have exactly two channels never runs this stage even if
the flag is set. The flag is not inert in that case, though: it still widens
the seekable tile's first-sample field by one bit (section 5.3), on a mono or
5.1 stream as much as on a stereo one. Reading it as "stereo only" and skipping
the extra bit desynchronises the residual layer at the first seekable tile.

### 8.2 The AC filter

A short FIR predictor over the channel's own reconstructed samples, with
history that survives across subframes. Its definition, on a seekable tile:

| order | field | width | value |
|---|---|---|---|
| 1 | order | 4 | `value + 1`, so 1 to 16 |
| 2 | scaling | 4 | 0 to 15 |
| 3 | `order` coefficients | `scaling` each, read as 0 bits when scaling is 0 | `value + 1`, so 1 to 2^scaling |

The coefficients are **unsigned**, at least 1, never 0. The reference stores
them in signed 16-bit slots, so scaling 15 with the maximum field wraps to a
negative value; the measured scaling is 6 everywhere, so the wrap is a corner
no file reaches.

Per channel, over the subframe, with `x` the channel's sample array and `prev`
the carried history:

```
for i in [0, subframeLen):
    pred = sum over j in [0, order) of coeff[j] * xAt(i - j - 1)
    x[i] = x[i] + (pred >> scaling)

where xAt(n) = x[n]                for n >= 0
             = prev[-n - 1]        for n < 0
```

The sum accumulates and wraps in 32 bits and the shift is arithmetic. Note that
`x[i]` is updated in place, so the predictor for index `i+1` sees the
reconstructed `x[i]`, not the residual.

Then the history is refreshed, most recent first:

```
prev[j] = x[subframeLen - 1 - j]            for j < subframeLen
prev[j] = old prev[j - subframeLen]         for j >= subframeLen
```

so `prev[0]` is the last sample of the subframe, `prev[1]` the one before it,
and a subframe shorter than the filter order shifts the older entries down
instead of discarding them. The refresh has to be computed from the highest
index down, or the shifted-down entries read values that have already been
overwritten.

`prev` is cleared at every seekable tile, along with the coefficients.

Measured: order 1, scaling 6, on every seekable tile of every file. So the sum
is one term and the history is one sample, and the branch above for the general
order is structural only. It is written out in full because nothing says a
different encoder will stay at order 1.

## 9. Output reconstruction

After the chain of sections 5 to 8, and only on the coded path:

```
if quantStep != 1:
    every sample of every channel is multiplied by quantStep
```

Then for every channel in the current subframe's group, and only those
channels, `subframeLen` samples are appended to that channel's output for the
frame at that channel's subframe offset:

```
sample = value << paddingZeroes
```

held at the stream's bit depth. That is the whole of the output stage. There is
no dither, no clipping, no gain, and no bit-depth conversion.

At **16 bits** the result is a 16-bit signed sample. The reference truncates to
16 bits both before and after the shift, so a value that does not fit comes
back wrapped rather than clipped; on a conforming stream nothing overflows and
the truncation is invisible.

At **24 bits** the result is a 24-bit signed sample. The reference presents it
in the top 24 bits of a 32-bit container, that is multiplied by a further 256,
which is an FFmpeg presentation convention and not part of the format. The
format's sample is the 24-bit value.

**Multichannel** output is per channel, in the channel-mask order of section 1.
Channels are entirely independent at this stage: each has its own subframe
list, its own offsets and its own history, and the only stages that mix them
are MCLMS and the two-channel lifting. A frame produces `samplesPerFrame`
samples on every channel, less the frame's end skip, and the subframe offsets
tile that span exactly.

A subframe that fails to decode truncates the frame to whatever was complete
before it. That is the reference's damage behaviour and it is a reasonable one
to copy.

## 10. Everything the decoder must not try

Each of these is a shape a decoder should refuse **by name**, with the reason,
rather than decode approximately. Every one of them is either undescribed or
described only by an implementation that is known to get it wrong.

### 10.1 Arithmetic coding

A seekable tile carries a flag, the first bit after the seekable bit itself,
that selects an arithmetic coder in place of the Golomb scheme of section 5.4.
The reference refuses it outright as unimplemented, so **no description of it
exists anywhere**, in the reference or in this file. There is nothing to write
down and nothing to infer: the bits after that flag mean something else
entirely and the reference has never parsed them.

Measured never set. Refuse it as an unsupported feature, not as damage: a
stream that uses it is well-formed and we are not.

### 10.2 The LPC mode behind decode-flags bit 8

When decode-flags bit 8 is set, every subframe that is not a raw PCM tile
carries an LPC flag after the per-channel coded flags. When that flag is set,
the subframe carries an LPC definition:

| order | field | width | value |
|---|---|---|---|
| 1 | order | 5 | `value + 1`, so 1 to 32 |
| 2 | scaling | 4 | 0 to 15 |
| 3 | integer bits | 3 | `value + 1`, so 1 to 8 |
| 4 | `channels * order` coefficients | `scaling + integerBits` each | two's complement signed |

The reference reads all of that, which keeps the bitstream in sync, and then
**never applies the filter**. It warns "Expect wrong output since inverse LPC
filter" and continues, producing samples that are simply wrong.

That has a specific consequence worth stating plainly, because it changes what
testing can prove: **for a stream that sets the LPC flag, FFmpeg is not an
oracle.** It stays in sync, so it can be used to confirm field widths and frame
boundaries, but its samples are not the right answer and a differential against
it means nothing. Nor is there anywhere else to look: the position of the
inverse LPC filter in the chain of section 8, what its state is, and how
`scaling` and `integerBits` divide the fixed point, are all **not determined**.

Measured: decode-flags bit 8 is set on every real file, and the per-subframe
LPC flag is clear on every subframe of every file. So the mode is reachable and
unused. Refuse a subframe that sets the flag, by name, and say that the filter
is undescribed.

That settles a question `wma-lossless-oracle-corpus.md` section 5 leaves open,
which is why FFmpeg prints no LPC warning although bit 8 is set everywhere. The
warning is gated on the **per-subframe** LPC flag, not on bit 8. Bit 8 only
makes that per-subframe flag present in the bitstream; no encoder here ever
sets it, so the warning never fires and every measured file decodes exactly.

### 10.3 Bit depths other than 16 and 24

The extra bytes could state any depth. Only 16 and 24 have a defined output
stage, a defined history width and a defined clipping range, and the reference
refuses everything else as invalid data. Refuse the same way.

### 10.4 More than 8 channels

The reference caps at 8 and refuses more as unimplemented. Nothing above 8 is
described. The channel mask can express more, so this must be checked after the
mask is expanded, not before.

### 10.5 The two conditions the reference warns on and decodes anyway

Both of these produce output. Both of them produce **wrong** output, or output
that cannot be trusted, and each needs a decision rather than a silent pass.

**Bitstream splicing.** The sixth bit read from a packet, immediately after
the seek flag. The reference warns
"Bitstream splicing" and carries on parsing the packet as if the bit were
clear. What the bit changes is not described anywhere. Measured clear on every
packet of every file. Refuse it by name: a decoder that continues is guessing
that the bit changes nothing, and nothing supports that guess.

**A CDLMS order that is not a multiple of 16 at 16 bits.** The reference's
16-bit dot product pads its length up to a multiple of 16 and runs over the
padding. The coefficients past `order` start at zero, so the first sample is
right, but the same pass also *writes* the coefficients it reads, so the
padding fills with non-zero values and every sample after the first is wrong.
The condition is exactly: bit depth 16, and a filter order whose value modulo
16 is 8, which is every order of the form `8 * odd`.

The reference's 24-bit path pads to a multiple of 8 instead, which for an order
that is always a multiple of 8 is no padding at all, and the 24-bit measured
files use order 8 and decode bit-exactly. So the correct behaviour is plainly
"exactly `order` taps", and the 16-bit path is simply defective.

That gives a from-scratch decoder a free correctness win and a testing problem
in the same breath: implement exactly `order` taps, and understand that on a
16-bit stream with an order of `8 * odd` **FFmpeg is not an oracle** and a
differential against it will fail on the second sample. Measured: 16-bit files
use order 16 and 24-bit files use order 8, so no measured file trips it, and
none can be built with the encoder that exists.

### 10.6 A non-aligned tiling

Section 3.2. The format admits per-channel subframe splits; the reference's
reads assume they are identical across channels and go wrong when they are not.
Refuse a frame whose per-channel subframe lists differ, by name.

### 10.7 Transmitted CDLMS coefficients

Section 7.1. The scaling is undetermined between two readings that disagree
everywhere except at one value of the scaling field, and no encoder exercises
the path. Refuse by name.

### 10.8 What is not on this list

Packet loss, a sequence number that jumps, a landing on a packet that is
entirely continuation, and a landing before the first seekable tile are all
**not** damage. They are the ordinary consequences of seeking and of a lossy
transport, and the recovery for all four is the same: drop the carry, drop the
continuation bits, and resume at the first seekable or raw PCM tile. A decoder
that reports them as malformed input turns a file that plays into a file that
will not seek.

## 11. Implementability checklist

Each item is a thing the session that writes the decoder must be able to do
with this file and the companion oracle note alone.

- [ ] Parse `WAVEFORMATEX` plus 18 extra bytes into depth, channel mask,
      channel count and order, `nBlockAlign` and the decode flags; ignore
      `nAvgBytesPerSec` and `wBitsPerSample` deliberately. (1)
- [ ] Compute `frameLenBits`, `samplesPerFrame`, `maxSubframes`,
      `minSubframeLen`, and `frameSizeBits = floorLog2(nBlockAlign) + 4`, with
      `floorLog2(0) = 0`. (2.1, 2.2)
- [ ] Parse a packet header and classify its continuation count into the three
      shapes of section 2.3, clamping the overshoot rather than refusing it.
      (2.3)
- [ ] Carry a bit string, not a byte string, across packets, and hold the
      sub-byte offset with it. (2.3)
- [ ] Discard an open carry, unread, on a packet whose continuation count is
      zero. (2.3)
- [ ] Implement the deferred model: emit packet k's frames when packet k+1
      arrives, and drain the carry at end of stream. Prove the drain with a
      file whose last packet begins more than one frame. (2.4)
- [ ] Read a frame header including the DRC gain, the skip fields and the
      trailer bit, and apply the end skip to the frame's output length. (2.5)
- [ ] Decode a tile header for a stream whose tile-aligned bit is 0 and whose
      per-channel bits are all 1, which is what every real file writes. (3.1)
- [ ] Read a subframe body: seekable flag, filter definitions, raw PCM flag,
      per-channel coded flags, the LPC flag under decode-flags bit 8, and the
      padding field. (3.3, 3.4)
- [ ] Clear all filter state at a seekable tile, including the AC filter
      history, the MCLMS coefficients and history, every CDLMS filter's
      coefficients, sample history and update history, and every channel's
      moving-average accumulator; then re-seed the ring positions. (3.4)
- [ ] Decode a raw PCM tile, for every channel of the stream, channel-major, at
      `bitsPerSample - paddingZeroes` bits, and leave the filter history
      untouched. (5.1)
- [ ] Consume the transient flag and, when set, its position field of
      `floorLog2(subframeLen)` bits, and use neither. (5.2)
- [ ] Read the seekable-tile seeds at full `bitsPerSample`, with the extra bit
      under inter-channel decorrelation, and start the residual loop at index
      1. (5.3)
- [ ] Decode the adaptive Golomb residual exactly: unary quotient, the 32-run
      escape with its 5-bit width, the rounded moving-average parameter, the
      `ceilLog2` remainder width, the moving-average update **before** the
      zigzag, and the zigzag itself. (5.4)
- [ ] Run the CDLMS cascade highest filter first, whole subframe at a time,
      with the dot product on pre-update coefficients, the contiguous
      double-length history window, the two update-decay shifts, and the
      arithmetic-shift versus truncating-division asymmetry in the update-speed
      rescale. (7)
- [ ] Implement MCLMS from section 6 knowing that no file can test it, and gate
      it so a stream that sets the flag either decodes or is refused by name
      rather than producing silence. (6)
- [ ] Apply the two-channel lifting in the right order, including both
      one-sided cases and the neither-coded case. (8.1)
- [ ] Apply the AC filter with history that survives across subframes,
      refreshed from the highest index down. (8.2)
- [ ] Apply the quantisation step and the `paddingZeroes` shift, and emit at 16
      and at 24 bits. (9)
- [ ] Seek: scan forward to a packet whose header seek flag is set, discard the
      carry and that packet's continuation bits, and resume at its first frame;
      then confirm by finding the seekable or raw PCM tile rather than trusting
      the flag. (4)
- [ ] Refuse, by name and before reading a bitstream: a bit depth other than 16
      or 24, more than 8 channels, a subframe depth above 5, `nBlockAlign` zero
      or absurd, and fewer than 18 extra bytes. (1.2, 10.3, 10.4)
- [ ] Refuse, by name and mid-stream: arithmetic coding, an LPC flag that is
      set, a spliced packet, transmitted CDLMS coefficients, a CDLMS order
      above 256, a non-aligned tiling, a subframe length outside its range, a
      channel that overruns the frame, more than 32 subframes on a channel, a
      frame with no channel taking a subframe, and `paddingZeroes` above the
      bit depth. (3, 10)
- [ ] Do **not** refuse: a sequence-number jump, an all-continuation packet, a
      landing before the first seekable tile, a raw PCM tile followed by a
      coded one, or a decode that outruns the container's declared duration.
      (5.1, 10.8)
- [ ] Assert **exact** equality against source PCM, with no tolerance anywhere.
      (the integer-arithmetic section)

Anything not on this list that the implementation session finds it needs is a
gap in this file, and closing it means another analysis pass, not a peek at the
reference.

## 12. No table is worth extracting

`libavcodec/wmalosslessdec.c` at the pinned commit contains **no constant
tables at all**: no Huffman book, no codeword list, no band layout, no window,
no scaling ladder. It declares no static array of any kind, and the only header
it takes from the rest of the tree contributes a single function that computes
the frame length from the sample rate by the rule already written out in
section 2.1.

So there is no `codec/wmalossless/tablesgen_test.go` and no `tables_*.go`, and
that is a statement about the format rather than about this pass. Everything
the decoder needs is either a field in the bitstream or a value computed from
one of these rules:

| quantity | rule |
|---|---|
| frame length | section 2.1, from the sample rate and decode-flags bits 1-2 |
| header field widths | `floorLog2(nBlockAlign) + 4` |
| subframe length selector width | `floorLog2(maxSubframes - 1) + 1` |
| skip field width | `frameLenBits + 1` |
| transient position width | `floorLog2(subframeLen)` |
| MCLMS and CDLMS field widths | `ceilLog2` of a scaling or order already read |
| Golomb remainder width | `ceilLog2` of the running moving average |
| clipping range | `2^(bitsPerSample - 1)` |

Two integer helpers cover every one of them, `floorLog2` with zero mapping to
zero and `ceilLog2` with anything at or below one mapping to zero. A decoder
that implements those two correctly needs no table and no generated file.

## Addendum from the implementation pass, 2026-09-10

Three places where writing the decoder found this file needed an answer it did
not give, or gave twice. Nothing above is rewritten: these are the resolutions,
with their reasons, so the next reader meets the disagreement and the decision
in the same place.

**The mid-stream continuation count of zero (sections 2.3 and 2.4 disagree).**
2.3 says a carry still open at a mid-stream `N == 0` is stale and must be
discarded unread. 2.4's procedure decodes it. Both are right about a different
stream: zero means "no bits here finish an earlier frame", which is TRUE and
benign when the previous packet's frames ended exactly on its boundary, and the
carry is then complete; it is also what a packet says when the packets between
it and the carry were lost, and a loss of an exact multiple of sixteen slips
past the four-bit sequence number.

The decoder decodes the carry, which keeps the benign case's audio, and treats
a failure of that particular walk as the discontinuity it is: drop the carry,
wait for a seekable tile, carry on. Refusing outright would turn recoverable
packet loss into a dead track, and discarding unread would lose real audio in
the case 2.3 itself calls benign. Neither behaviour is reachable from any
corpus cell, measured: no committed cell carries a mid-stream `N == 0`.

**The channel mask (section 1) is trusted more narrowly than described.** The
notes say a non-zero mask overrides `nChannels`, its population count being the
channel count, and on every stream any encoder writes the two agree. They do
not always: Windows also writes `SPEAKER_ALL` (0x80000000), which names no
speaker position and whose population count is 1 whatever the stream carries.
Overriding from it turns a stereo file into a mono track that passes every
format check, probes playable, and then dies partway through the first frame on
a field width unrelated to the real fault. So the count comes from `nChannels`
and the mask supplies only the ORDER, and only when it is positional and covers
exactly that count. Every real file is unaffected.

**The packet-header seek flag (section 4) is load-bearing after all.** The
notes describe it as an index a decoder may ignore, which is true of the
decoder and false of the container above it: WaxFlow's engine pre-rolls from a
seek landing by decoding and discarding forward, so a landing the decoder
cannot begin at produces nothing and the position it reports has no samples
behind it. The demuxer reads the flag to choose a landing. Two further facts
the notes do not state, both measured: an object's presentation time is NOT the
first sample a decode resumed at it produces, differing by 48 to 58 samples
against frames of 2048, so the landing is snapped to the nearest frame
boundary; and the millisecond rounding that causes it can never be more than a
fraction of a frame, which is what makes the snap safe.

