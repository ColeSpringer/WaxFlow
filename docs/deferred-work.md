# Deferred work

**Where:** `codec/wmapro/corpus_test.go`, cell `pro-96000-6ch-24-768k`.
**What:** decoded channel 3 correlates 0.004 with the source on this box.
The other five channels correlate 1.000, ffmpeg's decode of the same bytes
agrees with ours, and no permutation of the source channels correlates with
it either, so the encoder wrote nothing of the source into that channel.
**Why deferred:** it is this Windows build's encoder, not the decoder, and
the cell passes wherever the corpus was built. Fixing it means deciding
between a fixture whose LFE content survives every Media Foundation build,
a named per-cell deficit, and dropping the cell, which is a maintainer's
call rather than a repair.
**Found by:** the Media Foundation cells running under WSL for the first
time (2026-09-20); the failure reproduces at `d4c3101` with only the
`haveWMF` change applied, so it predates this round's work.
