package waxflow

import (
	"fmt"
	"io"
	"sort"
	"time"

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

// ToEnd is Slice's open-ended upper bound: the span runs to the end of the
// source.
const ToEnd = -1

// Slice bounds med to the sample range [from, to) of its own timeline, as
// a Media whose sample 0 is med's sample from and whose length is to-from.
// to is exclusive; ToEnd means to the end. The returned Media owns med and
// closes it.
//
// It is the primitive behind three things that looked like three features:
// a split job's cut points, an end trim (a span with a from of 0), and a
// virtual track streamed over an offset range of one file. All three are
// "bound this stream to a sample range", so they land once.
//
// A wrapper rather than a TranscodeOptions field, deliberately. An end
// bound as an option would need about six branches in the most
// invariant-dense function in the library, permanently: a clamp in the
// length math, a refusal in the segmented plan, the right interaction with
// the projected output length that feeds both the muxer's declared length
// and the edit list (get that wrong and every M4B lies about its
// duration), the progress total, and both canonical cache-key strings. A
// Media that is already the bounded stream needs none of them, because
// every one of those reads the length off the track and the track is
// already right.
//
// It composes, which is the clinching part. A sliced Media is the shifted
// stream, addressing from 0, so it hands the segmented path a start offset
// that PlanSegments refuses to take as an option ("segments address time");
// and "start the album at track 3" is Concat(members[2:]) with no new
// option at all.
//
// # Exactness is conditional, and the condition is worth stating
//
// A slice hands the chain a stream starting at sample from, and the chain
// starts fresh there, so any stateful node primes from nothing exactly as
// it does after a seek.
//
//   - A cut with no rate change is exact, and that is the case that
//     matters. A CUE split to FLAC at the source rate builds a chain with
//     no resampler and no limiter, so there is no state to prime and each
//     piece's sample 0 is the source's sample from, bit for bit.
//     TestSliceSplitRoundTrip proves precisely that: a transient would make
//     a bit-exact rejoin fail.
//   - A resampled span would carry a short transient at sample 0, because
//     the resampler's FIR window starts zero-filled, and that is what
//     Headroom exists to remove: a span is a window onto a longer stream, so
//     unlike a file it genuinely has audio before its own sample 0 to prime
//     with. The segmented run uses it, so a virtual track's first sample is
//     the same audio a continuous run of the whole source delivers there.
//     That is what lets consecutive virtual tracks of one rip play gaplessly.
//
// Cut points are not assumed frame- or packet-aligned. Slice sits
// downstream of decode, so it cuts at any sample, which is the whole reason
// it is sample-exact where a packet-level cut would not be.
func Slice(med format.Media, from, to int64) (format.Media, error) {
	if med == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: Slice of a nil Media")
	}
	track := med.Info().Default()
	spanned, err := SpanTrack(track, from, to)
	if err != nil {
		return nil, err
	}
	s := &slice{med: med, from: from, limit: ToEnd, fmt: track.Fmt}
	// limit is the clamp; the track's Samples is what it advertises. They
	// differ on purpose for the open-ended form: an explicit end is a
	// declaration this holds the source to, while a slice that just trims
	// the front inherits the source's own length and the source's own
	// honesty about it. Clamping the open form at an advisory length would
	// truncate a source that is merely mis-declared, which the unsliced
	// Media tolerates and which is not this wrapper's business to change.
	if to >= 0 {
		s.limit = to - from
	}
	in := med.Info()
	s.info = &format.Info{
		Container: in.Container,
		Tracks:    []container.Track{spanned},
		Chapters:  spanChapters(in.Chapters, from, s.limit, track.Fmt.Rate),
		Warnings:  in.Warnings,
	}
	return s, nil
}

