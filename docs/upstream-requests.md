# Upstream requests

The standing list of things WaxFlow wants from the sibling Wax repos it
depends on. Only wax-series dependencies belong here; today that is
WaxLabel alone (`cli/go.mod` and `oracletest/go.mod`; the root module's
require block is empty by design, see ADR-0002). Every entry is a
candidate for whenever upstream work is next scheduled; nothing here
implies timing, and none of it is a WaxFlow prerequisite. Each entry
names the shipped workaround WaxFlow runs on today and, where one exists,
the test that will notice the upstream fix landing. Agents: when you
defer something because it needs upstream support, add it here in the
same change; do not bury it in a progress note.

## WaxLabel

### `AudioTrack.Codec` is the raw fourcc for MP3 in QuickTime

**What:** a `.mov` whose audio sample entry carries QuickTime's `.mp3`
fourcc reads back as `Codec: ".mp3"` with an empty `CodecProfile`. The
same stream in an `mp4a`/`esds` entry reads `Codec: "MP3"`. waxlabel's own
doc comment on the field says the canonical name belongs in `Codec` and
the container's spelling in `CodecProfile` (naming the `mp4a` fourcc as
the example), so the wanted reading is `Codec: "MP3"`,
`CodecProfile: ".mp3"`.

**Why it matters here:** it is the one thing two readers of the same file
disagree about now that WaxFlow decodes MP3 in MP4. A consumer routing on
`Codec` sees a codec name no other container produces, and a `.mov` is
the only place it appears.

**Workaround WaxFlow runs on:** none needed. WaxFlow reads the fourcc
itself (`container/mp4`'s `.mp3` arm) and never reads waxlabel's codec
name for its own decisions; this is an agreement gap, not a defect
WaxFlow works around.

**The test that will notice the fix:** oracletest's
`TestWaxlabelAgreesMP3InMP4`, whose `mp3.mov` cell accepts either name
today and says which one it saw.

### `AudioTrack.BitsPerSample` reads WMA Lossless's decoration

**What:** a WMA Lossless stream's real depth is in the codec extra bytes
behind the `WAVEFORMATEX`, at extra offset 0; `wBitsPerSample` in the
fixed part is decoration, and the only value any derived decoder reads is
the extra one. waxlabel reads the fixed field. Windows' encoder writes
the same value into both, so the two readings agree on every file that
exists and the difference only shows on a header built to disagree: with
`wBitsPerSample` patched to 16 on a 24-bit stream, waxlabel reports 16
where the stream is 24.

**Why it matters here:** a consumer sizing a buffer or choosing an output
depth from waxlabel's number would be wrong by a factor of 256 on such a
file, and nothing in the file is malformed. It is latent rather than
live, since no encoder writes a disagreeing header.

**Workaround WaxFlow runs on:** none needed. WaxFlow reads the extra
bytes itself (`codec/wmalossless`'s `ParseConfig`) and never takes
waxlabel's depth for its own decisions.

**The test that will notice the fix:**
`oracletest.TestBothReadersTakeTheDepthFromTheExtraBytes`, which pins
waxlabel's answer as 16 today and fails when it becomes 24, so the fix
cannot land without this entry being retired.
