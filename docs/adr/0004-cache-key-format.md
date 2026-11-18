# ADR-0004: Cache key format

Status: Accepted (2026-07-02)

## Context

The transcode cache is content-addressed: the same request must find the
same entry across restarts, and any change that alters output audio must
miss. A stale hit serves wrong audio silently, the worst failure mode a
transcoder has.

## Decision

    key = SHA-256(
        cacheSchemaVersion || sourceIdentity || canonicalOutputParams || nodeVersions
    )

- `cacheSchemaVersion`: bumps invalidate everything (layout changes).
- `sourceIdentity`: `ref + size + mtimeNS`, identical to the identity
  inside signed URLs (ADR-0003). In resolver mode the ref is the
  `pid:<ULID>` reference itself, so the PID keys entries with no extra
  field; the catalog sequence is deliberately excluded (amended with resolver mode,
  see ADR-0003): a rename changes no bytes, so it must not orphan cache
  entries, while replaced content misses because size+mtimeNS come from
  the file the PID currently resolves to.
- `canonicalOutputParams`: every parameter that shapes output bytes
  (format, bitrate/quality, bits, rate, channels, gain mode, segment
  duration for HLS) serialized in one canonical order.
- `nodeVersions`: the `Version()` constant of **every sample-affecting node
  in the chain**: the source codec's decoder (amended when the first
  decoder revision shipped and found decode versions defined per codec
  but never wired into the key), each encoder (bitstream/algorithm
  revision, psy-model revision, deterministic-mode flag) *and* each DSP
  node (resampler, dither, limiter, mix matrices). A resampler fix that
  changes output samples must never serve stale audio; conversely,
  improving the Opus encoder invalidates only entries whose *output* is
  Opus, and revising the Opus decoder only entries whose *source* is.
  Demuxer changes that alter emitted samples (trim fixes) remain covered
  only by a schema bump; they are rarer than codec revisions and the
  standing PR question below applies.

Layout (fixed alongside the key): `cacheDir/v1/<aa>/<hash>/meta.json` plus
`out.<ext>` (progressive) or `init.mp4 seg-*.m4s media.m3u8` (HLS variant).
Writes go to `*.tmp` with atomic rename; only completed progressive outputs
promote; HLS segments are individually complete and cache incrementally.
Probe results and frame indexes live under `cacheDir/idx/`, keyed by the
same source identity.

## Consequences

- Every encoder and DSP node carries a `Version()` from birth; forgetting to
  bump it on a sample-affecting change is the residual risk, so PR review
  treats "does this change output samples?" as a standing question, and the
  golden-stream tests catch unbumped changes by failing byte-comparison.
- Cache entries are never shared across `cacheSchemaVersion` bumps; no
  migration code, ever.
- **Known gap: there is no container or muxer term.** `nodeVersions` covers
  decoder, DSP nodes and encoder, so a change to how a muxer frames the same
  encoded packets alters output bytes with nothing in the key to notice, and
  every cached response keeps serving the old bytes until eviction. This is
  the demuxer gap named above, seen from the write side, and it is the same
  standing PR question.

  Found by the Ogg-Vorbis granulepos fix (2026-07-29), which changed only
  `container/ogg/muxmap.go`. The workaround was to borrow the nearest
  encoder term, `vorbis.EncoderVersion`, with a comment at the constant
  saying so. It is the narrowest lever available, not a precise one, and it
  errs in both directions:

  - It **over-invalidates**: Vorbis-in-Matroska shares the encoder term, so
    those entries drop even though the mka muxer did not change and their
    bytes are identical. The cost is one re-encode per entry, no
    correctness risk.
  - It **under-covers** in general: a change to a muxer that carries several
    codecs has no single encoder term to borrow, and nothing narrower than a
    schema bump would work.

  The fix is to add a container term to the tuple. It is deliberately not
  done yet, because adding an element to the joined version string
  invalidates *everything* on first deploy, which is the same practical cost
  as a `cacheSchemaVersion` bump; the value is precision for every muxer
  change after this one. Land it with the next schema bump rather than
  spending a full invalidation on its own.

## Amendment (2026-09-10): the container term, added

