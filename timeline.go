package waxflow

import (
	"fmt"
	"io"
	"sort"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// concatContainer names the synthetic container a Concat reports, the way
// format.FromDemuxer labels an assembled one: there is no file here, so
// Info.Container has to say what the media actually is.
const concatContainer = "timeline"

// ConcatSource is one member of a timeline: its track, as Probe reported it,
// so a timeline can be planned without opening anything, and a function that
// opens it on demand.
type ConcatSource struct {
	// Track describes the member from its headers. Concat holds the member
	// to this declaration: one that opens in a different format, or delivers
	// a different number of samples, fails the run rather than silently
	// desyncing every position after it.
	Track container.Track
	// Open opens the member's decodable media. Concat calls it when the
	// timeline reaches this member and closes the result on advance, so a
	// 500-track queue costs one file descriptor rather than 500.
	//
	// Any context this closure binds must be the engine's own, never a
	// request's. Open fires lazily, mid-stream, long after the call that
	// built the Concat returned: live pipelines resolve under the server's
	// base context by design, so read-behind can finish an encode after the
	// client has left, and a request context captured here would instead
	// kill a member's first read at a track boundary minutes into playback.
	// container.Contextual exists for exactly this handoff.
	Open func() (format.Media, error)
}

// ConcatOptions configures a Concat.
type ConcatOptions struct {
	// Profile selects the resampler quality profile for normalizing members
	// whose rate is not the envelope's; empty means resample.HQ.
	//
	// It must be the profile the transcode's own TranscodeOptions carry.
	// PlanSegmentsTimeline names this profile in the plan's Versions on the
	// members' behalf, so a Concat built with a different one would resample
	// in a way the cache key does not describe.
	Profile resample.Profile
}

// ConcatTrack computes the synthetic track a Concat of these members
// presents: the common (envelope) format, the summed normalized length, and
// no gapless trims. It is a pure function of the headers, so planning and
// running cannot disagree about the delivered format.
//
// The envelope is the format no member loses information to reach: the
// maximum rate, the maximum channel count, and the wider sample domain
// (float if any member is float). Refusing mixed members instead would not
// push the problem to the caller, it would delete the feature for the normal
// case: HLS cannot change format mid-variant without an EXT-X-DISCONTINUITY
// and a second init, which one chain, one init, and one edit list forbid
// structurally, and a play queue is mixed by nature.
//
// A member whose format already equals the envelope is read straight
// through, with no chain and no copy, so a uniform timeline (a gapless
// album, which is one master at one rate) pays nothing for the machinery.
// That is structural rather than an optimization: it falls out of the
// envelope being a maximum.
//
// One member at 96 kHz makes every other member resample twice, member to
// envelope and envelope to output. The cost is real and deliberately
// unaddressed: collapsing it needs the output format, which is not known
// here (an output row's adjust hook owns the real rate, which is how Opus
// forces 48 kHz whatever the caller asked for). Likewise the channel count
// is a maximum and is not capped at stereo: capping would silently destroy a
// surround member, and it looks cheaper only because the output is usually
// stereo, which is the same output-aware knowledge this function does not
// have. The common mixed-channel case is a mono track in a stereo queue,
// where the maximum is exact and free.
//
// Delay and Padding are zero, and that is load-bearing rather than
// incidental: format.Media delivers already-trimmed PCM, so both trims
// happened inside each member before Concat saw a sample. That is exactly
// why concatenation is sample-exact, and a nonzero trim here would make a
// downstream consumer trim a second time.
func ConcatTrack(tracks []container.Track) (container.Track, error) {
	if len(tracks) == 0 {
		return container.Track{}, waxerr.New(waxerr.CodeInvalidRequest,
			"waxflow: a timeline needs at least one member")
	}
	env := audio.Format{Type: audio.Int}
	for i, t := range tracks {
		if err := t.Fmt.Valid(); err != nil {
			return container.Track{}, waxerr.Wrap(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("waxflow: timeline member %d", i), err)
		}
		if t.Samples < 0 {
			return container.Track{}, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: timeline member %d has no declared length; measure it before planning a timeline", i))
		}
		env.Rate = max(env.Rate, t.Fmt.Rate)
		env.Channels = max(env.Channels, t.Fmt.Channels)
		env.BitDepth = max(env.BitDepth, t.Fmt.BitDepth)
		if t.Fmt.Type == audio.Float {
			env.Type = audio.Float
		}
	}
	if env.Type == audio.Float {
		env.BitDepth = 32
	}
	env.Layout = audio.DefaultLayout(env.Channels)

	// The envelope's layout has to be the conventional one, because that is
	// the only layout the mix node targets: a member with fewer channels is
	// mixed up to audio.DefaultLayout(env.Channels), so any other envelope
	// layout would be one no normalized member could reach. A member that
	// already has the envelope's channel count runs no mix and keeps its own
	// layout, so its layout has to match already. That is true of every
	// layout the decoders produce; a WAVEFORMATEXTENSIBLE mask naming some
	// other pair of speakers is the one case it is not, and it is refused by
	// name rather than relabelled, since calling a back-left channel
	// front-right is a silent lie about what the file says it holds.
	for i, t := range tracks {
		if t.Fmt.Channels == env.Channels && t.Fmt.Layout != env.Layout {
			return container.Track{}, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
				"waxflow: timeline member %d lays its %d channels out as %v, not the conventional %v; "+
					"a timeline normalizes channel counts, not speaker assignments",
				i, t.Fmt.Channels, t.Fmt.Layout, env.Layout))
		}
	}

	var total int64
	for _, t := range tracks {
		total += concatMemberSamples(t, env)
	}
	return container.Track{
		Codec: codec.PCM,
		Fmt:   env,
		// Samples is authoritative rather than advisory, so SamplesExact is
		// honest: Concat holds every member to its declared length and fails
		// the run instead of delivering some other count.
		Samples:      total,
		SamplesExact: true,
		Default:      true,
	}, nil
}

