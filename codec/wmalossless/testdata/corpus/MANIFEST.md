# WMA Lossless committed corpus

Six cells, 400 KiB, committed because **nothing but Windows can write this
format**: FFmpeg decodes WMA Lossless and has no encoder for it, so without
these files a machine with no Windows has nothing to decode.

Provenance and the full measurement set are in
`docs/notes/wma-lossless-oracle-corpus.md`. This file is the command line for
each cell, so a reviewer can see exactly what produced it.

## The cells

| File | Rate | Channels | Depth | Frames | Coded frames | Packets | Bytes |
|---|---|---|---|---|---|---|---|
| `ll-44100-2ch-16.wma` | 44100 | 2 | 16 | 16384 | 8 | 4 | 58789 |
| `ll-44100-2ch-16-dup.wma` | 44100 | 2 | 16 | 16384 | 8 | 2 | 31977 |
| `ll-44100-2ch-16-skip.wma` | 44100 | 2 | 16 | 15000 | 8 | 3 | 45383 |
| `ll-44100-2ch-24.wma` | 44100 | 2 | 24 | 16384 | 8 | 6 | 85601 |
| `ll-44100-2ch-24-pad.wma` | 44100 | 2 | 24 | 16384 | 8 | 4 | 58789 |
| `ll-48000-6ch-24.wma` | 48000 | 6 | 24 | 8192 | 4 | 9 | 116040 |

Each is here to reach something nothing else does:

- `ll-44100-2ch-16` is the only 16-bit shape the encoder offers.
- `-dup` copies channel 0 over channel 1, so the encoder codes one channel and
  leaves the other to the two-channel lifting: the one-sided case.
- `-skip` is not a whole number of coded frames, so its last frame carries an
  end skip. Every other cell leaves that field zero.
- `ll-44100-2ch-24` is the 24-bit reconstruction.
- `-pad` carries 16-bit content in a 24-bit stream, which is what makes the
  encoder set padding zeroes: the one cell that exercises the output shift.
- `ll-48000-6ch-24` is multichannel, with a channel mask.

`TestMicrosoftCorpus` re-encodes every shape of the envelope on a Windows run;
the five that are not committed are never written to the tree.

## How each was made

The source PCM is synthesized by the same integer recipe the corpus test
uses, with no floating point, so it regenerates identically anywhere. The
recipe is in section 3 of the corpus note and in `corpus_test.go`.

```sh
# 1. write the source WAV (the corpus test's own synthesis, at these lengths)
#      ll-44100-2ch-16       44100 Hz  2ch  16-bit  16384 frames
#      ll-44100-2ch-16-dup   the same, channel 0 copied over channel 1
#      ll-44100-2ch-16-skip  the same at 15000 frames, so not a whole number
#      ll-44100-2ch-24       44100 Hz  2ch  24-bit  16384 frames
#      ll-44100-2ch-24-pad   the same, carrying the 16-bit signal shifted up
#      ll-48000-6ch-24       48000 Hz  6ch  24-bit   8192 frames
#
# 2. encode with Windows' own encoder, from Windows or over WSL interop
powershell.exe -NoProfile -ExecutionPolicy Bypass \
    -File scripts/wmfenc/wmfll.ps1 -In <cell>.wav -Out <cell>.wma
```

Two things a regeneration will show, both expected:

- The output is **not byte-identical** to the committed file. The encoder
  writes a fresh File ID GUID each run, so 52 bytes differ and the decoded
  audio does not.
- The encoder refuses any shape outside its eight-format envelope rather than
  resampling into it. `wmfll.ps1 -List` prints the envelope this Windows build
  offers.

## What verifies them

`TestCommittedCorpusIsBitExact` decodes each cell and requires it to equal the
regenerated source **sample for sample**, with no tolerance and no tool
installed. That is the whole gate: a lossless decode returns the encoder's
input or it is wrong. There is no golden digest here and no `wantDigestArch`,
because the source is the golden.

`TestFFmpegAgreesWithTheSource` repeats the comparison against FFmpeg's decode
when FFmpeg is present. That one checks the fixtures rather than the decoder:
two independent implementations returning the same samples from a file neither
of them wrote is what makes a committed fixture trustworthy.
