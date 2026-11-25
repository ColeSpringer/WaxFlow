# ADR-0013: The copy rungs

Status: Accepted (2026-11-25)

## Context

A request climbs a ladder, and the first rung that can serve it does. Rung 1
serves the source's own bytes. The copy rungs move the source's packets into
a new container without decoding them: the remux rung keeps every packet,
and the cut rung keeps the packets that fall in a list of sample spans and
synthesizes the trims and codec config around them. Rung 3 decodes and
re-encodes, and can serve anything the engine reads.

A copy saves a generation of loss and most of the CPU, but it can serve only
what a moved packet still means. Each copy rung has to know before the walk
which sources and destinations it can serve, and say so in a form the ladder
falls through on.

## Decision

### A copy rung declines, and errors only where no rung can serve

A copy rung's planner returns (nil, nil) for a request it cannot serve, and
the ladder falls through to the transcode. An error means the request is
wrong for every rung: an invalid span or option, a codec config that does
not parse, a damaged source. Falling through on one of those would fail in
the transcode with the same words, after decoding, under a status that told
the client to ask for another format.

A limit of the rung's own arithmetic is a decline even where it looks like
an error. A priming the cut computed that OpusHead's 16-bit pre-skip cannot
hold is the rung's limit, not the file's, and the transcode serves it.

A decline's reason is not actionable, since the caller's answer to every one
is the same transcode. It is logged at Debug and not returned.

### The remux rung

The remux rung invents no output. A row's forms already name the
destination (format=opus is Ogg Opus progressive and Opus in fMP4
segmented), and the rung only serves one of those forms in the row's own
codec. Opus in WebM to Opus in fMP4 is this rung with nothing added.

A codec survives a container change because `container.Packet.Data` is the
codec's own access unit, never the container's framing: an AAC
raw_data_block or an Opus packet is the same bytes in every container that
carries it, and moving it needs no bitstream filter (`codecSurvives`). PCM
is the exception. Its packet is raw samples whose wire layout (byte order,
the sign of 8-bit samples) is the container's choice and lives in
`CodecConfig`, where a codec ID comparison cannot see it, so two PCM tracks
with equal IDs are not interchangeable. PCM declines at no cost: the rung
exists to avoid generation loss, PCM has none, and the transcode repacks it
bit-exact with the right framing. Serving it here would take a
per-container wire-format table for nothing.

Options are remuxable when they ask for nothing but a container
(`remuxable`). The check compares the whole options value against the
request stripped to its format and container, so an option added later
counts as a transform with no edit there. A hand-kept field list is how one
slips through, and a bit rate slipping through would serve one set of
packets under two cache entries that claim different rates. A seek counts
as a transform, since a copy cannot cut mid-packet. Three more fields stay
in the baseline. `ResampleProfile` and `Shaping` say how a transform runs,
not whether one does, and a request that reaches the rung builds neither a
resampler nor a dither; the server sets a profile on every request, so
zeroing them would leave the rung unreachable from the daemon.
`SpliceTrims` shapes a cut's joins, which the cut reads through the same
check, and a whole-file copy has no join.

A parameter naming what the source already is asks for no transform. A
rate, channel count or integer depth equal to the source's is resolved
away before the comparison: the three `directPlayable` resolves, under the
same conditions, because under them the chain builds no resampler, mix,
dither or widen. Without it a request rung 1 serves as the original bytes
would re-encode once it names a container. A depth on a float source
quantizes, so it stays a transform.

A remux plan states no bit rate and no size. The packets have a rate, but
it is not in the headers, and a plan reads headers; an encoder's projected
rate for packets no encoder made would be a plausible wrong answer. A
caller that needs a rate promised takes a rung that can promise one.

The trailer restates the source's trims through `codec.Trailer`, the way an
encoder hands gapless to a muxer, so each muxer writes its own signalling
(iTunSMPB, an edit list, the Ogg pre-skip) as it does for an encode
(`remuxTrailer`). Against a length the track holds to (exact, or beside a
trim), the end padding is derived from the packets, not copied from the
header. Containers state an end trim two ways: MP4 states the count, while
Ogg Opus puts it in the last granule and reports a zero padding beside a
length already short. Copying that zero into a form that needs the count
(Matroska's DiscardPadding) writes no end trim, and the encoder's tail plays
as audio with no error. Decoded samples less the delay less the kept audio
is the padding by definition, and the packets agree with both conventions.

The trailer settles through `container.SettleLength`, the rule a walk
settles a track's own length with: the length is what the packets hold.
Where the source declares no length, or only an estimate, its padding is
taken at its word and the walk resolves the length, which the result
reports, so a cache entry completes with the length the run measured. A
declared count a truncated source cannot fill shrinks to the packets, so a
remux's edit list is never longer than the samples behind it.

A source that states its trims per packet states no end trim in its
header, so the packets supply it: the trims that run unbroken to the end.
They fill only a padding the track left at zero, never one it states, since
a cut's track carries trims its own arithmetic computed over the source's
packets.

FLAC and ALAC have no priming, so no padding to flush, and every muxer that
writes them refuses a nonzero trim. A copy declines a source that declares
one (`gaplessSurvives`), which would otherwise fail inside Begin
mid-response, and the transcode trims it in PCM. An unprimed track keeps
zero trims in its trailer and takes the run's count, whatever its packets
or the settle say: a container's own inconsistency (an MP4 sample table
that disagrees with its samples, a DiscardPadding a third-party muxer put on
a lossless track, a length that under-reports its packets) would otherwise
become a trim the muxer refuses at End, after the whole file is written.

