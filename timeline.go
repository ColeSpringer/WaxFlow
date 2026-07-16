package waxflow

import (
	"fmt"
	"io"
	"math"
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
//
// # Hand the same options to the plan and to the run
//
// PlanSegmentsTimeline(tracks, copts, ...) and Concat(members, copts) are two
// calls taking two separately constructed ConcatOptions, and nothing checks
// that they match. Both fields below make a mismatch a silent wrong answer
// rather than an error, so the convention is: build one ConcatOptions and pass
// it to both.
//
// For Profile a mismatch is a wrong cache key: the plan names one profile in
// its Versions and the run resamples through another, so the cached bytes
// describe processing that did not happen.
//
// For Crossfade it is worse, and it is why this paragraph exists rather than
// the field being left to speak for itself. A crossfade changes the timeline's
// length, so a plan built with one and a run built without it disagree about
// how many samples exist: the plan promises the sum less (N-1)*Crossfade
// against a run delivering the full sum. That is the prefix-sum desync and the
// tail 404 that ADR-0009's advisory-length section exists to prevent, arriving
// by a different door.
type ConcatOptions struct {
	// Profile selects the resampler quality profile for normalizing members
	// whose rate is not the envelope's; empty means resample.HQ.
	//
	// It must be the profile the transcode's own TranscodeOptions carry. See
	// the convention above.
	Profile resample.Profile

	// Crossfade is how many samples of each seam are a blend of the two
	// members meeting there, on the envelope's timeline. Zero, the default, is
	// a butt-join: sample len(a) is b's sample 0, exactly, which is what every
	// existing caller gets and what ADR-0009's primitive is.
	//
	// There is no nonzero default and there will not be one. A gapless album
	// must never blend, because the seam it would smear is the artifact this
	// primitive exists to deliver intact. A crossfade is a thing a caller asks
	// for on material that wants it (a declick between two independently
	// recorded takes, a play queue of unrelated tracks), never something the
	// library decides on their behalf.
	//
	// Each seam costs X samples of total length: N members crossfaded by X
	// deliver sum(len) - (N-1)*X. Member i's tail zone and member i+1's head
	// zone are the same region of the timeline, which is what the overlap is.
	// The blend is equal-power (cos/sin), so uncorrelated material holds its
	// level across the zone where a linear fade would dip 3 dB.
	//
	// Bounded twice, both refused at ConcatTrack so a plan and a run refuse
	// identically: every member must be long enough for the zones it carries
	// (head plus tail, so the edge members need only one), and a zone must fit
	// maxCrossfadeBytes.
	Crossfade int64
}

// maxCrossfadeBytes bounds one blend buffer, and the number is derived rather
// than chosen: audio/pool.go's top size class is maxClassBits = 22, "4 Mi
// samples = 16 MiB int32/float32", and audio.Get sizes on frames*Channels. So
// X*ch*4 <= 16 MiB is exactly X*ch <= 4 Mi, which is exactly the largest blend
// audio.Get will pool.
//
// One sample more and classBits returns -1: the buffer allocates directly and
// is dropped on Put (pool.go:14-17), so every seam of every timeline becomes a
// 16 MB allocate-and-discard. That is the cliff this refuses at, and it is why
// the constant is this number and not a round one near it.
//
// timeline.MaxMembers is the precedent for the form: a named constant carrying
// its own reason. (ADR-0009 and /caps spell that bound maxTimelineMembers,
// which is the wire field's name and not an identifier in the tree.)
const maxCrossfadeBytes = 16 << 20

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
//
// It takes the options because it is the single funnel and a crossfade
// changes the length: opts.Crossfade shortens the total by X per seam, and
// every refusal a crossfade needs lives here so that planning a timeline and
// running one refuse the same requests for the same reasons.
func ConcatTrack(tracks []container.Track, opts ConcatOptions) (container.Track, error) {
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

	lens := make([]int64, len(tracks))
	var total int64
	for i, t := range tracks {
		lens[i] = concatMemberSamples(t, env)
		total += lens[i]
	}
	if err := checkCrossfade(lens, env, opts.Crossfade); err != nil {
		return container.Track{}, err
	}
	// One zone per seam, and there are N-1 seams. Subtracted after every
	// member's own ceil, so the sum-of-ceils the members produce is untouched.
	total -= int64(len(tracks)-1) * opts.Crossfade
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

// checkCrossfade holds a crossfade to what the members and the envelope can
// actually carry: a legal length, a blend that fits one pooled buffer, and a
// zone that fits every member it lands on.
//
// It runs inside ConcatTrack, which is what makes a plan and a run refuse
// identically. lens are the members' normalized lengths, in the envelope's
// samples, which is the timeline X is measured on too.
func checkCrossfade(lens []int64, env audio.Format, x int64) error {
	if x == 0 {
		return nil
	}
	if x < 0 {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: negative crossfade %d", x))
	}
	// Divide rather than multiply: x is a caller's int64, so x*ch*4 overflows
	// before it refuses, and the refusal is the point. The message obeys the
	// same rule, which is why it quotes no byte count: x*perFrame would
	// overflow here too, on exactly the inputs this exists to catch.
	if perFrame := int64(env.Channels) * 4; x > maxCrossfadeBytes/perFrame {
		limit := maxCrossfadeBytes / perFrame
		// In the caller's own units. They set a frame count, not a byte count,
		// so an answer in MiB would leave them to rediscover the channel
		// arithmetic that produced it; the seconds are what make the number
		// mean anything at the rate they are actually running.
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"waxflow: a crossfade of %d samples is more than this timeline can blend; the most it can is "+
				"%d samples (%.1f s at %d Hz, %d channels), which is the largest buffer the sample pool holds",
			x, limit, float64(limit)/float64(env.Rate), env.Rate, env.Channels))
	}
	// The fit rule is head+tail <= L, not 2X <= L: the first and last members
	// carry one zone rather than two, and stating it this way makes N=1 pass
	// with no special case (its only member is both first and last, so it
	// carries neither zone).
	for i, l := range lens {
		var need int64
		if i > 0 {
			need += x
		}
		if i < len(lens)-1 {
			need += x
		}
		if need > l {
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: timeline member %d is %d samples, too short for the %d samples of crossfade it carries "+
					"(a crossfade of %d, on %d of its seams)",
				i, l, need, x, need/x))
		}
	}
	return nil
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
// continuous timeline whose sample len(a) is b's sample 0, exactly, unless
// opts.Crossfade asks for a blend.
//
// The butt-join is the default and the primitive. It is sample-exact by
// construction rather than by arithmetic: format.Media already delivers
// gapless-trimmed PCM, so there is no encoder delay or padding left to reason
// about at the seam, and concatenation is just reading one stream after
// another.
//
// A crossfade trades exactly that away, on purpose and only when asked: the
// seam becomes a zone of Crossfade samples that is both members at once, and
// the timeline shortens by one zone per seam. See ConcatOptions.Crossfade,
// which is zero for every caller that does not want it.
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
	env, err := ConcatTrack(tracks, opts)
	if err != nil {
		return nil, err
	}
	starts := make([]int64, len(members)+1)
	lens := make([]int64, len(members))
	for i, t := range tracks {
		lens[i] = concatMemberSamples(t, env.Fmt)
		// The next member begins where this one's tail zone does, which is X
		// before this one ends: the two share that region. The subtraction is
		// this member's tail, so the last member (which has none) contributes
		// its whole length and starts[N] is the total ConcatTrack computed.
		// Running the naive starts[i+1] = starts[i] + lens[i] - X through the
		// last hop instead would leave the timeline X short of its own track.
		tail := int64(0)
		if i < len(members)-1 {
			tail = opts.Crossfade
		}
		starts[i+1] = starts[i] + lens[i] - tail
	}
	return &concat{
		members: members,
		tracks:  tracks,
		opts:    opts,
		fmt:     env.Fmt,
		starts:  starts,
		lens:    lens,
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
	// starts is where each member begins on the timeline: starts[i] is member
	// i's first sample, and starts[len(members)] is the whole timeline's
	// length.
	//
	// lens is what each member is: lens[i] is member i's own normalized
	// length. The two are separate fields rather than one derived from the
	// other because a crossfade makes them genuinely different facts. A
	// butt-joined timeline has starts[i+1]-starts[i] == lens[i] and the
	// distinction is invisible; with a crossfade of X, consecutive members
	// overlap by X, so starts[i+1]-starts[i] is lens[i]-X and a member
	// occupies [starts[i], starts[i]+lens[i]), which runs past where the next
	// one begins. Reading a length off starts is then wrong by exactly the
	// overlap, and wrong in the direction that desyncs the prefix sum (see
	// count and advance, and ADR-0009's advisory-length section for what that
	// costs).
	starts []int64
	lens   []int64

	cur   int          // the open member's index; len(members) past the end
	med   format.Media // nil when no member is open
	chain *dsp.Chain   // nil when the open member needs no normalization

	local   int64 // the open member's own position, on the envelope timeline
	pos     int64 // timeline position of the next frame out
	discont bool
	closed  bool
	// blend holds the previous member's captured tail while the open member's
	// head zone is being read: the two are the same region of the timeline, so
	// the zone's output is the one mixed with the other. It is nil whenever no
	// zone is in flight, which is always at Crossfade 0.
	//
	// It outlives closeMember by design. The tail belongs to a member that is
	// finished and closed; the blend is what is left of it, and it has to
	// survive into the next member's first chunks or there is nothing to mix
	// them with. There is one at a time, ever.
	blend    *audio.Buffer
	blendOff int // frames of blend already mixed out
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
	c.releaseBlend()
	return c.closeMember()
}

