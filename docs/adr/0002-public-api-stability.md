# ADR-0002: Public API surface, module layout, and stability policy

Status: Accepted (2026-07-02)

## Context

The codecs are meant to be imported by anyone (WaxBin and WaxTap included),
which demands a stable, dependency-free public tree. But codec APIs will
churn heavily through v0.x, and multi-module repos tax every change with
tagging ceremony.

## Decision

- **Public packages**: root facade, `waxerr`, `audio`, `dsp/...`,
  `codec/...`, `container/...`, `format`, `source`, `server`, `client`.
  Everything else lives under `internal/`.
- **The public tree is stdlib-only.** `make depcheck` runs `go list -deps`
  over the public packages and fails CI on any non-stdlib import. Runtime
  third-party dependencies are confined to `cmd/` and `internal/`
  (`spf13/cobra`; later `colespringer/waxlabel`).
- **Single module through v1.0.** External importers of, say,
  `waxflow/codec/flac` inherit cobra in their module *graph* (never their
  binaries); that is acceptable during v0.x churn.
- **Structural extraction at v1.0**: when codec APIs freeze, the stdlib-only
  tree graduates to a nested module with an empty `require` block, so the
  guarantee becomes structural instead of script-enforced.
- **No compatibility promise before v1.0.0.** Tagged v0.x releases are
  usable snapshots, not contracts. At v1.0.0 every exported identifier in
  the public tree is semver-locked, preceded by a strict surface audit.
- Unfinished codecs compile and test but stay **unregistered** in `format`
  and `/caps`; the service never advertises what it cannot do.

## Consequences

- `depcheck`'s package list grows as public packages land; forgetting to add a
  new public package there is the main failure mode, so the Makefile derives
  the list from a single variable reviewed in PRs.
- Nested modules are how dependencies stay out of the main module graph;
  `resolver/` was the one planned before v1.0 (see the 2026-07-17
  amendment, which removed it).

## Amendment (2026-07-11): the extraction as implemented

The v1.0 structural extraction landed inverted from the original
wording, with the same guarantee: instead of the stdlib-only tree
moving into a nested module (which would have changed every published
import path), the tree stayed at the module root and everything
carrying dependencies moved out. The root module's `require` block is
now empty, so importing any public package pulls in nothing, by
construction rather than by CI script (`depcheck` stays as the fast
pre-push guard).

- `cli/` (nested module): the cobra command tree, the waxlabel-backed
  metadata mapper (`cli/label`), and the stock binary
  (`cli/cmd/waxflow`). Its packages exist to build the binary; they
  carry no independent stability promise, except for the extension
  surface carved out in the 2026-07-17 amendment below.
- `resolver/` (nested module, pre-existing): the WaxBin flavor,
  requiring `waxflow/cli` for the shared command tree. Removed
  2026-07-17; see the amendment below.
- `oracletest/` (nested module, test-only): the tests whose oracles are
  third-party modules (waxlabel metadata round trips, the go-mp3
  differential, the m4b chapter golden, the jobs e2e). The resolver's
  precedent (nested modules keep gated integration suites out of `go
  test ./...`) applied to test dependencies; `make test-oracle` and a
  dedicated CI step run it.
- The v1.0 surface audit also added `context.Context` to
  `source.Resolver.Resolve` (the recorded resolver limitation): catalog
  lookups now observe request cancellation, bounded by the resolver's
  own query timeout and still aborted by Close.

Internal packages (`internal/...`) remain in the root module and are
reachable by the nested modules through Go's path-prefix internal rule;
they add no dependencies (the root go.mod stays empty), and their
instability is contained by the replace-pinned, same-repo builds.

## Amendment (2026-07-17): `resolver/` removed, `cli.Flavor` promoted

`resolver/` is deleted. It was the only module here requiring WaxBin, so
every WaxBin pseudo-version bump moved this repo's HEAD for a release
containing no WaxFlow work, and its `replace waxflow => ../` had begun
redirecting *waxbin's own* waxflow dependency to the local tree once
WaxBin started requiring WaxFlow. The catalog lookup moves to WaxBin;
building a CLI that resolves `pid:` becomes a consumer's job.

That makes third-party integrators the intended consumers of `cli/`, so
the disclaimer above is carved back:

- **Supported extension surface**: `cli.Flavor`, `cli.ResolverOptions`,
  `cli.PIDSourceReporter`, `cli.ExecuteFlavor`, and `cli.Execute`. These
  are what a build outside this repo needs to add source schemes, and
  they are treated as public API.
- The rest of `cli/` keeps the "no independent stability promise"
  disclaimer.

`PIDSourceReporter` is there for the same reason the seam was narrowed.
`/caps` advertises `delivery.pid` from what the CLI knows, and what it
knew was `catalogDB != ""` -- exact while the only resolver was in-tree
and keyed on catalogDB, an inference about a stranger's build once it is
not. A resolver that implements it answers for itself; one that does not
keeps the inference. Optional, so it costs implementers nothing.

Freezing the pre-amendment signature would have been dishonest:
`Flavor.OpenResolver` named `internal/config.Config`, so no module
outside the `github.com/colespringer/waxflow/` import path could
implement it. `resolver/` only ever satisfied it by living inside the
prefix. It is narrowed to exported and stdlib types (`ResolverOptions`),
which is what makes a stability promise meaningful here.

This also retires the containment claim above. "Contained by the
replace-pinned, same-repo builds" held only while every implementer was
in this repo, and the deletion moves the implementer across that line.
What contains it now is `examples/catalogcli/`: a module whose path sits
outside the waxflow prefix on purpose, so Go's internal rule applies to
it exactly as to a consumer. Nothing else in the tree can fail that way
-- no test inside the prefix can, and `depcheck` does not look (it gates
third-party deps, filtering `waxflow/...` imports out by construction).
`make test-example` and a CI step run it.

Its reach is worth stating precisely, because it is narrower than "any
internal type in `cli/`". Go's internal rule is enforced against the
imports a package actually writes, so the canary fails only on internal
types a consumer is *forced to name*: the parameter and result types of
`OpenResolver`, which cannot be implemented without naming them, and any
field an implementer must set. An exported field of an internal type
that a consumer can leave zero slips through -- `server.Config.Meta`
(typed `internal/meta.Mapper`) is exactly that, and the canary
constructs a `server.Config` and compiles today. That gap is known and
accepted: promoting the metadata mapper is a much larger permanent
commitment, worth making only when an embedder asks. The canary guards
the seam it was built for, not the whole surface.

## Amendment (2026-09-10): `cue` joins the public tree

`internal/cue` moves to `cue`. WaxBin keeps its own CUE parser, whose
formula for frames-to-samples is the one this package exists to replace
(a CD frame is 1/75 s, which no `time.Duration` holds exactly, so a
sheet routed through one lands its cut points up to a sample off at
every track boundary of a gapless album). Two parsers means two answers
to the same sheet, which is the failure this package was built out of
in the first place, when the daemon and the CLI each had a copy.

The move is one direction only, which is why the package was internal
first: the promise added here cannot be withdrawn.

The surface exported for it is deliberately smaller than the package's
old internal one, because publishing changed which callers exist:

- **`Parse` is syntactic.** It refuses a line it cannot read (a missing
  operand, a `TRACK` indexed against no `FILE`, an `INDEX` outside a
  track, a timestamp that is not one or that the arithmetic cannot
  hold) and nothing else.
- **`File.Starts` holds the splitting invariants** that `Parse` used to:
  every track has an `INDEX 01`, and starts ascend strictly. `Cuts`
  calls `Starts`, so every splitting path in this repo refuses exactly
  what it refused before, at the same point in the same words.

The split is what a reader needs. A consumer listing a sheet's titles or
reading `REM DATE` has no stake in whether the tracks could be cut, and
refusing the whole sheet over a start-less track (which real sheets
carry, on a data track of a mixed-mode disc) would push that consumer
back to its own parser. It filters on `Track.Start`'s second return
instead.

`Sheet.Rems`/`Track.Rems` and the `Rem` lookup are added for the same
reason: `REM` is the format's only extension point, so the metadata CUE
has no command for lives there, and a parser that drops it is not one a
reader can use. The lines are kept as read; which keys mean anything is
the reader's call.

## Amendment (2026-09-19): one behavioural break, ADR-0010

`Concat` over members of differing channel counts followed by a transcode,
analysis or segmented run that converts the channel count is now refused
(`CodeInvalidRequest`) where it used to succeed. It is a behavioural break on
the public API with no signature change, and it is taken deliberately: the audio
it used to produce was between 0.8 and 6 dB under what each member's own
conversion gives, invisibly, because the error is a scalar every measurement
moves with. ADR-0010 has the table and the remedy
(`ConcatOptions.Channels`, `Engine.TimelineChannels`).

A uniform-width `Concat` converts exactly as before.

## Amendment (2026-09-22): cue reads whole operands, and tolerantly on request

WaxBin, the package's outside reader, filed three requests against the
2026-09-10 surface: an unquoted title kept only its first word, one bad
timestamp refused every track in the sheet, and a sheet with no `FILE`
line was refused outright. Four points change.

1. **The grammar.** A command with one string operand (`TITLE`,
   `PERFORMER`, `ISRC`, `CATALOG`, a `REM` value) reads the rest of its
   line, so `TITLE Jazz Album` reads `Jazz Album` where it read `Jazz`.
   The format has no escape, so quotes strip only as the pair around the
   whole operand, and there a quote that never closes runs to the end of
   the line. A quoted `FILE` name closes at its first quote unless that
   splits a name with quotes of its own (`"12" Single.flac" WAVE`). A
   `TRACK` before any `FILE` opens an implied file with an empty
   `File.Name`, and a `TRACK` line holding only a datatype keeps it as its
   type. A sheet whose lines end in CR alone (classic Mac OS) reads line by
   line. Sheets the 2026-09-10 surface refused now parse, and some it read
   now read differently.
2. **`ParseTolerant` and `Sheet.Warnings`.** Strict stays the default:
   `Parse` refuses at the first line it cannot read. `ParseTolerant`
   records that line as a `Warning` and keeps reading. The polarity is
   the reverse of `container`'s `Strict` option on purpose: a demuxer's
   tolerated damage is local to the bytes it skipped, while a skipped
   sheet line moves a cut. So splitters refuse and readers opt in; WaxTap
   and both splitters here stay on `Parse`. A reader that keeps state per
   track should count a sheet with warnings as unread, as it would a
   refusal, or a typo deletes the tracks behind the dropped lines.