The known gap above is closed. `nodeVersions` gains a fourth element, so
the tuple is `[decoder, dsp..., encoder, muxer]`:

- Every progressive muxing container package carries a `MuxerVersion`
  constant, spelled `<name>-mux-<n>`: `adts`, `aiff`, `apen`, `flacn`,
  `mka`, `mp4`, `mpa`, `ogg`, `riff`, `wv`. All start at `-1` except
  `mpa`, which ships at `mpa-mux-2` because the same change that added
  the term also fixed what that muxer writes.
  `waxflow.muxerVersion` maps a resolved container name onto its
  package's constant, keyed on the same name `output.mux` branches on so
  the term cannot name a muxer other than the one that ran.
- A remux plan carries `[RemuxVersion, muxer]`, and a progressive cut
  appends `CutVersion` to that. `RemuxVersion` keeps only what it always
  described best, the gapless trailer this rung synthesizes; a muxer
  change bumps the muxer's own term instead of borrowing it.
- Segmented plans are unchanged: their muxer *is* the segmenter, and
  `mp4.SegmenterVersion` already keys it. They do inherit the progressive
  term through the transcode plan they embed, and the term they inherit
  is the one their FORMAT's progressive container resolves to, not
  mp4's: an HLS FLAC plan carries `flacn.MuxerVersion` and an HLS Opus
  plan `ogg.MuxerVersion`, muxers that never run for a CMAF output,
  while `mp4.MuxerVersion` reaches an HLS key only for the aac, he-aac
  and alac rows, whose progressive container is MP4 too. So the
  inherited term over-invalidates for every segmented format and covers
  nothing that `mp4.SegmenterVersion` does not already cover. That is
  accepted rather than trimmed: the event is rare, the cost is one
  re-encode per entry, and stripping the last element of an inherited
  slice is a second version-assembly path that could disagree with the
  first. It is imprecise, not unsafe: the segmented form has no
  under-covered muxer.
- One `mp4.MuxerVersion` covers both `NewMuxer` (fragmented) and
  `NewProgressiveMuxer` (moov-first), because they share the sample
  entries and movie boxes a change here would be about; a revision to
  either bumps it.

`cacheSchemaVersion` is *not* bumped. The layout does not change and the
key already moves, so every cached progressive and HLS entry misses once
after deploy and regenerates on demand; the size-bounded GC ages the
orphans out. That one-time full invalidation is the cost the gap
paragraph above was waiting to spend, and it is spent here rather than
saved for a schema bump that may not come: the sample-entry fix
(2026-09-06) had already shipped bytes that cached progressive ALAC
transcodes were not regenerating.

The residual risk is the same standing PR question, moved: forgetting to
bump a `MuxerVersion` on a framing change.
`TestMuxerVersionNamesTheMuxerThatRuns` cannot see that; it checks which
term is used, not whether it was bumped.

What catches an unbumped change is a golden byte comparison, and all ten
packages now have one (see the `goldens` target). Within a case, the
golden constrains every byte: a paging retune that moves a page boundary
changes bytes and so has to bump `MuxerVersion` anyway, and the golden
failing is the reminder. Regeneration is `make goldens` plus a reviewed
diff, and the failure message names the version bump as well as the
command.

Coverage is per case, not per muxer, so the cases are chosen to sit on
the boundaries a retune would move: `ogg`'s Opus case packs packets that
land a page exactly on the byte target and one byte past it, so the
target is caught moving in either direction, and reaches the segment-table
and granule caps on their own exact boundaries. Where one version term
covers two writers it takes two cases: `mp4.MuxerVersion` covers the
fragmented muxer and `NewProgressiveMuxer`, which share little beyond the
sample entries, so each has its own golden. What no golden reaches is
still real: the optional tag, chapter and cover-art boxes several muxers
write are exercised by their own round-trip tests rather than pinned byte
for byte, so a change confined to those would reach review with nothing
failing.

