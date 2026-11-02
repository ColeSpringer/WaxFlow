# Deferred work

## A header walk runs to its end after a cancel

**Where:** `format.Walker.Walk` and `container.Walker.Walk` (fourteen
implementations) and the window readers under them (`srcwin`,
`mpegframes`).

**What:** `waxflow.MeasureLength` stops a decode count when its context
ends, and a caller left waiting on another's canceled measure measures
again, but a header walk (an MP3's frame index, a Matroska cluster walk, an
ASF frame index) runs to its end first. Sources are local files, so that is
one read of the file's headers at disk speed: 6 ms for a 15-minute Xing MP3.

**Why deferred:** a judgment call, open to override. A context there reaches
every Walk and the readers beneath it, for a wait the size of one header read.

**Found by:** the third-party review of the open-span hold, 2026-11-01.
