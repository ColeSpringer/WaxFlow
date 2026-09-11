# WMA Voice committed corpus

Seven cells, 76 KiB, one per format Windows' encoder offers. They are committed
because **FFmpeg cannot write this format**: it decodes WMA Voice and has no
encoder for it, so a machine with no Windows has nothing to encode and these
bytes are the only fixtures it gets.

Provenance and the full measurement set are in
`docs/notes/wma-voice-oracle-corpus.md`. This file is the command line for each
cell, so a reviewer can see exactly what produced it.

## The cells

| File | Rate | Bit rate | nBlockAlign | Source frames | Bytes |
|---|---|---|---|---|---|
| `voice-8000-4k.wma` | 8000 | 4000 | 150 | 31863 | 7955 |
| `voice-8000-5k.wma` | 8000 | 5000 | 187 | 31863 | 5615 |
| `voice-8000-8k.wma` | 8000 | 8000 | 300 | 31863 | 6991 |
| `voice-11025-10k.wma` | 11025 | 10000 | 544 | 43917 | 7457 |
| `voice-16000-12k.wma` | 16000 | 12000 | 450 | 63727 | 8912 |
| `voice-16000-16k.wma` | 16000 | 16000 | 600 | 63727 | 10533 |
| `voice-22050-20k.wma` | 22050 | 20000 | 1088 | 87833 | 12897 |

Those seven are the WHOLE envelope. Enumerated through the Windows Media Format
SDK's `IWMCodecInfo3`, "Windows Media Audio Voice 9" offers seven formats and no
more: every one is MONO and 16-bit, at 8, 11.025, 16 or 22.05 kHz. There is no
stereo format, no format above 22.05 kHz, and no depth other than 16, so the
corpus reaches every shape a WMA Voice stream can have and no cell here is a
sample of a wider space.

`voice-8000-5k` carries an ODD `nBlockAlign` (187), which no other cell in this
tree does.

`container/asf/testdata/voice-mono.wma` is the same recipe at 16 kHz and
12 kbit/s truncated to one second, so the demuxer's differential has a 0x000A
file without carrying one of these.

## How each was made

The source PCM is synthesized by the integer recipe `corpus_test.go` carries,
with no floating point, so it regenerates identically on any platform and
architecture. Eight equal segments over about 3.98 seconds:

1. voiced, pitch falling 200 Hz to 105 Hz
2. unvoiced, high-passed noise
3. digital silence
4. voiced, pitch rising 105 Hz to 200 Hz, with breath noise
5. half voiced at a steady 150 Hz, half noise
6. digital silence
7. noise near the floor
8. loud voiced from a standing start, pitch rising 90 Hz to 240 Hz

The voiced segments are a pulse train whose glottal pulse is a 48-tap literal in
Q15: two damped sinusoids at one twelfth and one fifth of the sample rate, so
the spectrum scales with the rate and every cell carries the same sound in the
codec's own terms. The pitch period is recomputed at each onset from the sweep's
value there, so the pitch is continuous and the phase never resets. Section 3 of
the corpus note has the recipe in prose.

Lengths are deliberately not a multiple of any plausible frame or superframe
size.

```sh
# 1. write the source WAV (the corpus test's own synthesis, at these lengths)
#      voice-8000-4k     8000 Hz  31863 frames
#      voice-8000-5k     8000 Hz  31863 frames
#      voice-8000-8k     8000 Hz  31863 frames
#      voice-11025-10k  11025 Hz  43917 frames
#      voice-16000-12k  16000 Hz  63727 frames
#      voice-16000-16k  16000 Hz  63727 frames
#      voice-22050-20k  22050 Hz  87833 frames
#
# 2. encode with Windows' own encoder, from Windows or over WSL interop
powershell.exe -NoProfile -ExecutionPolicy Bypass \
    -File scripts/wmfenc/wmfenc.ps1 \
    -Subtype "{0000000A-0000-0010-8000-00AA00389B71}" \
    -In <cell>.wav -Out <cell>.wma \
    -Rate <rate> -Channels 1 -BitRate <bitRate>
```

The GUID is not decoration. `MediaEncodingSubtypes` names no constant for WMA
Voice, so without the GUID form of `-Subtype` this codec has no encoder in reach
at all.

Three things a regeneration will show, all expected:

- The output is **not byte-identical** to the committed file. The encoder writes
  a fresh File ID GUID each run, so a few dozen bytes differ while the decoded
  samples do not.
- The transcoder RENEGOTIATES rather than refusing what it cannot reach. A
  stereo request comes back mono; a 32 kHz or 44.1 kHz request comes back at
  22.05 kHz; a bit rate between two offered ones comes back at the nearer. The
  corpus test reads back what it actually got for exactly this reason.
- The bit rate the encoder lands on is exactly the request here, because every
  request above names a format the encoder offers.