// spanChapters rebases a source's chapters onto the window [from, from+limit)
// of its own timeline: shifted so the window's start is zero, clipped to the
// window, and with everything outside it dropped. limit is ToEnd for an
// unbounded window. A chapter straddling an edge survives with its title and
// the part of its range that is inside.
//
// A zero End is the start-only chapter form (see container.Chapter): the
// chapter runs until the next one, or to the end of the stream. It stays zero
// on the way out, which is exact rather than a punt, because both things a
// consumer resolves it against are already this Media's own: the next chapter
// is in this list, rebased, and the stream ends where the window does.
// Writing an end here instead would declare one the source never did.
//
// The unbounded window clips nothing at the far end, for the reason Slice's
// limit exists: an open span holds the source to no length of its own, so a
// chapter running past the source's advisory end is the source's own business,
// exactly as the audio past it is.
//
// A rate of zero cannot place a chapter on a sample window at all, and there
// is then no answer to give rather than a wrong one to give (see slice).
func spanChapters(chapters []container.Chapter, from, limit int64, rate int) []container.Chapter {
	if len(chapters) == 0 || rate <= 0 {
		return nil
	}
	start := sampleTime(from, rate)
	end := time.Duration(-1)
	if limit >= 0 {
		end = sampleTime(from+limit, rate)
	}
	var out []container.Chapter
	for i, ch := range chapters {
		// Begins at or after the window's end, or is over before its start:
		// outside either way. The far test is skipped for an unbounded
		// window, which has no far end to be past.
		if end >= 0 && ch.Start >= end {
			continue
		}
		if e := chapterEnd(chapters, i); e >= 0 && e <= start {
			continue
		}
		ch.Start = max(ch.Start-start, 0)
		if ch.End > 0 {
			if end >= 0 {
				ch.End = min(ch.End, end)
			}
			ch.End -= start
		}
		out = append(out, ch)
	}
	return out
}

// chapterEnd is where chapter i really ends, for deciding whether it reaches
// a window at all: its own End, or the next chapter's start for the
// start-only form (a zero End). -1 means it runs to the end of the stream,
// which no window can begin after: that is the last chapter of a start-only
// list, and it always reaches.
//
// It resolves what spanChapters deliberately does not write down. Where a
// start-only chapter ends decides whether it is in the window, so the test
// needs the answer; the rebased list does not, and inventing an End there
// would put a boundary in the output the source never declared.
func chapterEnd(chapters []container.Chapter, i int) time.Duration {
	if e := chapters[i].End; e > 0 {
		return e
	}
	if i+1 < len(chapters) {
		return chapters[i+1].Start
	}
	return -1
}

// sampleTime is sample n's position on a stream's clock at rate.
//
// The division is split so the whole-second part stays exact at any stream
// length: the direct n*time.Second/rate overflows an int64 past about 53
// hours at 48 kHz, and a long file is exactly the kind that carries chapters.
// What is left rounds toward zero, below the nanosecond the Duration itself
// resolves.
func sampleTime(n int64, rate int) time.Duration {
	sec, rem := n/int64(rate), n%int64(rate)
	return time.Duration(sec)*time.Second + time.Duration(rem)*time.Second/time.Duration(rate)
}