The rung does not decline a destination with no gapless signalling. A
remux to ADTS drops the source's delay, and a transcode to ADTS drops its
own encoder's priming the same way, so declining would buy a re-encode of
the same defect. ADTS carrying no gapless is the container's documented
property, which is why fMP4 is the AAC row's default.

A segmented remux lays its segment boundaries on the source's packet grid.
The segmenter stamps each segment's decode time as its index times the
segment length, so a short packet mid-stream would put every later
segment's tfdt off its audio with no error. The plan declines a source
whose packets other than the last differ in duration or include one whose
duration is not positive, and the run checks the grid again as it walks,
since a plan made against another file would otherwise reach the
segmenter. The last packet is exempt: an encoder's tail flush is routinely
short and lands in the short final segment. The segment length snaps to a
whole number of packets, as a transcode's snaps to encoder frames, so a
duration that is not a whole number of packets is served, never declined.

A segmented remux primes nothing at a restart. Segment n holds the same
source packets whichever worker writes it, so segment n from a restarted
worker is byte-identical to a continuous run's. Priming a decoder that
starts mid-stream is the player's, as for the segmented cut below.

### Which codecs cut

The remux rung's premise is `codecSurvives`. A cut needs a second: the
packet means the same thing at another position in the stream.
The codecs that fail it do so silently, so the cut keeps an allowlist
(`cutCodecs`), and a codec added later is served by the transcode until it
is weighed here.

- MP3 fails on the bit reservoir. A frame's main data may begin up to 511
  bytes inside earlier frames, and a reference the cut removed decodes to
  silence with no error. MP3 is lossy and passes every other check (the
  codec rule, the trim rule, a uniform grid of 1152), which is why "lossless
  declines" is the wrong rule.
- FLAC fails on frame numbering in a multi-span cut: a gap in the ordinals
  reads as a hole, and the removed span comes back as silence with a
  warning. Renumbering the frames would leave the STREAMINFO MD5 stale. FLAC
  is lossless besides, so the transcode costs no generation, and its encoder
  numbers a mid-stream slice from the slice's first frame.
- Vorbis overlap-adds each packet with its predecessor, and a decoder treats
  the first packet it receives as priming and emits nothing. The packet grid
  declines Vorbis already; it is named here so the rule does not rest on
  that.
- ALAC frames move cleanly, but ALAC is lossless, so a cut saves no
  generation, and the MP4 muxer refuses ALAC trims, so the cut's Delay could
  not be written.
- PCM is out at `codecSurvives` already.

Opus and AAC-LC cut. HE-AAC cuts only from the stream head: file encoders
write the SBR header once, in the first access unit, and a decoder handed no
header conceals the high band to a muted upsample for the rest of the file,
with no error. A cut that keeps the head keeps the header, so its high band
decodes as the source's does, and the transcode serves a mid-stream slice with
the header it read on the way. Since most spans a client asks for would then
re-encode, HE-AAC is left out of the advertised cut formats.

How each span's edges snap to the packet grid is ADR-0011.

### What a destination must carry

The cut synthesizes a front trim and an end trim the source never had. The
copy's trim rule (`gaplessSurvives`) asks only whether the codec admits
trims, so the cut asks the destination too (`cutTrimsExpressible`). A cut
with no delay and no padding passes any destination.

- Ogg Opus states both (the rewritten pre-skip and the final granule), as
  does Matroska (CodecDelay and DiscardPadding), and so does progressive MP4,
  whose muxer writes the edit list at the end.