The goldens are compared on every architecture CI runs: amd64 under the
race pass, arm64 on the macOS leg, and 386 through `make test-386`. That
is the evidence behind them rather than a determinism argument, and the
distinction matters, because `docs/quality-gates.md` records that
byte-exactness is a within-a-build property and that a float encoder can
legitimately differ in the last ulp across architectures. Two encoders
are known to, and both are pinned to one architecture and skipped
elsewhere (`codec/vorbis.goldenEncodeArch`, `codec/wma.wantDigestArch`).
An encoder pinned that way must not feed a committed golden, which is why
the `ogg` Vorbis case demuxes a committed libvorbis stream for its
CodecConfig and packets and re-muxes those: it exercises the whole
mapping without running our own encoder. The Opus case is canned packets;
the FLAC, AAC, ALAC, WavPack and Monkey's Audio encoders behind the rest
were already pinned this way before these goldens were added, and have
held. If one ever stops holding, CI says so on the leg that disagrees,
which is the point of comparing there.

The one output too large to commit is `apen`'s unknown-length layout,
whose seek-table reservation is 233 KB of zeros at this frame length. Its
golden is the SHA-256 of the stream, kept in a file like any other so
`make goldens` regenerates it.

## Amendment (2026-10-31): a demuxer landing change borrows its decoder's term

`container/asf` now lands a WMA v1/v2 seek by counting the frames each media
object's superframe header says it completes, rather than on the millisecond
presentation time ASF states for the object, which sat up to two frames under
the sample a decode resumed there first produces. That changes the samples
every such seek delivers (an HLS worker restart, `/stream?t=`, a split piece),
and the same change lets `codec/wma` resume on a packet that begins a frame in
its last byte, which it used to refuse. The Decision above leaves a demuxer
change like this to a schema bump. It borrows the nearest term instead, as
the Ogg granulepos fix borrowed `vorbis.EncoderVersion`: `wma.Version` moved
from `wma-dec-2` to `wma-dec-3`, with a comment at the constant saying why.

It errs the way that one did. It over-invalidates: every cached transcode of
a WMA v1/v2 source misses once, seeked or not, though only a seeked one
changed, at a cost of one re-encode per entry. WMA Pro, Voice and Lossless
sources keep their keys, since their landings did not change. A demuxer term
in the tuple would be precise and would cost one full invalidation plus a
constant in every demuxing package; it remains the standing PR question.

The frame count is also an index worth persisting, and it is saved through
the same `cacheDir/idx/` sidecar as the MP3 frame table. Its blob carries its
own magic and version (`WXASFIDX`, 3), so a change to that format needs no
key term: a blob of another version is refused and the count is taken from
the data again. It carries what the count found without losing an object
(stamps that run backwards), so a restore reports what a walk does, and a
count that lost one is not saved at all. A restore checks a blob against
the file before using it: a CRC-32 for damage, the packet geometry, and a
sample of its steps against the packets they name. A blob built to pass
those checks is trusted where they do not look, as the transcodes beside it
in the cache are.

## Amendment (2026-11-01): reader changes borrow their decoders' terms

The FLAC and Ogg readers now deliver different samples for some inputs:
`container/flacn` resyncs past a corrupt frame and fills it with silence
where a read used to stop there; Ogg FLAC takes its length and seeks from its
frame headers; both read a stream whose first frame is past 0 from that
frame; and `container/ogg` honours an initial granule, so a shifted Opus or
Vorbis stream changes length and a Vorbis start before zero trims its front.
As the 2026-10-31 amendment did for WMA, each change borrows the decoder term
of the codec it changes: `flac-dec-2`, `opus-dec-3`, `vorbis-dec-3`. It
over-invalidates the same way, one re-encode per cached entry of those codecs,
and the demuxer term remains the standing question.


## Amendment (2026-11-13): a demuxer names its own revision

`container/mka` now reads a track that leaves `FlagDefault` out, or writes
it with no octets, as a default track, which is what Matroska defines, and
passes over a track whose `FlagEnabled` is 0 while a usable one exists. A
file of several audio tracks can select another one, and every plan built
from that file then holds other samples. The Decision leaves a demuxer
change to a schema bump, and the two amendments above borrowed a decoder's
term instead. Matroska carries every codec, so it has no decoder term to
borrow: the case the muxer side met on 2026-07-29 and closed on 2026-09-10.