3. **The refusal list is replaced**, not amended at one point. Gone are
   "a missing operand" (a bare string command is an empty value) and "a
   `TRACK` indexed against no `FILE`" (the implied file). `Parse` refuses
   a number or a time that is missing or is not one (a sign included,
   which `Atoi` used to take), an `INDEX` number outside 00 to 99, a time
   past what the arithmetic holds, an `INDEX`, `PREGAP` or `POSTGAP`
   outside a track, a quote that never closes anywhere but a string
   operand, a `REM` line or a command this skips, a `FILE` without a name,
   and one structural refusal in both modes: `INDEX` numbers that do not
   ascend within a track. That is the trace of a swallowed `TRACK` line.
   Unknown commands are skipped by design, so `TRCK 02 AUDIO` vanished,
   track 2's `INDEX 01` landed on track 1, `Start` took the first, and a
   split lost a track without a word. The format requires ascending index
   numbers, so a sheet accepted yesterday with a repeat or a descent is
   refused today.
4. **The error text** is `cue: line N: ` and the finding, one
   `waxerr.Error` where there were two nested, each saying `cue:`.
   Findings on a track's `INDEX`, `PREGAP` and `POSTGAP` lines begin
   `track N: `, and a numeric field past what an int holds reads `is not
   MM:SS:FF` rather than `strconv.Atoi`'s own words.

`File.Starts` gains one refusal, reachable only from `ParseTolerant`
output: a track numbered -1, since a split names and tags its pieces by
number. Its messages say `the implied file` for a nameless one.

## Amendment (2026-09-22): a data track is a piece nothing writes

A mixed-mode disc's sheet lists its data track (`TRACK 01 MODE1/2352`)
beside the audio, and both splitters cut it as audio: a piece of noise
named after a song. Four points change. Every sheet of audio tracks
alone that the 2026-09-10 surface cut still cuts the same way; a sheet
with a data track is refused by `Cuts` where it used to be cut wrong,
and by `Pieces` where its data track is cooked and ahead of audio, or is
every track it has, and `Starts` answers a data track's entry
differently (its `INDEX 00`, or 0 when first). These are the behavioural
breaks, taken because each replaces a silently wrong cut.

1. **`File.Pieces(rate, total)` is the splitting funnel.** `Cuts` is
   built on it for a caller that can only cut and pairs pieces with
   tracks by position, and refuses by name any sheet with a data track,
   since a cut list cannot skip a piece and a data track occupying
   nothing would leave a track with no piece. The invariants move with
   it: every audio track has an `INDEX 01`, and starts ascend, strictly
   past an audio track and loosely past a data one (EAC's image holds
   the audio session alone and lists the data track at frame 0 with the
   audio), though never inside one. A sheet naming one track still has
   nothing to cut, whatever its `INDEX 01` says, and so does a file
   whose pieces reduce to one.
2. **`Track.IsAudio`** reads the datatype: the MODE and CDI modes are
   data, and AUDIO, CDG, an absent and an unfamiliar token are audio, the
   rule WaxBin's own reader already applies. A data track's piece carries
   `Audio` false and is skipped: the CLI names it on stderr, the daemon
   resolves it into the job's `skip` list. The audio before a data track
   ends at its `INDEX 00`, a first data track owns the file up to the
   first audio `INDEX 01` (the mode change puts data-mode sectors inside
   that pregap), one occupying nothing is no piece, and a cooked one
   (`MODE1/2048`) ahead of audio is refused, since only 2352-byte sectors
   keep the sheet's frames on samples past it. CUETools, the reference
   consumer of these sheets, ends the audio at the same index.
3. **`File.Starts`** returns each track's boundary under those rules: a
   data track's entry is the sample the audio before it ends at, and a
   first data track's is 0 whatever its indexes say.
4. **`Sheet.SingleFile`** returns the one file holding audio when the
   sheet's other files hold only data tracks, which is how EAC and XLD
   write a mixed-mode or Enhanced CD rip. A sheet whose files each hold
   audio is refused as before. `File.PrecededByData` says the file
   follows such a data track, so the audio ahead of its first `INDEX 01`
   is the mode change's pregap and is skipped, as it would be were the
   data track in the file.

The daemon places a trailing data track against the same length it
holds the cuts to and the run enforces (the declared count, or the
measure that replaced an advisory or absent one). A header that
under-declares cannot turn that into noise: no demuxer delivers audio
past the count it declares (an MP3 whose Xing count is short files the
surplus frames as padding), so a data track addressed past that count is
past the audio whatever the file holds behind it. A split job carries
`skip`, the pieces it does not write; its outputs are named and indexed
by what it wrote.

## Amendment (2026-10-29): a WavPack stream in a WAV is refused by name

A WAV whose data chunk holds a WavPack stream now fails to open with
`unsupported-format`. Every WavPack file before 4.0 is written that way
(the source's WAV header, then the compressed stream where the samples
go), and such a file used to open as PCM and decode its compressed bytes
as noise. A bare stream of a version this build does not decode, pre-4.0
included, is refused for its version where it used to fail as unparsable.
It is a behavioural break taken for the data-track amendment's reason: a
refusal replaces a silently wrong decode.

## Amendment (2026-10-30): `m3u8` joins the public tree

`internal/hls`'s M3U8 writer and parser move to `m3u8`; the descriptor,
the variant manager and the HLS client stay internal. A host rendering a
presentation through the exported segmenter needs the daemon's playlist
text, and without a shared writer each host writes its own: WaxDeck's
fixtures already carried a copy that rounded the target duration its own
way. `SegmentPlan.MediaSegments` and `SegmentPlan.MasterVariant` give a
plan's rows, which `Media` and `Master` write. As with `cue`, the move is
one direction only.

Publishing changed both halves, because it changed who calls them:

- **The writers refuse what M3U8 cannot carry.** `Master` and `Media`
  return an error where they returned text. A URI must already be RFC
  3986's characters (RFC 8216 asks for a URI, and a space or a non-ASCII
  byte left unencoded is where other readers split a line this one does
  not), a URI line may not start with `#`, CODECS may hold no control
  character or double quote, and a duration must be finite and not
  negative. What they write the parsers read back as given, durations to
  the five decimals EXTINF is written with.
- **The parsers refuse what their types cannot say.** `ParseMedia` refuses
  a playlist with a byte range, encryption, a gap, a delta update, an
  I-frame playlist or more than one init, where it read past the tag and
  listed each segment as a whole, clear resource. An EXTINF duration must
  be one of RFC 8216's unsigned spellings. The errors carry codes: damage
  is `malformed-input`, and a playlist past the size caps or using a tag
  the types cannot describe is `unsupported-format`.

The package is named for the format, as `cue` is, which leaves `hls` free
should the descriptor or the client ever go public.

## Amendment (2026-10-30): a library root that does not open is unmounted, not refused

`source.OpenRoots` and `Roots.Reload` no longer fail on a path that does not
open: the root is kept unmounted and reported (`Roots.Status`,
`ReloadResult.Unmounted`, every change through `Roots.Watch`), and only an
invalid set (a bad or duplicate name, an empty path) is refused. A folder
that opens empty is unmounted too (`ErrRootEmpty`): that is a mount point
before its drive arrives. `Resolve` answers an unmounted root with the new
`root-unavailable` code (exit class io, HTTP 503 with `Retry-After`), mounts
it the first time its folder opens, and remounts a root whose folder was
removed or replaced; `Roots.Remount` checks the rest for a caller's sweep.
No call waits on an unmounted root's folder for more than a few seconds, so
a network mount that stops answering before it mounts holds up nothing but
its own root.
A root re-pointed by a reload to a path that fails stops serving the old
one. Under a healthy root, an I/O error (on Windows also a file another
program holds locked) is `source-unreadable` and running out of file handles
is `overloaded`, where both were `invalid-request`.

It is a behavioural break taken for availability: one unplugged drive kept
the daemon from starting, and one bad path made a reload refuse every other
change in it. A caller that relied on the refusal reads `Status` or
`Unmounted` instead.

## Amendment (2026-11-02): measured lengths say how they were measured, and holes are counted and filled

Three signatures changed. `waxflow.MeasureLength(ctx, med)` takes a context,
which its decode count (WMA Pro, Voice and Lossless in ASF) checks between
chunks. `waxflow.MeasuredMedia(med, measured)` and
`waxflow.SliceMeasured(med, measured, from, to)` take the track
`MeasureLength` returned in place of a bare sample count, a negative
`Samples` still meaning "leave the declaration alone", so the measure's
route travels with its length.

New fields:
- `container.Track.SamplesCounted` marks a length `MeasureLength` counted by
  decoding. A span over such a source that delivers less fails `internal`;
  one over a length the file stated fails `malformed-input`. The mark rides
  in `Info`, which every wrapper forwards, so no walk or type check decides
  it.
- `container.Packet.Lost` is the samples damage took before a packet, which
  `format.Media` delivers as silence first, so positions after a hole stay
  where the stream puts them. flacn and the Ogg FLAC mapping set it, for a
  hole after damage and for frame numbers that skip where the stream could
  hold the frames.
- `format.Info.Damage` counts the damage met so far, past the `Warnings`
  list's cap and de-duplication.
- `mp4.SegmenterOptions.Block` is the FLAC block size a run restarted past
  segment 0 numbers its frames by.

New interfaces and methods: `container.Damaged` (`DamageCount`), which the
ten demuxers that cap their warning lists implement, with
`container.DamageOf` answering for any demuxer. `ClearMD5` on the native
FLAC, Ogg and Matroska muxers drops the STREAMINFO MD5 at End, for a run
whose audio no longer matches the signature it was handed.

New `codec/flac` exports: `Numberer` (`NewNumberer`, `Number`) and
`RenumberFrame` number frames from where a run starts, the way every FLAC
writer now numbers what it writes; `FrameLen` finds a frame's end by its own
parse; `SilentFrame` builds the frames a copy carries a hole in; `Settle`
(`Settled`), `RestateStreamInfo` and `ClearMD5` settle a reader's length and
STREAMINFO against its frames; `MaxSamplesIn` bounds what a run of bytes can
hold.

Behaviour that changed with them: an open span (`Slice` with `ToEnd`) is held
to an exact length as a bounded one is, unless the source reported damage on
the way; a FLAC copy carries its source's MD5 only into a header it patches at
End and only while it fills no hole, so a streamed or segmented copy states
none.

## Amendment (2026-11-02): a master playlist can name its renditions

`m3u8.ParseMaster` read past `EXT-X-MEDIA`, so a variant whose `AUDIO`
attribute names a rendition group was listed like one carrying its own
audio, and a client following its URI got video alone. The types now say
it: `Rendition` is one `EXT-X-MEDIA` tag (TYPE, GROUP-ID, NAME, URI,
LANGUAGE, DEFAULT, AUTOSELECT, CHANNELS, INSTREAM-ID; FORCED,
ASSOC-LANGUAGE and CHARACTERISTICS are read past), `MasterPlaylist.Media`
lists them, and `MasterVariant.Audio` is the variant's AUDIO group.
`Master` takes the whole `MasterPlaylist`, so it stays `ParseMaster`'s
inverse. It writes the renditions and refuses one its TYPE or its group
forbids (a URI on CLOSED-CAPTIONS, none on SUBTITLES, an INSTREAM-ID
missing, misplaced or outside CC1 to CC4 and SERVICE1 to SERVICE63, a
second DEFAULT=YES or a repeated NAME in a group) and an AUDIO naming no
audio group. The daemon's masters carry neither and are byte-identical.