// ReadChunk fills dst from the timeline, crossing member boundaries.
//
// A chunk never spans a seam. The member in flight is read first, and when
// it ends the next one opens and fills a fresh chunk, so the only cost of a
// boundary is one short chunk, which the Stage contract allows everywhere.
// Filling across the boundary instead would need a carry buffer, since an
// audio.Buffer is planar with a stride and has no sub-buffer view.
//
// That is exactly true of a butt-join, which is what a boundary is at
// Crossfade 0 and what every seam was before crossfades existed. A crossfade
// is the case where the carry buffer buys something, and it is the blend: the
// zone's chunks come from the outgoing member's captured tail and the incoming
// member's first frames, so the two do meet inside one chunk. The seam is
// still not spanned, because with a zone there is no seam to span. The zone is
// the seam, X samples wide, and the members overlap across it.
//
// This is where position and continuity are stamped and nowhere else, which is
// what lets the seek's pre-roll drive fill directly: it wants the samples
// without the bookkeeping, since a pre-rolled frame is one nobody is ever told
// the position of.
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
	if err := c.fill(dst); err != nil {
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
	//
	// A crossfade needs the override for the same reason and more of it: a
	// zone's chunks are the incoming member's freshly opened first chunks,
	// which is precisely the case above, so the override is what makes the
	// blend invisible downstream rather than something a crossfade works
	// around.
	dst.Discont = c.discont
	c.discont = false
	c.pos += int64(dst.N)
	return nil
}

