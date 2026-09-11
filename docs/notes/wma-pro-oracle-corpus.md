# WMA Pro oracle corpus and black-box behaviour

ADR-0001 black-box analysis artifact, the oracle half of the WMA Pro analysis
pass. It is the sibling `wma-pro-bitstream.md` names, and together the two are
the only inputs the session that writes the decoder consumes. Everything here
was produced by running Windows' own encoder and decoder and the `ffmpeg`
binary, all of which ADR-0001 permits as tools, and by an instrumented decoder
written during the analysis pass from the bitstream note and then deleted.

Measured with `ffmpeg version 8.0.1-3ubuntu2` (Ubuntu, gcc 15) on this WSL box,
and with the Windows Media Format SDK and Media Foundation on Windows 11
reached over `powershell.exe` interop. The identities (packet size, frame
length, exact sample count, resumability) are exact and portable; the
floating-point figures are from these builds.

## 1. Which oracle answers which question

Three sources of truth are in reach and none of them is the source PCM.

**The source PCM is not an oracle here.** WMA Pro is lossy. The corpus's source
WAVs are regenerated deterministically (section 3) and they pin the
**alignment**, because the decode is exactly as long as the source and lines up
at offset zero, but a sample-by-sample comparison against them is a measurement
of the encoder, not of the decoder. That is the opposite of WMA Lossless, where
the source is the whole gate.

**FFmpeg's scalar decode is the differential.** It is a genuinely independent
implementation of bytes it did not write: FFmpeg has **no encoder** for this
format, so every fixture here is foreign to it. Pin the scalar path with
`-cpuflags 0`; the vectorised path differs from it, in the last bits, and
section 6 gives the size of that difference.

**Windows' own decoder is the cross-check that keeps FFmpeg honest.** Reached
through Media Foundation with `scripts/wmfenc/wmfenc.ps1 -Decode`, it is a
third implementation and the only one written by the people who wrote the
format. It costs a Windows box, so it cannot be a gate, but it is what turned
one class of stream in this corpus from "a cell" into "a refusal" (section 7).

## 2. Reaching Windows' encoder

`scripts/wmfenc/wmfenc.ps1` writes this format through Media Foundation's
transcoder with the `Wma9` subtype, which is the same path that produces WMA
v2 and which, unlike WMA Lossless, works without any `_VBRENABLED` ceremony.
The command line for each cell is in `codec/wmapro/testdata/corpus/MANIFEST.md`.

Two behaviours to know before trusting an encode:

1. **Media Foundation renegotiates rather than refusing.** Ask for a shape the
   codec does not offer and you get a successful encode of a different shape.
   Every WMA Pro format above 48 kHz is 24-bit, so a 96 kHz request at the
   default depth comes back as a **48 kHz** file that looks fine from the
   outside. Read back what you actually got, every time.
2. **The bit rate lands near the request, not on it.** 128000 comes back as
   128016. At 32 kHz the request is ignored entirely, because the codec offers
   exactly one 32 kHz format.

### The envelope

`IWMCodecInfo3` enumerates 189 WMA Pro format rows for the codec Windows names
**Windows Media Audio 10 Professional**, each with its full `WAVEFORMATEX` and
18 extra bytes. That enumeration, not the files on disk, is the authority on
what can be produced:

| axis | offered | not offered |
|---|---|---|
| sample rate | 32000, 44100, 48000, 88200, 96000 | everything else |
| channels | 2, 6, 8 | **1, 3, 4, 5, 7** |
| depth | 16, 24 | everything else |
| 32000 | 2ch/16-bit at 32 kbps only | every other 32 kHz shape |
| 8 channels | 48000 and 96000 only | 8ch at 44100 or 88200 |
| 88200 | 24-bit only | 16-bit at 88.2 kHz |
| decode flags | 0x00e0 (183 rows), 0x0060 (6 rows) | everything else |
| extra word at offset 16 | 0x0000 (168), 0xc042 (19), 0x20c6 (2) | everything else |

There is **no mono format at any rate**, which matters twice: a mono stream is
describable by the container and unproducible anywhere in this tree, and the
same was true of WMA Lossless, so neither codec's mono path has a fixture.

The 0x0060 rows are exactly the three 32 and 48 kbps stereo formats; the only
bit that differs from 0x00e0 is the dynamic-range-gain bit. The non-zero
offset-16 rows are exactly the formats at or below 96 kbps stereo, and they are
the refusal of section 7.