// concatMemberSamples is the member's length on the envelope timeline. It
// goes through the resampler's own exact output count, so the prefix sum a
// Concat rebases positions by and the length a plan projects are the same
// arithmetic rather than two roundings that agree by inspection.
func concatMemberSamples(t container.Track, env audio.Format) int64 {
	return resample.OutputLen(t.Samples, t.Fmt.Rate, env.Rate)
}

// concatSpec is the normalization one member needs to reach the envelope, in
// one place so the version accounting (timelineVersions) and the run build
// the identical chain.
//
// The dither strategy is pinned to TPDF rather than left to the zero value
// it happens to equal. TPDF is keyed by absolute position and holds no
// history, so a member normalized from a mid-stream start requantizes to the
// same samples a continuous run produces; Shaped carries error feedback,
// which would make a restarted worker's segments differ from a continuous
// worker's for a reason no test here would name.
func concatSpec(env audio.Format, opts ConcatOptions) dsp.ChainSpec {
	spec := dsp.ChainSpec{
		Rate:     env.Rate,
		Channels: env.Channels,
		Profile:  opts.Profile,
		Shaping:  dither.TPDF,
	}
	if env.Type == audio.Float {
		spec.Float = true
	} else {
		spec.BitDepth = env.BitDepth
	}
	return spec
}

// Concat sequences members into one gapless format.Media: a single
// continuous timeline whose sample len(a) is b's sample 0, exactly.
//
// It is sample-exact by construction rather than by arithmetic. format.Media
// already delivers gapless-trimmed PCM, so there is no encoder delay or
// padding left to reason about at the seam; concatenation is just reading
// one stream after another.
//
// Members open on demand and close on advance. That is the design and not an
// optimization: besides costing one file descriptor for a queue of any
// length, it makes planning and running symmetric (both are driven by the
// members' tracks alone) and it removes the rewind problem outright, since a
// member reached a second time is a member opened a second time, from the
// top, with no state to have gone stale.
//
// The returned Media owns nothing until it is read and closes whatever it
// opened on Close. It also satisfies format.Composite, so a consumer keying
// its own cache can reach the members' tracks rather than only the envelope.
func Concat(members []ConcatSource, opts ConcatOptions) (format.Media, error) {
	tracks := make([]container.Track, len(members))
	for i := range members {
		if members[i].Open == nil {
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("waxflow: timeline member %d has no Open function", i))
		}
		tracks[i] = members[i].Track
	}
	env, err := ConcatTrack(tracks)
	if err != nil {
		return nil, err
	}
	starts := make([]int64, len(members)+1)
	for i, t := range tracks {
		starts[i+1] = starts[i] + concatMemberSamples(t, env.Fmt)
	}
	return &concat{
		members: members,
		tracks:  tracks,
		opts:    opts,
		fmt:     env.Fmt,
		starts:  starts,
		info:    &format.Info{Container: concatContainer, Tracks: []container.Track{env}},
	}, nil
}

