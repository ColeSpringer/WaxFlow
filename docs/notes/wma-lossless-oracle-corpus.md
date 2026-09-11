# WMA Lossless oracle corpus and black-box behaviour

ADR-0001 black-box analysis artifact, the oracle half of the WMA Lossless
analysis pass. Everything here was produced by running Windows' own encoder
and the `ffmpeg` binary, both of which ADR-0001 permits as tools. It records
what the corpus is, how it was made, what the two implementations do, and
what neither can be asked.

Measured with `ffmpeg version 8.0.1-3ubuntu2` (libavcodec 62) on this WSL box,
and with the Windows Media Format SDK on Windows 11 build 26100 reached over
`powershell.exe` interop. Figures are from those builds; the identities
(packet size, frame length, zero delay, exact sample count) are exact and
portable, the speed figures are not.

## 1. The oracle here is not FFmpeg

For WMA v1/v2 the differential asks "do we agree with FFmpeg", because FFmpeg
is both the only encoder and the only decoder in reach. Lossless is the
opposite case and a much stronger one:

**A lossless decode must return the encoder's input, sample for sample.** The
source PCM is therefore the oracle, it is synthesized deterministically in Go
(section 3), and a mismatch is a bug rather than a tolerance to argue about.
No tool takes part in that comparison, so it runs on every platform including
one with nothing installed.

FFmpeg is the secondary check, and it is a real one: it is an independent
implementation, and its decode of every cell below is byte-identical to the
source. Two implementations agreeing bit-exactly on a file neither wrote is
what makes the fixtures trustworthy without a specification.

Its SIMD and scalar paths agree here, checked with `-cpuflags 0` on all three
committed cells, so this codec needs none of the `FFmpegDecodeF32NoSIMD`
care that `codec/wma` and `codec/vorbis` take. That is what an all-integer
decoder should do, and checking costs one flag.

The asymmetry to keep in mind: **only Windows can write this format**, so a
reviewer on Linux can verify a committed fixture against FFmpeg's decode and
against the regenerated source, but cannot reproduce the file itself. The
exact command line for each cell is in section 3 so the attempt is at least
well defined.

## 2. Reaching Windows' encoder

`scripts/wmfenc/wmfll.ps1` is the whole story and its header carries the
detail. The three findings worth repeating here, because each looks like a
missing feature from the outside:

1. **Media Foundation cannot write this format.** `MediaTranscoder` refuses
   the lossless subtype with `InvalidProfile` at every bit rate, and the MF
   ASF file sink refuses it with `MF_E_ASF_UNSUPPORTED_STREAM_TYPE`
   (0xC00D3AA0) even when handed a media type the encoder itself produced.
   The write path that produces WMA v2 for `wmfenc.ps1` is a dead end here.
2. **The encoder MFT enumerates no lossless output type.**
   `MFTranscodeGetAudioOutputAvailableTypes` returns zero of them, and so does
   `IMFTransform::GetOutputAvailableType` after a PCM input type is set,
   although the WMAudio Encoder MFT registers 0x0163 among its output
   subtypes. That is not a bug: a lossless output type has no bit rate to
   enumerate over, since its output is its input.
3. **The Format SDK lists the codec with zero formats until VBR is enabled.**
   `IWMCodecInfo3` names "Windows Media Audio 9.2 Lossless" and reports
   `GetCodecFormatCount` 0 for it. After
   `SetCodecEnumerationSetting(WMMEDIATYPE_Audio, codec, "_VBRENABLED",
   WMT_TYPE_BOOL, true)` it reports eight, each carrying the 18 bytes of
   codec private data the encoder will accept. Nothing else unlocks it; the
   `NumChannels`, `AudioSamplingRate` and `BitsPerSample` settings the SDK
   documents for other codecs are all refused here with 0xC00D0BCD.

Writing then goes through `IWMWriter`, which is the path Windows Media
Player's own lossless rip uses.

### The envelope

Those eight formats are the entire set of streams this encoder can produce,
measured rather than assumed (`wmfll.ps1 -List` reprints it):

