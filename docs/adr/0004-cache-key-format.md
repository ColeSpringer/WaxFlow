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