The demuxer side is closed the same way, without the full invalidation.
`container.Track` carries `DemuxerVersion`, spelled `<name>-demux-<n>`, and
the engine puts it ahead of the versions of every plan built from the
track, on the copy rungs as on the transcode one (`withDemuxer`); a
timeline lists each member's once (`timelineVersions`). The tuple is
`[demuxer, decoder, dsp..., encoder, muxer]`, its first element present
only where a demuxer names one. `mka.DemuxerVersion` is `mka-demux-2`;
revision 1 is every key built before the term existed.

A demuxer that has not revised since names nothing, so its sources keep
their keys: only cached entries of Matroska and WebM sources, and of
timelines with such a member, miss once, at one re-encode or one copy per
entry. That is what the standing question was
waiting on. It priced the term at a constant in every demuxing package and
one full invalidation, and an empty term standing for revision 1 costs
neither. From here a change to what a demuxer delivers for a file that has
not changed bumps that package's own constant in the same change, adding it
at `-2` where there is none yet. The terms already borrowed (`wma-dec-3`,
`flac-dec-2`, `opus-dec-3`, `vorbis-dec-3`) stay: moving them back would
retire those entries a second time for nothing.

The engine reads the term from the track, so a caller that builds a
`container.Track` by hand names no demuxer and keys as before. The shared
fuzz driver holds every demuxer's track to the spelling, and the server's
cache-key golden shows the term on each rung of a Matroska source.

## Amendment (2026-11-13): a transcode names the build that rounds its floats

Byte-exactness holds within a build and not across them
(docs/quality-gates.md): the compiler fuses a multiply into an add on arm64
and on amd64 built at `GOAMD64=v3`, and the math package's
per-architecture routines need not agree in the last ulp, either of which
can move a lossy encoder to other codewords. The key carried no `GOARCH`
and no `GOAMD64` level, so a cache directory that outlived a change of
either held entries the new build would encode to other bytes, under the
same key and the same strong ETag. A cached output stayed whole. What could
mix was a partly cached HLS variant, whose remaining segments came from the
new build, and a ranged resume of a progressive entry evicted and encoded
again in between, where the byte offsets of two encodes do not line up.

Every plan that runs a chain now carries `waxflow.FloatBuildVersion`
behind its decoder's term: `fp-<GOARCH>`, or `fp-amd64v3` and `fp-amd64v4`
on amd64. It is empty for amd64 below v3, the one target where nothing
fuses, so no key moves there. Every other build's transcode entries miss
once, arm64's among them. The term is opt-in for the reason the demuxer's
is: an empty one standing for the baseline costs no invalidation where
nothing changed. The copy rungs compute nothing and name no build. It
over-keys a lossless transcode of an integer source, whose bytes are the
same everywhere, at one re-encode per entry.

What it does not cover is a toolchain that changes where it fuses under an
unchanged architecture: the term names no Go version, since that would
retire a fusing build's cache on every toolchain bump for an event that
has not been seen. Explicit conversions in the kernels would forbid fusing
and make the bytes one thing everywhere, at a cost in speed and a revision
of every float codec; that stays open. The byte goldens are where a move
would show.

## Amendment (2026-11-15): WavPack names its demuxer's revision

A WavPack decoder takes its blocks in stream order whatever their indexes
say. `container/wv` now states other things about a damaged file than it
did, and every plan built from such a file holds other samples:

- The length is that of the last block of the stream's own shape, and it is
  exact only where the open checked the indexes it rests on: the header's
  total and the tail agree, or the block in front of the tail carries on to
  it, and with no total the block behind the first carries on from it too. A
  stream with no total whose ends are not borne out now opens with no
  length (-1), where it had the tail's, exact. A tail past what the bytes
  could hold, a first index past the tail, and a block of another shape
  behind the stream's last each leave it not exact, so a decode is not cut
  there. A tail short of the total and past the block in front of it is
  still the cut file it was: warned, counted as truncated, and not exact.
  One rule then holds for every length the open states and does not call
  exact: it is no less than the last audio block's own end, of any shape,
  where every open took it before, wherever the bytes could hold that. A
  read, a seek or a measure may reach that far, so a caller's span to it is
  served as it was. Past a total it is noted; short of one it is warned, and
  only the samples past it count as truncated. An open that states no length
  (-1) is not raised by this rule.