## 3. Generating the corpus

The source PCM is synthesized in Go by the recipe `corpus_test.go` carries, and
it is **entirely integer**, with no floating point anywhere, so it regenerates
identically on any platform and any architecture. In prose, so that it can be
reimplemented rather than copied:

Each cell is four equal segments. `full = 2^(depth-1) - 1`. For channel `c`:

- A splitmix64 generator seeded `0x5741584C4F5700 + c * 0x100`, advanced by the
  usual golden-ratio increment, gives a noise sample `n` in `[-full, full]` by
  taking the next output modulo `2*full + 1` and subtracting `full`.
- Two triangle periods, `p1 = 97 + 13c` and `p2 = 251 + 29c`, so every channel
  gets a different pair and the channels are genuinely decorrelated.
- `tri(i, p, a)` is the integer triangle of amplitude `a`: take `x = i mod p`,
  form `y = 4*a*x / p`, fold it with `y = 4a - y` when `y > 2a`, and return
  `y - a`. **Compute `4*a*x` in int64.** At 24 bits, `4 * (3*full/8) * x`
  overflows a 32-bit int, and `make test-386` found exactly that in an earlier
  stage of this work.

The four segments, per sample index `i`:

| segment | content |
|---|---|
| 1 | `tri(i, p1, 3*full/8) + tri(i, p2, 2*full/8)` -- two tones, tonal and quiet enough to code cleanly |
| 2 | `tri(i, p1, 2*full/8) + tri(i, p2, full/8) + n/4` -- tones plus quarter-scale noise |
| 3 | 0 -- digital silence |
| 4 | `n` -- full-scale noise, incompressible |

then clamp to `[-full, full]`.

The silence-to-noise edge at three quarters is a **hard transient landing on a
frame boundary**, which is what drives the encoder to split a frame into short
subframes. That edge is why the corpus reaches every subframe length rather
than only the full-frame one, and it is the single most productive feature of
the recipe.

Lengths are powers of two so that the decoded length equals the source length
exactly (section 6). Do not take that for granted at other lengths; section 6
says what happens.

**The tonal recipe**, added by the implementation pass for the cell that
closes two rows of section 5: one pure cosine per channel, 440 Hz on the left
and 550 Hz on the right, each at `3*full/8`, for 100000 frames at 44100/2/16.
The cosine is the integer rotation recurrence `x[n+1] = 2 cos(w) x[n] -
x[n-1]` carried in int64 fixed point with `32 - depth` fractional bits, with
`cos(w)` a literal in thirty fractional bits (1071632635 for 440 Hz and
1070446823 for 550 Hz), started from `x[0] = A` and `x[-1] = A cos(w)`. No
transcendental is evaluated anywhere, so it regenerates identically on every
platform; the rounding drifts the amplitude by a unit or two over the file,
which is a property of the fixture and not a defect. One tone per channel is
load-bearing: measured against three other mixes of the same two tones (both
in both channels, with either sign on the second), it is the only one on which
the encoder enables the channel transform band by band. With both tones in
both channels it decides once for all bands, every time.

## 4. The corpus

Six cells are committed at `codec/wmapro/testdata/corpus/`, 356 KiB together,
and `MANIFEST.md` beside them repeats each command line. They are committed
because **FFmpeg cannot write this format**: a machine with no Windows has
nothing to encode with, and these bytes are the only fixtures it gets.

| file | rate | ch | depth | bit rate | nBlockAlign | flags | word@16 | frames | packets | bytes |
|---|---|---|---|---|---|---|---|---|---|---|
| `pro-44100-2ch-16-128k.wma` | 44100 | 2 | 16 | 128016 | 5945 | 0x00e0 | 0x0000 | 65536 | 6 | 37543 |
| `pro-44100-6ch-16-128k.wma` | 44100 | 6 | 16 | 128016 | 5945 | 0x00e0 | 0x0000 | 65536 | 6 | 37543 |
| `pro-48000-6ch-24-384k.wma` | 48000 | 6 | 24 | 384000 | 16384 | 0x00e0 | 0x0000 | 65536 | 7 | 116590 |
| `pro-96000-2ch-24-384k.wma` | 96000 | 2 | 24 | 384000 | 16384 | 0x00e0 | 0x0000 | 131072 | 7 | 116590 |
| `pro-32000-2ch-16-32k.wma` | 32000 | 2 | 16 | 32000 | 1536 | 0x0060 | 0x20c6 | 49152 | 7 | 12654 |
| `pro-44100-2ch-16-128k-tonal.wma` | 44100 | 2 | 16 | 128016 | 5945 | 0x00e0 | 0x0000 | 100000 | 7 | 43517 |