// fill puts the timeline's next samples in dst, opening members, capturing
// tails and mixing zones as the geometry requires. It answers what comes next;
// ReadChunk owns what position and continuity to stamp on it (ADR-0006's
// boundary), and the seek's pre-roll drives this without any of that.
//
// io.EOF means the timeline is spent.
func (c *concat) fill(dst *audio.Buffer) error {
	for c.cur < len(c.members) {
		if c.med == nil {
			if err := c.open(c.cur); err != nil {
				return err
			}
		}
		// The open member has arrived at its own tail zone. Capturing it is
		// what ends this member, so there is no ordinary read left to do.
		n := c.bound()
		if n == 0 {
			if err := c.captureTail(); err != nil {
				return err
			}
			continue
		}
		dst.N = 0
		err := c.readBounded(dst, n)
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
		if c.blend != nil {
			c.mixBlend(dst)
		}
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
		c.releaseBlend()
		c.unpositioned = true
		return 0, err
	}
	c.unpositioned = false
	return pos, nil
}

// seekTo is SeekSample's body, split out so every failure inside it lands on
// the one latch above rather than each error path having to remember.
func (c *concat) seekTo(target int64) (int64, error) {
	// A blend describes where the stream was, and a seek is what makes that
	// wrong. Dropped before anything else, so no path below has to remember.
	c.releaseBlend()
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
	off := target - c.starts[i]
	// Inside member i's head zone, which is member i-1's tail: those samples
	// are a blend of two members, so they cannot be had by positioning one.
	// Unreachable at Crossfade 0, where no member has a head zone.
	if i > 0 && off < c.opts.Crossfade {
		return c.seekIntoBlend(i, off)
	}
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
	landed, err := c.seekBody(off)
	if err != nil {
		return 0, err
	}
	c.local = landed
	c.pos = c.starts[i] + landed
	c.discont = true
	return c.pos, nil
}

