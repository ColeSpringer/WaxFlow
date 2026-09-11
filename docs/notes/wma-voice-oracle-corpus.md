# WMA Voice oracle corpus and black-box behaviour

ADR-0001 black-box analysis artifact, the oracle half of the WMA Voice analysis
pass. It is the sibling `wma-voice-bitstream.md` names, and together the two are
the only inputs the session that writes the decoder consumes. Everything here
was produced by running Windows' own encoder and decoder and the `ffmpeg`
binary, all of which ADR-0001 permits as tools, and by an instrumented decoder
written during the analysis pass from the bitstream note and then deleted.

Measured with `ffmpeg version 8.0.1-3ubuntu2` (Ubuntu, gcc 15) on this WSL box,
cross-checked against a locally built FFmpeg n9.0 at the pinned commit
(*measured*: the two are **bit-identical** on all seven cells with `-cpuflags 0`
and `--disable-asm`, so the pin and the box agree and nothing below depends on
which one runs). Windows-side numbers come from the Windows Media Format SDK
and Media Foundation on Windows 11 reached over `powershell.exe` interop. The
identities (packet size, superframe count, exact sample count) are exact and
portable; the floating-point figures are from these builds.

## 1. Which oracle answers which question

Three sources of truth are in reach and none of them is the source PCM.

**The source PCM is not an oracle here.** WMA Voice is a low-rate speech codec:
at 4 kbit/s it throws away almost everything. The corpus's source WAVs are
regenerated deterministically (section 3) and they pin the **alignment**,
because the decode is exactly as long as the source and lines up at offset zero
with no lag, but a sample-by-sample comparison against them measures the
encoder, not the decoder.

**FFmpeg's scalar decode is the differential**, and it is the only one that runs
on the machines that run `make test`. FFmpeg has **no encoder** for this format,
so every fixture here is foreign to it. Pin the scalar path with `-cpuflags 0`;
section 6 gives the size of the vectorised path's disagreement with itself.
Section 7 gives the one place where the reference is simply wrong and a gate
must skip it.

**Windows' own decoder is the third implementation**, reached through Media
Foundation with `scripts/wmfenc/wmfenc.ps1 -Decode`. It costs a Windows box, so
it cannot be a gate. It is what settled the open question this pass was
dispatched with (section 7), and it is also the reason section 7 ends with a
disagreement nobody in reach can arbitrate.

## 2. Reaching Windows' encoder

`scripts/wmfenc/wmfenc.ps1 -Subtype "{0000000A-0000-0010-8000-00AA00389B71}"`
writes this format through Media Foundation's transcoder. The command line for
each cell is in `codec/wmavoice/testdata/corpus/MANIFEST.md`.

### The envelope

`IWMCodecInfo3` enumerates **seven** format rows for the codec Windows names
**Windows Media Audio Voice 9**, and no more. Every one is mono and 16-bit.
`_VBRENABLED` changes nothing here; the enumerator sets it anyway.

| # | rate | avg bytes/s | `nBlockAlign` | bit rate |
|---|---|---|---|---|
| 0 | 8000 | 500 | 150 | 4000 |
| 1 | 8000 | 625 | **187** | 5000 |
| 2 | 8000 | 1000 | 300 | 8000 |
| 3 | 11025 | 1250 | 544 | 10000 |
| 4 | 16000 | 1500 | 450 | 12000 |
| 5 | 16000 | 2000 | 600 | 16000 |
| 6 | 22050 | 2500 | 1088 | 20000 |

The full 46-byte extradata of each is in the manifest, and
`wma-voice-bitstream.md` section 1.3 decodes all seven field by field.

Two behaviours to know before trusting an encode, both the same trap stage 6
added `-Bits` for:

1. **Media Foundation renegotiates rather than refusing.** Ask for 32 kHz or
   44.1 kHz and you get a **22050 Hz** file. Ask for stereo and you get mono.
   The encode succeeds and the output looks fine from the outside. Read back
   what you actually got, every time.
2. **The bit rate lands on one of the seven**, whatever you asked for.

