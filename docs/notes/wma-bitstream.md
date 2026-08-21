# WMA v1/v2 bitstream notes

ADR-0001 black-box analysis artifact. This file, `wma-oracle-corpus.md` beside
it, and `codec/wma/tables_*.go` are the **only** inputs the session that writes
the decoder consumes; that session does not open FFmpeg, and neither file
contains code from it.

WMA is the one codec in this tree with no published bitstream specification.
Microsoft never released one, and every independent decoder descends from the
same reverse-engineering effort, so the layout below was recovered in this
analysis pass and cross-checked against the black-box behaviour of the
`ffmpeg` binary (see `wma-oracle-corpus.md`, which pins the measurements
quoted here). Where a statement is a measurement rather than a structural
fact, it says so.

Scope: `wFormatTag` 0x0160 (v1) and 0x0161 (v2), one or two channels. The rate
is whatever the frame-length rule in section 2 covers, bounded above by a
refusal at 50 kHz; 8 to 48 kHz is what encoders actually produce and what the
corpus spans, and the band tables of section 6 exist only from 22.05 kHz up.
WMA Pro, Lossless and Voice are different codecs that share only the container
and are refused by name.

## 1. Everything starts with WAVEFORMATEX

A WMA track carries no in-band configuration at all: no sync word, no frame
header, no per-frame length. Every parameter below is derived from the
`WAVEFORMATEX` the container hands over (`container/asf` already produces it
as `Track.CodecConfig`, 18 bytes plus `cbSize` extra bytes) plus the codec
extra bytes behind it.

| Field | Use |
|---|---|
| `wFormatTag` | 0x0160 = v1, 0x0161 = v2. The only discriminator. |
| `nChannels` | 1 or 2. Anything else is not this codec. |
| `nSamplesPerSec` | Sets the frame length and every rate-dependent ladder. |
| `nAvgBytesPerSec` | The bit rate, and so the bits-per-sample figures below. |
| `nBlockAlign` | The superframe size in bytes. Every packet is exactly this. |
| `cbSize` + extra | Carries `flags2`. |