- A skip in the block indexes is a hole of lost samples only in a stream
  that bears every skip out: from the first block to the last no index
  steps back, each gap is one the bytes could hold, the stream does not
  change shape, and the last block ends inside any exact length. One walk
  of the stream's headers says so for every skip: a 32-byte read a block,
  no further than 131,072 blocks or 4 MiB of resynced bytes, and at most
  1,024 gaps kept. The packet behind a hole carries `Lost` and `Discont`,
  and one warning names the samples, merged with a resync's bytes warning
  where there is one. In any other stream, and where the walk has no
  answer, every skip and every step back is read on in stream order with
  one warning and nothing lost, as before the indexes were read.
- A block of another shape whose index carries on from the stream's own
  numbering is the mid-stream format change it was, on a read. One whose
  index does not is a stray block: a read steps over a run of them with
  one warning, as a seek does, and goes on with the stream.
- Each of those is a finding, so Strict refuses it, on a read and on a seek
  whose walk crosses it.
- A read from the first block (or behind a seek whose walk began there and
  found every block at its own index) that ends off the exact length takes
  the claim back. Short of it, the read warns in the open's words, the
  track takes what the read reached, and the rest is counted truncated;
  past it, the overrun is noted. A Media takes the track on its next Walk,
  which for a demuxer with no walk of its own now refreshes the length.
- A length the open did not call exact is measured from the same walk of
  the headers (`container.Measurer`, which `MeasureLength` asks before it
  seeks and every Media wrapper forwards), so the measure decodes nothing
  and a damaged last block does not fail it. In a stream that bears its
  skips out it is where the indexes end, or the tail the open found where
  a seek reaches further. In any other every read is the read it was, in
  stream order, so it is the further of the length every open gave before
  this revision (the last audio block's own end) and where a read from the
  first block leaves the run of blocks at their own indexes: a seek lands
  in that run on the timeline the read keeps, so a span or an HLS plan
  holds every window it held, and one bad last index no longer cuts the
  plan to that block's own end. Past the run a seek lands on a block's own
  index, off the read's timeline, and a window there would end short.
  A change of shape is the measure's finding. The track
  is left as it was: nothing is cut at the measure.
- A copy carries a hole as the packets it copies: a WavPack block states
  its own index, so the copy rung writes the bytes it wrote before, counts
  the samples lost (`LostSamples`), and refuses nothing. That is the rule
  for any codec whose packets state their own positions. FLAC's copy
  still writes silent frames, and a codec with neither still refuses, as
  does a cut with a hole inside a kept window: no cut rung serves WavPack.

A seek lands where it did, by bisection on the blocks' own indexes and a
walk, which reports what it crosses in the read's words. A skip it crosses
walks the headers under the seek's context, which pauses that walk with
`CodeCanceled` where the next seek resumes it.

`wv.DemuxerVersion` is `wv-demux-2`, on every plan over a WavPack source,
so every cached entry of a WavPack source, and of a timeline with one as a
member, misses once. The server's cache-key golden shows the term on a
WavPack source's transcode and HLS lines.

An open reads the head and one tail window when the header's total and the
tail agree. For a stream with no total, or one carved out of a longer one,
it also reads the header behind the first block (32 bytes) and, where the
tail window does not hold the block in front of the tail, reads back for
it: that block's bytes in windows of 128 KiB, 2 MiB at most (measured
against the open before this revision: 131 KiB more for blocks of 100 KiB,
262 KiB for 200 KiB, 655 KiB for 600 KiB). Nothing short of that finds a
block's header from behind it, and a walk from the front costs a read a
block. An exact length says nothing of the blocks between: a file that
holds more than its ends state, such as two streams end to end, is not
told apart at open, and a consumer cuts the decode at the stated length,
as before.

## Amendment (2026-11-15): ASF names its demuxer's revision for WMA v1/v2