Because the envelope is seven rows and the corpus is seven cells, **the corpus
covers the whole envelope**. There is no wider generated set for this codec and
none is possible: no cell here is a sample of a larger space of encoder
settings. What the corpus does not cover is the space of *bitstreams* the
format allows but this encoder never writes, and section 9 lists that.

## 3. The corpus

`codec/wmavoice/testdata/corpus/`, seven cells, 76 KiB total, one per format:

| File | rate | bit rate | bytes | source frames | media objects |
|---|---|---|---|---|---|
| `voice-8000-4k.wma` | 8000 | 4000 | 7955 | 31863 | 18 |
| `voice-8000-5k.wma` | 8000 | 5000 | 5615 | 31863 | 18 |
| `voice-8000-8k.wma` | 8000 | 8000 | 6991 | 31863 | 16 |
| `voice-11025-10k.wma` | 11025 | 10000 | 7457 | 43917 | 10 |
| `voice-16000-12k.wma` | 16000 | 12000 | 8912 | 63727 | 15 |
| `voice-16000-16k.wma` | 16000 | 16000 | 10533 | 63727 | 14 |
| `voice-22050-20k.wma` | 22050 | 20000 | 12897 | 87833 | 10 |

`container/asf/testdata/voice-mono.wma` is the same recipe at 16000 Hz and
12000 bit/s truncated to 16000 frames (1 s, 4 media objects, 34 superframes),
so the demuxer's differential has a 0x000A file without carrying one of these.

**Only Windows can regenerate any of it.** FFmpeg decodes this format and does
not encode it, and no other encoder for it exists. The committed bytes are the
whole test basis, and a reviewer without a Windows box cannot rebuild them.
That is also what makes them a good differential: they are foreign to the only
decoder the test suite can run.

### The source

A speech-like synthesis, all integer, deterministic, described by the recipe the
implementation session's `corpus_test.go` will carry. Eight equal segments over
about 3.98 s:

| segment | content |
|---|---|
| 0 | voiced, falling pitch 200 to 105 Hz |
| 1 | unvoiced, high-passed noise |
| 2 | digital silence |
| 3 | voiced, rising pitch 105 to 200 Hz, with breath noise |
| 4 | half voiced at 150 Hz, half noise |
| 5 | digital silence |
| 6 | noise near the floor, amplitude 300 |
| 7 | loud voiced from a standing start, 90 to 240 Hz |

The voiced segments are built from a single 48-sample glottal pulse whose two
damped sinusoids sit at a twelfth and a fifth of the sample rate, so every cell
carries the same sound **in the codec's own terms** rather than at the same
absolute frequencies. Lengths are deliberately not a multiple of any plausible
frame size, which is what makes the last superframe's declared sample count
(section 5) reachable on every cell.

The two silence segments are what turned this corpus from a coverage exercise
into the thing that found the reference's defect: they are where the encoder
drops to a hundred bits per superframe, and that is what overruns the
reference's carry buffer (section 6).

A regeneration will not be byte-identical: the encoder writes a fresh File ID
GUID every run.

## 4. What the corpus reaches

From an instrumented decoder run over every cell, counting each bitstream path
as it was taken. A number is a count of occurrences; a list is the set of values
observed; a dash means the path was never taken. Section numbers refer to
`wma-voice-bitstream.md`. **The implementation pass should pin every one of
these numbers against its own path counters.**

