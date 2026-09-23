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