// SpanTrack computes the track a Slice of track to [from, to) presents: the
// same format, the window's length, and no gapless trims. It is a pure
// function of the header, so planning a span and running it cannot disagree
// about what gets delivered.
//
// It is the single funnel, the discipline ConcatTrack applies to a
// timeline: Slice resolves its track through this at open, and a caller
// planning a span resolves through it too, from the probed track alone and
// without opening anything. Without that, a plan's length and the slice's
// actual delivery drift, and the drift is invisible until a cache entry
// holds segments for a track that is not the one being served.
//
// to is exclusive; ToEnd means to the end of track.
func SpanTrack(track container.Track, from, to int64) (container.Track, error) {
	switch {
	case from < 0:
		return container.Track{}, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: negative span start %d", from))
	case to < ToEnd:
		return container.Track{}, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: span end %d: want a sample offset or %d for the end of the source", to, ToEnd))
	case to >= 0 && to < from:
		return container.Track{}, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: span [%d, %d) ends before it starts", from, to))
	}
	total := track.Samples

	// A span past the end is refused rather than clamped, and that is the
	// same call ConcatTrack makes about a member's declared length. A span
	// is content identity: it says which samples are this track. So a cut
	// point past the end means the caller's cut points do not describe this
	// file (a CUE sheet paired with the wrong rip, a chapter list from a
	// different edition), and silently clamping would hand back a track
	// shorter than the caller believes it asked for, with no way to notice.
	// That is precisely the desync a prefix sum cannot survive.
	//
	// The bound is the declared length whatever SamplesExact says, which
	// looks like the wrong predicate and is not. SamplesExact is a
	// truncation instruction (the decoder over-produces and must be cut back
	// to this), not a claim about precision, so gating on it would drop the
	// refusal for exactly the sources a split is usually pointed at: WAV and
	// FLAC leave it false because their totals can lie, not because they are
	// approximate. Gating here would trade a real refusal on the common case
	// for a narrow one on Matroska, whose advisory total can sit up to a
	// millisecond under the audio it has.
	if total >= 0 {
		if from > total {
			return container.Track{}, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span starts at sample %d, past the source's %d samples", from, total))
		}
		if to > total {
			return container.Track{}, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span ends at sample %d, past the source's %d samples", to, total))
		}
	}

	out := track
	switch {
	case to >= 0:
		out.Samples, out.SamplesExact = to-from, true
	case total >= 0:
		out.Samples = total - from
	}
	// Zero, and load-bearing rather than incidental, exactly as a Concat's
	// envelope is: a Media delivers gapless-trimmed PCM, so both trims
	// happened inside it before a slice sees a sample. Passing the
	// container's declaration through would make a downstream consumer trim
	// a second time, against a stream that has no delay left to cut.
	out.Delay, out.Padding = 0, 0
	// The source's own codec is kept, unlike a Concat's synthetic PCM
	// envelope, and that is not cosmetic: the codec is what names the
	// decoder revision in a plan's Versions, so a span of a FLAC keys on the
	// FLAC decoder and a decoder fix invalidates its cached bytes. A Concat
	// cannot do that (N members, N codecs), which is why it has to repair
	// the hole afterward; a span has exactly one source and keeps the truth.
	return out, nil
}

// Headroomer is implemented by a Media that has real audio before its own
// sample 0, as a window onto a longer stream does. It is an optional
// capability in the same idiom as container.Indexer, container.Warner, and
// dsp.Settler: a Media opened from a file has nothing before its first
// sample and does not implement it, so the assertion is an honest gate.
//
// It exists because priming a chain and starting a stream are different
// questions, and only a span can answer the first one for its own sample 0.
// A stateful node primes from nothing at a stream's start, which is correct
// for a file (there is nothing earlier) and wrong for a span (there is). A
// consumer that wants a span's sample 0 to hold the same audio a continuous
// run of the whole source delivers there reads Headroom, seeks to a
// negative position, and discards the output it fed through.
//
// Positions below 0 are the whole point of the interface and are legal only
// on a Media that implements it. They stay within [-Headroom(), 0): the
// samples are real, they are simply upstream of the window this Media
// presents.
type Headroomer interface {
	// Headroom is how many samples of real audio lie before sample 0, so a
	// caller knows how far back it may seek. Zero means none.
	Headroom() int64
}