| path (section) | 8k/4k | 8k/5k | 8k/8k | 11k/10k | 16k/12k | 16k/16k | 22k/20k |
|---|---|---|---|---|---|---|---|
| media objects (3.1) | 18 | 18 | 16 | 10 | 15 | 14 | 10 |
| superframes (3.2) | 67 | 67 | 67 | 92 | 133 | 133 | 183 |
| frames | 201 | 201 | 201 | 276 | 399 | 399 | 549 |
| blocks | 558 | 688 | 968 | 768 | 980 | 980 | 1467 |
| LSP order (1.1) | 10 | 10 | 10 | 10 | 16 | 16 | 16 |
| residual LSP superframes (5.4) | 67 | 67 | 67 | 92 | 133 | 133 | 183 |
| **independent LSP frames (5.4)** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| distinct LSP interpolation indices (5.4) | 23 | 23 | 23 | 27 | 28 | 28 | 30 |
| LSP stabiliser: first clamp (5.6) | - | - | - | - | - | - | - |
| LSP stabiliser: last clamp (5.6) | - | - | - | - | - | - | - |
| LSP stabiliser: reorder (5.6) | - | - | - | - | - | - | - |
| superframes with a sample count (3.4) | 1 | 1 | 1 | 1 | 1 | 1 | 1 |
| the value it carries | 183 | 183 | 183 | 237 | 367 | 367 | 473 |
| **statistics field present (3.4)** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| packets with spillover zero (3.1) | 1 | 1 | 2 | 6 | 8 | 11 | 6 |
| packets with spillover non-zero | 17 | 17 | 14 | 4 | 7 | 3 | 4 |
| superframes per packet, values (3.1) | 1,2,3,5,7,9 | 1,2,3,5,7,8,9 | 1,2,3,4,8,9 | 5,7,9,10,11,13 | 1,6,10,14,15,17 | 5,7,8,10,15 | 9,14,16,20,23,24 |
| **superframe count escape (3.1)** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| carry above 2048 bits (3.3) | - | - | - | **3** | **4** | **6** | **5** |
| frame type 0, silence (4) | 40 | 40 | 40 | 56 | 82 | 82 | 115 |
| frame type 1, hardcoded | 62 | 62 | 60 | 84 | 185 | 185 | 192 |
| frame type 2, window pulses | 15 | 4 | - | - | - | - | - |
| frame type 3 | 2 | 2 | - | - | - | - | - |
| frame type 4 | 2 | 10 | - | - | - | - | - |
| frame type 5 | 2 | 5 | - | - | - | - | - |
| frame type 6 | 26 | 26 | - | - | - | - | - |
| frame type 7 | 15 | 6 | - | - | - | - | - |
| **frame types 8, 9, 10** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| frame type 11 | 9 | 3 | - | - | - | - | - |
| frame type 12 | 19 | 3 | - | - | - | - | - |
| frame type 13 | - | - | - | 136 | 132 | 132 | 242 |
| **frame type 14** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| frame type 15 | - | 1 | - | - | - | - | - |
| frame type 16 | 9 | 39 | 101 | - | - | - | - |
| asymmetric-codebook blocks (6.1, 9) | 210 | 180 | - | - | - | - | - |
| asymmetric interpolation runs (9) | 1978 | 1388 | - | - | - | - | - |
| asymmetric interpolation phases seen | 1..8 | 1..8 | - | - | - | - | - |
| **asymmetric interpolation phase 9** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| pitch reset by the 5% rule (6.1) | 3 | 7 | - | - | - | - | - |
| **pitch clamped to maxPitch-1 (6.1)** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| Hamming first-block pitch fields (6.2) | 37 | 46 | 101 | 136 | 132 | 132 | 242 |
| Hamming delta pitch fields (6.2) | 147 | 298 | 707 | 408 | 396 | 396 | 726 |
| Hamming phases 0/1/2/3 (6.2) | 97/26/34/27 | 204/27/81/32 | 624/33/110/41 | 409/20/83/32 | 377/27/101/23 | 377/27/101/23 | 585/43/277/63 |
| pitch conversion arm 1 (6.2) | 100 | 119 | 320 | 204 | 246 | 246 | 410 |
| pitch conversion arm 2 | 43 | 154 | 370 | 264 | 236 | 236 | 462 |
| pitch conversion arm 3 | 41 | 71 | 118 | 76 | 46 | 46 | 96 |
| **pitch conversion arm 4, saturating** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| gain predictor weight 1 (7.3) | 72 | 320 | 808 | - | - | - | - |
| gain predictor weight 2 | 284 | 172 | - | 544 | 528 | 528 | 968 |
| gain predictor weight 4 | 38 | 32 | - | - | - | - | - |
| window pulse range 24 (8.2) | 15 | 4 | - | - | - | - | - |
| **window pulse range 16 (8.2)** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| **window extended index (8.2)** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| **window zero-count branch (8.2)** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| **window concealment (8.2)** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |
| distinct silence gain indices (7.1) | 1 | 1 | 1 | 1 | 1 | 1 | 1 |
| postfilter Kalman succeeded (10.3) | 197 | 198 | 201 | 271 | 264 | 264 | 474 |
| postfilter Kalman failed (10.3) | 1 | - | 1 | 1 | - | - | 10 |
| postfilter denoise ran (10.5) | 322 | 322 | 322 | 440 | 634 | 634 | 868 |
| denoise tilt correction (10.5) | - | - | **on** | - | - | - | - |
| **DC removal filter (10.7)** | **-** | **-** | **-** | **-** | **-** | **-** | **-** |