Both parsers refuse more, each for the RFC 8216 clause named:

- **What a client must fail on** (`malformed-input`): a second
  `EXT-X-VERSION` (4.3.1.2), `EXT-X-INDEPENDENT-SEGMENTS` or `EXT-X-START`
  (4.3.5), and a master tag beside a media playlist or media segment tag
  (4.3.4), which each parser reads as any tag of the other kind.
- **What the types cannot represent** (`malformed-input`): an `EXT-X-MEDIA`
  without TYPE, GROUP-ID or NAME, with an empty URI or INSTREAM-ID, a URI on
  CLOSED-CAPTIONS, an INSTREAM-ID missing on CLOSED-CAPTIONS or present
  elsewhere (4.3.4.1); no URI on SUBTITLES (4.3.4.2.1); a STREAM-INF without
  BANDWIDTH or with one that is not a decimal-integer, an empty AUDIO or one
  naming no TYPE=AUDIO group, a STREAM-INF without its URI line or a URI
  line without its STREAM-INF (4.3.4.2); in `ParseMedia`, an `EXT-X-KEY`
  without METHOD (4.3.2.4), an `EXT-X-MAP` without URI (4.3.2.5) and a
  repeated media playlist tag (4.3.3); an attribute without a name or `=`,
  with a quote that wraps no whole value, which moves the commas the list
  splits on, or a decimal-integer or enumerated-string value in quotes
  (4.2). A quoted-string written bare reads as written. A BANDWIDTH past an
  int is `unsupported-format`.
- **Two SHOULDs taken**: an attribute name given twice (4.2) is
  `malformed-input`, and a leading byte order mark (4.1), refused before as
  a missing `#EXTM3U`, is now refused by name.
- **Two deviations**: `EXT-X-DEFINE` is rfc8216bis's (4.4.2.3), and RFC 8216
  6.3.1 has a client ignore a tag it does not know, but a URI left
  unsubstituted names nothing a client can fetch, so both parsers refuse it
  as `unsupported-format`. And where 6.3.1 drops a tag whose enumerated
  value it does not define, a DEFAULT or AUTOSELECT value other than YES
  reads as NO: those only steer the choice within a group, and dropping the
  rendition would lose the audio a variant names.

An `EXT-X-MEDIA` whose TYPE RFC 8216 does not define is dropped, as 6.3.1
asks, and an AUDIO naming its group is refused with that TYPE named. The
group rules a server must keep (one DEFAULT=YES, distinct NAMEs, no
AUTOSELECT=NO beside DEFAULT=YES) are read as written, since a host listing
variants can still use such a master. `IsMaster` matches the tag's whole
name, so `#EXT-X-STREAM-INFO` no longer counts, and `ParseMedia` matches
every tag by name, so a bare `#EXT-X-KEY` or `#EXT-X-MAP` is refused rather
than read past and `#EXT-X-ENDLIST:` ends the list.

It is a behavioural break taken for the 2026-10-30 reason: a parser that
reads past a tag its types cannot describe lists a stream a client cannot
play. The internal HLS client follows a variant's audio rendition (the
group's DEFAULT=YES member, then AUTOSELECT=YES, then the first; the
variant's own URI when the rendition has none) and probes a VOD's init
before it downloads the segments.

## Amendment (2026-11-02): a header walk stops with its context

Nine signatures changed. `container.Walker.Walk(ctx)` and
`format.Walker.Walk(ctx)` take a context, which every walk reads before each
of its steps (a frame, a moof, a Matroska element, an ASF packet): a context
that ends stops the walk there with `canceled`, as a pause rather than a
failure. Nothing is settled or latched, `Walked` reads false, and the next
`Walk` resumes where the paused one stopped, so damage is met and reported
once: the frame index as it stands (MP3 in any container, ADTS), the count and
its reader (WMA v1/v2), a fragment cursor (fragmented MP4), the walk's own
reader cursor (Matroska, exact for Vorbis too). `format.WalkMedia(ctx, med)`,
`format.LengthConfirmer.ConfirmLength(ctx)`, `format.Probe(ctx, ...)` and
`waxflow.Engine.Probe(ctx, ...)` carry it, as do the `Walk` methods of the
adts, aiff, asf, mka, mp4, mpa and riff demuxers: a strict probe's walk stops
with the probe's context, and a tolerant probe walks nothing. With it
`waxflow.MeasureLength` stops on every route.

`container.Seeker.SeekSample(ctx, track, sample)` and
`format.Media.SeekSample(ctx, target)` take a context too, with every
demuxer's `SeekSample` and `waxflow.Slice`'s and `Concat`'s: the walk a seek
runs to place its landing (an MP3 or ADTS index walk to the target, a
Matroska cluster walk without a usable cue, a fragmented MP4's moof scan, a
WMA v1/v2 count to the target) reads it the same way and pauses the same way.
A read carries no context, so a `Slice` positioning itself on its first read
does so under the context it was built with: the background one for `Slice`,
the caller's for `waxflow.SliceMeasured(ctx, ...)`, which every daemon and CLI
window goes through. `waxflow.Engine.PacketGrid(ctx, src, hint)` reads its
context per packet, since the grid is a walk of every packet in the file.

New in `container`: `Warnings`, the list every demuxer now records its
findings in, with `MaxWarnings` (64) and `WarningsMark` for a speculative
scan's rollback. Damage is listed up to the cap and deduplicated, counted past
both (`DamageCount`); notes are listed on a cap of their own and take no
damage slot, where they used to share the one list's 64 entries. mp4 joins
`container.Damaged` with it. New in `source`: `File.Retain`, a second holder
on an open file, which closes with its last holder; the daemon's shared
measure and packet-grid walks hold the file they were started on this way,
so a caller that leaves and closes its handle leaves the walk its file.

Behaviour that changed with them. A strict refusal is kept: a walk the owner
refused (MP3 in any container, ADTS) is not `Walked` and `Walk` refuses again,
where the second call used to settle the length. A Matroska full walk that
fails keeps its error, which `Walk` reports again at no cost (it used to
latch itself as run before running, so the second `Walk` returned nil over a
track nothing had measured), and every seek after it lands on what the walk
indexed. A Matroska walk runs on a cursor of its own, so a Vorbis seek takes
the Cues bound like any other codec's, and a read failure on an element
header is `source-unreadable` rather than damage that ends the stream. A
fragmented MP4 scan that failed resumes at the box it stopped on rather than
reading every moof again, one that ended on tolerated damage is over, and a
second `Walk` after a refused compare reads nothing. A WMA v1/v2 walk reports
each finding once, however often `Walk` is called. `mka.Demuxer` implements
`container.Damaged`, with its warning list capped at 64 and deduplicated like
the ten that already were: a block damaged on every cluster no longer grows
the list without bound, and a block read twice across a seek back is listed
once.

## Amendment (2026-11-06): `pid:` served in-tree, over HTTP

The stock build now resolves `pid:<id>` itself: `source.PIDLookup` asks a
host configured by URL (`pidLookupURL`) for the root-relative reference
behind an id, `GET <url>/<id>` answering `{"ref": "<root>/<path>"}`, and
opens it through the roots. The one consumer of the 2026-07-17 seam,
WaxDeck, stopped shipping an engine image with a catalog reader linked in
and asked for exactly this, so the published image serves pids for any
host. The `pid:` lookup host section of docs/api.md is the host contract.

The `cli.Flavor` seam and `catalogDB` stay as the extension surface for a
build that resolves pids another way (against a catalog directly), with
one change to the contract stated above: an implementation that is
present but unconfigured delegates to `opts.Next`, the stock chain,
rather than refusing its own schemes, since that chain now answers or
refuses `pid:` precisely (naming `pidLookupURL`); it may add its own key
to that refusal, as the example does. `ResolverOptions.Next` is that
chain, the roots plus the lookup when one is configured, and `catalogDB`
and `pidLookupURL` are mutually exclusive. `delivery.pid` is inferred from
either; a `PIDSourceReporter` still overrides.

The id grammar is wider than the ULID ADR-0003/0004 name: 1 to 128
characters of `A-Z a-z 0-9 _ -`, so a host's prefixed ids fit. The
identity rule those ADRs state is unchanged: the opened file keeps
`pid:<id>` as its reference, size and mtime pin the bytes, and a rename
the host tracks keeps URLs and cache entries.

## Amendment (2026-11-12): a reference has a text form

A source reference is a byte string, since a file name is, and JSON holds
only UTF-8: an encoder writes U+FFFD for a stray byte and the name that
arrives is another file's. WaxBin, which catalogs such files by their raw
path, asked for a pid lookup answer that could name one. The gap was wider
than the answer: `POST /sign`, `/probe`, `/jobs` and `/hls/timeline`
rewrote the name the same way and answered for the wrong file or none, and
the HLS descriptor refused it.

New in `source`: `Ref`, a string type whose text form is the reference
itself when that is valid UTF-8 and otherwise `pct:` followed by the
reference with each stray byte, and each `%`, written `%XX`; `ParseRef`
reads the form back. `Ref` marshals as its text form, so a struct that
carries a reference through JSON types the field `Ref`. A plain reference
marshals exactly as a string did, so stored jobs, timeline digests and HLS
URLs are unchanged. `Resolver.Resolve` is untouched: the form is read at
the edge and a resolver still receives the reference's own bytes.
docs/api.md's Source references section is the wire contract.

Behaviour that changed with it. A reference in JSON is read strictly
(`Ref.UnmarshalJSON`, `source.CheckJSON`): a stray byte in it, or a
`\uDXXX` escape of half a surrogate pair, is `400 invalid-request`, where
a decoder used to read both as U+FFFD and the request was answered for
whatever file that named. Only the reference is held to that, so a stray
byte in a field beside it reads as before. A pid lookup answer whose `ref`
is such text is `500 internal`, where it used to open the U+FFFD name.
`/hls/master.m3u8` mints for a name that is not UTF-8, where it refused,
and refuses `src` beside `tl` by presence, where an empty `src` was
dropped. On Windows a reference or a root path whose bytes UTF-16 cannot
hold is refused (`404`, and a configuration error), where the conversion
opened the name with U+FFFD in it. A URL the daemon, the CLI or the
client mints escapes a reference that itself starts with `pct:`
(`Ref.QueryValue`), since every reader decodes one. A message that quotes
a name goes out with each stray byte written `\xNN`: the envelope, a job
document's text (escaped once, where the job is stored), and a probe's
warnings and notes. `server`'s `TimelineSrc.Src` is a `Ref` and
`SignRequest.Params` a `SignParams`, whose `src` is the reference's own
bytes in Go and its text form in JSON. The
`client` package's request types keep `string` fields that hold a
reference's own bytes, and marshal them in text form themselves.