Five are gate cells and the sixth is a **refusal cell**: `pro-32000-2ch-16-32k`
is there so that a decoder's refusal of the low-bit-rate tool has something to
refuse, not so that it can be decoded. It is also the only cell with decode
flags 0x0060, so it is the one that proves the dynamic-range-gain byte is
genuinely conditional.

Why each of the other four earns its bytes:

- `pro-44100-2ch-16-128k` is the ordinary case: stereo, 16-bit, the rate and
  bit rate anyone actually ships.
- `pro-44100-6ch-16-128k` is the same rate, depth and bit rate at six channels,
  so it is the controlled comparison that isolates the channel count. It is
  what showed that the FFmpeg-versus-Windows gap of section 6 tracks channels
  and not depth or rate.
- `pro-48000-6ch-24-384k` is multichannel at 24 bits with a channel mask and an
  LFE channel.
- `pro-96000-2ch-24-384k` is hi-res, and the only committed cell with
  4096-sample frames. Every Pro format above 48 kHz is 24-bit, so there is no
  16-bit cell up here to pair it with.
- `pro-44100-2ch-16-128k-tonal` is the tonal recipe of section 3, committed by
  the implementation pass to close the two rows of section 5 that this pass
  named as closable: it reaches the per-band channel transform enables 24
  times and carries an end trim of 864 on its last frame. Its 100000-frame
  length is not a multiple of the frame, and its decode is 99488 frames, the
  number section 6 predicted and pins.

`container/asf/testdata/pro-s16.wma` is the demuxer's cell: the same recipe at
16384 frames, 44100/2/16 at 128 kbps, two packets, 13647 bytes. It exists so
the ASF tests do not reach into another package's testdata.

Six more shapes are generated on Windows and never committed, in
`.../scratchpad/gen/`: 44100/2/16 at 64 kbps, 44100/2/24 at 440 kbps,
44100/6/16 at 192 kbps, 48000/2/24 at 192 kbps, 88200/2/24 at 384 kbps and
96000/6/24 at 768 kbps. They are in the coverage matrix below because they
reach rows the committed five do not, and a reviewer with Windows can rebuild
them.

Two things a regeneration will show, both expected. The output is **not
byte-identical** to the committed file, because the encoder writes a fresh File
ID GUID every run. And the ASF Play Duration overshoots the audio, by different
amounts per cell, which section 6 covers.

## 5. What the corpus reaches

This matrix comes from an instrumented decoder run over every cell, counting
each bitstream path as it was taken. A number is the count of occurrences; a
list is the set of values observed; a dash means the path was never taken in
that cell. Section numbers refer to `wma-pro-bitstream.md`.