| Rate | Channels | Depth | nBlockAlign | Extra bytes |
|---|---|---|---|---|
| 44100 | 2 | 16 | 13375 | `1000 03000000 00000000 00000000 a101 0000` |
| 44100 | 2 | 24 | 13375 | `1800 03000000 ...` |
| 48000 | 2 | 24 | 12288 | `1800 03000000 ...` |
| 48000 | 6 | 24 | 12288 | `1800 3f000000 ...` |
| 88200 | 2 | 24 | 13375 | `1800 03000000 ...` |
| 88200 | 6 | 24 | 13375 | `1800 3f000000 ...` |
| 96000 | 2 | 24 | 12288 | `1800 03000000 ...` |
| 96000 | 6 | 24 | 12288 | `1800 3f000000 ...` |

**There is no mono format, no 16-bit format above 44.1 kHz, and no 5.1 format
at 44.1 kHz.** Everything the corpus can cover is in that table, and
everything it cannot is in section 5. The extra bytes vary in exactly two
places: the first word is the depth (0x0010 or 0x0018) and the second field is
the channel mask (3 for stereo, 0x3f for 5.1). The decode-flags word at offset
14 is **0x01a1 on every stream this encoder writes**, so the corpus exercises
one flag combination and no other.

## 3. Generating the corpus

The source is synthesized in Go with **no floating point anywhere**, so the
sample at frame `i` is the same on every platform, Go version and
architecture. That is not a stylistic choice: the committed fixtures are
compared against a regenerated source rather than against a checked-in copy of
it, so the recipe has to be exactly reproducible or the gate is comparing
against a moving target.

Noise comes from splitmix64 rather than from a library generator, for the same
reason: the algorithm is four lines and carries its own stability.

```
seed(c)  = 0x5741584C4F5700 + c*0x100          per channel c
next()   = s += 0x9E3779B97F4A7C15
           z  = s;  z = (z ^ (z>>30)) * 0xBF58476D1CE4E5B9
                    z = (z ^ (z>>27)) * 0x94D049BB133111EB
           return z ^ (z>>31)

tri(i,p,a) = let x = i mod p, y = 4*a*x/p  (integer division, 64-bit)
             if y > 2a then y = 4a - y
             y - a

full     = 2^(bits-1) - 1
n(i)     = (next() mod (2*full+1)) - full
p1(c)    = 97 + 13c        p2(c) = 251 + 29c
```

Four equal segments, each a whole number of coded frames (section 6), each
reaching something a lossless decoder has to get right:

| Segment | Frames | Value | What it reaches |
|---|---|---|---|
| 1 | `[0, n/4)` | `tri(i,p1,3*full/8) + tri(i,p2,2*full/8)` | tonal, correlated across channels: the filters and the channel transform |
| 2 | `[n/4, n/2)` | `tri(i,p1,2*full/8) + tri(i,p2,full/8) + n(i)/4` | the ordinary mixed case |
| 3 | `[n/2, 3n/4)` | `0` | digital silence, every residual zero |
| 4 | `[3n/4, n)` | `n(i)` | full-scale noise: incompressible, and the escape paths |

The result is clamped to `±full`. The silence-to-noise edge at three quarters
is a hard transient, and it is deliberately on a frame boundary so a failure
names one frame rather than smearing across two.

**The 64-bit intermediate in `tri` is load-bearing.** At 24 bits with the
widest period here, `4*a*x` reaches 2.34e9, which is past what a 32-bit `int`
holds. Computed in one it wraps negative and the value clamps to `-full`, so
the same recipe produces different samples on a 32-bit build and the committed
fixtures decode "wrong" there while the decoder is fine. `make test-386` is
what found it, on the FFmpeg cell as well as ours, which is what said the fault
was in the recipe rather than in the decoder.

Channels are decorrelated by construction (different triangle periods and
different noise streams), and the generator prints `max|c0-c1|` per cell so
the WMA corpus note's trap, a stereo fixture that is secretly mono, cannot
recur silently: it is 64892 of 65535 on the 16-bit cell and around 16.6M of
16.7M on the 24-bit cells. The one cell where it is 0 is `-dup`, where that is
the point.