A path in the configuration is bytes as well, and takes the same form:
`roots[].path`, `dataDir`, `cacheDir`, `scratchDir`, `tlsCert`, `tlsKey`
and `catalogDB` are read from it, in the file and in their `WAXFLOW_*`
variables, so a file or a compose block that holds only UTF-8 can name a
folder that is not. A config file that is not UTF-8, or that escapes half
a surrogate pair, is refused at load. `GET /roots` writes `path` in the
form and quotes it that way in `error` (`RootStatus.PathText`, `ErrText`;
`client.Roots` hands back the bytes). A path that starts with `pct:` is
therefore read as marked.

## Amendment (2026-11-13): the client mirrors every wire type, and refusals move to where a value is read

What a caller of the public packages or of the wire sees:

- `client` gains `CapsDelivery.HEv2`, which `/caps` has sent and the mirror
  dropped, `UploadResponse` with `(*Client).Upload` and `DeleteUpload`, and
  `SilenceMap` with `SilenceMapSpan` for the `silence.json` an analyze job
  writes. A test in the server suite now holds every struct the daemon
  writes or reads to having a mirror or a stated reason for none.
- `source.OpenLocal` codes its open failures as a root's file is coded:
  only a path that names nothing is `not-found` (a missing file, or a path
  that runs through a file). One that is there and does not open
  is `source-unreadable`, and running out of file handles is `overloaded`,
  so an upload the daemon cannot read is a 500 or a retryable 503 where it
  was a 404.
- `server.New` refuses an API key no request can present, an empty one or
  one with white space at an end, and a metrics key with white space at an
  end (`invalid-request`). Such an API key counted for the fail-closed rule
  and admitted nobody. `server.LoopbackAddr` is gone: nothing called it
  once the rule and its sentence had one home.
- `PlanSegments`, `PlanRemuxSegments` and `PlanCutSegments` refuse a segment
  duration that is NaN (`invalid-request`), which passed the range check and
  became a frame count by a conversion the language leaves to the platform.
  On the wire, a `segDur` that is NaN or infinite at the HLS mint or at
  `POST /sign` is a 400; it used to panic the handler.
- A Matroska track that leaves `FlagDefault` out, or writes it with no
  octets, is a default track, as the format says, and a track whose
  `FlagEnabled` is 0 is passed over while a usable one exists (the flag was
  not read before). In a file of several audio tracks the one selected can
  change: a track that writes `FlagDefault` 0 no longer wins over a later
  one that writes nothing.
- `container.Track` gains `DemuxerVersion`, a cache-key term a demuxer sets
  when a change makes it deliver other samples for an unchanged file, and
  `container/mka` exports its own as `DemuxerVersion` (`mka-demux-2`, for
  the track selection above). Every plan built from a track that names one
  carries it first in `Versions`, and a timeline's plan lists each
  member's, so cached entries of Matroska and WebM sources, and of
  timelines with such a member, miss once and no other key moves (ADR-0004,
  amended the same day).
- `waxflow.FloatBuildVersion` is new: a cache-key term naming how the
  build rounds floats, `fp-<GOARCH>` (`fp-amd64v3` and `fp-amd64v4` on
  amd64) and empty for amd64 below `GOAMD64=v3`. Every plan that runs a
  chain carries it behind the decoder's term, so on every build but that
  baseline cached transcodes miss once (ADR-0004).
- The `hls-js` profile in `/caps` loses its second note, which said
  `bits=12` and `bits=20` are honored. `/stream` has always taken 16 or 24.

The configuration is not a public package, and its behaviour is the CLI's.
A `defaultGain` that is no mode and no number, a `signingSecret` that is no
key list (or names one key id twice) and an `apiKeys` entry or a
`metricsKey` no request can present are refused when the configuration
loads, by every command that loads it, and by `POST /roots/reload`, which
re-reads it; the daemon used to refuse the first two after it had opened
its stores, and `doctor` passed them all. A refused `signingSecret` or key
is named by its position and never quoted, and a refused `pidLookupURL` is
quoted without its query or fragment. The two rules that belong to a daemon's start
alone, no keys on a non-loopback `addr` and a `debugAddr` that is not
loopback, are refused by `server` before it creates anything and failed by
`doctor`, which also loads a configured TLS pair and warns when a
non-loopback `addr` is served without one (ADR-0007).

## Amendment (2026-11-13): one demuxer options type, revisions and measured lengths

What a caller of the public packages sees:

- The twelve `<pkg>.DemuxerOptions` types (`adts`, `aiff`, `apen`, `asf`,
  `flacn`, `mka`, `mp4`, `mpa`, `mpc`, `ogg`, `riff`, `wv`) are gone, each
  a struct of one field, `Strict`. `container.DemuxerOptions` replaces them
  with `Strict` as it was and a new field `Base`, where the source's byte 0
  sits in the file it was cut from. Every `NewDemuxer` takes
  `*container.DemuxerOptions`, and nil still means the defaults. No alias is
  left behind, so `mp4.NewDemuxer(src, &mp4.DemuxerOptions{Strict: true})`
  is now `mp4.NewDemuxer(src, &container.DemuxerOptions{Strict: true})`.
  The options change alone moves no sample or packet a demuxer delivers. A
  demuxer adds `Base` to every offset it reports: a warning's `Offset`, the
  `(at offset N)` of a Strict refusal, and an offset a message names.
  `format` sets it to the length of the leading ID3v2 tag it steps over, so
  a finding behind one names its place in the file; with no tag ahead
  nothing moves. A finding with no place stays at -1, and its Strict
  refusal now names no offset where it used to say `(at offset -1)`.
  `mp4.Demuxer` gains `DamageCount` (`container.Damaged`), so
  `container.DamageOf` counts its damage past the list's cap and its
  de-duplication, as it does every other demuxer's.
- `container/wv` exports `DemuxerVersion` (`wv-demux-2`), the term its track
  carries into every cache key over a WavPack source, as `mka.DemuxerVersion`
  does for Matroska (ADR-0004, 2026-11-15). `codec/wavpack` gains
  `MaxSamplesIn`, the most samples a run of bytes can hold as blocks, which
  is what the demuxer holds a skip in the block indexes to. `container`
  gains `Measurer` (`MeasureSamples`), for a demuxer that measures a track
  from headers where its open did not call the length exact, and `format`
  gains `Measurer` and `MeasureMedia`, which ask a Media's demuxer for it
  and which a wrapper forwards as it forwards `Walker`; `MeasureLength`
  takes that route before it seeks. `wv.Demuxer` implements it.
- `container/asf` exports `DemuxerVersion` (`asf-demux-2`), the term a WMA
  v1/v2 track carries into every cache key over such a source; a WMA Pro,
  Voice or Lossless track carries none (ADR-0004, 2026-11-15).