// concat is Concat's Media: one member open at a time, each normalized to
// the envelope by its own chain, positions rebased by the prefix sum.
type concat struct {
	members []ConcatSource
	tracks  []container.Track
	opts    ConcatOptions
	info    *format.Info
	fmt     audio.Format
	// starts is the prefix sum of the members' normalized lengths: starts[i]
	// is member i's first sample on the timeline, and starts[len(members)]
	// is the whole timeline's length.
	starts []int64

	cur   int          // the open member's index; len(members) past the end
	med   format.Media // nil when no member is open
	chain *dsp.Chain   // nil when the open member needs no normalization

	local   int64 // the open member's own position, on the envelope timeline
	pos     int64 // timeline position of the next frame out
	discont bool
	closed  bool
	// unpositioned latches a failed seek: pos no longer describes where the
	// next sample comes from, so reading is refused until a seek succeeds.
	unpositioned bool
}

func (c *concat) Info() *format.Info { return c.info }

// Members reports the members' tracks (format.Composite).
func (c *concat) Members() []container.Track {
	return append([]container.Track(nil), c.tracks...)
}

func (c *concat) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.closeMember()
}

// ReadChunk fills dst from the timeline, crossing member boundaries.
//
// A chunk never spans a seam. The member in flight is read first, and when
// it ends the next one opens and fills a fresh chunk, so the only cost of a
// boundary is one short chunk, which the Stage contract allows everywhere.
// Filling across the boundary instead would need a carry buffer, since an
// audio.Buffer is planar with a stride and has no sub-buffer view, and would
// buy nothing.
func (c *concat) ReadChunk(dst *audio.Buffer) error {
	switch {
	case c.closed:
		return waxerr.New(waxerr.CodeInternal, "waxflow: ReadChunk on a closed timeline")
	case c.unpositioned:
		return waxerr.New(waxerr.CodeInternal,
			"waxflow: reading a timeline whose seek failed; its position is unknown until a seek succeeds")
	case dst.Fmt != c.fmt:
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: chunk buffer is %v, timeline is %v", dst.Fmt, c.fmt))
	case dst.Cap() == 0:
		return waxerr.New(waxerr.CodeInvalidRequest, "waxflow: zero-capacity chunk buffer")
	}
	for c.cur < len(c.members) {
		if c.med == nil {
			if err := c.open(c.cur); err != nil {
				return err
			}
		}
		dst.N = 0
		err := c.readMember(dst)
		if err == io.EOF {
			if err := c.advance(); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := c.count(dst.N); err != nil {
			return err
		}
		dst.Pos = c.pos
		// A pure override, never an OR with what the member said. A freshly
		// opened or seeked member's own first chunk can carry Discont, and
		// passing that through would stamp a discontinuity at the seam. That
		// is the one thing this whole primitive exists to avoid: it drains
		// the downstream resampler to end-of-stream and re-anchors, which
		// both re-creates the seam and changes the timeline's output length
		// from ceil((nA+nB)*L/M) to ceil(nA*L/M)+ceil(nB*L/M).
		dst.Discont = c.discont
		c.discont = false
		c.pos += int64(dst.N)
		return nil
	}
	return io.EOF
}

// SeekSample repositions to target on the timeline: the member holding it is
// opened and positioned, and everything after it follows in order.
//
// A failed seek leaves the timeline unpositioned rather than half-moved, and
// that is what the latch below is for. Every step of a seek moves state the
// position depends on (the open member, its chain, where its media sits), so a
// failure part way through leaves pos describing where the stream used to be
// and the media sitting somewhere else. Closing the member is not enough on
// its own: the next read would reopen it and deliver from its start, still
// stamped with the old pos, which is the same desync by a longer route. So the
// failure latches, and a caller that ignores it and reads anyway gets the
// error again instead of samples labelled with a position they do not have.
// Another seek clears it, because a seek is what puts the timeline back
// somewhere known.
func (c *concat) SeekSample(target int64) (int64, error) {
	switch {
	case c.closed:
		return 0, waxerr.New(waxerr.CodeInternal, "waxflow: SeekSample on a closed timeline")
	case target < 0:
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: negative seek target")
	}
	pos, err := c.seekTo(target)
	if err != nil {
		c.closeMember()
		c.unpositioned = true
		return 0, err
	}
	c.unpositioned = false
	return pos, nil
}

// seekTo is SeekSample's body, split out so every failure inside it lands on
// the one latch above rather than each error path having to remember.
func (c *concat) seekTo(target int64) (int64, error) {
	total := c.starts[len(c.members)]
	if target >= total {
		// Past the end lands at the end of stream, as a single Media does.
		if err := c.closeMember(); err != nil {
			return 0, err
		}
		c.cur = len(c.members)
		c.local, c.pos, c.discont = 0, total, true
		return total, nil
	}
	i := c.memberAt(target)
	if c.med == nil || c.cur != i {
		if err := c.closeMember(); err != nil {
			return 0, err
		}
		if err := c.open(i); err != nil {
			return 0, err
		}
	} else if err := c.buildChain(); err != nil {
		return 0, err
	}
	landed, err := c.seekMember(target - c.starts[i])
	if err != nil {
		return 0, err
	}
	c.local = landed
	c.pos = c.starts[i] + landed
	c.discont = true
	return c.pos, nil
}

// memberAt returns the member holding timeline sample target, which must be
// inside the timeline. It searches for the first member ending past the
// target rather than the last one starting at or before it, so a zero-length
// member (an odd but legal header) is skipped rather than selected.
func (c *concat) memberAt(target int64) int {
	return sort.Search(len(c.members), func(i int) bool { return target < c.starts[i+1] })
}

// open opens member i and wires its normalization.
func (c *concat) open(i int) error {
	med, err := c.members[i].Open()
	if err != nil {
		return err
	}
	// The declared format is what the envelope was computed from and what
	// the chain is built for, so a member that opens as something else would
	// otherwise surface as a buffer-format mismatch from deep inside the
	// chain. Say what actually happened instead.
	if got := med.Info().Default().Fmt; got != c.tracks[i].Fmt {
		med.Close()
		return waxerr.New(waxerr.CodeSourceChanged, fmt.Sprintf(
			"waxflow: timeline member %d opened as %v, its headers declared %v", i, got, c.tracks[i].Fmt))
	}
	c.med, c.cur, c.local = med, i, 0
	if err := c.buildChain(); err != nil {
		c.closeMember()
		return err
	}
	return nil
}

// buildChain wires the open member's normalization to the envelope, leaving
// the member read straight through when it already matches.
//
// It always builds a fresh chain, which matters after a seek. Reusing one
// would make its kernels take the seek's Discont as a splice and drain the
// pre-seek segment first, delivering samples from the position we just left;
// a chain that has never been read anchors on its first chunk instead, which
// is the post-seek position exactly.
func (c *concat) buildChain() error {
	if c.chain != nil {
		c.chain.Release()
		c.chain = nil
	}
	in := c.tracks[c.cur].Fmt
	if in == c.fmt {
		return nil
	}
	chain, err := dsp.NewChain(dsp.NewSource(c.med, in), concatSpec(c.fmt, c.opts))
	if err != nil {
		return err
	}
	c.chain = chain
	return nil
}

// readMember pulls the open member's next normalized chunk, holding the
// member to the one part of the Stage contract a timeline cannot survive being
// wrong about: io.EOF is the only empty answer.
//
// A member that returns no frames and no error would make this timeline return
// the same to its own caller, which would spin whatever loop is reading it. The
// chain path is already checked at the seam where a reader enters a chain
// (dsp.NewSource); a uniform member has no chain by design, so it is read
// straight through and this is that member's seam.
func (c *concat) readMember(dst *audio.Buffer) error {
	if c.chain != nil {
		return c.chain.ReadChunk(dst)
	}
	err := c.med.ReadChunk(dst)
	if err == nil && dst.N == 0 {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf(
			"waxflow: timeline member %d returned no frames and no error; io.EOF is the only empty answer", c.cur))
	}
	return err
}

