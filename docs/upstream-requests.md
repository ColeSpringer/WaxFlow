# Upstream requests

The standing list of things WaxFlow wants from the sibling Wax repos it
depends on.

## WaxLabel

### A carried edit for a caller with no source Document

**What:** `Editor.Prepare` refuses a whole edit over one item it cannot
write: text holding a NUL or invalid UTF-8, a chapter that starts before
zero or ends before it starts, a chapter list or a set of synced lyrics
past the format's count, a second picture of an icon role. A `Transfer`
drops the one item and writes the rest, but it starts from a parsed
source `Document`. Wanted: the same item-level drops for an editor fed
from a caller's own model, as an option beside `WithAllowUnsupportedDrop`
or a way to mark the edit carried.

**Why it matters here:** `Mapper.Apply` writes metadata that came off a
source and then passed through WaxFlow's model (a cut's shifted chapters,
a piece's tags, measured ReplayGain), so it cannot hand the library the
source document. Each of these refusals cost a finished transcode all of
its metadata.

**Workaround WaxFlow runs on:** `cli/label`'s `applier` makes each item
writable or leaves it out before `Prepare` sees it, and reads the count
limits off `Capabilities`.

**The test that will notice the fix:** none fails; `cli/label`'s
`TestUnwritableTextIsRepairedNotFailed` and
`TestUnwritableTimesAndCountsDoNotFailTheWrite` pin the behaviour the
workaround gives.

### The name behind `invalid-tag-key` lives only in the message

**What:** `WarnInvalidTagKey` names the native field it could not map
inside `Warning.Message` alone; `Warning.Keys` is empty, since the name is
no canonical key. Wanted: the native name as data on the warning.

**Why it matters here:** WaxFlow demotes this warning to a note for the
iTunes machine tags (`iTunSMPB`, `iTunNORM`), which every MP4 source
carries, and reads the name off the prose to tell them apart. v1.9.0 put
the name in quotes, and every MP4 source drew a client-visible warning
until a test caught it.

**Workaround WaxFlow runs on:** `cli/label`'s `machineKeyLint` cuts the
message at its first `": "` and strips the quote.

**The test that will notice the fix:** `cli/label`'s
`TestFragmentedIsANoteNotAWarning`, which fails when the wording moves.

### No accessor for what a legacy block holds beyond tag values

**What:** `Document.Families` lists a legacy block's tag values, and
`HasOpaqueLegacyContent` says a block holds more (a picture, chapters or
lyrics in a stray ID3v2 tag, a binary APEv2 item), but nothing returns
that content. Wanted: the pictures, chapters and synced lyrics a legacy
block holds, marked as legacy.

**Why it matters here:** a transcode writes a fresh file, so a cover only
a stray ID3v2 tag holds is lost. WaxFlow carries the tag values and can
only warn about the rest.

**Workaround WaxFlow runs on:** `cli/label` adds the legacy-only values
to the tags it reads and warns when `HasOpaqueLegacyContent` is set.

**The test that will notice the fix:** none fails;
`TestOpaqueLegacyContentWarns` pins the warning.

### The ID3 reader returns a chapter its own editor refuses

**What:** an ID3 `CHAP` frame whose end time comes before its start reads
as a chapter with that end, with no warning, and `Prepare` refuses the
same chapter when it is set on another file. The Matroska reader turns
such an end into an open one. Wanted: the ID3 reader doing the same.

**Why it matters here:** the chapter comes off a source and goes straight
back into an edit.

**Workaround WaxFlow runs on:** `cli/label`'s `applier.chapters` opens the
end.

**The test that will notice the fix:** none fails.

### A FLAC track in Matroska reads its header over its STREAMINFO

**What:** where a Matroska track's `SamplingFrequency` and `Channels`
disagree with the STREAMINFO in its `CodecPrivate`, `AudioTrack` reports
the track header's values. The decoder, and ffmpeg, follow the
STREAMINFO.

**Why it matters here:** an agreement gap between two readers of one
file, seen only on a header built to disagree.

**Workaround WaxFlow runs on:** none needed. WaxFlow reads the STREAMINFO
itself.

**The test that will notice the fix:** none yet.

### A leading ID3v2 tag that overruns the file misnames the stream

**What:** behind a leading ID3v2 tag that declares more bytes than the
file holds, a FLAC, WavPack or Monkey's Audio stream reads as an MP3 with
no audio, and a Musepack stream as a 17 ms MP3.

**Why it matters here:** the mapper names another format than the
demuxer for the same bytes. WaxFlow refuses these four files and reads
MP3 and ADTS past such a tag.

**Workaround WaxFlow runs on:** none needed.

**The test that will notice the fix:** none yet.

### The per-format name tables are internal

**What:** the tables that map a format's native names onto canonical keys
(`apeKeys`, `asfNames` and the MP4 atom, freeform and keys-store tables in
`internal/mapping`) have no exported view. Wanted: a read-only accessor
for each format's names, with the key each resolves to.

**Why it matters here:** WaxFlow's native readers mirror those tables by
hand so both readers give one key for one field, and a mirror has drifted
on an upgrade before.

**Workaround WaxFlow runs on:** oracletest finds the waxlabel module
directory and reads the APE and ASF tables out of its source with
`go/parser`, then sweeps every name through both readers.

**The test that will notice the fix:** none fails; the extractor in
`oracletest/apev2_mirror_test.go` fails when a table moves, which is the
day to switch it to the accessor.

### ASF header objects past the stated count are unread

**What:** `parseHeaderObject` hands the Header Object's child count to
`walkObjects`, which returns no more than that many children, so an object
past the count is never read: no tags, no markers, no stream properties.
ffmpeg's ASF muxer states one object too few whenever it writes markers,
and reads past the count itself. Wanted: the walk bounded by the header's
size, as ffmpeg's is.

**Why it matters here:** WaxFlow's demuxer reads every object the header
holds, so on such a file it is the only reader of the last one.

**Workaround WaxFlow runs on:** none needed. The native tags are the
floor under the mapper's, and the number split counts only what the
library sees.

**The test that will notice the fix:** oracletest's
`TestASFTagsPastTheHeaderCount`, which fails when the library reads the
object.

### A joined MP3's length is measured two other ways

**What:** for MP3 files joined end to end, a VBR run is trimmed by the
first stream's end padding where the last stream's is the one at the end,
and a CBR run is measured as bytes over bitrate, with both Info frames
counted and no trims. Wanted: the frame counts summed, the first stream's
delay and the last stream's padding trimmed.

**Why it matters here:** the two readers state different lengths for one
file. WaxFlow's is the samples a decode delivers.

**Workaround WaxFlow runs on:** none needed.

**The test that will notice the fix:** oracletest's
`TestWaxlabelAgreesRawStreamLength`, whose joined rows carry the distance
as a named deficit (6.26 ms for two VBR files, 74.56 ms for two CBR files)
and fail when it moves.

### An MP3 frame the file's end cuts short is counted as audio

**What:** where the last MPEG frame is cut short, the bytes left of it
count toward the duration. Wanted: the duration of the whole frames.

**Why it matters here:** the cut frame does not decode, so the library's
length runs past what any decode delivers.

**Workaround WaxFlow runs on:** none needed.

**The test that will notice the fix:** the cut-frame rows of
`TestWaxlabelAgreesRawStreamLength`, which carry 19.86 ms as a named
deficit.