// slice is Slice's Media: one source, positioned at the window's start and
// cut off at its end.
//
// What it says about the source follows one rule: rebase onto the window's
// own timeline where a right answer exists there, and answer nothing where
// none does.
//
// Chapters have one, so they are rebased (spanChapters). A chapter is a
// range on the very timeline the window cuts, so the part of the list lying
// inside the window is a fact about the window rather than a guess at it.
// Forwarding the source's list verbatim is the wrong answer and not a
// cautious one: it says a span holds chapters it does not, at times it does
// not hold them, and a consumer that writes them into the output (a split
// job stamping a piece with its source's metadata) has no way to notice.
//
// format.Composite has no right answer, so it is deliberately not forwarded.
// A slice of a timeline is not a timeline of the same members (its window
// covers some part of some of them) and no member list describes the window,
// so answering with the inner Media's would be a plain lie to a consumer
// keying a cache on it. Nothing slices a Concat today; the point is that if
// something does, it gets no answer rather than a wrong one.
//
// container.Indexer needs no forwarding either, for a different reason: the
// engine wraps index restore and save around the Media inside OpenStream,
// under this, and the save fires on Close, which this delegates. The
// sidecar keeps working through a slice without this knowing about it.
type slice struct {
	med  format.Media
	info *format.Info
	fmt  audio.Format
	// from is the window's first sample on med's timeline.
	from int64
	// limit is the window's length, ToEnd for an unbounded one. See Slice.
	limit int64

	pos     int64 // delivered-timeline position of the next frame out
	started bool  // med has been positioned at from
	discont bool
	closed  bool
}

func (s *slice) Info() *format.Info { return s.info }

// Headroom is the audio ahead of the window: the samples between the inner
// media's start and this span's, plus whatever the inner media can itself
// reach back to.
//
// The second term is what makes a span of a span report the truth. Nothing
// nests them today, and the sum is still the right answer rather than
// speculation: headroom means "how far back can I be positioned", and for
// an inner span that is its own window's start plus its own headroom, all
// of which SeekSample below can actually deliver. Reporting only from
// would under-report it, which does not fail loudly. It quietly primes a
// chain with less than it asked for.
func (s *slice) Headroom() int64 {
	if h, ok := s.med.(Headroomer); ok {
		return s.from + h.Headroom()
	}
	return s.from
}

func (s *slice) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.med.Close()
}

// ensureStart positions med at the window's start, lazily.
//
// Lazily, and only when from is nonzero, so that a pure end trim (a span
// starting at 0) neither seeks nor requires a seekable source. The
// unbounded, unshifted slice is then free.
//
// A failed seek leaves the span unstarted, so the next read attempts it
// again, and that is the opposite of the call concat makes about its own
// failed seek. The two are not inconsistent: the difference is whether
// re-attempting is even defined. concat's seek walks toward a target the
// caller chose, moving the state its position is relative to on the way
// (which member is open, its chain, where that member's media sits), so a
// failure part way leaves pos describing somewhere the stream no longer is
// and nothing coherent to retry; it has to latch. This has one target for
// the life of the Media, from, and reaches it in one step. A failure writes
// nothing, so the state after it is the state before it, and the next read
// re-attempts the identical seek from the identical place: a sticky error
// would refuse what a retry can still get right, and would need a field to
// say what started already says.
//
// What both answers share is the only part that is not a choice: a position
// that never succeeded never becomes one a read can deliver samples against.
// started latches after the seek here, exactly as it does in SeekSample.
func (s *slice) ensureStart() error {
	if s.started {
		return nil
	}
	if s.from == 0 {
		s.started = true
		return nil
	}
	landed, err := s.med.SeekSample(s.from)
	if err != nil {
		return err
	}
	// A landing past the ask is what container.Seeker permits when the
	// stream's first sync point lies beyond the target, and it means the
	// span really does start late. Report where the stream is rather than
	// pretending, exactly as a Concat's member seek does; for every
	// seekable source in the tree the Media pre-rolls and this is 0.
	//
	// The floor is not the same floor SeekSample deliberately does without,
	// and the difference is which question was asked. A negative target
	// there is a caller reaching into the headroom on purpose, so a
	// negative answer is the truth. Here the target is the window's own
	// start, and a format.Media seek cannot land below its target except by
	// running out of stream: it lands on a sync point at or before the ask
	// and then decodes forward to the ask exactly, so a short answer means
	// the source ended early, not that this is somehow positioned in its own
	// headroom. Zero is then the honest position, and it is what lets
	// endOfSource say the source ended n samples into a span of m rather
	// than report a negative count.
	s.pos = max(landed-s.from, 0)
	s.started = true
	return nil
}