The demuxer cell, `container/asf/testdata/voice-mono.wma`, on the rows it
changes: 4 media objects, 34 superframes, 102 frames, 304 blocks, frame types 1
(52) and 13 (50), **no silence frames at all**, one sample-count superframe
carrying 160, and 3 packets with non-zero spillover.

What that says in one paragraph. The corpus reaches both LSP orders, both
quantiser modes, both default modes, both fixed-codebook gain paths, all four
block counts, thirteen of the seventeen frame types, both adaptive codebook
kinds, all nine phases of the Hamming interpolator's low three arms plus eight
of the nine asymmetric phases, all three non-saturating arms of the pitch
conversion ladder, all three gain predictor weights, both denoise tilt settings,
the packet layer's spillover in both the zero and non-zero cases, superframe
counts from 1 to 24, and the sample-count field on every cell. It reaches
**none** of: the independent LSP path, the statistics field, the superframe
count escape, frame types 8, 9, 10 and 14, the window coder's 16-sample range,
its extended index, its zero-count branch and its concealment, the saturating
pitch conversion arm, LSP stabilisation, and the DC removal filter.

**Rows nothing reaches**, which become the deficit list in
`docs/quality-gates.md`:

| path | what to do |
|---|---|
| the independent per-frame LSP path (5.4) | implement from the note, carry as a deficit; it is a large amount of untested code |
| frame types 8, 9, 10, 14 (4) | implement from the note, carry as a deficit; they are rows of a fixed table, so the risk is low |
| the window coder's other arms (8.2) | implement from the note, carry as a deficit; this is the **highest-risk** untested code in the codec |
| the statistics field (3.4) | skip by its declared length, carry as a deficit |
| the superframe count escape (3.1) | implement, carry as a deficit |
| the saturating pitch arm (6.2) | implement, carry as a deficit |
| LSP stabilisation (5.6) | implement, carry as a deficit |
| the DC removal filter (10.7) | implement, carry as a deficit |
| asymmetric interpolation phase 9 (9) | it falls out of the same arithmetic as 1..8; no separate work |

None of these can be closed with a new fixture, because the encoder that would
have to write one offers seven formats and the corpus already has all seven.
Closing any of them needs a hand-built bitstream, which is the implementation
session's option and not this pass's.

## 5. Measured behaviour

### Length and alignment

*Measured*, on all seven cells plus the demuxer cell: **a correct decode is
exactly the source length**, to the sample, and a best-fit lag search against
the source returns 0. There is no priming, no trim and no delay. That is
unusual for this tree and it is worth a test of its own.

The mechanism is the superframe's optional 12-bit sample count
(`wma-voice-bitstream.md` section 3.4), which exactly one superframe per file
carries, the last, and which is exactly what makes the total come out right. The
container plays no part: ASF's declared duration is 3.982 s on every cell,
which is 7 to 30 samples **short** of the true length, so a decoder that trusted
it would truncate.

FFmpeg produces 63840 and 87840 on `voice-16000-12k` and `voice-22050-20k`,
over-running the source by 113 and 7 samples. Both differences are exactly
`480 - declared`, and both are the defect of section 6 landing on the last
superframe of those two files. It is not a second length rule.