// seekBody positions the open member at local and holds the landing to the
// member's body: the part of it that is its own audio rather than a zone.
//
// The refusal is what container.Seeker's contract makes necessary. A member
// may land past the ask when its first sync point lies beyond the target, and
// seekMember reports that rather than pretending; with a crossfade, a landing
// past the body is a landing inside the member's own tail zone, where the next
// read would capture a tail shorter than X and blend it against a zone that
// declared X. The result is audio, so nothing downstream would notice.
//
// Landing exactly at the body is legal and needs no case of its own: bound()
// returns 0 there and the ordinary capture, advance and blend produce the zone.
// Unreachable at Crossfade 0, where the body is the whole member.
func (c *concat) seekBody(local int64) (int64, error) {
	landed, err := c.seekMember(local)
	if err != nil {
		return 0, err
	}
	if body := c.lens[c.cur] - c.tailOf(c.cur); landed > body {
		return 0, waxerr.New(waxerr.CodeSourceUnreadable, fmt.Sprintf(
			"waxflow: timeline member %d could not be positioned before its crossfade zone: "+
				"a seek to %d landed at %d, past the %d samples that are the member's own",
			c.cur, local, landed, body))
	}
	return landed, nil
}

// seekIntoBlend positions inside member i's head zone, off frames into it.
//
// The zone's samples come from two members and a curve, so it lands at the
// zone's start and pre-rolls: member i-1 is opened and read to its tail, which
// is captured exactly as a continuous read would capture it, and then off
// frames of the zone are pumped through fill and discarded. Seeking both sides
// into the middle of the zone would be the alternative, and this is cheaper as
// well as more honest.
//
// # What is exact here, and what is not
//
// The incoming member's half is bit-identical whether the zone is reached
// continuously or by this seek, unconditionally: member i is opened here and
// read from its own sample 0, which is exactly what advance would have done.
// That is what lazy opening buys (ADR-0009).
//
// The outgoing member's half is exact only on a uniform timeline. Member i-1
// is opened fresh and seeked to its body's end, so buildChain builds a cold
// chain and seekMember's back-off primes it by a frame or two. On a resampled
// member the FIR window is still zero-filled, and that transient lands at gain
// cos(0) = 1.0, at exactly zone[0], the one frame the curve promises is the
// outgoing member's own sample. A continuous read reaches that frame with a
// warm chain, so the two differ. A uniform timeline has no chain to be cold
// (see buildChain), which is both the gapless album and the two-slice declick,
// so it is every caller there is today.
//
// Priming the outgoing member is possible and is not worth it: it would be a
// new invariant for one case, and the consumer that would notice already
// primes. The segmented restart is exact even here, and for a reason worth
// keeping straight: its priming window (primeStarts) reaches back at least the
// chain's own prime before the first kept sample, which dwarfs any FIR window,
// so the cold-chain transient at zone[0] is discarded before p0 rather than
// served.
func (c *concat) seekIntoBlend(i int, off int64) (int64, error) {
	if err := c.closeMember(); err != nil {
		return 0, err
	}
	if err := c.open(i - 1); err != nil {
		return 0, err
	}
	// seekBody refuses a landing past the body, and a member seek never lands
	// short, so this lands exactly at the body's end: the tail captured below
	// is the full X the zone declares.
	landed, err := c.seekBody(c.lens[i-1] - c.opts.Crossfade)
	if err != nil {
		return 0, err
	}
	c.local = landed
	// Captures i-1's tail and opens i, which is what makes the zone below the
	// same zone a continuous read produces.
	if err := c.captureTail(); err != nil {
		return 0, err
	}
	if err := c.preRoll(off); err != nil {
		return 0, err
	}
	c.pos = c.starts[i] + off
	c.discont = true
	return c.pos, nil
}