ASF states presentation times and no durations, so `container/asf` gives an
object the distance to its successor, and the last object the duration of
the one before it. The only packet of a one-object WMA v1/v2 stream has
neither, and took what the track's length left. That length is the rounded
play duration at the open and the counted frames once a walk, a seek's
count or a read to the end has counted the stream, so one file read two
ways: a Strict open, which always walks, delivered another packet than a
tolerant one, and a walk taken mid-read moved the end.

That packet now lasts what its own frames deliver, walked or not, which is
the length the count settles: its frames (one without the bit reservoir,
the count its superframe header states with it) times the frame length,
less the last frame where the reservoir leaves it incomplete. Where that
is nothing (no frame stated, or the reservoir's one frame), the packet
lasts 0. A stream of several objects reads as it did. So does a WMA Pro,
Voice or Lossless stream, whose length no walk moves.

`asf.DemuxerVersion` is `asf-demux-2` and is carried by a WMA v1/v2 track
alone: a Pro, Voice or Lossless track names none. So every cached entry of
a WMA v1/v2 source, and of a timeline with one as a member, misses once,
and no key of the other three codecs moves. The server's cache-key golden
shows the term on a WMA v2 source's transcode and HLS lines, and none on a
WMA Pro source's line.

## Amendment (2026-11-15): FLAC names its demuxers' revisions

`container/flacn`, and `container/ogg` over a FLAC stream, place a damaged
or oddly headed file's frames otherwise than they did, and every plan built
from such a file holds other samples:

- After damage at the head the first frame read can be the stream's short
  last one, and it used to place itself, the hole in front of it and the
  length in its own size. Where that frame is known to be the last (the
  data, or the Ogg stream, ends behind it with no damage between) and
  STREAMINFO's bounds are equal and ones a stream may state (16 to 65535,
  RFC 9639 section 8.2), its number is now counted in that block size. With
  a frame behind it, or nothing known of what follows, the first frame's
  own size is the block size as before. A length behind a hole at the head
  is held to what the bytes that hole took could hold as well as the
  frames' own, so the larger positions leave it exact.
- A hole from sample 0 in front of the first frame read is held to the
  stream's length: a decode is cut there, and the hole's silence used to
  fill it. A first frame numbered at or past where the closing frame ends
  starts a native FLAC stream late, as one whose head bytes could not hold
  the hole. In Ogg it is joined on with nothing owed, and so is a first
  frame behind damage that is numbered above frames that follow it, which
  used to disown them as another stream's. Such a head is placed at 0, as
  a stream starting late is, with the frames numbered on from it behind
  it, and the stream's own frames follow at their numbers, what lies
  between counted lost. A seek lands on those packets as a read from the
  open comes to them. Where neither the packet behind the head nor the
  tail shows a frame numbered below it, the open reads through the frames
  numbered on from the head, as far as a resync reaches (1 MiB), for the
  one that ends their run.
- A first frame with the frame numbered right in front of it lying behind
  it, nothing damaged in front, used to start the stream late and drop
  that frame as another stream's, natively and in Ogg. The stream now
  starts at the earlier frame and both are read in order with nothing
  lost. Only a stream numbered from past 0 is asked, at the cost of one
  window at the first frame natively and one packet ahead in Ogg.
- Native FLAC: a closing frame numbered before the successor of the whole
  frame in front of it lies inside the frames a read delivers and is out
  of sequence to it. The open states where it ends, notes it, and does not
  call the length exact, so a decode that used to be cut there delivers
  those frames whole. Where the frames in front are the ones numbered
  ahead, a walk from the nearest frame the closing one could follow
  settles whether a read comes to it, however long their run. A closing
  frame numbered on is asked of the same walk, one step from the frame in
  front on a healthy stream, since a read can take that frame's bytes on
  through it. The frame in front is looked for in the tail window the open
  already holds, at the cost of its parse and checksum and no read. A
  healthy stream whose frames are larger than that window pays one read
  more, back to where the largest frame could start; only damage in front
  of the closing frame reads further, as far as a resync reaches, and a
  closing frame with no whole frame in that reach in front of it is not
  called exact either. Where no frame in front of it is whole at all, the
  walk is a read's from the first frame. The parses that look for the
  frame in front may read twice the bytes that search can hold; past that
  the length is called as it was.