Each cell is written as a canonical PCM WAV and encoded with:

```sh
powershell.exe -NoProfile -ExecutionPolicy Bypass \
    -File scripts/wmfenc/wmfll.ps1 -In <cell>.wav -Out <cell>.wma
```

**The encoder is not byte-deterministic, but its payload is.** Two runs over
the same input differ in 52 bytes, all of them ASF identity: the File ID GUID
at offset 54 and one field near the end. The decoded audio is identical. A
regenerated fixture will therefore not match a committed one byte for byte,
and should not be expected to.

## 4. The corpus

| Cell | Rate | Ch | Depth | Frames | Coded frames | Packets | Bytes | Committed |
|---|---|---|---|---|---|---|---|---|
| `ll-44100-2ch-16` | 44100 | 2 | 16 | 16384 | 8 | 4 | 58789 | yes |
| `ll-44100-2ch-16-dup` | 44100 | 2 | 16 | 16384 | 8 | 2 | 31977 | yes |
| `ll-44100-2ch-16-skip` | 44100 | 2 | 16 | 15000 | 8 | 3 | 45383 | yes |
| `ll-44100-2ch-24` | 44100 | 2 | 24 | 16384 | 8 | 6 | 85601 | yes |
| `ll-44100-2ch-24-pad` | 44100 | 2 | 24 | 16384 | 8 | 4 | 58789 | yes |
| `ll-48000-2ch-24` | 48000 | 2 | 24 | 32768 | 16 | 11 | 140674 | no |
| `ll-48000-6ch-24` | 48000 | 6 | 24 | 8192 | 4 | 9 | 116040 | yes |
| `ll-88200-2ch-24` | 88200 | 2 | 24 | 32768 | 8 | 10 | 139225 | no |
| `ll-88200-6ch-24` | 88200 | 6 | 24 | 16384 | 4 | 16 | 219665 | no |
| `ll-96000-2ch-24` | 96000 | 2 | 24 | 32768 | 8 | 11 | 140674 | no |
| `ll-96000-6ch-24` | 96000 | 6 | 24 | 16384 | 4 | 17 | 214592 | no |

`ll-44100-2ch-16-dup` is the same recipe with channel 0 copied over channel 1,
and it is the only cell that reaches the **one-sided** case of the two-channel
lifting: the encoder leaves channel 1's coded flag clear and the decoder has to
reconstruct it from the transform rather than from any bits
(`wma-lossless-bitstream.md` section 8.1). It costs almost nothing, half the
packets of its decorrelated twin, because the second channel carries no
residual at all.

Two more cells exist to reach one field each, and both were added because a
mutation showed the field was dead. `ll-44100-2ch-16-skip` is 15000 frames,
which is not a whole number of coded frames, so its last frame carries an END
SKIP of 1384; every other cell here is an exact multiple and leaves that field
zero, and deleting the decoder's end-skip term passed the whole corpus.
`ll-44100-2ch-24-pad` carries the 16-bit signal in a 24-bit stream, which is
the only thing that makes the encoder set PADDING ZEROES (measured 8, as the
bitstream note says): it is the one cell that exercises the output stage's
shift, and without it the shift could be deleted and everything stayed green.
It costs nothing, compressing to exactly the size of its 16-bit twin.

The six committed cells live in `codec/wmalossless/testdata/corpus/` with a
`MANIFEST.md` repeating each cell's command line, and they are 400 KiB
together. The other five shapes are generated on Windows and never committed.

`container/asf/testdata/lossless-s16.wma` is the demuxer's cell: the same
recipe at 8192 frames, so 2048 per segment, two packets, 31977 bytes. It
exists so the ASF tests do not reach into another package's testdata.

## 5. What the corpus reaches, and what nothing can