### Packet geometry

Every ASF media object is exactly `nBlockAlign` bytes and FFmpeg's ASF demuxer
delivers exactly one media object per `AVPacket` on every cell (*measured*: the
raw packet stream of each cell is an exact multiple of `nBlockAlign`, and every
`AVPacket` size reported by `ffprobe -show_packets` equals it). So a decoder
that expects one codec packet per media object is right, and the reference's
packet-splitting arithmetic is defensive rather than load-bearing here.

Object presentation times are **irregular** and are not a constant per object.
For `voice-8000-4k`, 18 objects over 3982 ms:

    0 180 300 480 780 1200 1620 1740 1860 1920 1980 2160 2340 2520 3060 3480 3780 3960

Differences of 60 to 540 ms. At 8000 Hz, 60 ms is exactly one 480-sample
superframe, so an object covers between 1 and 9 superframes, which matches the
superframe counts in the coverage table. `voice-22050-20k` has 10 objects for
183 superframes and `voice-16000-12k` 15 for 133. The container is padding
constant-size packets with a variable number of superframes, which is exactly
why the carry of `wma-voice-bitstream.md` section 3.2 can be kilobits long.

### The oracle's own noise floor

FFmpeg's vectorised decode against its own `-cpuflags 0` scalar decode, per
cell, absolute on a nominal full scale of 1.0:

| cell | RMS | max |
|---|---|---|
| `voice-8000-4k` | 3.04e-08 | 7.75e-07 |
| `voice-8000-5k` | 2.56e-08 | 7.45e-07 |
| `voice-8000-8k` | 3.60e-08 | 1.19e-06 |
| `voice-11025-10k` | 4.22e-08 | 2.21e-06 |
| `voice-16000-12k` | 2.62e-08 | 1.01e-06 |
| `voice-16000-16k` | 3.70e-08 | 9.54e-07 |
| `voice-22050-20k` | 2.99e-08 | 1.19e-06 |
| `voice-mono` (ASF cell) | 2.27e-08 | 6.56e-07 |

So **nothing can gate below about 4.3e-08 RMS or 2.3e-06 max**, because the
oracle disagrees with itself by that much. Always pin `-cpuflags 0`.

### Speed, for scale

FFmpeg decodes `voice-22050-20k` (3.98 seconds of audio) in about 0.029 s on
this box with the scalar path pinned, which is startup-dominated and is not a
target. The realtime floors `docs/quality-gates.md` records for this decoder
should be measured on WaxFlow's own decoder.

## 6. The reference's superframe cache defect

**This is the answer to the open question this pass was dispatched with, and it
decides what the gate can be.**

The measurement set taken before this pass found FFmpeg's decode peaking at 2.97
and 4.66 inside digital-silence segments where Windows' peaks at 0.01 to 0.19,
with the divergence propagating into the loud segment that followed. That is not
a level rule, not a gain rule and not a pseudo-random generator. It is a bug.

**What it is.** A superframe may span two packets, so the decoder holds the tail
of a packet as a bit-string carry and prepends it to the next packet's spillover
bits. The reference holds that carry in a **256-byte** buffer and copies into it
through a helper that returns without copying anything when the copy does not
fit, while the recorded carry length is set regardless. When a packet's tail
exceeds **2048 bits**, the reference therefore decodes the next superframe out
of whatever bytes an earlier packet left in that buffer. The result is a
syntactically valid superframe of unrelated content, usually much louder than
the passage it lands in, and its filter memories and LSP history poison what
follows.

**Why the corpus provokes it.** In a silent or near-silent passage the encoder
spends 90 to 160 bits on a superframe while the packet stays a fixed
`nBlockAlign` bytes, so the tail left over at the end of a packet is kilobits
long. *Measured* carries across the corpus run from 81 to 6422 bits, and 18 of
them exceed 2048.

**How it was confirmed.** Three ways, independently.

1. A decoder written from the bitstream note, with no such limit, reproduces the
   reference exactly everywhere the reference's carry stays under 2048 bits and
   differs from it exactly where it does not.