// ReadChunk fills dst from the window.
//
// The front of the window is handled by the seek in ensureStart rather than
// by shifting a buffer, which is what lets the inner Media fill dst
// directly: an audio.Buffer is planar with a stride and has no sub-buffer
// view, so a shifted fill would need a copy. The back is a clamp on dst.N,
// the same shape the gapless padding trim already uses one layer down.
func (s *slice) ReadChunk(dst *audio.Buffer) error {
	switch {
	case s.closed:
		return waxerr.New(waxerr.CodeInternal, "waxflow: ReadChunk on a closed span")
	case dst.Fmt != s.fmt:
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: chunk buffer is %v, span is %v", dst.Fmt, s.fmt))
	case dst.Cap() == 0:
		return waxerr.New(waxerr.CodeInvalidRequest, "waxflow: zero-capacity chunk buffer")
	}
	if err := s.ensureStart(); err != nil {
		return err
	}
	if s.limit >= 0 && s.pos >= s.limit {
		return io.EOF
	}
	dst.N = 0
	err := s.med.ReadChunk(dst)
	if err == io.EOF {
		return s.endOfSource()
	}
	if err != nil {
		return err
	}
	if dst.N == 0 {
		return waxerr.New(waxerr.CodeInternal,
			"waxflow: a span's source returned no frames and no error; io.EOF is the only empty answer")
	}
	if s.limit >= 0 {
		if allowed := s.limit - s.pos; int64(dst.N) >= allowed {
			dst.N = int(max(allowed, 0))
		}
	}
	dst.Pos = s.pos
	dst.Discont = s.discont
	s.discont = false
	s.pos += int64(dst.N)
	return nil
}

// endOfSource reports the source running out, which is only legal when the
// window did not declare where it ends.
//
// A bounded span whose source ends early is an error rather than a short
// stream, and for the same reason a Concat holds its members to their
// declared lengths: the track this Media advertises says to-from samples,
// a plan has already promised a segment count built from that number, and
// delivering fewer produces the tail 404 that number exists to prevent.
// Failing here names the real cause instead.
func (s *slice) endOfSource() error {
	if s.limit >= 0 && s.pos < s.limit {
		return waxerr.New(waxerr.CodeSourceUnreadable, fmt.Sprintf(
			"waxflow: the source ended %d samples into a span that declared %d; its cut points do not describe this file",
			s.pos, s.limit))
	}
	return io.EOF
}

// SeekSample repositions to target on the window's own timeline.
//
// A negative target is legal here, and only here, down to -Headroom(): it
// addresses the real audio ahead of the window, which is what a consumer
// priming a chain for the span's sample 0 asks for. Everything below the
// window is still the source's own audio, so the seek is an ordinary one
// once rebased. See Headroomer.
func (s *slice) SeekSample(target int64) (int64, error) {
	// The bound is what Headroom advertises, not from alone: the two have to
	// agree, or a caller that primes by exactly the headroom it was told
	// about gets refused for asking.
	room := s.Headroom()
	switch {
	case s.closed:
		return 0, waxerr.New(waxerr.CodeInternal, "waxflow: SeekSample on a closed span")
	case target < -room:
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"waxflow: seek to %d is %d samples before the source's start; the span has %d samples of headroom",
			target, -target-room, room))
	}
	// Past the window's end lands at its end, as a single Media does at the
	// end of a file.
	if s.limit >= 0 {
		target = min(target, s.limit)
	}
	landed, err := s.med.SeekSample(s.from + target)
	if err != nil {
		return 0, err
	}
	s.started = true
	// Rebased, and not floored at 0: a landing inside the headroom is a
	// real position on this timeline, just a negative one.
	s.pos = landed - s.from
	if s.limit >= 0 {
		s.pos = min(s.pos, s.limit)
	}
	s.discont = true
	return s.pos, nil
}

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
