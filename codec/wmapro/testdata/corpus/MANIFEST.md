# WMA Pro committed corpus

Six cells, 356 KiB, committed because **FFmpeg cannot write this format**. It
decodes WMA Pro and has no encoder for it, so a machine with no Windows has
nothing to encode and these bytes are the only fixtures it gets. The wider
generated corpus is Windows-only and never lands in the tree.

Provenance and the full measurement set are in
`docs/notes/wma-pro-oracle-corpus.md`. This file is the command line for each
cell, so a reviewer can see exactly what produced it.

## The cells

| File | Rate | Ch | Depth | Bit rate | nBlockAlign | Flags | Word @16 | Frames | Bytes |
|---|---|---|---|---|---|---|---|---|---|
| `pro-44100-2ch-16-128k.wma` | 44100 | 2 | 16 | 128016 | 5945 | 0x00e0 | 0x0000 | 65536 | 37543 |
| `pro-44100-6ch-16-128k.wma` | 44100 | 6 | 16 | 128016 | 5945 | 0x00e0 | 0x0000 | 65536 | 37543 |
| `pro-48000-6ch-24-384k.wma` | 48000 | 6 | 24 | 384000 | 16384 | 0x00e0 | 0x0000 | 65536 | 116590 |
| `pro-96000-2ch-24-384k.wma` | 96000 | 2 | 24 | 384000 | 16384 | 0x00e0 | 0x0000 | 131072 | 116590 |
| `pro-32000-2ch-16-32k.wma` | 32000 | 2 | 16 | 32000 | 1536 | 0x0060 | 0x20c6 | 49152 | 12654 |
| `pro-44100-2ch-16-128k-tonal.wma` | 44100 | 2 | 16 | 128016 | 5945 | 0x00e0 | 0x0000 | 100000 | 43517 |

Five of the six are gate cells and the sixth is a refusal cell. Each is here
to reach something nothing else does:

- `pro-44100-2ch-16-128k` is the ordinary case: 16-bit stereo at the lowest bit
  rate this shape reaches with a decodable stream.
- `pro-44100-6ch-16-128k` is the same bit rate spread over six channels, so it
  is the heaviest quantisation any gate cell carries: about 21 kbit/s each.
- `pro-48000-6ch-24-384k` is multichannel at 24 bits, with a channel mask.
- `pro-96000-2ch-24-384k` is hi-res. Every Pro format above 48 kHz is 24-bit,
  so there is no 16-bit cell up here to pair it with.
- `pro-32000-2ch-16-32k` is **the refusal cell**, and it is committed for that
  and not as a gate. Its extradata word at offset 16 is non-zero, which selects
  a tool no reference decoder in reach implements, so nothing can say what the
  right samples are. 32 kHz is also the one rate at which Windows offers a
  single format, so **every** 32 kHz WMA Pro stream is this shape. The corpus
  note's section on the oracle carries the measurement behind the refusal.
- `pro-44100-2ch-16-128k-tonal` is a different recipe: one pure tone per
  channel, 440 Hz left and 550 Hz right, at a length that is not a multiple of
  the frame. It reaches the two rows the four-segment recipe cannot: the
  channel transform enabled **band by band** (24 times), which only sustained
  tonal stereo provokes, and an **end trim** on the last frame. Its decode is
  99488 frames, 512 short of the source, and FFmpeg agrees; the corpus note's
  section 6 says why that number is pinned rather than the source length.

`container/asf/testdata/pro-s16.wma` is the same recipe at 16384 frames,
encoded the same way, so the demuxer's differential has a 0x0162 file without
carrying one of these.

## How each was made

The source PCM is synthesized by the integer recipe `corpus_test.go` carries,
with no floating point, so it regenerates identically on any platform and
architecture. Four equal segments: two triangles per channel, triangles plus
quarter-scale noise, digital silence, and full-scale noise, which puts a hard
transient on a frame boundary. The tonal cell is one integer cosine per
channel from a fixed-point rotation recurrence whose cosine is a literal.
Section 3 of the corpus note has both recipes in prose.

```sh
# 1. write the source WAV (the corpus test's own synthesis, at these lengths)
#      pro-44100-2ch-16-128k   44100 Hz  2ch  16-bit   65536 frames
#      pro-44100-6ch-16-128k   44100 Hz  6ch  16-bit   65536 frames
#      pro-48000-6ch-24-384k   48000 Hz  6ch  24-bit   65536 frames
#      pro-96000-2ch-24-384k   96000 Hz  2ch  24-bit  131072 frames
#      pro-32000-2ch-16-32k    32000 Hz  2ch  16-bit   49152 frames
#      pro-44100-2ch-16-128k-tonal  44100 Hz  2ch  16-bit  100000 frames (the tonal recipe)
#
# 2. encode with Windows' own encoder, from Windows or over WSL interop
powershell.exe -NoProfile -ExecutionPolicy Bypass \
    -File scripts/wmfenc/wmfenc.ps1 -Subtype Wma9 \
    -In <cell>.wav -Out <cell>.wma \
    -Rate <rate> -Channels <channels> -BitRate <bitRate> -Bits <depth>
```

`-Bits` is not decoration. Media Foundation's transcoder RENEGOTIATES rather
than refusing a shape it cannot reach, and every WMA Pro format above 48 kHz is
24-bit, so `pro-96000-2ch-24-384k` asked for at the default depth comes back as
a 48 kHz file that looks like a successful encode. The corpus test reads back
what it actually got for exactly this reason.

The bit rate the encoder lands on is near the request and not equal to it
(128016 for 128000), and at 32 kHz it ignores the request entirely: the codec
offers one 32 kHz format and that is the one you get.

Two things a regeneration will show, both expected:

- The output is **not byte-identical** to the committed file. The encoder
  writes a fresh File ID GUID each run, so a few dozen bytes differ while the
  decoded samples do not.
- The ASF Play Duration OVERSHOOTS the audio, by 1231 to 2464 frames across
  these cells. It is a rounded total that counts the encoder's own padding, so
  it is advisory and nothing may be trimmed to it.