2. Rebuilding the reference with the buffer enlarged and nothing else changed
   makes the difference vanish: *measured*, the analysis decoder then agrees
   with it to a max of **6.5e-06** and an RMS of **1.5e-07** on all seven cells,
   and both produce exactly the source length on all seven.
3. Windows' own decoder produces the quiet passage in those superframes,
   agreeing with the analysis decoder's level and not with the reference's.

**Where it happens.** Exactly at the superframes below, and nowhere else. The
predicate is computable by any decoder that tracks its own carry length, so the
implementation session can generate this list rather than hard-coding it, and
should assert it as a path counter:

| cell | superframes the reference decodes from a stale carry | of |
|---|---|---|
| `voice-8000-4k` | none | 67 |
| `voice-8000-5k` | none | 67 |
| `voice-8000-8k` | none | 67 |
| `voice-11025-10k` | 29, 69, 79 | 92 |
| `voice-16000-12k` | 49, 99, 109, **132** | 133 |
| `voice-16000-16k` | 39, 49, 79, 89, 99, 109 | 133 |
| `voice-22050-20k` | 59, 119, 139, 159, **182** | 183 |
| `voice-mono` | none | 34 |

The two in bold are the last superframe of their file, which is why those two
cells come out 113 and 7 samples long: the reference reads the sample-count
field out of stale bytes.

**How far it spreads.** *Measured*, comparing the analysis decoder against stock
FFmpeg superframe by superframe, every divergence run begins exactly at one of
the superframes above:

| cell | diverging runs |
|---|---|
| `voice-11025-10k` | 29-31, 69-71, **79-91 (to the end)** |
| `voice-16000-12k` | 49-51, 99-101, 109-111, 132 |
| `voice-16000-16k` | 39-41, 49-51, 79-81, 89-91, 99-101, 109-111 |
| `voice-22050-20k` | 59-61, 119-121, 139-140, **159-182 (to the end)** |

Three superframes is the usual cost, because a poisoned superframe in a quiet
passage is followed by comfort-noise superframes that overwrite the excitation
outright. Two of the runs never recover: both begin in the file's final loud
voiced segment, where the adaptive codebook's long memory keeps the two decodes
apart for good. That is the same property section 8 measures for seeking, seen
from the other side.

## 7. The three decoders, and where they part company

### Against the reference

The analysis decoder, written from `wma-voice-bitstream.md` alone, against
`ffmpeg -cpuflags 0`, absolute on full scale 1.0:

| comparison | max | RMS |
|---|---|---|
| against the reference with its carry buffer enlarged, whole file, all seven cells | **6.5e-06** | **1.5e-07** |
| against stock FFmpeg, the three 8 kHz cells and the ASF cell, whole file | **2.0e-06** | **6.6e-08** |
| against stock FFmpeg, the other four cells, excluding the runs of section 6 | **5.1e-05** | **9.3e-07** |
| against stock FFmpeg, the other four cells, whole file | 4.66 | 0.19 |

Read the four rows in order. The first says the bitstream note is right: two
independent implementations of a recursive speech codec, on files neither
encoded, agreeing to 17 bits. The second says the three 8 kHz cells are clean
differentials with no exclusions at all. The third says the other four are
clean differentials once the reference's defect is skipped, with about 25 times
the headroom of the first row because the poison bleeds a little past the runs
the table names. The fourth says what happens if the defect is not skipped.

**So the gate is:**

| shape | bound against `ffmpeg -cpuflags 0` |
|---|---|
| `voice-8000-4k`, `voice-8000-5k`, `voice-8000-8k`, `voice-mono` | whole file, **1e-05 max** |
| the other four cells, superframes outside the runs of section 6 | **2e-04 max** |
| the other four cells, the runs of section 6 | **skipped, by name, with section 6 quoted in the gate's comment** |

Both bounds sit about a factor of five above the measured figures, which is the
usual headroom, and both sit far above the oracle's own floor of section 5, so
neither is green for the wrong reason. The skip is a **named per-cell allowlist
with the superframe indices in it**, not a widened tolerance: widening the
tolerance to 4.66 would pass a decoder that was wrong everywhere.