Reached, on every cell: tonal and noisy content, digital silence, full-scale
incompressible noise, a hard transient on a frame boundary, decorrelated
stereo, and both frame lengths. The silence quarter of every cell is where
neither channel is coded; the `-dup` cell is where exactly one is.

Reached only by some: 16-bit reconstruction (44.1 kHz only), 24-bit
reconstruction (everything else), the 5.1 channel mask (48/88.2/96 kHz), and
the 4096-sample frame (88.2/96 kHz).

**Out of reach, and the reason in each case:**

- **Mono, and any 16-bit stream above 44.1 kHz.** Windows' encoder offers no
  such format. Nothing in the tree can write one, so a decoder that mishandled
  a mono stream would fail no test here. The decoder should still accept one
  if a file arrives, since the container can describe it.
- **Any decode-flags word other than 0x01a1.** One encoder, one setting.
  Whatever the other bits select is unreachable and untested.
- **32-bit and other depths.** Not offered; refused by name.
- **More than 6 channels.** Not offered. The 8-channel case the format
  describes has no fixture.
- **Arithmetic coding**, which a seekable tile can select in place of the
  Golomb residual coder. Never set here, and no description of it exists
  anywhere, so it is refused by name.
- **The LPC mode**, which decode-flags bit 0x100 makes reachable. The flag
  **is** set on every file above and FFmpeg decodes all of them bit-exactly
  with no warning: `wma-lossless-bitstream.md` section 10.2 settles why. Bit
  0x100 only makes a **per-subframe** LPC flag present in the bitstream, and
  that per-subframe flag is what FFmpeg's "Expect wrong output" warning is
  gated on. No encoder here ever sets it, so the mode is reachable, unused,
  and refused by name; where it does appear, FFmpeg is not an oracle for it.

## 6. Measured behaviour

**Delay is zero and the sample count is exact.** Every cell decodes to
precisely the frames that went in, aligned at offset 0, on both
implementations. There is no encoder priming to compensate and no tail to
trim. This is the strongest single property of the format from a pipeline's
point of view and it is what makes the source-PCM gate possible at all.

**The declared length is short of the truth, in every cell.** The ASF Play
Duration rounds to milliseconds, so the length the container declares is
always a little under what the stream delivers:

| Cell | Frames delivered | Frames declared | Short by |
|---|---|---|---|
| `ll-44100-2ch-16` | 16384 | 16317 | 67 |
| `ll-44100-2ch-24` | 16384 | 16317 | 67 |
| `ll-48000-2ch-24` | 32768 | 32736 | 32 |
| `ll-48000-6ch-24` | 8192 | 8160 | 32 |
| `ll-88200-2ch-24` | 32768 | 32722 | 46 |
| `ll-88200-6ch-24` | 16384 | 16317 | 67 |
| `ll-96000-2ch-24` | 32768 | 32640 | 128 |
| `ll-96000-6ch-24` | 16384 | 16320 | 64 |

So the declared length is **advisory**, exactly as `container.Track.
SamplesAdvisory` records for WMA, and **trimming a decode to it would destroy
real audio** at the rate of one millisecond per file. A lossless codec is the
worst place to do that. The rule is: deliver what the decoder produces.

**Every packet is exactly `nBlockAlign` bytes and every one is a sync point.**
Measured on all nine cells: 13375 bytes at 44.1 and 88.2 kHz, 12288 at 48 and
96 kHz, no short final packet, and `ffprobe` flags every packet `K`. The
container pads.

**A packet holds a variable amount of audio**, which is what a variable-rate
codec in a fixed packet implies, and it shows in the presentation times. On
the 10-second bench cell they run 0.000, 0.185, 0.324, 0.464, 0.603, 0.789,
0.928, ... : spacings of 139 to 186 ms with no pattern. Nothing can be
computed from a packet index.

The times do not land on frame boundaries either. `ll-44100-2ch-16` holds 8
coded frames of 2048 samples, so frames start at 0, 2048, 4096, ... , and its
four packets are timed 0.000, 0.138, 0.324 and 0.370 s, which is 0, 6086,
14288 and 16317 samples. None of the last three is a multiple of 2048, and the
last is the declared total. Whatever a packet time means here, it is not "the
first sample this packet decodes to", and no seek should assume it is.