`flags2` is a little-endian 16-bit word inside the extra bytes, at a
version-dependent offset: **extra+2 for v1** (4 extra bytes) and **extra+4 for
v2** (6 or more; ffmpeg's ASF muxer writes 10). A file with fewer extra bytes
than its version needs has `flags2` 0, which means LSP exponents and no
reservoir, not "invalid".

| Bit | Name | Meaning when set |
|---|---|---|
| 0x0001 | `use_exp_vlc` | Exponents are VLC-coded deltas. Clear means LSP-coded. |
| 0x0002 | `use_bit_reservoir` | A frame may straddle two superframes. |
| 0x0004 | `use_variable_block_len` | Blocks within a frame may be shorter than the frame. |
| 0x0018 | block-size depth | `((flags2 >> 3) & 3) + 1`, see 3. |

Compatibility fact worth carrying: a v2 file whose `flags2` word reads exactly
0x000d is known to claim variable block lengths it does not use, and readers
clear the bit for that value. It comes from one bad writer.

## 2. Frame length

```
frameLenBits = 9                       rate <= 16000
               10                      rate <= 22050, or rate <= 32000 and v1
               11                      otherwise
frameLen     = 1 << frameLenBits
```

That is the whole rule for v1 and v2 (the 12 and 13 cases belong to WMA Pro).
Note the v1 clause: **at 32 kHz, v1 uses 1024-sample frames and v2 uses
2048**, which makes 32 kHz the single most useful rate for exercising the
version split.

Measured, all 18 corpus cells: `nBlockAlign == floor(bitRate * frameLen /
(rate * 8))`. That identity is the cheapest way to confirm a frame-length
implementation without decoding a sample, and it is what the decoder's config
test should assert.

## 3. Block sizes inside a frame

With `use_variable_block_len` clear there is one block size and it is the
frame length. With it set:

```
nb = ((flags2 >> 3) & 3) + 1
if bitRate/channels >= 32000 { nb += 2 }
nb = min(nb, frameLenBits - 7)
nbBlockSizes = nb + 1
```

Block index k has length `frameLen >> k`, so the shortest block is 128
samples and k runs 0..nbBlockSizes-1.

**Neither ffmpeg encoder ever sets this bit** (measured: `flags2` is 1 in
every cell of the sweep), so a corpus built from ffmpeg exercises exactly one
block size, and with it exactly one exponent band layout. See
`wma-oracle-corpus.md` section 4 for everything that falls out of reach with
it.

## 4. Superframe layout

A packet is one superframe of exactly `nBlockAlign` bytes, read as a
big-endian-first bit stream.

**Without the reservoir** the superframe is one frame and decoding starts at
bit 0. Nothing else is present.

**With the reservoir** the superframe opens with:

```
4 bits   superframe index          (ignored by readers)
4 bits   frame count field
N bits   bit offset, N = byteOffsetBits + 3
```

where

```
bps            = bitRate / (channels * rate)
byteOffsetBits = floor(log2(int(bps * frameLen / 8 + 0.5))) + 2
```

The bit offset counts bits at the head of the packet, immediately after these
fields, that finish the frame the **previous** packet left open. The frames
that start in this packet begin at absolute bit position

```
bitOffset + 4 + 4 + byteOffsetBits + 3
```

The frame count field F feeds one subtraction:

```
n = F         when a carry is pending
    F - 1     when none is
```

and **n is the number of frames this packet outputs**, not the number that
start in it. With a carry pending, one of the n is the carried frame,
completed from the carry buffer plus the `bitOffset` bits at the head, so n-1
start here; with none pending, all n start here.

`n == 0` is the legal all-continuation packet: append its bytes to the carry
and emit nothing. It arises as F == 0 with a carry pending, or F == 1 with
none. `n < 0` is damage, which is F == 0 with no carry pending. A packet that
reaches `n == 0` with a byte or less of payload left is damage too.

The `n < 0` rule holds for a LINEAR decode only. After a seek the decoder has
no carry but the stream does, so a landing on a packet that only continues an
earlier frame produces exactly that shape and is not damage: nothing there is
decodable, the frame in progress is not the reader's to finish, and the next
packet's own frames resynchronise the walk. A reader that refuses it turns a
file that plays into a file that will not seek, since a demuxer offering every
media object as a landing will hand it one.

Whatever follows the last complete frame is carried into the next packet, and
the carry is not byte-aligned: the bit position within the first carried byte
has to be remembered alongside the bytes, or the next packet resumes the
frame off by up to seven bits.

Bounds a reader owes itself: the bit offset must not exceed the bits left in
the packet, the computed start position must land inside the packet, and the
carry buffer needs a cap (32 KiB is what upstream uses; it is a reader's
choice, not a format constant). `byteOffsetBits + 3` must fit a bit-reader
word; a bit rate absurd for the frame length overflows it and is a refusal,
not a decode.

## 5. Block layout

`prevBlockLenBits`, `blockLenBits` and `nextBlockLenBits` are decoder state
that walks forward one block at a time. With variable block lengths the
selectors are `n = floor(log2(nbBlockSizes - 1)) + 1` bits each, and a
selector value v means `blockLenBits = frameLenBits - v`; v >= nbBlockSizes is
malformed.

- On the first block after a **reset**, two selectors are read first, giving
  prev and current, then one more gives next.
- Otherwise prev takes current, current takes next, and one selector is read
  for next.

Without variable block lengths all three are the frame length and nothing is
read.

A reset happens in exactly two places, and the second is narrower than it
looks. The first is when decoding starts. The second is **only in a reservoir
stream, and only for the frames that start in a packet**: the frame completed
from the carry does *not* reset, because it continues a run that began in the
previous packet, and the reset lands after it, on the first frame that begins
here. A non-reservoir stream therefore has no per-packet reset at all: it
resets once, at the start of decoding, and the three lengths then walk forward
across every packet in the file.

That last point is a hazard for seeking rather than for linear decode. Nothing
in the bitstream re-states the three lengths, so a decoder that resumes at a
mid-file packet in a non-reservoir variable-block stream has no way to recover
them and must reset, which is a guess. Whether that guess matters cannot be
settled with an ffmpeg corpus, because neither encoder writes such a stream
(section 3).

Then, in order:

```
1 bit    ms_stereo                       (stereo only)
1 bit    per channel: channel coded
```

If no channel is coded the block **stops the bitstream reading and nothing
else**. It still runs the transform stage of section 10 with an all-zero
spectrum, because that is what completes the previous block's overlap tail
into the accumulator. Skipping it either drops that tail or leaves
two-frames-old samples to be added into the next frame, and a fully silent
block is ordinary in real material and entirely absent from a
broadband-noise corpus. Otherwise:

```
repeat   7 bits, added into a total that starts at 1, until a value != 127
```

That total is the block gain. It also picks the escape width for coefficients:

| total gain | escape bits |
|---|---|
| < 15 | 13 |
| < 32 | 12 |
| < 40 | 11 |
| < 45 | 10 |
| otherwise | 9 |

Then, when noise coding is on, the high-band flags and gains (section 8), then
exponents (section 6), then coefficients (section 7).

**Each of those is one pass over the channels, and the passes do not
interleave.** For stereo the order is: every coded channel's noise flags, then
every coded channel's noise gains, then the single exponent-reuse bit, then
every coded channel's exponents, then every coded channel's coefficients.
Reading a whole channel end to end before starting the next desynchronises
every stereo block.

**Exponent reuse**: when the block is shorter than the frame, **one bit for
the whole block, not one per channel**, says what happens. **The bit set means
exponents are transmitted; clear means the previous block's are reused.** A
full-length block always transmits and reads no bit. Reused exponents keep the
block size they were decoded at, so a reader must remember that size and index
the exponent array with the ratio between it and the current block size. A
coded channel whose exponents were never initialised is malformed input.

`blockPos` accumulates block lengths; a block that would push it past the
frame length is malformed. The frame ends when `blockPos` reaches
`frameLen`.

**v1 with two channels byte-aligns the reader after each channel slot**, and
the align happens whether or not that channel was coded, so a stereo v1 block
with one silent channel still aligns twice. v2 never aligns. This is the one
place where the two versions differ inside a block.

## 6. Exponents, two strategies

Exponents are a piecewise-constant curve over the block: one value per
exponent band, repeated across the band's width. Both strategies produce that
curve; `flags2` bit 0 picks which.

### Band layout

Bands are laid out on the 25 critical-band edges in `criticalFreqs`
(`tables_bands.go`).

- **v1** places band ends at `round(blockLen * 2 * edge / rate)`, clamped to
  the block length, and takes the differences as widths; the run stops at the
  first end that reaches the block length.
- **v2** rounds the same quantity to the **nearest** multiple of four,
  `((blockLen*2*edge + 2*rate) / (4*rate)) << 2` (the `+2*rate` over a `4*rate`
  denominator is the half-step, and the `<<2` restores the scale), and drops
  any band that does not advance.

  **Except** for the three shortest block sizes at 22.05 kHz and above, where
  the widths are tabulated. Which table is picked from the **raw** sample rate,
  by the first threshold it clears: `>= 44100` takes `expBands44100` (so 48 kHz
  uses it too), `>= 32000` takes `expBands32000`, `>= 22050` takes
  `expBands22050`, and anything below computes. The row is
  `frameLenBits - 7 - k` for block k, and only rows 0..2 exist, so the tables
  apply exactly when `frameLenBits - 7 - k < 3`, which needs `k >= 1` and
  therefore variable block lengths. **No ffmpeg-produced file reaches a
  tabulated row at all.**

  The tabulated rows partition their block exactly, and each is a coarsening of
  the computed layout at its own rate: every tabulated edge sits within one
  quarter-step of an edge the formula places there, and further out at the
  other two rates. `TestExponentBandRowsPartitionTheirBlock` and
  `TestExponentBandTablesMatchTheirOwnRate` pin both.

Every v2 band width is a multiple of four, which is what lets a decoder fill
exponents four at a time.

**Where the reference is ambiguous.** Upstream computes v1's band layout into
slot 0 for every block size rather than into slot k, while the exponent
decoder indexes slot `frameLenBits - blockLenBits`. With one block size, which
is all any real v1 file has, the two agree and nothing is wrong. A v1 file
that also set the variable-block-length bit would index slots that were never
filled. No such file is known and ffmpeg cannot produce one; a decoder should
treat v1 plus variable block lengths as a refusal rather than invent a layout
for it, and say so in the error.

### VLC-coded exponents (bit 0 set)

Deltas coded with the AAC scalefactor book (`expScaleCodes`/`expScaleBits`),
biased by 60: a decoded symbol s adds `s - 60` to a running exponent index.
Verified during extraction: this is byte-identical to the book
`codec/aac/tables_hcb.go` already carries, extracted independently from a
different upstream file.

- **v1** seeds the running index from a 5-bit field plus 10, fills the first
  band with that value, and then reads one code per remaining band.
- **v2** seeds it to 36 and reads a code for every band including the first.

The exponent value is `pow(10, index/16)`, a curve a decoder tabulates over
**index -60..95**, 156 entries running from about 1.778e-4 to about 8.660e5.
That range is not a spec constant; it is the width of the table every derived
decoder carries, so an index outside it is treated as malformed input rather
than saturated. Saturating would be the more forgiving choice and the wrong
one: the table *is* the definition of the value here, so a stream that indexes
past it is a stream no decoder agrees with, and reading off the end is how a
fuzzer gets out of bounds. Track the maximum exponent as you go; section 9
divides by it.

### LSP-coded exponents (bit 0 clear)

Ten coefficients, read most-significant first: **3 bits for coefficient 0 and
for 8 and 9, 4 bits for 1 through 7**, each indexing its own row of
`lspCodebook`. Rows 0, 8 and 9 therefore use only their first eight entries,
which is why those rows are half full.

The curve is the Vorbis LSP evaluation. With `w = 2*cos(pi*i/frameLen)` for
bin i (note `frameLen`, not `blockLen`: a short block uses the first
`blockLen` entries of a frame-length cosine table, so the angular step does
not change with the block size):

```
p = q = 0.5
for j = 1, 3, 5, 7, 9:
    q *= w - lsp[j-1]
    p *= w - lsp[j]
p *= p * (2 - w)
q *= q * (2 + w)
curve[i] = (p + q) ^ (-1/4)
```

The maximum over the block is the maximum exponent. The `^(-1/4)` is
sometimes done with an exponent/mantissa table and linear interpolation, but
measured on ffmpeg 9.0 (noise-off LSP streams, WMA-in-WAV wrap) its float
path agrees with the exact power to about 6e-7 relative with no structure in
p+q, so there is no coarse table there to match. What a differential does
show is a floor near 1e-6 relative from per-bin float rounding order on this
path, which is where the LSP corpus cells' misses come from.

### The one entry that looks wrong

`lspCodebook[5][10]` is -1.40037388, sitting between -0.03062936 and
-0.25128968. Every other entry in that row falls monotonically, and -0.14 is
what would belong there, so this looks like a digit that slipped when the
codebook was recovered from the binary decoder. It has been that value in
every derived implementation for two decades, which means deployed files are
coded against it: **reproduce it, do not fix it.** Row 7 has a second
discontinuity at entry 8, where a fresh descending run starts; that one looks
structural rather than accidental. `TestLSPCodebookFallsExceptWhereItDoesNot`
pins both, so neither can be quietly smoothed away by a re-extraction.

## 7. Coefficients: run-level

Per coded channel, into a `blockLen` buffer zeroed first. The channel decodes
`nbCoefs` values, where

```
nbCoefs = coefsEnd[k] - coefsStart   minus the width of every noise-filled high band
coefsStart = 3 for v1, 0 for v2
coefsEnd[k] = (frameLen - frameLen*9/100) >> k
```

so roughly the low 91% of the block is codeable and the top is never
transmitted.

Which book: three pairs, selected once per stream from the rate and the
per-channel bits-per-sample. The rate here is the **raw** one, not the
normalised rate section 8 introduces, and `bps` is
`bitRate / (channels * rate)` with `bps1 = bps * 1.6` for stereo and `bps` for
mono:

```
pair = 0    raw rate >= 32000 and bps1 < 0.72
       1    raw rate >= 32000 and bps1 < 1.16
       2    otherwise, including every rate below 32 kHz
```

The distinction is load-bearing at exactly 32 kHz on v2, where the raw rate
clears the threshold and the normalised one (22050) does not: raw picks pair 1
and normalised would pick pair 2, which are different books entirely.

`bitRate` is `nAvgBytesPerSec * 8` from the WAVEFORMATEX, **not** derived from
`nBlockAlign`. Section 2's block-align identity runs the other way and is not
an invitation to invert it: the identity floors, so a bit rate recovered from
`nBlockAlign` comes back slightly low, and the thresholds here are knife
edges. A 44.1 kHz stereo stream at 64000 sits 0.001 above the 1.16 boundary on
the declared rate and just below it on the recovered one, which flips the
pair.

Within the pair, the first book codes an ordinary channel and the second codes
channel 1 of a mid/side block, where less energy is expected. Pair 2 is the
common case for ordinary stereo music despite being the fallback arm.

The decode loop, with `offset` starting at 0 and incrementing once per
iteration:

```
code = VLC(book)
code > 1   ->  offset += run(code); read 1 sign bit; coef[offset & (blockLen-1)] = ±level(code)
code == 1  ->  end of block
code == 0  ->  escape: level = escapeBits bits, offset += frameLenBits bits,
               then 1 sign bit, coef[offset & (blockLen-1)] = ±level
```

**A sign bit of 1 means positive and 0 means negative**, which is the opposite
of the usual convention and the same in both arms.

The escape reads its run with `frameLenBits` bits rather than
`blockLenBits`, which wastes bits on short blocks; that is the format, not a
bug to fix.

End-of-block may be **omitted** when the run fills the channel exactly, so
running out of coefficients is a normal exit. An offset that ends up past
`nbCoefs` is malformed input.

`run(code)` and `level(code)` are not stored. They are expanded from
`coefLevels`: starting at index 2 (index 0 is the escape, index 1 is
end-of-block), the next `coefLevels[0]` indices carry level 1 with runs
0,1,2,..., the next `coefLevels[1]` carry level 2 with runs from 0 again, and
so on. `TestCoefLevelLadderCoversTheBook` pins that the ladder covers each
book exactly.

## 8. Noise coding

Whether noise coding is on at all is decided once per stream, from the rate
and bit rate, and it also sets how far up the spectrum ordinary coefficients
reach. Normalise the rate first: **v2 snaps the rate down to the nearest of
44100, 22050, 16000, 11025, 8000; v1 uses the rate as-is**, so 32 kHz and 48
kHz fall to the default arm for v1 and to the 22050 and 44100 arms for v2.

| normalised rate | rule |
|---|---|
| 44100 | `bps1 >= 0.61` turns noise coding off, else high frequency x 0.4 |
| 22050 | `bps1 >= 1.16` off; `>= 0.72` x 0.7; else x 0.6 |
| 16000 | `bps > 0.5` x 0.5, else x 0.3 |
| 11025 | x 0.7 |
| 8000 | `bps <= 0.625` x 0.5; `bps > 0.75` off; else x 0.65 |
| anything else | `bps >= 0.8` x 0.75; `>= 0.6` x 0.6; else x 0.5 |

The multiplier above is the only thing the ladder produces. The frequency it
scales starts at `rawRate / 2` (the **raw** rate, even though the ladder arm
was chosen by the normalised one), and the band index it turns into divides by
the raw rate again, so the rate cancels outright:

```
highBandStart[k] = round(blockLen * mult)
```

Worth writing that way rather than leaving `rate` in it twice: reading either
occurrence as the normalised rate moves the boundary by a quarter of the
spectrum on a v2 32 kHz stream, and the cancelled form cannot be read wrong.

The high bands are then the exponent bands clipped to
`[highBandStart[k], coefsEnd[k]]`, dropping empty ones.

Per block, **two separate passes over the channels, not one interleaved pass**.
First pass, for each coded channel: one bit per high band saying the band
carries no coefficients and is filled with noise instead. Second pass, for
each coded channel: the gains, where the first noise-filled band of that
channel reads **7 bits minus 19** and each later one adds an `hgainHuff` delta
(the book's symbol is biased by 18, so the delta is `symbol - 18`). A stereo
reader that reads flags and gains together per channel desynchronises.

### Building the noise-gain book

`hgainHuff` stores `{symbol, length}` and no codewords, so the **listed order
is what assigns them**. Walk the list front to back with an accumulator:

```
maxLen = 13
acc = 0
for each entry: code = acc >> (maxLen - length); acc += 1 << (maxLen - length)
```

The accumulator closes on exactly `1 << maxLen`, which is what makes the
result a complete prefix code. This is **not** the textbook canonical build:
that one sorts by length first, and the two disagree everywhere. The four
3-bit entries sit at the end of the listing and take codes `100`, `101`,
`110`, `111`; sorting them to the front would hand them `000` through `011`
instead, and every noise gain after the first in a block would decode wrong.
`TestHgainBook` walks the order and pins the last codeword so a reader cannot
tidy it into the sorted form.

### The noise source is decoder state, not a table

The 8192-entry noise table is generated, not stored:

```
seed = 1
for i in 0..8191:
    seed = seed*314159 + 1        (unsigned 32-bit wrap)
    noise[i] = float(int32(seed)) * (2^-31 * sqrt(3) * noiseMult)
noiseMult = 0.02 with VLC exponents, 0.04 with LSP exponents
```

A running index walks that table and wraps at 8192. **It is never reset per
block, per frame or per packet.** Every noise-filled band, every coefficient
below `coefsStart`, every coefficient above `coefsEnd`, and the small noise
added to *coded* coefficients all draw from it in bitstream order, so the
index at any point is a function of the entire decode history.

This is the same shape as the CELT PRNG lesson: the noise index is decoder
state, so a seek that resumes at a packet boundary produces different noise
from a linear decode of the same bytes, and a differential against another
decoder only matches sample-for-sample if the whole history matches. A
seek test must compare against a linear decode with a settling
allowance, not demand equality.

## 9. Putting a block back together

Per coded channel, with `n4 = blockLen/2`:

```
mdctNorm = 1 / n4,  and for v1 additionally  * sqrt(n4)
mult     = 10^(totalGain/20) / maxExponent * mdctNorm
```

Then the block is filled in three regions:

- **below `coefsStart`** (v1 only, since v2 starts at 0): noise * exponent *
  mult when noise coding is on, zero when it is off.
- **the coded span**, walked as one region from `coefsStart` to
  `highBandStart[k]` followed by one region per high band. A noise-filled high
  band j gets `noise * exponent * mult1` with

  ```
  mult1 = sqrt(expPower[j] / expPower[lastHighBand])
          * 10^(gain[j]/20)
          / (maxExponent * noiseMult)
          * mdctNorm
  ```

  where `expPower[j]` is the mean of exponent-squared over band j and
  `lastHighBand` is the highest noise-filled band in this channel. Every other
  band gets `(coef + noise) * exponent * mult`: the small noise is added to
  *coded* coefficients too, which is why a bit-exact decode is impossible
  without matching the noise index.
- **above `coefsEnd`**: noise * mult * the **last** exponent of the coded span,
  one value reused for the whole tail rather than a curve.

With noise coding off the same span is simply `coef * exponent * mult` with
zeros on both sides.

When exponents were reused from a longer or shorter block, indexing them takes
the ratio between the block the exponents were decoded at and the current
one, not the current block size alone.

Measured 2026-08-21 (ffmpeg 9.0, hand-built streams through a WMA-in-WAV
wrap): ffmpeg follows this rule exactly in the coded span at every block-size
ratio, but inside the NOISE regions of a reuse block it reads the resampled
curve one source bin behind, in both the per-bin fill and the band powers
behind the noise gains. The one reuse-at-a-different-length block Microsoft's
encoder writes (its startup frame) therefore decodes about 1.4 percent off
against ffmpeg however faithfully this rule is followed.
codec/wma/synthdiff_test.go pins the divergence and notices if ffmpeg stops
producing it; the ms-22050 msDeficits entry carries the corpus-side number.

**Mid/side** is undone on the coefficients, before the inverse transform: a
butterfly `(a, b) -> (a+b, a-b)`, channel 0 carrying the mid and channel 1 the
side. Both one-sided cases occur and they are not symmetric:

- **Only channel 1 coded.** Channel 0 is zeroed, marked coded, and the
  butterfly runs, giving `L = S`, `R = -S`. Legal and rare.
- **Only channel 0 coded.** The butterfly does **not** run. The side is
  absent, so both outputs are the mid, and a decoder gets there by leaving
  channel 1's transform buffer holding channel 0's result rather than clearing
  it. This is the common near-mono case, and a decoder that clears every
  uncoded channel's buffer silences the right channel on exactly the material
  where a listener would notice.

## 10. Filterbank and overlap

A full inverse MDCT takes the block's `blockLen` coefficients to `2 * blockLen`
time samples, carrying a scale of **1/32768** (the coefficients are on a 16-bit
integer scale, and this is where they become normalised floats). Then a window
and overlap-add into an accumulator of `2 * frameLen` samples per channel. The
block writes at

```
index = frameLen/2 + blockPos - blockLen/2
```

**The window is the half-sample sine**, `w[i] = sin((i + 0.5) * pi / (2*n))`
for `i` in `0..n-1`, with one such window per block size (`n = blockLen`).
The half-sample offset is not cosmetic: only that form satisfies the
Princen-Bradley condition `w[i]^2 + w[n-1-i]^2 = 1`, so the plain `sin(pi*i/n)`
breaks time-domain alias cancellation quietly rather than loudly. The second
half of the window is the first half reversed, which is why the right half of
the block below is windowed backwards rather than with a second table.

**Calibrate the remaining normalisation empirically.** Three constants stack:
the transform's own convention, the `1/32768`, and the `1/n4` (plus v1's
`sqrt(n4)`) that section 9 folds into `mult`. Which part of the total belongs
to the transform depends on how that transform is defined, so derive the
residual from a decoded fixture rather than from these numbers.

Windowing is asymmetric at a block-size change, which is what makes variable
block lengths work without a start/stop window signal. With
`n = (blockLen - otherLen)/2` for the neighbour in question:

- **Left half**, added into what is already in the accumulator. If this block
  is no longer than the previous one, window the whole half with this block's
  window and add. Otherwise add only the middle `prevBlockLen` samples,
  windowed with the *previous* block's window; **leave the leading `n` samples
  alone**, because the previous block already finalised them and writing there
  destroys finished output; and **store**, not add, the trailing `n` straight
  from the transform, which is the flat part of the asymmetric window.
- **Right half**, stored rather than added, since nothing has written there.
  If this block is no longer than the next one, window the whole half with
  this block's window reversed. Otherwise store the leading `n` straight
  through, window the middle `nextBlockLen` with the *next* block's window
  reversed, and zero the trailing `n`.

The asymmetry between the two halves is the point: the left half's leading
edge is untouched and its trailing edge is copied, while the right half's
leading edge is copied and its trailing edge is zeroed.

After the last block of a frame, the first `frameLen` samples of the
accumulator are the frame's output and the second half shifts down to become
the first.

## 11. Length, delay and trimming

WMA carries no gapless metadata: no encoder delay field, no padding count, no
sample-exact total. What the container states is a play duration in
milliseconds, which `container/asf` converts at the track rate with
`SamplesExact` false.

Measured across all frame lengths and both versions (`wma-oracle-corpus.md`):
**an untrimmed decode trails its source by exactly `frameLen` samples**, that
is, it opens with `frameLen` samples of lead-in before the source's first
sample, so aligning it means dropping that many from the head. ffmpeg declares
`2 * frameLen` of decoder delay and trims that much, which is one `frameLen`
too many, so its CLI output starts one `frameLen` *into* the source. Both
figures come from a round trip through ffmpeg's own encoder, so they cannot be
split between encoder lookahead and decoder delay without a file from a
different encoder; pin the head trim against a real-world file rather than
inherit either number on faith.

The declared length is not the coded capacity either. On the corpus, packets
cover `packets * frameLen` samples while the container declares up to 60
samples fewer at 44.1 kHz, because the muxer's duration accounting is coarser
than a sample. Neither number is the source length. So: do not trim the tail
to the declared length, and do not treat a decode that overruns it as damage.

## 12. What v1 and v2 actually differ on

| | v1 (0x0160) | v2 (0x0161) |
|---|---|---|
| `flags2` offset | extra+2 | extra+4 |
| Frame length at 32 kHz | 1024 | 2048 |
| Rate normalisation | none, so 32k/48k take the default arm | snaps down to 44100/22050/16000/11025/8000 |
| First coded coefficient | 3 | 0 |
| Exponent band layout | always computed from critical bands | tabulated for the three shortest blocks at >= 22.05 kHz, computed otherwise, 4-aligned |
| VLC exponent seed | 5-bit field + 10, first band pre-filled | fixed 36, code read for every band |
| Coefficient normalisation | extra `sqrt(blockLen/2)` | none |
| Byte alignment | after each channel's coefficients when stereo | none |

Everything else, including the whole superframe and block structure, the run
level coding, the noise model and the filterbank, is shared.

## 13. Implementability checklist

Each item is a thing the session that writes the decoder must be able to do
with this file, `wma-oracle-corpus.md`, and `codec/wma/tables_*.go` alone.

- [ ] Parse `WAVEFORMATEX` + extra bytes into version, rate, channels, bit
      rate, block align and `flags2`, including the short-extra case and the
      0x000d quirk. (1)
- [ ] Compute `frameLen` and assert `nBlockAlign == floor(bitRate * frameLen /
      (rate * 8))` on the corpus. (2)
- [ ] Compute `nbBlockSizes` and the per-block lengths. (3)
- [ ] Walk a superframe with and without the reservoir, including a packet
      that is entirely continuation, and carry the tail across packets. (4)
- [ ] Read a block header: selectors, ms flag, per-channel coded flags, the
      total-gain escape ladder, the escape width. (5)
- [ ] Build the exponent band layout for both versions and every block size,
      matching the tabulated v2 rows where they apply. (6)
- [ ] Decode both exponent strategies to a curve, and carry the maximum. (6)
- [ ] Expand the run/level ladder from `coefLevels` and decode a channel's
      coefficients, including the escape and the omitted end-of-block. (7)
- [ ] Decide noise coding and the high-frequency scale from the normalised
      rate and bits-per-sample; place the high bands. (8)
- [ ] Generate the noise table from its LCG and thread the running index
      through the whole decode without resetting it. (8)
- [ ] Reconstruct a block: the three regions, the noise-band gain, exponent
      reuse indexing, and both one-sided mid/side cases. (9)
- [ ] Run the transform stage for a block with no coded channel, so the
      overlap tail completes. (5, 10)
- [ ] Build the half-sample sine windows, IMDCT, apply the asymmetric window
      and overlap-add, and emit a frame from the accumulator. (10)
- [ ] Refuse, by name and before reading a bitstream: a rate above 50 kHz,
      more than two channels, a missing or zero `nBlockAlign`, and a
      non-positive `nAvgBytesPerSec`. That last one is not decorative: `bps`
      divides by it and `byteOffsetBits` takes a logarithm of a quantity
      derived from it, and `container/asf` validates neither, so the refusal
      has to live here. (1, 4)
- [ ] Refuse, by name and mid-stream: a block selector out of range, a block
      that overruns the frame, an exponent index outside -60..95, a run-level
      offset past the channel, a bit offset past the packet, a frame count
      that goes negative, and v1 with variable block lengths. (4, 5, 6, 7)
- [ ] Reproduce `lspCodebook[5][10]` unchanged. (6)

Anything not on this list that the implementation session finds it needs is a
gap in this file, and closing it means another analysis pass, not a peek at
the reference.