One caveat on the first row. It assumes the implementation reproduces the
reference's `h64` value (`wma-voice-bitstream.md` section 10.5). If it does not,
every bound above becomes **1e-02 max, 2e-03 RMS**, whole file, which is a much
weaker gate and should be taken only with the reason written down.

### Against Windows

Windows' decoder against the analysis decoder, both clipped to 16-bit and
compared in one sample format, absolute on full scale 1.0:

| cell | signal RMS | max | RMS | relative |
|---|---|---|---|---|
| `voice-8000-4k` | 0.111 | 0.453 | 0.0338 | 30% |
| `voice-8000-5k` | 0.114 | 0.368 | 0.0178 | 16% |
| `voice-8000-8k` | 0.112 | 0.239 | 0.0277 | 25% |
| `voice-11025-10k` | 0.099 | 0.143 | 0.0081 | 8% |
| `voice-16000-12k` | 0.088 | 0.176 | 0.0153 | 17% |
| `voice-16000-16k` | 0.089 | 0.154 | 0.0062 | 7% |
| `voice-22050-20k` | 0.082 | 0.073 | 0.0065 | 8% |

**Microsoft's decoder is not the same decoder.** That is the second half of the
open question's answer and it is a genuine finding, not a measurement artifact.
What was ruled out:

- **Not a delay.** A best-fit lag search over a loud window returns 0 on every
  cell.
- **Not a level error.** Over loud superframes the energy ratio is 0.987 to
  1.004.
- **Not the postfilter alone.** Running the analysis decoder with the postfilter
  disabled changes the figure in both directions: it improves on
  `voice-8000-8k` (25% to 11%) and worsens on the other six.
- **Not the cache defect.** These figures are against the analysis decoder,
  which does not have it, and they are spread evenly across the file rather
  than concentrated on the superframes of section 6.

What it looks like instead: over loud superframes the correlation between the
two is 0.997 at 16 and 22 kHz and 0.95 at 8 kHz, so the two reconstruct the same
waveform with a small uncorrelated residue that is worst where the bit rate is
lowest and the CELP recursion is longest. That is the shape of an arithmetic
difference somewhere inside the recursion, compounding. Nothing in reach can
narrow it further: there is no fourth implementation to break the tie, and
Microsoft's is the only one written by the people who wrote the format.

**So Windows is not a gate and cannot become one.** It is a maintainer's sanity
check, and the number to expect from it is **0.04 RMS at 8 kHz and 0.02 at 16
and 22 kHz**, with the reason written beside it or it reads as slop. The
reference is the gate, and the first table in this section is why that is
defensible: two implementations that never saw each other's code agree to 22
bits on the same bytes.

## 8. Seeking

*Measured*, with the analysis decoder restarted at every media object boundary
of four cells, 47 resume points, compared superframe by superframe against a
linear decode.

**Every media object is a decode start point.** Discard the carry, skip the
packet's spillover bits unread, and begin at the first superframe that starts in
the packet. No key-frame flag exists and none is needed.

**The frame counter must be restored, or the decode never converges.** Comfort
noise is drawn from a codebook whose offset is a deterministic function of the
frame index since the decoder started (`wma-voice-bitstream.md` section 11). A
resumed decode that starts its counter at zero draws different noise in every
silent passage, forever. *Measured*, on four cells: with the counter left at
zero, 14 to 18 superframes of a resumed decode differ from the linear decode
indefinitely; with it set to `3 * superframeIndex` at the resume, 2 to 4 do.
This is a first-class requirement on the decoder's API: the seek entry point
has to take the landing position, not just the packet.

**Convergence, with the counter restored:**

| superframes until exact and stays exact | resume points |
|---|---|
| 0 to 2 | 18 |
| 3 | 14 |
| 4 to 8 | 11 |
| 9 to 11 | 3 |
| 22 | 1 |

