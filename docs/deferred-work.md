# Deferred work

## A split cuts a data track as audio

**Where:** `cue/cue.go` `File.Starts` and `Cuts`, `cli/split.go` `cuePieces`,
`server/jobs.go` `cutsFromCue`.

**What:** a mixed-mode disc's sheet can index its data track
(`TRACK 01 MODE1/2352`) in the same FILE as the audio, and both splitters cut
it like any other track, so the rip yields a piece of noise named after it.
WaxTap refuses a non-AUDIO track itself and WaxBin drops one from its carve;
the CLI and the daemon do neither.

**Why deferred:** the fix is a product choice: refuse the sheet, drop the data
track's piece, or fold its span into the lead-in. Which is right depends on
how rippers lay out a mixed-mode image, and that needs real rips to check.

**Found by:** the CUE parser review, 2026-09-22.