// seekMember positions the open member at local, its own position on the
// envelope timeline, and returns where it landed.
func (c *concat) seekMember(local int64) (int64, error) {
	if c.chain == nil {
		return c.med.SeekSample(local)
	}
	// The member's own timeline runs at its source rate. Map back to it,
	// flooring and then backing off one more sample so the landing is at or
	// before the target whatever the ratio rounds to; the slop is discarded
	// below.
	src := local
	if l, m := c.chain.Ratio(); l != m {
		src = local * int64(m) / int64(l)
		if src > 0 {
			src--
		}
	}
	landed, err := c.med.SeekSample(src)
	if err != nil {
		return 0, err
	}
	// Where the fresh chain's first chunk will start, without reading one:
	// every kernel here anchors on the source position it is handed, and the
	// resampler's anchor is the same ceil(pos*L/M) that OutputSamples is.
	out := c.chain.OutputSamples(landed)
	if out >= local {
		// The member's first sync point lay past the target, which the
		// container.Seeker contract permits. Report where the stream really
		// starts rather than pretending it starts where it was asked to.
		return out, nil
	}
	if err := c.dropOutput(local - out); err != nil {
		return 0, err
	}
	return local, nil
}

// dropOutput decodes and discards n frames of the open member's normalized
// output: the slop between where the source seek could land and the sample
// the caller asked for. A scratch sized to what is left can never over-read
// past the target, so no carry survives the seek.
//
// The loop needs no zero-progress guard of its own: a chunk that is empty
// without being io.EOF cannot reach it, because dsp.NewSource rejects one at
// the point a reader enters the chain.
func (c *concat) dropOutput(n int64) error {
	for n > 0 {
		buf := audio.Get(c.fmt, int(min(n, int64(audio.StandardChunk))))
		err := c.chain.ReadChunk(buf)
		got := int64(buf.N)
		audio.Put(buf)
		switch {
		case err == io.EOF:
			return waxerr.New(waxerr.CodeInternal,
				fmt.Sprintf("waxflow: timeline member %d ended inside a seek pre-roll", c.cur))
		case err != nil:
			return err
		}
		n -= got
	}
	return nil
}