Relative RMS against the linear decode by superframe after the resume, over all
47 points: median 0.77 at +0, 0.11 at +1, 6.0e-05 at +2, 3.2e-06 at +3, and
exactly 0 from +7. The 90th percentile tells the other half of the story: 1.00,
0.99, 0.98, 0.97, 0.37, 0.055, and still 0.054 at +7.

**Two populations.** A resume into a transient, a silent passage or an unvoiced
passage is exact in two or three superframes. A resume into sustained voiced
speech is not, because the adaptive codebook is a pitch-lag copy of the
decoder's own past output with a gain near 1, so a wrong history decays only as
fast as that gain lets it. The worst point measured needed **22 superframes**,
0.48 s at 22050 Hz.

So the seek gate is:

- **Pre-roll at least three superframes (1440 samples) after any resume**, which
  is what the container should ask for.
- **Assert convergence, not immediate exactness**: a test that resumes at every
  object and requires bit-equality from superframe 3 will fail on the voiced
  landings. Requiring it from superframe 22 passes on this corpus and is not a
  bound; requiring the relative RMS to fall below the linear decode's own level
  within three superframes for the median landing, and to be exact by the end
  of the file, is the honest assertion.
- **Snap the landing to the 480-sample superframe grid.** An object's
  presentation time is in milliseconds and is not the first sample a decode
  resumed at it produces. The irregular object times of section 5 make that
  unmissable here.
- **Check the landing against the position it produced, never the position it
  was asked for.** `ffmpeg -ss` lands where its own ASF seek puts it, which is a
  property of the demuxer and not of the codec.

## 9. What nothing in this tree can reach

Each of these is describable by the format and unproducible here. The bitstream
note says what to do about each; this is the list of what has no fixture and
why.

- **Any sample rate other than 8000, 11025, 16000 and 22050.** The format admits
  322 to 22097 Hz and the encoder offers four rates. The pitch-bound arithmetic
  is exercised at four points of a continuous range.
- **Stereo, or any depth but 16.** Neither is describable by this codec at all;
  the container's `WAVEFORMATEX` could claim them and the decoder should refuse.
- **The independent per-frame LSP path.** Every packet of every file sets the
  residual flag. This is the largest untested surface in the codec: two
  codebook readers and a whole alternative frame layout.
- **Frame types 8, 9, 10 and 14.** The encoder's variable bit mode trees give
  them codewords and then never emit them.
- **The pitch-adaptive window coder's other arms**: a range of 16, the extended
  8-bit index, the zero-or-negative pulse count branch, and the concealment path
  when no free position is left. Two cells reach the coder at all and both
  through the same arm.
- **The superframe statistics field.** Never present on any superframe.
- **The superframe count escape** (a 6-bit field of 63 followed by another).
  Never written; the largest count observed is 24.
- **The saturating arm of the block pitch conversion ladder.** Never reached.
- **An LSF set that needs stabilising.** Never reached; the encoder's LSFs are
  always ordered and spaced.
- **The DC removal filter.** The DC level field is 1 to 4 on all seven formats
  and the filter needs more than 8.
- **A superframe whose speech/music bit is clear.** The reference has never seen
  one either. It is a refusal, permanently, unless someone implements WMA Pro in
  WMA Voice superframes.
- **A stream that provokes the reference's cache defect and can be compared to
  it.** The four cells that provoke it are the only fixtures for the skip list
  of section 7, and there is no way to produce a cell that provokes it in a
  loud passage where the error would be masked.

## Addendum from the implementation pass (2026-09-11)

- The coverage matrix's "eight of the nine asymmetric phases" is every phase
  the arithmetic can produce: the rounding in the bitstream note's section 9
  confines the phase to 1..8, so the table's phase 0 is unreachable rather
  than unreached (see that note's addenda).
- The demuxer cell (`voice-mono`) is scored in the same differential as the
  seven corpus cells, at the clean bound: measured 1.4e-06 max, 6.3e-08 RMS.
- The four cells scored outside the reference's stale runs are gated per cell
  at about twice their own measured figure rather than at the class-wide
  2e-04 of the table above, each entry a ceiling and a floor.