**Frame length is 2048 samples at 44.1 and 48 kHz and 4096 at 88.2 and 96
kHz.** Measured from FFmpeg's decoded frame counts against known sample
counts, on all eight cells; the bitstream note gives the rule this is a
consequence of.

## 7. Seeking

The ASF index is in millisecond presentation times, and packets hold variable
amounts of audio, so a seek is approximate in both directions. Measured with
`ffmpeg -ss` on the 10-second bench cell, against the frames that should
remain:

| Target | Frames delivered | Frames expected |
|---|---|---|
| 1 s | 401408 | 398268 |
| 3 s | 303104 | 310068 |
| 5 s | 106496 | 221868 |
| 7 s | 106496 | 133668 |

Over at one target, under at the rest, and the last two land in the same
place. That is FFmpeg's ASF seek rather than a property of the codec, but the
lesson from `wma-oracle-corpus.md` holds unchanged and is now measured on a
second codec: **a seek landing has to be checked against the position it
produced, never against the position it was asked for**, and a caller that
needs an exact length must count a read rather than seek past the end.

What this decoder does is stronger and simpler, because a seekable tile clears
every filter: a decode started at any packet is **bit-identical** to the same
samples of a decode that started at the beginning. No tolerance, no
correlation gate, no ratio, which is the opposite of `codec/wma`'s situation
where a resumed decode draws different noise forever.

How often a landing succeeds is a property of the encoder's tile spacing, and
it is measured per cell:

| Cell | Packets | Landings that reach a tile |
|---|---|---|
| `ll-44100-2ch-16` | 4 | 3 |
| `ll-44100-2ch-16-dup` | 2 | 1 |
| `ll-44100-2ch-16-skip` | 3 | 3 |
| `ll-44100-2ch-24` | 6 | 5 |
| `ll-44100-2ch-24-pad` | 4 | 3 |
| `ll-48000-6ch-24` | 9 | 1 |

Most packets on the stereo cells; only the first on the 5.1 cell. That is not
an anomaly: the encoder places seekable tiles on a **time** interval of about
0.4 seconds rather than a packet interval, and the 5.1 cell is 0.17 seconds
long, so it holds exactly one and its other eight landings reach no tile before
the end of the file. A landing that produces nothing is legal and must not be
reported as damage.

### Two things a container has to do with that

Neither was obvious and both were found by an engine-level seek test rather
than by a codec-level one, which is the reason that test exists.

**Land on an object the decoder can begin at.** Every ASF media object used to
be handed over as a sync point, which is true for WMA v1/v2 and false here.
The engine's pre-roll decodes and discards forward from the landing, so a
landing the decoder cannot start at produces nothing, the discard comes up
short, and the position reported has no samples behind it. Measured before the
fix on `ll-48000-6ch-24`: seeking to 4096 of 8192 frames reported a landing of
2016 and then delivered nothing at all, with three quarters of the file
unreachable. The codec's own packet-header flag is what says which objects
qualify.

**Snap the landing onto the frame grid.** An object's presentation time is not
the first sample a decode resumed at it will produce. ASF states times in
milliseconds, and the difference is real: measured 48 and 58 samples against
frames of 2048. The engine's pre-roll assumes the landing is where the decode
starts, so reporting the object's own time leaves it short by exactly that
much, which showed up as a seek near the end of a file landing 48 samples early
and delivering 48 too few. The true landing is the nearest frame boundary,
which the millisecond rounding can never be more than a fraction of a frame
away from.

## 8. Speed, for scale

FFmpeg decodes the 10-second 44.1 kHz stereo 16-bit cell at 874x realtime on
this box, and the 96 kHz 5.1 cell at 77x (the second figure is startup
dominated and should not be compared to the first). These are context for the
realtime floor `docs/quality-gates.md` records for this decoder, not a target:
the floors in that file are measured on WaxFlow's own decoder.