// preRoll pumps n frames of the timeline through fill and discards them.
//
// It is dropOutput's twin one level up, and the level is the whole point: it
// drives fill rather than a chain, because a zone's samples come from two
// members and a curve, and no chain knows about any of them. The two pre-rolls
// do not know about each other either, which is what keeps dropOutput and
// seekMember member-level concerns below the blend.
//
// A right-sized buffer can never over-read past the target, so no carry
// survives the seek. The loop needs no zero-progress guard for the same reason
// dropOutput's does not: fill cannot answer with an empty chunk that is not
// io.EOF, since readMember refuses one.
func (c *concat) preRoll(n int64) error {
	for n > 0 {
		buf := audio.Get(c.fmt, int(min(n, int64(audio.StandardChunk))))
		err := c.fill(buf)
		got := int64(buf.N)
		audio.Put(buf)
		switch {
		case err == io.EOF:
			return waxerr.New(waxerr.CodeInternal,
				fmt.Sprintf("waxflow: timeline member %d ended inside a crossfade seek pre-roll", c.cur))
		case err != nil:
			return err
		}
		n -= got
	}
	return nil
}

// memberAt returns the member holding timeline sample target, which must be
// inside the timeline. It searches for the first member ending past the
// target rather than the last one starting at or before it, so a zero-length
// member (an odd but legal header) is skipped rather than selected.
//
// With a crossfade, starts[i+1] is no longer where member i ends: it ends at
// starts[i+1]+X, overlapping its successor by the zone they share. The search
// is right anyway, because what it actually computes is the last member that
// has begun at or before the target, and that is unaffected by where members
// end. A target inside a zone selects the later of the two members, which is
// the answer rather than a rounding of it: the zone is that member's head, and
// its head is where a seek into the zone has to start from (see seekIntoBlend).
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

// tailOf is member i's tail zone: X samples for every member but the last,
// which has no seam after it to blend across. Its twin, the head zone, is X
// for every member but the first, and it is not a function of its own because
// the head zone is not a thing this member reads: it is the previous member's
// tail, arriving as the blend.
func (c *concat) tailOf(i int) int64 {
	if i >= len(c.members)-1 {
		return 0
	}
	return c.opts.Crossfade
}

// bound is how many frames the next read may take, or -1 for no bound at all.
// It is the one place the zone geometry is read.
//
// The two bounded cases are disjoint, and that is the fit rule doing the work
// rather than luck. head+tail <= L means a member's tail zone starts at or
// after its head zone ends (equality lands them adjacent, with no body
// between), so at any moment at most one of them is in flight: while the blend
// is live the read is bounded by what is left of it, and once it is spent the
// read is bounded by the member's own tail zone. There is never a third case
// where a frame belongs to both.
//
// The -1 is absent rather than computed, and structurally so. Cap() == Stride,
// so bounding a read means a right-sized audio.Get plus a CopyFrames, and a
// bound computed for a member with no tail zone would put the last chunk of
// every member of every butt-joined timeline through that copy. A timeline at
// Crossfade 0 has no tail zones anywhere, so it takes today's path for the
// same reason it always did: there is nothing to bound (ADR-0009's zero-copy
// case).
func (c *concat) bound() int64 {
	if c.blend != nil {
		return int64(c.blend.N - c.blendOff)
	}
	tail := c.tailOf(c.cur)
	if tail == 0 {
		return -1
	}
	return c.lens[c.cur] - tail - c.local
}