- Fragmented MP4 states an end trim only as its edit list's length, which a
  player may not read (ffmpeg's does not), and then plays up to a packet of
  the audio the caller cut, where a transcode's tail would be encoder
  padding. A cut with a delay is served all the same, for the generation it
  saves; a cut with an end trim and no delay declines. A segmented cut writes
  fragmented MP4 whatever the row's progressive form, so it takes this rule.
- ADTS declines. A remux to ADTS may drop the source's priming, because a
  transcode to ADTS cannot signal its own encoder's priming either. A cut's
  Delay is different: it covers at least the pre-roll the head backed off by,
  real source audio from before the cut point, so dropping it plays audio
  the caller removed, and only this rung would.
- A container not named declines, so a destination added later is weighed
  before it is served.

### What the cut declines

The cut declines a codec off the allowlist; an HE-AAC span that does not
start at sample 0; a source with no uniform packet grid; a declared Delay or
a grid outside the range its arithmetic computes in (`maxCutSample`); a
source that trims inside its run at places the walk did not record or no
grid places; a span that keeps no whole packet; two spans closer than their
windows can be kept apart; a priming the codec config cannot hold; a cut
whose kept packets carry a trim, to every destination but Matroska
(`PlanRemux`); and a destination that cannot carry the cut's trims.

What the track, options and spans rule out for every grid declines before
the grid is measured: a format or options that need a re-encode, an inner
trim list as above, spans SpliceTrims cannot splice, trims the destination
cannot carry, and a track the destination's muxer would refuse. A source
whose inner trims fit exactly one grid is planned on that grid first, since
every other declines. A codec config that does not parse errors once the
grid is asked; a request ruled out before then declines.

### What a plan promises its run

The run is handed the pair the plan accepted, the plan's grid and the
measured source track, and re-derives none of them, so the bytes it writes
are the ones the plan and its cache key were computed against. The run
repeats the declines it cannot write past, the cut's own (`CutTrack`) and the
copy rule's (`PlanRemux`, with the destination muxer's refusal on the
progressive run), and errors on them without transcoding: a caller that
reached it directly has chosen the rung. Whether the destination states the
cut's trims (`cutTrimsExpressible`) is the plan's question alone, since its
decline is a choice and no write fails, so a caller that skips the plan
skips it.

The measurement carries what a fresh header open cannot know. A source that
declares no length (AAC-LC in ADTS) has one only once measured, and the
fMP4 init segment encodes it. A Matroska source states its trims per block,
so only a walk finds them, and they place the windows. Plan and run read
both through one function (`adoptMeasured`) and agree field for field. The
run takes the source's track, never the plan's: the cut's own track carries
trims the source never stated, and planning from it would make a different
cut. Handed no measurement of a source whose trims are unread, the run walks
it once and cuts from that, and where the walk and an estimate differ, the
file wins.

The walk checks what the plan assumed. A packet straddling a window edge, an
inner trim other than the one the plan placed, or a length that takes in
samples the last packets trim refuses, since a plan made against another
file would otherwise splice mid-packet or state trimmed samples as audio,
with no error.

A span is applied by wrapping a demuxer, never as a TranscodeOptions field.
`remuxable` decides a copy by comparing whole option values, so an option
added later counts as a transform with no edit there, and a span field would
break that.

### The segmented cut

The segmented cut is the segmented remux of the cut's track, with the cut's
view in front of the walk. It declines on the progressive cut's grounds,
asking the init header where the progressive cut asks the destination's
muxer, and fragmented MP4's trim rule where the progressive cut asks its
form's: an Opus cut with an end trim and no delay declines here and is
served to Ogg. A restart adds what the progressive cut never meets: a worker
that starts at segment n seeks the source.

The seek backs off no pre-roll. A mid-stream segment begins exactly on its
grid boundary and the walk drops the packets before it, so segment n from a
restarted worker is byte-identical to a continuous run's; packets sought
back to prime it would repeat the end of the segment before. The init
segment writes no roll signalling, so priming a decoder that starts
mid-stream is the player's concern. The head's pre-roll is in the first
window, which the cut widened for it, so it never reaches a restart's seek.

The seek crosses two timelines. The source demuxer seeks on the timeline it
delivers, which leaves out every trim before the landing, while the cut's
windows and the segment boundaries are raw grid positions. The two agree on
the output only while no kept packet trims, and fMP4 states no trim per
packet in any case, so the segmented cut serves no cut whose kept packets
trim. A landing is a packet start on the delivered timeline, and where a
trim empties a packet that start names two packets, so the segmented cut
declines a source with any such trim, kept or not. A trim starting at raw 0
empties the first packet on any grid it fits, so that source declines before
the grid is measured.

## Consequences

- Every codec the cut moves, and every destination for its trims, is an
  explicit entry. One not listed is served by the transcode.
- A codec whose packets would decode wrong after a move, and ALAC, is cut
  only by the transcode: every FLAC, ALAC, MP3 and Vorbis cut, and every
  HE-AAC cut that starts past the head.
- The cut's arithmetic runs again at each entry point (`CutTrack`, `Cut`,
  `PlanCut`) from the caller's own inputs, which holds only while it stays
  pure.
- A caller that skips the plan skips the destination check: the trims it
  writes may be ones the destination drops.