- With the blocking strategy bit clear, bounds that state no block size no
  longer make the numbers sample numbers by themselves. The frame behind
  the first settles it: numbered one on, the stream counts frames; numbered
  a block on, samples. Where a native FLAC's first frame does not check,
  the first frame behind it that does, and the header behind that one,
  settle it the same way. With no such frame to read, or one that says
  neither, unequal bounds mean sample numbers as before, whether a stream
  may state them or not. A STREAMINFO with damaged bounds used to turn a
  plain stream's frames 0, 1, 2 into samples 0, 1, 2, of which a read
  delivered one, or none where the first frame was damaged too. A copy
  numbers its frames as it did (`flac.Numberer`).
- Reading that second frame is paid only where it can change the answer:
  bounds that state no block size, or a first frame shorter than the one
  they state. Native FLAC under a seek window reads nothing its tail scan
  does not, and a longer stream one window at the first frame, which is
  also all a damaged first frame's search reads (eight headers that parse
  at most); Ogg reads one packet ahead. A read that fails there is no
  answer and not the open's error.
- Native FLAC: a frame that is whole by its own checksum with more bytes
  behind it than a frame can be long, none of them a successor, ends the
  stream as a junk tail does. The frame is delivered and the rest reported
  as trailing bytes, which Strict refuses; a read used to fail there and
  lose the frame.
- Native FLAC: a seek whose walk begins on a header with no frame behind it
  starts again further back, and what the abandoned walk found is neither
  listed nor counted.
- Behind damage, a frame that checks and is numbered where the stream could
  not have it (past the exact length, or further on than the damaged bytes
  could hold) used to end a native FLAC read there, and in Ogg was paid out
  as silence up to its number. Native FLAC now reads on from the next frame
  the stream could have, the strays in front of it lying in the hole, and
  where there is none joins the strays on in order with nothing lost; Ogg
  holds a hole behind a damaged page to the exact length and joins a frame
  past it on. A native seek into a hole at the stream's head lands where
  the read behind it places what it owes, which in front of frames joined
  on is not sample 0.
- Native FLAC: whole frames behind a frame that are numbered before its
  successor check together with it, since a frame's own checksum leaves
  zero, and used to ride in its packet, which a decoder ignored and a copy
  refused. The frame now ends where they begin, and they are dropped and
  reported as frames out of sequence.
- Native FLAC: numbers that skip, with the next frame starting where the
  last one ends, used to be taken at once: as samples lost where the stream
  could have the frame, and joined on where not. A frame numbered inside
  the gap that follows within a resync's reach (1 MiB) is now read on from
  instead, whether it lies right behind the frames numbered ahead or behind
  damage. The whole frames of that run are held and read in order (a
  swapped pair reads as the pair), or, where the run fills the gap in front
  of that frame exactly and is not numbered right behind it, read at the
  gap, as frames misnumbered. A run numbered past the stream that is
  shorter than its gap is read at the gap's head, the rest counted lost;
  longer, it is joined at its own numbers, as it was. A held run is read
  behind the first frame that no frame numbered before the run follows: the
  frame in front of its numbers, or one in front of a frame gone, of
  damage, of another skip or of the stream's end. Every other end of the
  wait takes the skip as before, so the frame the skip lands on and the
  whole frames numbered on from it are read whatever follows them: a
  second run while one is held, a run in front of a frame numbered past
  it, and a wait that finds no frame numbered in the gap. What follows a
  run is found as after any frame. Such a skip costs a scan of the bytes it
  waits over, a run held past a resync's reach is read again from the
  source when it is due, and a seek whose walk meets a skip behind its
  first frame, takes a run aside there, or meets frames out of sequence
  starts one block further back, where the frames due in it may lie.
  Behind frames out of sequence the read always waited for the successor,
  so there a frame numbered ahead ends nothing by itself: the successor is
  read on from, the run held where the stream could have it and dropped
  with those frames where not, as is a frame behind damage; only with no
  successor to the scan's end does the first frame numbered in the gap, or
  the skip, settle it. A frame behind damage that a read of the run would
  not come to, numbered inside the run or before where it ends (frames
  numbered by sample can lie over one another), is the hole the read
  always took.