// readBounded reads the open member's next chunk, holding it to n frames. n is
// bound()'s answer, taken as a parameter rather than re-read: fill has already
// established it is not 0 (that case is the tail capture, not a read), and a
// bound is a fact about one moment, so the caller that acted on it is the
// caller that should hand it over rather than ask twice and hope.
//
// A bound at or above dst's capacity is no bound: the read cannot reach it, so
// it fills dst directly and the copy is skipped. That keeps the body of a
// crossfaded member as zero-copy as a butt-joined one, and leaves the scratch
// for the one chunk per zone edge that genuinely has to stop short. A
// right-sized audio.Get can never over-read, because Get sets Stride to
// exactly the frames asked for (the same property dropOutput's scratch rests
// on).
func (c *concat) readBounded(dst *audio.Buffer, n int64) error {
	if n < 0 || n >= int64(dst.Cap()) {
		return c.readMember(dst)
	}
	buf := audio.Get(c.fmt, int(n))
	err := c.readMember(buf)
	if err == nil {
		audio.CopyFrames(dst, 0, buf, 0, buf.N)
		dst.N = buf.N
	}
	audio.Put(buf)
	return err
}

// captureTail reads the open member's tail zone into a blend buffer and opens
// the next member, whose head zone that buffer now is.
//
// The two are one step on purpose. A blend belongs to the member about to
// open, so publishing it before the advance would leave a failed advance's
// next read mixing a member into its own tail.
//
// It reads the member's rest to io.EOF rather than a count, which is what
// keeps both of the timeline's existing length checks firing unchanged. The
// buffer cannot overflow, and the reason is worth following: capture begins at
// local == L-X, so blend.N == local - (L-X) throughout, and count's own
// ceiling (local <= L) is therefore blend.N <= X. The bounds check and the
// length check are the same check, and count runs first, so it refuses exactly
// the read that would overrun the buffer. A member that instead ends early
// inside its own tail reaches advance's "delivered %d, declared %d" rather
// than blending against uninitialized frames.
//
// All of which rests on where capture begins, so that is checked rather than
// assumed. fill arrives here only on bound() == 0, which is the body's end by
// construction; seekIntoBlend arrives here from a member seek, and a member is
// a caller's Media whose seek can land wherever it likes. A landing short of
// the body would silently break the identity above and overflow the buffer,
// and audio.CopyFrames bounds its offsets by Stride only as a contract on this
// caller: the overrun would corrupt each channel into the next one's region
// and panic on the last. The whole point of the identity is that no other
// check stands behind it.
func (c *concat) captureTail() error {
	x := c.tailOf(c.cur)
	if want := c.lens[c.cur] - x; c.local != want {
		return waxerr.New(waxerr.CodeSourceUnreadable, fmt.Sprintf(
			"waxflow: timeline member %d is at sample %d, not the %d where its crossfade zone begins; "+
				"its seek landed somewhere the member's own headers say it should not have", c.cur, c.local, want))
	}
	blend := audio.Get(c.fmt, int(x))
	// One scratch for the whole capture: it is the same size every pass, so a
	// fresh one per chunk would be a pool round-trip per 4096 frames to hand
	// back exactly what it just took.
	buf := audio.Get(c.fmt, audio.StandardChunk)
	defer audio.Put(buf)
	for {
		buf.N = 0
		err := c.readMember(buf)
		if err == io.EOF {
			break
		}
		if err == nil {
			err = c.count(buf.N)
		}
		if err != nil {
			audio.Put(blend)
			return err
		}
		audio.CopyFrames(blend, blend.N, buf, 0, buf.N)
		blend.N += buf.N
	}
	if err := c.advance(); err != nil {
		audio.Put(blend)
		return err
	}
	c.blend, c.blendOff = blend, 0
	return nil
}

// mixBlend blends the captured tail into dst, which holds the incoming
// member's head zone, and retires the buffer when it is spent.
func (c *concat) mixBlend(dst *audio.Buffer) {
	blendFrames(dst, c.blend, c.blendOff, int(c.opts.Crossfade))
	c.blendOff += dst.N
	if c.blendOff >= c.blend.N {
		c.releaseBlend()
	}
}