- `container/flacn` exports `DemuxerVersion` (`flacn-demux-2`) and
  `container/ogg` exports `DemuxerVersion` (`ogg-demux-2`), the terms their
  tracks carry into every cache key over a native FLAC source and over an
  Ogg one, whatever its codec (ADR-0004, 2026-11-15). `codec/flac` gains `FrameLenRead`, which is `FrameLen` with the bytes
  it read to answer, `StreamInfo.FixedBlock`, the block size
  STREAMINFO states for a fixed-blocksize stream, `StreamInfo.NumberingBy`,
  `StreamInfo.NumberingFrom`, `StreamInfo.NumberingLast` and
  `StreamInfo.NumberingUnsettled`, which settle a stream's numbering by what
  follows the first frame (the frame behind it, the frame behind the first
  whole one where the first does not check, or the stream's end) where
  STREAMINFO and that frame leave it open, and `ReadOff`, the words of a
  read that ends off an exact length. `StreamInfo.Numbering` reads as it
  did. `container` gains `SettleRead`, which holds a track's exact length
  against where a whole read ended, for both FLAC demuxers and WavPack's.
  `ogg.Demuxer` implements `container.Measurer` for a FLAC track: where the
  open did not call the length exact, or a read took the claim back,
  `MeasureLength` is what a read from the first frame delivers.
- `format` gains `Forwarder`, which a wrapper of a Media embeds in place of
  the Media to forward `Walker`, `Measurer`, `MixedWidth` and `Headroomer`,
  each answering for a Media without it as that Media does unwrapped.
  `LengthConfirmer` and `Composite` are not forwarded. `waxflow.Headroomer`
  moves to `format.Headroomer`, beside `format.MediaHeadroom`, with no
  alias left.
- `waxerr` gains `Fixed`, an error that reads and classifies as `New`'s and
  that no caller can change: `errors.AsType[*waxerr.Error]` on it gets a
  fresh copy. `mp3.ParseHeader`'s refusals and the Musepack decoder's frame
  refusals are `Fixed`, so a caller's edit reaches no other caller.

What changes meaning without moving an identifier (ADR-0004, 2026-11-15):

- `Result.LostSamples` counts a WavPack copy's holes: its blocks state their
  own indexes, so the copy carries the gap, writes no silence, and counts
  the gap in `Samples` and in the header's total as a transcode does. The
  copy hands `container.Packet.Lost` to the muxer for such packets, and
  `wv.Muxer` counts it in the length it writes.
- `container.Truncated` and `format.Info.Truncated` can grow at a read's
  end: a read from the first packet that ends short of an exact length adds
  the rest, past what the open counted.
- A Media's `Walk` (`format.Walker`) refreshes the length for a demuxer with
  no walk, taking a claim a read's end withdrew, which can lift the cap.

## Amendment (2026-11-18): the HLS client is test support

The client that reads an HLS presentation back (`OpenVOD`, `OpenLive`,
`Fetcher`, `HTTPFetcher`) moves from `internal/hls` to
`internal/testutil/hlsclient`. It exists so the tests can read what the
segmenter and the daemon write, and no product package calls it.
`internal/hls` keeps the descriptor and the variant manager. This
supersedes the two sentences above that place the client in `internal/hls`
(2026-10-30 and 2026-11-02, left as they were written); what the second
says the client does still holds. No exported identifier of a public
package moves and no behaviour changes.

`make depcheck` gains a second clause to keep it that way: nothing in the
public tree, and nothing `cli/cmd/waxflow` links, imports
`internal/testutil` or a package under it. Both clauses read the host's
build and the Windows one, and a listing that fails, or that lacks the
packages it must hold, fails the check.

## Amendment (2026-11-18): a Media wrapper refuses a span, one closer for a Media, and errors that name no path

What a caller of the public packages sees:

- `format` gains `WithCloser(med, c)`: a Media that closes `med` and then
  `c`, and reports the first error of the two. It closes both once, and a
  later `Close` returns nil. It forwards what `Forwarder` forwards, and over
  a `Composite` it answers `Members` as that Media does. A Media that is a
  `LengthConfirmer` (a span) is refused with `invalid-request` and neither
  is closed, because the wrapper would hide the confirming and a run would
  commit a length nothing confirmed. A nil `med` or `c` is refused the same
  way.
- `waxflow.MeasuredMedia(med, measured)` returns `(format.Media, error)`.
  It refuses a `LengthConfirmer` the same way, and only where it would
  restate the length: a negative `Samples`, or a Media that already
  declares the measure, still comes back untouched with a nil error. A
  `Concat` is restated as before and now still answers `Members`. A refused
  Media stays open and the caller's.
- `waxflow.SliceMeasured` keeps its signature and returns that refusal for a
  span whose length the measure would restate, closing the span as it
  closes its source on every error. With a negative `Samples` a span of a
  span is served as before.
- `source.File.Close` reports a close the system refuses as
  `source-unreadable`, with the system's error behind it and no path in the
  message. It returned the bare `*fs.PathError`.
- `source.File.ReadAt` returns a failed read's error without the
  `*fs.PathError` around it, so the message a demuxer builds on it holds no
  path of the daemon's; `io.EOF` and a short read are unchanged, and
  `errors.Is` still finds the system's error. A stat that fails in
  `Roots.Resolve` or `source.OpenLocal`, and an open that fails in
  `OpenLocal`, drop the path the same way.
- `waxerr` gains `Message(err)`, the text of an error for a client: every
  `*fs.PathError` and `*os.LinkError` in the chain whose path is absolute
  keeps its operation and cause and loses the path, and a relative path,
  the client's own reference under a root, stays. The daemon applies it
  where an error becomes something a client reads: the error envelope, a
  job's error and wait, and a job warning made from an error. The cache,
  jobs and timeline stores make their directory absolute when they open,
  as the uploads store does, so the rule sees every path of theirs. A
  failed job's message and a refused request hold no part of a store's
  directory.
- `waxerr` gains `Pathless(err)`, which returns the cause of the
  `*fs.PathError` or `*os.LinkError` in `err`, or `err` itself, so whatever
  wraps the path error goes with the path. `source` builds the errors
  above on it: an embedder of that package has no boundary that applies
  `Message`.
- `source.File` gains `NamedCloser(name)`, a closer of the file that names
  it in a failed close's error and keeps the code: how an HLS timeline
  names the member below.

What changes meaning without moving an identifier:

- A merge job reports a member whose file fails to close as a warning that
  names the member (`member 1 (lib/b.flac): source: closing the file: ...`)
  and finishes with its output: every sample the member declared was read
  before the close. The job runner used to drop the error. An HLS timeline
  has no warning to carry it, so the read that advanced past the member
  still fails there, `source-unreadable`, and now names the member
  (`member 1: source: closing the file`).

## Amendment (2026-11-21): one output table, and options that are a value

The output table now holds each format's container forms, its encoder knobs
and whether it is lossy, and every plan, run and boundary asks it.
`TranscodeOptions` holds only what shapes a plan, so it compares with `==`.

What a caller of the public packages sees:

- `TranscodeOptions` loses `Tags`, `Chapters`, `Art` and `Progress`. None of
  them shaped a plan, and a slice, a pointer and a func made the options a
  value nothing could compare. What is left compares with `==` and keys a
  map.
- `waxflow` gains `RunOption`, what rides along with one run and never
  shapes its plan, and its four constructors: `WithTags(tags)`,
  `WithChapters(chapters)`, `WithArt(art)` and `WithProgress(fn)`. Each
  means what the field of the same name meant. A later option of a kind
  replaces an earlier one.
- The five run entry points take them as a trailing variadic:
  `Transcode(ctx, src, hint, dst, opts, run...)`,
  `TranscodeMedia(ctx, med, dst, opts, run...)`,
  `Remux(ctx, src, hint, dst, opts, run...)`,
  `RemuxDemuxer(ctx, demux, track, dst, opts, run...)` and
  `CutStream(ctx, src, hint, dst, opts, spans, grid, measured, run...)`.
  A call with none compiles unchanged and writes no tags, chapters or art
  and reports no progress, as the zero fields did. The plan calls keep
  their signatures.
- `TranscodeOptions` replaces `MP3Bitrate`, `OpusBitrate`, `AACBitrate` and
  `VorbisBitrate` with one `Bitrate int`, and `MP3VBR` and `OpusVBR` with
  one `VBR bool`. Each means what the field of its format meant: bits per
  second, 0 for the encoder's default. The naming rule for the struct: a
  knob more than one format takes has no format prefix.
- `TranscodeOptions` gains `Check() error`, which refuses what the options
  alone rule out, in this order: a non-finite `GainDB` or `VorbisQuality`,
  a negative `Bitrate` or one above `MaxBitrate`, a negative `FromSample`
  (each `CodeInvalidRequest` naming the field), an empty `Format`
  (`CodeInvalidRequest`) or an unregistered one (`CodeUnsupportedFormat`),
  a `Container` the format has no form for (`CodeInvalidRequest`, in the
  form lookup's words), a set knob the format does not take
  (`CodeUnsupportedFormat` wrapping a `*KnobError`), and a `FLACLevel`,
  `WavPackLevel`, `APELevel`, `OpusComplexity` or `OpusSignal` outside its
  range (`CodeInvalidRequest`, in the words the plan used). `PlanTranscode`,
  `PlanRemux`, `PlanCut`, `PlanSegments`, `PlanRemuxSegments`,
  `PlanCutSegments`, `InitSegment`, `TranscodeMedia`, `TranscodeSegments`,
  `TranscodeSegmentsMedia`, `RemuxSegments`, `CutStream` and `CutSegments`
  run it first, and every other engine method that takes the options
  reaches one of them before it plans or encodes anything. `CutTrack` and
  `Cut`, which read only the trim options, do not run it.
- `waxflow` gains `MaxBitrate`, the largest `Bitrate` in bits per second,
  `math.MaxInt32`, so a rate fits an `int` on every build. Every boundary
  that takes kbit/s refuses a value above `MaxBitrate/1000` before it
  multiplies.
- `waxflow` gains `Knob`, an encoder option only some output formats take,
  with one constant per knob: `KnobBitrate`, `KnobVBR`,
  `KnobVorbisQuality`, `KnobHEAACv2`, `KnobFLACLevel`, `KnobWavPackLevel`,
  `KnobAPELevel`, `KnobOpusComplexity` and `KnobOpusSignal`. `Knob.String`
  is the option's field name. `TranscodeOptions.Knobs()` reports the knobs
  a value sets: each field that differs from its zero value, with
  `OpusSignal` "auto" counted as unset.
- Each format takes a fixed set: wav, aiff and alac none, flac
  `FLACLevel`, wavpack `WavPackLevel`, ape `APELevel`, mp3 `Bitrate` and
  `VBR`, aac `Bitrate`, he-aac `Bitrate` and `HEAACv2`, opus `Bitrate`,
  `VBR`, `OpusComplexity` and `OpusSignal`, vorbis `VorbisQuality`.
- `waxflow` gains `KnobError{Knob, Format, Takers, Lossless}`, a set knob
  the named format does not take. `Takers` lists the formats that take it
  in table order, and `Lossless` says the refusal's reason is a lossless
  format, which holds no bit rate (set for `Bitrate` and `VBR` only).
- `OutputInfo`, what `Outputs()` lists, gains `Lossy` and `Knobs`, the
  format's knob set. `LossyFormat` is removed and `OutputInfo.Lossy`
  replaces it: a name `Outputs()` does not list is the old `known` false.
- The codec packages name their frame sizes: `opus.FrameSize` (960),
  `aac.FrameSize` (1024), `aac.HEFrameSize` (2048) and
  `mp3.FrameSize(rate int) int`.
- `vorbis.EncoderVersion` is a function, `EncoderVersion(quality float64)
  string`, where it was a constant, so an importer that reads it as one no
  longer builds. Quality 0 and 3 give the old string (ADR-0004, amended the
  same day). `vorbis.NewEncoder` refuses a NaN quality, which passed its
  range check.
- `aac.NewEncoder`, `aac.NewHEEncoder` and `aac.HEPlanBitrate` refuse a
  negative bit rate, as the mp3 and opus encoders do.

What changes meaning without moving an identifier:

- A knob the format does not take is refused. Before this the engine
  ignored it, except a vorbis bit rate, which the encoder refused with
  `CodeInvalidRequest`; that is now `CodeUnsupportedFormat` from `Check`.
- A negative `Bitrate` is refused on every format, lossless ones included.
  Before this mp3 and opus refused it, aac and he-aac clamped it up to their
  floor, and the lossless formats ignored it.
- A non-finite `GainDB` was refused by the DSP chain in its own words where a
  chain was built and declined by the remux and cut plans, and a NaN
  `VorbisQuality` planned and encoded. `PlanRemux` and `PlanCut` refuse a
  negative `FromSample` where they declined it, and `format=aac` with
  `HEAACv2` on an HE-AAC track is refused against the named aac row where
  `PlanRemux` declined it.
- A container the format has no form for is refused ahead of any other
  option, on every row, by every plan and run; only what `Check` refuses
  before it comes first. Before this `PlanTranscode` refused the other
  option first while a run refused the container, on every row but aac and
  he-aac. The he-aac refusal says `he-aac container` where it said `aac
  container`.
- `OutputExt` and `OutputEmbedsTags` answer a pair no form takes for the
  row's default form: (`wavpack`, `mka`) names `wv` where it named `mka`,
  and (`mp3`, `ogg`) does not embed where every pair with `ogg` did.
  `OutputEmbedsTags` of an unknown format is false where it was true with
  `ogg`.
- The segmented plans and runs and `InitSegment` refuse a format with no
  HLS form in one sentence that names the formats with one, where
  `RemuxSegments` and `CutSegments` answered with the remux rung's decline
  (`CodeInvalidRequest`) or the segmenter's refusal. `PlanSegments`,
  `InitSegment` and the two segmented transcode runs refuse a `FromSample`
  in one sentence too, and `InitSegment`, which accepted one, refuses it.
- A `FLACLevel`, `WavPackLevel`, `APELevel`, `OpusComplexity` or
  `OpusSignal` outside its range is refused by `Check`, so every plan and
  run refuses it before it plans, opens or encodes, in the words the plan
  or encoder used; an Opus complexity reads "waxflow: Opus complexity 11
  outside -1..10" where the encoder said "opus: complexity 11 outside
  -1..10".
- `PlanSegments`, `PlanSegmentsTimeline`, `InitSegment`,
  `TranscodeSegments`, `TranscodeSegmentsMedia`, `PlanRemuxSegments`,
  `RemuxSegments`, `PlanCutSegments` and `CutSegments` refuse a non-empty
  `Container`, `CodeInvalidRequest`, in one sentence, after `Check`. The
  segmenter writes fragmented MP4 whatever the options name, and a plan for
  `{aac, adts}` answered with the ADTS container, media type and muxer
  version while its run wrote fMP4. No product caller passes one.
- `OpusSignal` "auto" is the unset hint in the plan cache's key and in the
  remux rung's test, as in `Knobs`. On an Opus source, `{opus, OpusSignal:
  "auto"}` now remuxes where it re-encoded, and it keys the plan `{opus}`
  keys.

On the wire and in the CLI:

- A vorbis `bitrate` or `q` at /stream, and a vorbis `bitrate` at a job's
  creation, is refused, `415`. At 7cbb90d both were accepted and ignored
  (the knob golden's lines answered `200` and `201`). A job's
  `flacLevel`, `wavpackLevel` or `apeLevel` for another format is `415`
  where it was `201`.
- The server words a `*KnobError` in its parameter names, at the point it
  refused a bit rate on a lossless format: /stream, HLS and job creation.
  An HLS refusal names the bit rate as `bitrate/bitrates` and only the
  formats HLS serves.
- An unknown format on a timeline stream or on any HLS path (a `v=` URL,
  the keyed master mint and `POST /sign` of a master), and a container the
  format has no form for on a timeline stream, are refused before any
  source resolves. On a `v=` URL the unknown format now comes before a bad
  gain, timeline gain or dynamics, which answered first. The mint checks
  the query before it resolves the source, which it must do before
  `Canonical` because a descriptor needs the source's ID, so a knob refusal
  there now also beats a missing source (`404` became `415`) and the
  descriptor's own checks (`format=flac&bits=20&bitrate=128` answered
  `400` "bits 20: want 16 or 24" and now answers `415`).
- `POST /sign` of a single-source `/stream` URL answers what a `HEAD` of it
  would, through the `HEAD`'s own preparation, where it signed any URL
  whose source resolved: what the query alone decides (an unknown format, a
  bad container, a knob the format lacks, under `format=auto` too) is
  refused before the source resolves, and a source container's own name
  that may direct-play is decided against the source. An `id` in the
  params is replaced by the source's current one, unchecked, as it was.
- `waxflow transcode` and `waxflow split` send `--flac-level`,
  `--wavpack-level`, `--ape-level`, `--opus-complexity`, `--opus-signal`
  and `--he-v2` only when given, so a bare command writes the bytes it did,
  and a flag the format does not take exits 5. That includes the bit-rate
  and VBR flags (`--mp3-bitrate`, `--mp3-vbr`, `--opus-bitrate`,
  `--opus-vbr`, `--aac-bitrate`), which were dropped in silence for a
  format they do not name.
- `waxflow split --dry-run` refuses what the run would: a pair no form
  takes, and a level the encoder refuses. It listed the pieces.
- `waxflow transcode` and `waxflow split` refuse what the flags alone decide
  before the source is opened or probed, in the engine's order: the format
  and container, then a bit-rate or VBR flag, then the other knob flags. A
  missing source behind a bad flag answers the flag, and `--format opsu
  --opus-bitrate 64` answers the unknown format where it named the flag.
- `--flac-level` and `--opus-complexity` refuse a value outside 0 to 8 and
  0 to 10, exit 2, in the flag's words ("--flac-level -1: want 0 to 8"). -1
  wrote level 0 and complexity 0 through the engine's sentinel, and the
  rest were refused after the source opened in the engine's range, which
  names the sentinel. `--wavpack-level`, `--ape-level` and `--opus-signal`
  out of range are refused before the source opens too, in the engine's
  words.
- Every kbit/s value is refused above 2147483, `MaxBitrate/1000`, `400`
  and exit 2, in its surface's words: /stream `bitrate` and `maxBitRate`,
  the HLS mint's `bitrate` and each `bitrates` rung, a `v=` descriptor's
  `bitrate` and `bitrates`, a job's `bitrate` and the CLI's
  `--mp3-bitrate`, `--opus-bitrate` and `--aac-bitrate`. A larger value was
  multiplied by 1000 unchecked, and one like 2305843009213694080 wrapped to
  128000 and played. A negative CLI bit-rate flag is refused in the flag's
  words too. A stored job whose `bitrate` is past the bound fails when it
  runs, `invalid-request`, in the job's words, where it ran at the wrapped
  rate.
- The HLS mint refuses a `bitrates` rung that is not positive in the
  ladder's words, `400` "ladder bitrate -5 is not positive", ahead of the
  options check, which answered in the engine's field name and value.
- Job creation refuses what the request alone decides before any source,
  sheet or merge member resolves or is measured: the job-only output
  rules and `Check` for transcode, split and merge, and the field rules for
  analyze. A level out of range is among them now that `Check` holds it, so
  `flacLevel: 9` beside a missing source answers `400` where it answered
  `404`. A merge with a knob its format does not take and a missing
  member answers the knob's `415` where it answered `404` after measuring
  every member before it, and likewise a transcode or split's missing
  source, and an analyze job with a stray field answers `400` before its
  source resolves.
- A CLI error whose message already starts with `waxflow: ` is printed
  once, not as `waxflow: waxflow: ...`.

## Amendment (2026-11-22): one request shape

The server's surfaces carry one output shape. `/stream` and the HLS mint
read it through one parser, so a parameter they share is refused in the
same words on both. They and a job map it onto engine options through one
function and check it by one wire policy, and a knob the format does not
take is refused in one sentence on every surface and by the CLI, each in
its own names for the parameters. The CLI names each knob with one flag.

For a Go caller:

- `waxflow` gains `CheckKnobs(format, set)`, the knob rule `Check` applies:
  `CodeUnsupportedFormat` wrapping a `*KnobError` for the lowest knob in
  `set` the format does not take, the refusal `Check` gives an unregistered
  format, and nil when the format takes them all. A bit in `set` that names
  no knob is `CodeInvalidRequest` ("waxflow: Knob(4096) is not a knob"),
  before the format is asked. `Check` calls it, so a boundary that knows
  which knobs it was given, set or not, asks the engine rather than copying
  its takers and its lossless rule.
- The `Bitrate` doc says what the mp3 encoder does: a whole number of
  kbit/s, clamped to the highest rate the layer holds at or below it. It
  said an mp3 rate must be a legal Layer III rate, which nothing enforced.
- `TranscodeOptions.Check` refuses a `VorbisQuality` off -1..10,
  `CodeInvalidRequest` "waxflow: Vorbis quality 50 outside -1..10", after the
  knob rule and beside the other ranges. The encoder refused it when a plan
  built it, in its own words ("vorbis: quality 50 outside -1..10"), and still
  does.
- `client.JobRequest` gains `VorbisQuality`.
- `waxflow` gains `RangeError`: `Check` refuses a level, Opus complexity, Opus
  signal or Vorbis quality out of range as `CodeInvalidRequest` wrapping a
  `*RangeError` holding the knob, the value as given, and what the knob takes
  (`Min` and `Max`, or `Values`, with `Unwritten` for an APE level the format
  decodes and the encoder does not write), so a boundary words it in its own
  names as it does a `*KnobError`. `Want` spells what the knob takes ("-1 to
  8", "auto, voice, or music"). `Error` gives the sentences `Check` gave,
  word for word.

On the wire:

- The HLS mint refuses `crossfadeSeconds`, `segDur` and `bitrates` present
  but empty, `400` on the keyed master form and on `POST /sign`, as `/stream`
  refuses an empty `crossfadeSeconds`. The first two minted a URL that left
  them out and the third was ignored. The refusal comes before the source
  resolves and ahead of a knob or format refusal, so codes move on both
  forms: `segDur=` on a missing source answers `400` where it answered
  `404`, and `hev2=1&bitrates=` and `format=nosuch&segDur=` answer `400`
  where they answered `415`.
- `rate`, `ch`, `bits` and `bitrate` are refused in one sentence on both
  surfaces: `rate "abc": want a positive integer` for any value that is not
  a positive integer, where `/stream` said `rate "abc": want an integer`,
  `rate must be positive; omit it to keep the source's value` and
  `bitrate must be a positive kbit/s value`. The mint's empty `bitrate`
  says `omit it to take the format's default`, as `/stream`'s did, where it
  said `omit it to keep the source's value`.
- Both surfaces refuse in one order: `rate`, `ch`, `bits` and `bitrate`,
  each empty and then not a positive integer, then `q`, then `hev2`, then
  the `bits` and kbit/s bounds. `rate=&ch=abc` answers `rate` where
  `/stream` answered `ch`, and `format=mp3&bitrate=0&q=low` answers the
  bitrate where `/stream` answered that `q` and `bitrate` are exclusive.
- On `/stream` that order runs after `src`, `tl` and `crossfadeSeconds` and
  ahead of `container requires an explicit format`, `track` and `maxBitRate`.
  What answered ahead of the shared parameters and no longer does: the
  container rule ahead of all of them (`container=adts&rate=abc` answers
  `rate`); a non-integer `track` or `maxBitRate` ahead of an empty or
  non-positive `rate`, `ch` or `bits` (`rate=0&track=x` answers `rate`); and
  every `track` and `maxBitRate` refusal ahead of `bitrate`, `q`, `hev2` and
  the `bits` bound (`track=-1&bitrate=0` answers `bitrate`, `bits=8&track=x`
  and `bits=8&maxBitRate=0` answer `bits`). A non-integer `rate`, `ch` or
  `bits` already answered first (`rate=abc&track=x` did not move). The `bits`
  bound now follows `bitrate`'s empty and integer checks, `q` and `hev2`, so
  `bits=8&bitrate=`, `bits=8&bitrate=abc` and `bits=8&bitrate=0` answer
  `bitrate` and `bits=8&q=huge` answers `q`; and the kbit/s bound follows
  `hev2` (`bitrate=2147484&hev2=` answers `hev2`). An empty `maxBitRate`
  says `omit it for no cap`, where it said `omit it to keep the source's
  value`, which a cap does not have. A `maxBitRate` that is not a positive
  integer reads as `bitrate` does, `maxBitRate "0": want a positive
  integer`, where it read `maxBitRate "abc": want an integer` and
  `maxBitRate must be positive; omit it to keep the source's value`.
- On the mint the order comes before the span, `crossfadeSeconds`, `segDur`,
  `bitrates`, `gain` and `dynamics`, where `bitrate`, `bits` and `rate` were
  read first and `ch` and `hev2` after the span: `bitrate=abc&rate=abc`
  answers `rate`, and `from=-1&hev2=` answers `hev2`. A `bitrate` above
  2147483 answers after an empty `hev2` where it answered first.
- The mint applies the `bits` bound when it parses, as `/stream` does:
  `bits=20` answers `400` "bits 20: want 16 or 24" ahead of every later
  parameter and before the source resolves, where it answered after them
  as "hls: descriptor bits 20: want 16 or 24", or `404` for a missing
  source. So `format=flac&bits=20&bitrate=128`, which answered `415` for
  the knob since the amendment of 2026-11-21, answers `400` for the depth
  again, and so does `format=nosuch&bits=20`, which answered `415` for the
  format.
- A `POST /jobs` body takes `vorbisQuality` for `transcode`, `merge` and
  `split`; `vorbis` takes it and every other format answers `415`, and `analyze`
  refuses it with `400`. A job's `bitrate` on `vorbis` reads "bitrate does not
  apply to vorbis, which is quality-driven; use vorbisQuality", where it named
  the formats that take a bit rate. A `vorbisQuality` off -1..10 is `400`
  "vorbisQuality 50: want -1 to 10" before the source resolves. A
  job document's `request` writes the same keys, `gain` and `loudness` now
  after the shape's.
- A job's negative `rate`, `ch` or `bitrate` names the field and its value
  and says what 0 means there, "rate -1: want 0 for the source's value or a
  positive integer" (for `bitrate`, "the format's default"), where each read
  "rate, ch, and bitrate must be non-negative". The order did not move.
- A JSON body (`POST /jobs`, `/probe`, `/sign` and `/hls/timeline`) holding
  a value of the wrong type is refused by its key path and the type the key
  wants, "request body: bitrate: want an integer", "request body: srcs.from:
  want an integer", "request body: cuts: want an array of integers", where
  it quoted encoding/json, Go struct and type names included ("request
  body: json: cannot unmarshal string into Go struct field
  jobRequest.bitrate of type int"). A number the key's type cannot hold is
  named by its value, "request body: vorbisQuality: 1e400 is out of range",
  as is an integer past the build's `int` on a 32-bit one, where encoding/json
  wrote "cannot unmarshal number 1e400 into Go struct field ...". A number
  written with a fraction or an exponent for an integer key still reads "want
  an integer". Still `400`.
- `q` on `/stream` and `POST /transcode` selects a Vorbis quality on
  `vorbis`, `low` 2, `med` 4 and `high` 6, where it answered `415`; on a
  format that takes a bit rate it names 96, 128 or 192 kbit/s as before. With
  `hev2=1` on `vorbis` the refusal names `hev2`, where it named the bit rate.
  The quality joins the progressive cache key's canonical string as
  `&vq=<quality>` after `seek`, only when one is set, so no existing key
  moves.
- `/stream`, `POST /transcode` and `POST /sign` word a `bitrate` and a `q`
  apart. A `bitrate` on a format that takes none reads "bitrate applies to lossy
  output (opus, mp3, aac, he-aac); flac is lossless", where it read "bitrate/q
  apply to ...", and on `vorbis` "bitrate does not apply to vorbis, which is
  quality-driven; use q=low, med, or high", where it named the formats that take
  a bit rate. A `q` on a format that takes neither a bit rate nor a quality
  reads "q applies to lossy output (opus, vorbis, mp3, aac, he-aac); flac is
  lossless", where it read "bitrate/q apply to lossy output (opus, mp3, aac,
  he-aac); flac is lossless". Both stay `415`, and a container the format has no
  form in is still refused first, `400`.
- A stored job is held again, when it runs, to every rule creation applies to
  its type from the request alone, in creation's words and order: which fields
  its type takes (`srcs` and `titles` for a merge, `cuts` and `skip` for a
  split, the silence fields for an analyze job, `gain` and `loudness` for a
  transcode), `src` or `srcs` and the member bound, the format, the shape's wire
  policy, a knob its format does not take or out of range, `loudness` and its
  exclusion with `gain`, `gain`'s spelling, the silence fields' need for
  `silence: true` and their bounds, and a merge's title per member. Only the
  `cue` rules stay with creation, since a job carries the cut points its sheet
  resolved to. A stored job that broke one of these ran, or failed later in
  other words, and now fails in creation's: one holding a knob its format does
  not take fails in the job's field name ("flacLevel applies to flac output;
  this request names opus"), where it failed in the engine's ("waxflow:
  FLACLevel applies ..."), and a negative `bitrate` fails "bitrate -1: want 0
  for the format's default or a positive integer", where it failed as "waxflow:
  Bitrate is -1000, ...". A timeline job holding `src`, a shaping field, `gain`,
  `loudness`, `titles`, `cuts`, `skip` or a silence field fails "type timeline
  takes srcs, spans and crossfadeSeconds".
- A job's level out of range is worded in its field names on the engine's
  scale, at creation and when a stored job runs: "flacLevel 9: want -1 to 8"
  where it read "waxflow: FLAC level 9 outside -1..8", "wavpackLevel 7: want
  1 to 4" where it read "waxflow: WavPack level 7 outside 1..4", "apeLevel
  2500: want 1000, 2000, or 3000" where it read "waxflow: APE level 2500 is
  not one of 1000, 2000, or 3000", and "apeLevel 4000 decodes but is not
  written; want 1000, 2000, or 3000" where it read "waxflow: APE level 4000
  decodes but ...". Still `400`, at the same point in the order.
- A query parameter that holds a number, a boolean or an `id` pin refuses an
  empty value, `400` "<name> is present but empty; omit it to ...", where
  each read as absent and answered as if omitted. A named choice (`format`,
  `container`, `gain`, `dynamics`, `q`) still takes empty as its default.
  What moved: `from=`, `to=` and `t=` on `/stream`, `POST /transcode` and a
  `/stream` `POST /sign`, and `from=` and `to=` on the HLS mint (both
  forms), each refused where its value is parsed, so before the source
  resolves and, on the mint, before the query's format and knob refusals:
  `src=lib/missing.wav&from=` answers `400` where it answered `404`, and
  `format=nosuch&to=` on the mint `400` where it answered `415`. An empty
  `from`, `to` or `t` also comes before `/stream`'s and `POST /transcode`'s
  format and knob refusals: `src=lib/sine.wav&format=flac&bitrate=128&from=`
  answers `400` where it answered `415`. `id=` on `/stream`, `POST
  /transcode`, `/art` and `/lyrics` is refused where a malformed `id` is,
  after the source resolves, so a missing source still answers `404`, and
  ahead of every refusal the plan makes once it has: `format=nosuch&id=` and
  `format=flac&bitrate=128&id=` on a source that resolves answer `400` where
  they answered `415`. `POST /sign` still replaces a sign request's own `id`,
  empty or not.
- The HLS mint refuses an empty `tl`, "tl is present but empty; omit it to
  stream one src", as `/stream` does, on both forms and ahead of every other
  rule but the unknown-parameter one. `src=lib/sine.wav&tl=&format=opus`
  answered `200` with the `tl` dropped and now answers `400`; `tl=` with no
  `src` answers it where it answered "src or tl is required"; and
  `src=lib/missing.wav&tl=` answers `400` where it answered `404`.
- `GET /probe` reads `strict` as `1`, `true` or `yes` and `0`, `false` or
  `no`, in any case, and refuses anything else, `400` `strict "maybe": want
  a boolean`, and an empty one "strict is present but empty; omit it for a
  tolerant probe", where every other value, empty included, read as false.
  The refusal comes after `src`'s spelling and before `src is required` and
  the source's resolution: `strict=maybe` with no `src` answers the
  `strict` refusal where it answered `src is required`, and beside a missing
  source `400` where it answered `404`.

On the CLI:

- `waxflow transcode` takes `--bitrate` (kbit/s) and `--vbr` in place of
  `--mp3-bitrate`, `--opus-bitrate`, `--aac-bitrate`, `--mp3-vbr` and
  `--opus-vbr`, which are removed: a script that passes one now fails with
  "unknown flag", exit 2. Each new flag reaches `Bitrate` or `VBR` only
  when given, so a bare command writes the bytes it did, and the bound,
  exit 2 before the source opens, is the one the old flags had.
- A knob flag the format does not take is worded by one rule for all of
  them, exit 5. `--bitrate` or `--vbr` on a lossless format reads as the
  server's refusal does ("--bitrate applies to lossy output (opus, mp3, aac,
  he-aac); flac is lossless"), and `--bitrate` on vorbis names the flag that
  applies ("--bitrate does not apply to vorbis, which is quality-driven; use
  --vorbis-quality").
- `waxflow transcode` and `waxflow split` refuse a knob flag the format
  does not take whenever it is given, whatever its value, exit 5, at the
  point in the order where a nonzero value was refused. A value that means
  the encoder's default (`--bitrate 0`, `--vbr=false`, `--he-v2=false`,
  `--wavpack-level 0`, `--ape-level 0`, `--opus-signal auto`) was dropped
  in silence there, and the command went on to open the source. On a
  format that takes the knob it still means the encoder's default.
- `waxflow transcode` gains `--vorbis-quality`, a float on libvorbis's -1 to
  10 scale that reaches `VorbisQuality` only when given. An explicit 0 is
  refused, exit 2, with a value off the scale, NaN or infinite: the encoder
  reads 0 as its default, 3, so a typed 0 would have encoded at 3.
- `--flac-level`, `--opus-complexity` and `--opus-signal` show no default of
  their own in the help (they showed 5, 5 and auto) and name the encoder's,
  as the other level flags did, and `--flac-level` likewise on `waxflow
  split`. Each already reached the options only when given, so no output
  changes.
- The flags that leave their range to the engine name themselves in the
  refusal, exit 2 as before: `--wavpack-level 9: want 1 to 4` where it read
  "waxflow: WavPack level 9 outside 1..4", `--ape-level 2500: want 1000,
  2000, or 3000` and `--ape-level 4000 decodes but is not written; want
  1000, 2000, or 3000` where each read "waxflow: APE level ...", on
  `transcode` and `split`, and `--opus-signal "foo": want auto, voice, or
  music` where it read `opus signal hint "foo" is not auto, voice, or
  music`.

## Amendment (2026-11-22): one file pipeline

A transcode job and `waxflow transcode` write a file through one pipeline,
`internal/tofile`: metadata fold, loudness pass, tags, encode, ReplayGain,
MP4 patch, sync, close and the tagging post-pass. It is not public API.
What a job reports changes as follows:

- A transcode job's `warnings` come in the order things happen: the input
  damage each read finds, a damaged read-back and a failed post-pass, then
  the clipping and loss notes read from the finished result. They came
  clipping, input damage, loss, read-back, post-pass. Each line reads as it
  did.
- A daemon shutdown that lands in a transcode job's post-pass leaves the
  job running on disk, so the next boot reruns it from zero, post-pass
  included. The job used to finish `done` with an output the post-pass
  never tagged, and a mapper failing on the shutdown no longer adds a
  "metadata post-pass" warning.
- A failed sync or close of a job's output reads "output sync: ..." and
  "output close: ..." in the job's error, where it read "jobs: output
  sync: ..." and "jobs: output close: ...", for transcode, merge and split
  jobs.
- A split job carries its metadata read's warnings, as a transcode job
  does: "metadata unread: ..." for a source whose tags cannot be read, and
  the tag library's own. A split job carried none.

`waxflow transcode` runs on the same pipeline, and changes as follows. The
daemon's shutdown does not change.

- Ctrl-C or SIGTERM cancels the command. It removes what it was writing,
  leaves a `--force` target as it was, and exits 6 (canceled). A second
  signal kills the process. The first signal used to kill it, leaving a
  partial file at the final path, which a rerun then refused as "output
  exists", or a temp beside it under `--force`. Every other command but
  `server` is canceled the same way, where the signal killed it: `ping`,
  `probe` and `sign` exit 6, and `doctor` stops its self-bench, prints no
  report and exits 6.
- Every output of `waxflow transcode` and `waxflow split` is written to a
  temp beside its target and published only once finished, as a `--force`
  one was: a command killed outright (a second signal, SIGKILL, a crash)
  leaves a temp beside the file it was writing, never a partial file. A
  killed transcode's rerun is not refused. A killed split's pieces finished
  before the kill stand, so its rerun needs `--force`. A file another
  writer puts at the target meanwhile is kept, and the command refuses
  "output exists (use --force to overwrite)", exit 2, where the two wrote
  over each other. Both refusals end "stage <path>: file exists"; the
  up-front one ended "open <path>: file exists". A failed publish reads
  "publishing output: ...", where it read "replacing output: ..." and, for
  a split piece, "renaming output into place: ...".
- `waxflow split` without `--force` checks every piece's path before it
  writes any, and refuses the first that exists ("02.flac: output exists
  (use --force to overwrite)", exit 2) with nothing written. It refused
  that piece only on reaching it, after writing the pieces ahead of it.
- A cancel that lands in the tagging post-pass is a cancel: exit 6 and no
  output.
- The output is synced before it is reported written, and a failed sync
  reads "output sync: ...". A failed close reads "output close: ..." where
  it read "closing output: ...".
- The input damage the loudness pass finds is printed, which it was not.
  Each finding's "input damage:" line comes as it is found, ahead of the
  clipping note and the loss line; they came after the clipping note.
- A fragmented MP4's loudness line reads "loudness: output ~%.2f LUFS
  (estimated), ..." where it read "loudness: output %.2f LUFS, ...": the
  value is projected from the source, never read back.

Split jobs tag their pieces as `waxflow split` does: both read the source's
metadata once through one call and tag each piece through another, so one
sheet over one rip gives the same tags from either. `waxflow split` writes
each piece through the transcode's tail: a piece is synced before it is
reported, and a failed close reads "output close: ..." where it read
"closing output: ...". A sheet title too large to embed is refused before
any piece is written, exit 2, "cue: track 2: a 60000-byte title is too
large to embed in a piece's tags", where the pieces before it were written
and its own failed, exit 5. A cancel in a piece's post-pass ("split
canceled") exits 6 without publishing that piece.

Every tag set a file writer hands a muxer is fitted to that muxer's own
block, by one function both front ends share. The writer's own tags (a
split piece's TRACKNUMBER, TRACKTOTAL and TITLE, or the ReplayGain a
loudness run embeds) are never dropped, and a piece title too large to embed
on its own is refused before anything is written. The source's tags go whole
when the set fits, so an output written before is as it was; in a set the
muxer would refuse, the source's tags are walked in order, each kept if it
still fits. A tag left out is reported only where it is lost: the output's
muxer is the final tag writer (WavPack, APE, the MP4 family, FLAC in Ogg),
or no tag library runs the post-pass. Elsewhere the post-pass writes the
full set and nothing is reported. `waxflow split` prints "tag too large for
the output, not embedded", `waxflow transcode` "metadata: tag KEY is too
large for the output and was not embedded", and a job warns "tag KEY is too
large for the output and was not embedded"; a transcode or a split reports
each key once.

On a source whose tags sit just under the 48 KiB budget, that moves:

- `waxflow split` with a long piece title, which stopped at that piece
  ("TITLE does not fit ...", exit 5), writes every piece: to flac, opus or
  vorbis the post-pass tags each in full and nothing is printed, and to
  WavPack, APE or FLAC in Ogg the tag the block cannot hold beside the title
  is dropped, with the line. With short titles WavPack and APE drop the
  rip's COMMENT, which their block cannot hold beside a piece's own tags,
  where the split stopped at its first piece ("TRACKTOTAL does not fit ...",
  exit 5); every other output is as it was.
- `waxflow transcode --loudness analyze` and a `loudness: analyze` job to
  WavPack, APE or FLAC in Ogg write the output with its ReplayGain and drop
  the source tag the block cannot hold beside it, with the line or warning,
  where they failed ("REPLAYGAIN_TRACK_GAIN does not fit ...", exit 5).
- `waxflow transcode`, `waxflow split` and a transcode job no longer report
  a tag the post-pass writes as lost, for any output but WavPack, APE, the
  MP4 family and FLAC in Ogg, where the line or warning said otherwise.

The source's tags are fitted to the output's own block alone, where they
were first trimmed to a worst-case estimate of the 48 KiB every embedding
muxer holds. So an output whose block holds a tag keeps it, and the line or
warning moves with it:

- `waxflow transcode`, `waxflow split` and transcode and split jobs to the
  MP4 family keep a source tag past 48 KiB (an .m4a's 60 KB COMMENT), where
  they dropped it with the line or warning. WavPack, APE and FLAC in Ogg
  still drop it with the line or warning.
- A set between the estimate and the writer's real cap is embedded where it
  was dropped. FLAC in Ogg keeps a lone 49100-byte COMMENT with no line or
  warning; WavPack and APE gain a few bytes. The native FLAC, Opus and
  Vorbis muxers embed it themselves where only the post-pass wrote it, so
  their tagged files read as before.
- A split's piece title is held to the pieces' own writer: a 60000-byte
  title is written to the MP4 family, and accepted for a form whose muxer
  writes no tags (WAV, AIFF, ADTS, Matroska), where the post-pass writes
  it. Both were refused: `waxflow split` exited 2, a split job answered
  `400` and a stored one failed. Every other output refuses it as before.

What is reported moves too:

- A key with several values left out is reported once, where `waxflow
  transcode` printed a line and a transcode job warned once per value.
- With no tag library, a tag is no longer reported too large for an output
  whose muxer writes no tags or does not map its key (MP3's ID3v2 for
  COMMENT). Size was never why it was left out there: it was reported only
  when it was also over the old estimate.

A split's TRACKTOTAL moves on a sheet numbered past its count:

- TRACKTOTAL is never less than any audio track's number: `waxflow split`
  and a split job on a sheet whose audio is TRACK 02 and 03 with a data
  TRACK 04 after it write TRACKTOTAL 3 where they wrote 2, on a second
  disc's sheet numbered from 13 they write 14, and on one whose TRACK 05
  comes before TRACK 02 they write 5.

For a Go caller:

- `cue.(*Sheet).TrackTotal` is the disc's track count a split's TRACKTOTAL
  takes: every track in any of the sheet's files up to the last audio one,
  and never less than any audio track's number.
  `cue.(*File).Label(p)` is the number and title a split gives piece `p`:
  its track's, or 0 and none for a lead-in.
- `waxflow.TagsFit(format, container, tags)` says whether the muxer of that
  output form would take a tag set: nil when it would, and the muxer's own
  refusal when its block cannot hold it. Every form answers, each from the
  builder its muxer writes with: `ogg.OpusTagsFit`, `ogg.VorbisTagsFit` and
  `ogg.FLACTagsFit` for the Ogg comment header of each codec,
  `flacn.TagsFit` for the VORBIS_COMMENT block, `mpa.TagsFit` for the ID3v2
  tag (a key it does not map takes no room), `wv.TagsFit` and
  `apen.TagsFit` for the APEv2 block, and `mp4.TagsFit` and `mka.TagsFit`,
  nil always: an ilst has no cap, and the Matroska muxer writes no tags. A
  form whose muxer takes none (WAV, AIFF, ADTS) answers nil.
- `internal/posixfs`, which stages and publishes every CLI output, is
  internal: its `Stage` without replace now writes a temp and publishes it
  only where nothing is, which moves no API.
- `client.JobRequest` gains `Pieces` (`[]client.JobPiece`, a `Number` and a
  `Title`) and `TrackTotal`, both echo-only. `Titles` is a split's per-piece
  titles as well as a merge's per-member ones.

On the wire:

- A split's pieces carry the source's tags without its ReplayGain, chapters
  and synced lyrics, and its front cover, with their own `TRACKNUMBER`,
  `TRACKTOTAL` and `TITLE` on top. They carried no tags before.
- A split takes `titles`, one per piece its `cuts` open or none, else a 400
  in the merge rule's words ("titles has 1 entries but the split has 2
  pieces; a title per piece, or none"). With `cue`, `titles` is a 400:
  "titles are exclusive with cue: the sheet names a split's pieces".
  `titles` on a transcode or analyze now reads "titles applies to merge and
  split jobs" where it read "titles applies to merge jobs".
- A split job echoes `pieces` (`[{"number", "title"}]`, index-aligned with
  the pieces) and `trackTotal`: from `titles`, numbered from 1 and totalled
  by the piece count, or from the sheet. A body that sends either is a 400,
  as for the other echo-only fields. A stored split with no `pieces` numbers
  its pieces by place from 1.
- A split whose piece title cannot be embedded beside the piece's number and
  total is a 400 at creation: "titles[1]: a 60000-byte title is too large
  to embed in a piece's tags", and "cue: track 2: ..." for a sheet's. A
  stored split is held to the same rule with the field it holds, "pieces[1]:
  ...", and its count refusal reads "pieces has 2 entries ..." where a body's
  reads "titles has ...". A stored split carrying member titles reads "a
  split's titles name its pieces, so a split job carries them as pieces".
- A split piece's tags follow the tag rule above: a tag left out is a "tag
  KEY is too large for the output and was not embedded" warning only where
  it is lost, once per split. A piece the post-pass could not tag is a
  warning naming it, "metadata post-pass of out.1.flac: ...".