- A seek whose walk from further back comes to frames joined on in front
  of its target, with nothing lost between, lands by the walk it began
  with, natively and in Ogg: no read from there comes to the target, and
  a seek by the frames' own numbers always did. So does a native seek
  whose first walk began on the frame that holds the target where the
  walk from further back lands on another, and an Ogg seek where that
  walk meets the same hole behind the same frame. Past the end, a native
  seek whose walk from further back ends in front of the frame the first
  began on lands by whichever ends later, so the measure stays at the
  furthest end a seek reaches. The walk the native open
  measures a damaged tail by starts further back by the same rules, and
  counts where it comes past the frame it first began on. So a seek past
  the end, which is the native measure, comes to where a read places the
  frames it can come to. An Ogg seek into the hole the stream starts with
  keeps the frame behind its landing that is numbered before it, as a
  read from the open does.
- Ogg: where the open did not call a FLAC length exact, or a read took the
  claim back, the measure is what a read from the first frame delivers,
  counted by reading the packets (`container.Measurer`), once for an
  open. A seek past the end lands by the last frames' numbers, which a
  read through frames joined on or out of order does not come to.
- Ogg: two FLAC frames out of order, a frame numbered ahead with the frame
  due in the next packet, damage in front of them or not, used to be read
  as two holes whose silence a decode cut at the exact length paid for with
  the stream's last frames. They are now read in order with nothing lost.
  Behind a seek that lands on the later one the earlier is dropped, and a
  seek walk that finds a hole behind its first frame starts further back,
  where the frame due in it may lie. The same holds behind a hole, at the
  head or in the stream: a frame with the one due right in front of it in
  the next packet is read behind that one, which is owed what is left of
  the hole.
- Ogg, every mapping: damage with no page in the 1 MiB a resync scans
  behind it used to fail a read, and a seek whose probe met it. A tolerant
  read now ends there, as every other demuxer's does, with the bytes
  dropped reported, and a seek looks in front of the damage. Strict
  refuses it as before, and an open that finds no audio page in reach
  still refuses the file.
- A read from the first frame, or behind a seek whose walk began there,
  that ends off the exact length says so and takes the claim back. Short
  of it, the read warns (in the open's words where the length is
  STREAMINFO's own), which Strict refuses, the track takes what the read
  reached with its STREAMINFO restated, and the rest is counted truncated;
  past it the read notes it, as the open notes frames past STREAMINFO's
  total, and the length stays. Either side of it the length is no longer
  exact, since a seek lands by a frame's own number and can reach frames a
  read cannot. A hole stays held to the length the open called exact, and
  an Ogg seek to the length the open gave, so a second pass reads and
  lands as the first did. A Media takes the length on its next Walk.

`flacn.DemuxerVersion` is `flacn-demux-2`, on every plan over a native FLAC
source. `ogg.DemuxerVersion` is `ogg-demux-2` and is carried by every Ogg
track: a FLAC one for the rules above, and a Vorbis or an Opus one because
a damaged stream that failed a read at the resync horizon now delivers
the packets in front of it. So every cached entry of a native FLAC or an
Ogg source, and of a timeline with one as a member, misses once. The
server's cache-key golden shows `flacn-demux-2` on the copy, transcode,
timeline and HLS lines of native FLAC sources, and `ogg-demux-2` on the
lines of an Ogg FLAC, an Ogg Opus and an Ogg Vorbis source.

An intact file reads as it did, and at the same cost but for one kind: an
intact old-format stream (unequal block size bounds) pays the numbering
look at open, one more window natively (131,072 bytes) and one or two
pages in Ogg. A native seek with no SEEKTABLE costs less: each bisection
probe is one read of the largest frame's bytes and a header (what the
largest block codes verbatim, or the size STREAMINFO states where that is
less, so an overstated field costs nothing), where it was three to five
reads that doubled from 1 KiB. What an exact length
still does not say is unchanged: the open confirms the closing frame and
the frame in front of it, not the frames between.