// releaseBlend drops the blend buffer. It is called on exhaustion, on Close,
// and at the top of every seek, because a blend describes a position and a
// seek is what makes that position wrong.
func (c *concat) releaseBlend() {
	if c.blend != nil {
		audio.Put(c.blend)
		c.blend = nil
	}
	c.blendOff = 0
}

// blendFrames mixes out's frames (the outgoing member's tail, from frame off)
// into dst (the incoming member's head) with an equal-power curve, in place.
// x is the zone's full width; off is how far into it dst's first frame lies.
//
// Unexported, and not in dsp/gain, which is where a gain curve would look like
// it belongs. That package's convention is per-channel []float32 with no
// buffers and no strides; this takes two strided audio.Buffers, in either
// domain, and saturates at the envelope's rails. It cannot be a chain stage
// either, because a uniform member has no chain (see buildChain) and that is
// exactly the case a gapless album hits. ADR-0002: exported surface is cheap
// to add and expensive to remove, and there is one caller.
//
// cos out and sin in, so the two gains square to one and uncorrelated material
// holds its level across the zone where a linear fade dips 3 dB. t runs [0,1)
// over the zone and reaches 1 only at the frame after it, which is the
// half-open interval the zone is: the zone's first frame is the outgoing
// member's own sample exactly, and the first frame past it is the incoming
// member's. That is what makes a declick a declick, and it is also why there
// is no dither here. This is a mix at the working depth rather than a
// reduction to a shallower one, and dither would deny the zone's first frame
// the exactness the endpoints promise.
func blendFrames(dst, out *audio.Buffer, off, x int) {
	n, chans := dst.N, dst.Fmt.Channels
	// The domain branch is hoisted rather than per-sample, and the gains are
	// computed per frame rather than per sample: they do not depend on the
	// channel, so stereo halves the trigonometry and 8-channel divides it by
	// eight.
	if dst.Fmt.Type == audio.Float {
		// Float is deliberately not clamped. audio.Buffer says float is
		// nominal, and convertStage's own doc says the output quantizer
		// absorbs the overshoot. The asymmetry with the int path below is the
		// tree's, not this function's.
		for k := 0; k < n; k++ {
			sin, cos := math.Sincos(float64(off+k) / float64(x) * (math.Pi / 2))
			gi, go_ := float32(sin), float32(cos)
			for ch := 0; ch < chans; ch++ {
				d, o := &dst.F[ch*dst.Stride+k], out.F[ch*out.Stride+off+k]
				*d = o*go_ + *d*gi
			}
		}
		return
	}
	// The rails, and the rounding, are dither.Quantize's own (NewQuantizer
	// sets lo/hi to exactly these; Quantize applies them after a
	// math.Floor(x+0.5)). There is no audio.Buffer contract that an int sample
	// fits its bit depth, so this is the anchor that exists: the quantizer is
	// the only producer of int samples in the pipeline, and a crossfade is the
	// first one that is not it, so it inherits the obligation.
	//
	// It is a real overflow and not a theoretical one. An equal-power blend of
	// correlated material peaks at +3 dB (two identical signals at 0.707 each
	// reach 1.414 where they meet), and for an Int/16 envelope delivered to
	// FLAC, NewChain inserts nothing between here and the encoder.
	scale := math.Ldexp(1, dst.Fmt.BitDepth-1)
	lo, hi := -scale, scale-1
	for k := 0; k < n; k++ {
		sin, cos := math.Sincos(float64(off+k) / float64(x) * (math.Pi / 2))
		for ch := 0; ch < chans; ch++ {
			d := &dst.I[ch*dst.Stride+k]
			v := math.Floor(float64(out.I[ch*out.Stride+off+k])*cos + float64(*d)*sin + 0.5)
			if v < lo {
				v = lo
			} else if v > hi {
				v = hi
			}
			*d = int32(v)
		}
	}
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
	if want := c.lens[c.cur]; c.local > want {
		return waxerr.New(waxerr.CodeSourceUnreadable, fmt.Sprintf(
			"waxflow: timeline member %d holds more audio than the %d samples its headers declared", c.cur, want))
	}
	return nil
}

// advance closes the finished member and moves to the next.
func (c *concat) advance() error {
	if want := c.lens[c.cur]; c.local != want {
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