// count advances the open member's position and holds it to the length its
// headers declared.
//
// An advisory length is a tolerated oddity for one file and fatal for a
// timeline: two samples of drift desync the prefix sum, and the playlist then
// promises a segment count the stream cannot fill, which is the tail 404 the
// exact-length walk exists to prevent. So this is enforced rather than
// trusted, which is why a timeline's mint measures every member whose length
// is not exact.
func (c *concat) count(n int) error {
	c.local += int64(n)
	if want := c.starts[c.cur+1] - c.starts[c.cur]; c.local > want {
		return waxerr.New(waxerr.CodeSourceUnreadable, fmt.Sprintf(
			"waxflow: timeline member %d holds more audio than the %d samples its headers declared", c.cur, want))
	}
	return nil
}

// advance closes the finished member and moves to the next.
func (c *concat) advance() error {
	if want := c.starts[c.cur+1] - c.starts[c.cur]; c.local != want {
		return waxerr.New(waxerr.CodeSourceUnreadable, fmt.Sprintf(
			"waxflow: timeline member %d delivered %d samples, its headers declared %d", c.cur, c.local, want))
	}
	if err := c.closeMember(); err != nil {
		return err
	}
	c.cur++
	c.local = 0
	return nil
}

// closeMember releases the open member's chain and media.
func (c *concat) closeMember() error {
	if c.chain != nil {
		c.chain.Release()
		c.chain = nil
	}
	if c.med == nil {
		return nil
	}
	med := c.med
	c.med = nil
	return med.Close()
}