Committed cells first, then the generated-only ones. The implementation pass
pinned every count below against its own decoder's path counters
(`codec/wmapro`'s `TestCorpusReachesWhatTheNotesMeasured`), and they agree
exactly; the tonal cell it added is in a third table after the two.

| path (section) | 44k/2/16 128k | 44k/6/16 128k | 48k/6/24 384k | 96k/2/24 384k | 32k/2/16 32k |
|---|---|---|---|---|---|
| samples per frame (2) | 2048 | 2048 | 2048 | 4096 | 2048 |
| frames | 33 | 33 | 33 | 33 | 25 |
| subframes | 55 | 65 | 64 | 55 | 47 |
| subframes per channel (6.2) | 1, 2, 8 | 1, 2, 6, 8 | 1, 2, 5, 8 | 1, 2, 8 | 1, 2, 8 |
| subframe lengths (6.1) | 128..2048 | 128..2048 | 128..2048 | 256..4096 | 128..2048 |
| uniform tiling bit (6.2) | 1 only | **0 and 1** | **0 and 1** | 1 only | 1 only |
| per-channel presence bits (6.2) | - | 140 | 140 | - | - |
| channels per subframe (6.3) | 2 | 1, 5, 6 | 1, 5, 6 | 2 | 2 |
| group sizes (9.1) | 2 | 1, 2, 3, 5, 6 | **1..6** | 2 | 2 |
| stereo M/S matrix (9.2) | 42 | - | - | 42 | 47 |
| scaled 2-channel matrix (9.2) | - | 4 | 4 | - | - |
| explicit "no transform" (9.2) | 13 | 2 | - | 13 | - |
| explicit rotation matrix (9.4) | - | 6 | 7 | - | - |
| built-in matrix (9.3) | **-** | **-** | **-** | **-** | **-** |
| per-band transform enables (9.5) | **-** | **-** | **-** | **-** | **-** |
| scale factor step values (10) | 1, 2, 3 | 2, 3 | 1, 2 | 1, 2 | 1, 2 |
| scale factors, DPCM form (10.1) | 52 | 156 | 156 | 52 | 40 |
| scale factors, run-level form (10.2) | 32 | 70 | 84 | 32 | 30 |
| scale factor 14-bit escape (10.2) | 43 | 66 | 152 | 53 | 40 |
| resample with no transmission (10) | - | 15 | - | - | 2 |
| both run-level books (11.3) | 0 and 1 | 0 and 1 | 0 and 1 | 0 and 1 | 0 and 1 |
| 4-vector escape (11.1) | 10227 | 1370 | 30049 | 29695 | 2942 |
| 2-vector escape (11.1) | 450 | 339 | 20442 | 19309 | 125 |
| large-value escape (11.2) | 30 | 15 | 429 | 99 | **-** |
| run-level tail reached (11.3) | 84 | 239 | 240 | 84 | 36 |
| vector phase covers the block | - | - | - | - | - |
| run-level escape (11.3) | 168 | 219 | 605 | 399 | 33 |
| end-of-block symbol (11.3) | 84 | 239 | 240 | 84 | 36 |
| quantisation step escape (12.2) | - | - | 1 | - | - |
| per-channel modifiers (12.3) | 18 | 167 | 155 | 12 | **-** |
| modifier widths (12.3) | 0..3 | 0..5 | 0..5 | 0..3 | 0 |
| explicit vector limit (12.1) | **-** | **-** | **-** | **-** | **-** |
| extended payload (8.1) | - | - | - | - | **26** |
| post-processing transform (7.1) | **-** | **-** | **-** | **-** | **-** |
| LFE content above cutoff (13.2) | n/a | **-** | **-** | n/a | n/a |
| subframe with no channel coding | yes | yes | yes | yes | yes |
| continuation count zero (4.1) | 1 | 1 | 1 | 1 | - |
| continuation count saturates (4.1) | - | - | - | - | - |
| frames per packet (4.2) | 2, 3, 8, 10 | 2, 3, 8, 10 | 1, 2, 3, 8, 10 | 1, 2, 3, 8, 10 | 1, 2, 5, 7 |
| start trim (14.2) | 2048 on frame 0 | 2048 on frame 0 | 2048 on frame 0 | 4096 on frame 0 | 2048 on frame 0 |
| end trim (14.2) | **-** | **-** | **-** | **-** | **-** |

The generated-only cells, for the rows they add:

| path | 44k/2/16 64k | 44k/2/24 440k | 44k/6/16 192k | 48k/2/24 192k | 88k/2/24 384k | 96k/6/24 768k |
|---|---|---|---|---|---|---|
| subframes per channel | 1, 8 | 1, 2, 8 | 1, 2, 6, 8 | 1, 2, 8 | 1, 2, 8 | 1, 2, 8 |
| uniform tiling bit | 1 | 1 | **0 and 1** | 1 | 1 | **0 and 1** |
| group sizes | 2 | 2 | 1, 2, 3, 5, 6 | 2 | 2 | 1, 2, 3, 5, 6 |
| explicit rotation matrix | - | - | 5 | - | - | 6 |
| vector phase covers the block | - | **42** | - | - | - | **88** |
| quantisation step escape | - | 1 | 1 | - | - | **4** |
| modifier widths | 0..3 | 0, 1, 3 | **0..6** | 0..2 | 0..2 | 0..5 |
| resample with no transmission | 3 | - | - | - | - | - |
| extended payload | **45** | - | - | - | - | - |
| continuation count saturates | - | - | - | **1** | - | - |
| continuation count zero | 1 | 2 | - | 1 | 1 | 1 |

What that says, in one paragraph. The committed five reach both frame lengths,
every subframe length, non-uniform tiling with its per-channel presence bits,
group sizes one through six, three of the four channel-transform selections,
both scale factor forms and the 14-bit escape, both coefficient books, both
vector escapes, the run-level escape and the large-value escape, the start
trim, and the continuation count's zero case. The generated six add the
quantisation step escape in quantity, the widest modifier width, the
saturating continuation count, and the case where the vector phase covers a
whole block so the run-level tail is never entered.

The tonal cell the implementation pass committed, measured with that pass's
decoder on the same rows:

| path | 44k/2/16 128k tonal |
|---|---|
| frames | 50 |
| subframes | 58 |
| stereo M/S matrix (9.2) | 55 |
| explicit "no transform" (9.2) | 3 |
| **per-band transform enables (9.5)** | **24** |
| scale factors, DPCM form (10.1) | 100 |
| scale factors, run-level form (10.2) | 10 |
| scale factor 14-bit escape (10.2) | 16 |
| 4-vector escape (11.1) | 2236 |
| 2-vector escape (11.1) | 2882 |
| large-value escape (11.2) | 483 |
| run-level tail reached (11.3) | 110 |
| run-level escape (11.3) | 32 |
| end-of-block symbol (11.3) | 110 |
| quantisation step escape (12.2) | 2 |
| per-channel modifiers (12.3) | 6 |
| start trim (14.2) | 2048 on frame 0 |
| **end trim (14.2)** | **864 on the last frame** |
| continuation count zero (4.1) | 6 |

**Rows nothing reaches**, which become the deficit list in
`docs/quality-gates.md`:

| path | reached by | what to do |
|---|---|---|
| built-in decorrelation matrices (9.3) | nothing | implement from the note, carry as a deficit; the tables ship unused |
| per-band transform enables (9.5) | **the tonal cell, 24 times** (closed by the implementation pass) | a gate row now |
| explicit vector coefficient limit (12.1) | nothing | implement from the note, carry as a deficit |
| post-processing transform field (7.1) | nothing | skip the bits as the note says, carry as a deficit |
| LFE content above the subwoofer cutoff (13.2) | nothing | compute the cutoff, do not apply it, carry as a deficit |
| an end trim (14.2) | **the tonal cell** (closed by the implementation pass) | a gate row now |

Two of those six were reachable with a fixture this pass did not commit; the
implementation pass committed it, and the paragraphs below are how it was
found:

- **Per-band transform enables.** A 100000-frame two-tone file (440 Hz and 550
  Hz, 44100/2/16 at 128 kbps) encoded during this pass reached the per-band
  form **25 times**. Tonal stereo content is what provokes it; the corpus's
  recipe is too broadband. A committed fixture with a sustained tonal stereo
  passage would close this row.
- **An end trim.** The same file, whose 100000-frame length is not a multiple
  of 2048, carried an end trim of 864 on its last frame. Every corpus cell has
  a power-of-two length, so no cell reaches it. A cell of a deliberately odd
  length would close this row, subject to the caveat in section 6.

## 6. Measured behaviour

### Length and alignment

*Measured*, on all eleven cells above plus the ASF demuxer cell: **FFmpeg's
decode is exactly the source frame count**, and a best-fit lag search against
the source returns 0 on every one. So FFmpeg applies the bitstream's trims
itself and a comparison against the source needs no alignment search.

A decoder of our own does not get that for free. It produces one frame of
lead-in that must be dropped (`wma-pro-bitstream.md` section 14.2) before any
comparison, and the bitstream's trims must be applied on top. With both done,
the sample counts match exactly.

**The caveat, and it is a real one.** *Measured* on a file encoded during this
pass with a source of 100000 frames, which is not a multiple of the 2048-sample
frame: the encoder wrote 50 frames and an end trim of 864, giving **99488**
output samples, 512 short of the source. Both FFmpeg and the analysis decoder
agree on 99488, so the loss is upstream of the decode. **Do not assert that a
decode recovers the source length for an arbitrary source length.** Assert it
only for the committed corpus, whose lengths are powers of two, and if an
odd-length cell is ever added, pin the number the encoder actually produced
rather than the source length. The implementation pass added exactly such a
cell (`pro-44100-2ch-16-128k-tonal`, 100000 frames) and it came back 99488
again, with an end trim of 864; the corpus test pins 99488.

### The declared duration overshoots

Unlike WMA Lossless, where the ASF Play Duration falls a few frames short, here
it runs long, because it counts the encoder's own padding:

| cell | frames delivered | frames declared | over by |
|---|---|---|---|
| `pro-44100-2ch-16-128k` | 65536 | 66767 | 1231 |
| `pro-44100-6ch-16-128k` | 65536 | 67561 | 2025 |
| `pro-48000-6ch-24-384k` | 65536 | 67536 | 2000 |
| `pro-96000-2ch-24-384k` | 131072 | 133536 | 2464 |
| `pro-32000-2ch-16-32k` | 49152 | 51200 | 2048 |
| `pro-s16` (ASF cell) | 16384 | 18389 | 2005 |

The lesson is the same in both directions: **the declared duration is advisory
and nothing may be trimmed to it.** `container/asf` already marks every ASF
track `SamplesAdvisory`, which is exactly the right answer and needs no change
for this codec. A caller that needs an exact length must count a read.

### Packet geometry

Every ASF media object is exactly `nBlockAlign` bytes, on every cell, with no
short final object. `ffprobe -show_packets` counts 6 objects on the two 44.1
kHz cells and 7 on the others.

Media object presentation times are **not on a frame grid** and are not evenly
spaced. On `pro-44100-2ch-16-128k` they run 0.000, 0.354, 0.557, 0.731, 1.253,
1.439 seconds. That is not damage: an object's time is the time of the first
frame that **starts** in it, frames cross object boundaries, and a packet can
hold anywhere from one to ten whole frames (*measured*). A container that
treats an object's own time as the first sample a resumed decode produces will
be wrong by up to a frame; snap the landing to the frame grid.

### The oracle's own noise floor

FFmpeg's vectorised decode against its own `-cpuflags 0` scalar decode, per
cell, absolute on a nominal full scale of 1.0:

| cell | RMS | max |
|---|---|---|
| `pro-32000-2ch-16-32k` | 2.05e-08 | 2.38e-07 |
| `pro-44100-2ch-16-64k` | 2.78e-08 | 2.38e-07 |
| `pro-44100-2ch-16-128k` | 3.66e-08 | 2.98e-07 |
| `pro-44100-6ch-16-192k` | 2.97e-08 | 3.58e-07 |
| `pro-44100-2ch-24-440k` | 4.43e-08 | 4.17e-07 |
| `pro-48000-2ch-24-192k` | 3.98e-08 | 3.58e-07 |
| `pro-48000-6ch-24-384k` | 3.22e-08 | 3.58e-07 |
| `pro-88200-2ch-24-384k` | 4.20e-08 | 4.17e-07 |
| `pro-96000-2ch-24-384k` | 4.19e-08 | 4.17e-07 |
| `pro-96000-6ch-24-768k` | 4.13e-08 | 4.17e-07 |

So **nothing can gate below about 4.4e-08 RMS or 4.2e-07 max**, because the
oracle disagrees with itself by that much. Always pin `-cpuflags 0` for the
differential, and set the bound from this table together with the 4.2e-07
worst-case figure the analysis decoder hit against the same scalar path
(`wma-pro-bitstream.md`, the float section): both numbers land in the same
place, which is a useful coincidence rather than a coincidence to rely on.

### The clip is load-bearing in the comparison

A float decode and an integer decode of the same stream are not comparable
until the float one is clipped. *Measured* on `pro-44100-2ch-16-128k`: **940 of
131072 samples exceed full scale**, and comparing FFmpeg's unclipped float
output against Windows' integer PCM gives an RMS of 0.0121, which looks exactly
like a decoder bug. Clip both to the stream's depth first and the same
comparison gives 7.94e-06.

Nothing in the codec clips; the transform's normalisation puts the output at a
nominal full scale of 1.0 and real material overshoots it. The clip belongs to
the PCM conversion, and any harness that crosses the float/integer boundary has
to do it before measuring.

## 7. The two independent decoders, and where they part company

Windows' decoder against FFmpeg's scalar decode of the same bytes, both clipped
to the stream's depth and compared in one sample format, absolute on full scale
1.0:

| cell | channels | RMS | max |
|---|---|---|---|
| `pro-44100-2ch-16-128k` | 2 | 7.94e-06 | 3.05e-05 |
| `pro-48000-2ch-24-192k` | 2 | 1.81e-07 | 1.19e-06 |
| `pro-88200-2ch-24-384k` | 2 | 1.84e-07 | 1.85e-06 |
| `pro-96000-2ch-24-384k` | 2 | 1.80e-07 | 1.55e-06 |
| `pro-44100-2ch-24-440k` | 2 | 1.90e-07 | 1.49e-06 |
| `pro-44100-6ch-16-128k` | 6 | 6.04e-05 | 2.28e-03 |
| `pro-44100-6ch-16-192k` | 6 | 5.53e-05 | 1.88e-03 |
| `pro-48000-6ch-24-384k` | 6 | 8.55e-05 | 2.19e-03 |
| `pro-96000-6ch-24-768k` | 6 | 4.68e-05 | 2.95e-03 |
| `pro-44100-2ch-16-64k` | 2 | 0.188 | 1.65 |
| `pro-32000-2ch-16-32k` | 2 | 0.273 | 1.72 |

Three groups, and each says something different.

**Stereo 24-bit: 1.8e-07.** That is float32 rounding and nothing else. Two
independent implementations agreeing to 22 bits on a file neither of them
encoded is the strongest evidence in this pass that the bitstream note is
right.

The 16-bit stereo cell sits higher, at 7.94e-06 RMS and a max of 3.05e-05,
which is **exactly one LSB of 16-bit**. That is the quantisation of the
comparison itself, not a disagreement.

**Every 5.1 cell: about 5e-05 RMS and 2e-03 max, at both depths, at every rate
and every bit rate.** A thousandfold gap that tracks the channel count and
nothing else. -2.3e-03 on full scale is -53 dB, so it is inaudible and is not a
correctness problem, but a gate written from the stereo figure will fail on
5.1.

The instrumentation narrows the cause without settling it. The multichannel
cells are the only ones that use the explicit rotation matrix of
`wma-pro-bitstream.md` section 9.4 (*measured*: 5 to 7 matrices per cell, and
the stereo cells transmit none). That matrix is built by cascading rotations
through a 33-entry sine table, each entry a `float32`, with `n*(n-1)/2`
rotations for `n` channels: 15 rotations for six channels against one fixed
2x2 matrix for stereo. A different rounding of that cascade, or a different
order of the accumulation in the matrix multiply, reaches every coefficient of
every coupled band and would produce exactly this shape of error. **That is a
hypothesis, not a measurement**; nothing here can tell a rounding difference in
the cascade apart from one in the multiply, and no third implementation exists
to break the tie.

Which number to use:

| shape | differential bound against FFmpeg's scalar path |
|---|---|
| any cell, WaxFlow against FFmpeg | set from section 6: a few times 4.2e-07 max |
| stereo, WaxFlow against Windows | 2e-06 max is achievable |
| 5.1, WaxFlow against Windows | 3e-03 max, and say why in the gate's comment |

Only the first is a gate, because only FFmpeg runs on the machines that run
`make test`. The other two are for a maintainer with Windows, and the 5.1 row
must carry its reason or it reads as slop.

**The last two rows are the refusal.** `pro-44100-2ch-16-64k` and
`pro-32000-2ch-16-32k` are the two cells whose extra-bytes word at offset 16 is
non-zero, and FFmpeg is 19% and 27% wrong on them. The split across all eleven
cells is perfect: zero word to agreement, non-zero word to disagreement, with
nothing in between.

On the 32 kHz cell a 4096-point spectrum at frame 12000 shows FFmpeg producing
nothing above about 8.5 kHz, bin magnitude falling from 4.3 in the 8-9 kHz band
to 0.012 in the 9-10 kHz band, a cliff of 350x, while Windows produces roughly
60 across every band to 16 kHz. The instrumented decoder confirms the cause
from inside: on those two cells and no others, a per-subframe payload is
present that the reference skips (26 of 47 subframes on the 32 kHz cell, 45 of
61 on the 64 kbps cell, never once in the 622 subframes of the other cells),
and on those two cells and no others the coded spectrum stops at about half of
Nyquist. But the disagreement is not only the missing top: below 6 kHz FFmpeg
is still 91% wrong on the 32 kHz cell and 9% wrong on the 64 kbps one, so the
core is reconstructed differently too.

**So there is no oracle for those streams.** `wma-pro-bitstream.md` section 1.2
makes a non-zero word at extra offset 16 a refusal by name, and
`pro-32000-2ch-16-32k` is committed so that the refusal has a fixture.

## 8. Seeking

*Measured*, with the analysis decoder restarted at every packet boundary and
compared frame by frame against a linear decode of the same file:

| cell | resume points | frames after the first that match exactly |
|---|---|---|
| `pro-44100-2ch-16-128k` | 5 | all |
| `pro-44100-6ch-16-128k` | 5 | all |
| `pro-48000-6ch-24-384k` | 6 | all |
| `pro-96000-2ch-24-384k` | 6 | all |
| `pro-32000-2ch-16-32k` | 6 | all |
| all thirteen distinct files | 65 | all |

"Exactly" means a maximum absolute difference of **zero**, not a tolerance.
That is the strongest property this codec has for a container:

- **Every media object is a decode start point.** No key-frame flag is needed
  and none may be depended on. Discard whatever was carried over from the
  previous packet, skip the packet's continuation bits unread, and begin at the
  first frame that starts in the packet.
- **The lead-in is exactly one frame.** The first frame after a resume is wrong
  by up to 1.6 on a full scale of 1.0, because it overlap-adds against a
  transform tail the decoder does not have. The second frame is exact, because
  it overlaps against the first frame's own tail.
- That is because the only state crossing a frame boundary is the overlap-add
  tail and the previous subframe length. Scale factor reuse resets at every
  frame, quantisation is per subframe, and channel transforms are per subframe.

This is the same shape as WMA Lossless (a resumed decode is bit-identical) and
the opposite of WMA v1/v2 (a resumed decode draws different noise forever), but
it gets there by a different route: Lossless needs to find a seekable tile and
can fail to, while here **every** packet qualifies and only the one-frame
pre-roll is needed.

Two things the container must still do, both already learned from WMA Lossless
and both still true here:

**Snap the landing onto the frame grid.** An object's presentation time is in
milliseconds and is not the first sample a decode resumed at it produces. The
irregular object times in section 6 make that unmissable here.

**Check the landing against the position it produced, never the position it
was asked for.** `ffmpeg -ss` lands where its own ASF seek puts it, which is
a property of the demuxer and not of the codec.

## 9. What nothing in this tree can reach

Each of these is describable by the format and unproducible here. The bitstream
note says what to do about each; this is the list of what has no fixture and
why.

- **Mono, and 3, 4, 5 or 7 channels.** Windows' encoder offers 2, 6 and 8 only.
  A mono stream is describable by the container and the decoder should handle
  it, but nothing here can test that.
- **Eight channels.** Offered by the encoder at 48 and 96 kHz, but no 8-channel
  cell is committed or generated, so the 7- and 8-channel group paths, and in
  particular the refusal of a built-in decorrelation matrix for a group above
  six channels, have no fixture.
- **Frame lengths other than 2048 and 4096.** The rate arms for 512, 1024 and
  8192 samples need rates at or below 22050 or above 96000, which the encoder
  does not offer, and the decode flags' frame-length adjustment is zero on
  every format it does offer.
- **Subframe depths other than 16.** Both decode-flags words set the same
  depth, so the other five encodings of the subframe length field, including
  the raw form used at depths 2, 8 and 32, are untested.
- **Frames with no length prefix.** Both decode-flags words set the prefix bit.
  The unprefixed frame layer, where a frame ends at the first 1 bit, has no
  fixture at all.
- **The built-in decorrelation matrices.** Never selected on any cell; the
  encoder always transmits an explicit rotation instead. The tables ship unused.
- **An explicit vector coefficient limit.** Never transmitted, so the variant
  of the coefficient phase where the zero-run switch is disabled is untested.
- **The post-processing transform field in the frame header.** Never present.
- **An LFE channel with coded content above the subwoofer cutoff.** Never
  happens, which is why the reference's ineffective cutoff and a correct one
  cannot be told apart here.
- **Any third value of the extra word at offset 16.** Only 0x0000, 0xc042 and
  0x20c6 exist in the encoder's whole enumeration, so which bits of it actually
  enable the low-bit-rate tool cannot be settled. The refusal predicate is
  "non-zero" for that reason.
- **A stream the low-bit-rate tool decodes correctly.** Two cells carry the
  tool and neither can be decoded by anything in reach except Windows. They are
  refusal fixtures, permanently, unless someone implements the tool.

## 10. Speed, for scale

FFmpeg decodes `pro-44100-2ch-16-128k` (1.49 seconds of audio) in 0.02 seconds
on this box and `pro-96000-2ch-24-384k` (1.37 seconds at 96 kHz) in 0.20, both
with the scalar path pinned. Those are startup-dominated and neither is a
target. The
realtime floors `docs/quality-gates.md` records for this decoder are measured
on WaxFlow's own decoder, not on FFmpeg's.
