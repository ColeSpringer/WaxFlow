package waxflow

import (
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/mix"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/satmath"
	"github.com/colespringer/waxflow/waxerr"
)

// concatContainer is the Info.Container a Concat reports: there is no file,
// so it names what the media is.
const concatContainer = "timeline"

// ToEnd is Slice's open-ended upper bound: the span runs to the end of the
// source.
const ToEnd = -1

// MeasuredMedia returns med declaring measured's length (the track
// MeasureLength returned) as its default track's, exact and with measured's
// SamplesCounted, so a run is held to the length the caller measured rather
// than the one the headers state (ADR-0002). It returns med itself when
// measured.Samples is negative, or when med already declares that length
// exactly with the same SamplesCounted; the same number declared as an
// estimate is not that. Warnings and Notes stay med's, live, and only what
// format.Forwarder forwards is forwarded. A Media that confirms its own
// length (format.LengthConfirmer) and is not returned itself is refused with
// CodeInvalidRequest and left open, since the wrapper would hide the
// confirming. A Concat confirms nothing, so it is restated and still answers
// Members (format.Composite).
func MeasuredMedia(med format.Media, measured container.Track) (format.Media, error) {
	info := med.Info()
	samples := measured.Samples
	if t := info.Default(); samples < 0 ||
		(t.Samples == samples && t.SamplesExact && t.SamplesCounted == measured.SamplesCounted) {
		return med, nil
	}
	if _, ok := med.(format.LengthConfirmer); ok {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"waxflow: MeasuredMedia of a Media that confirms its own length")
	}
	patched := *info
	patched.Tracks = slices.Clone(info.Tracks)
	// The default track is the one a span bounds; mirror Default's pick.
	idx := 0
	for i, t := range patched.Tracks {
		if t.Default {
			idx = i
			break
		}
	}
	patched.Tracks[idx].Samples = samples
	// A measurement is exact, so the advisory claim it replaces goes.
	patched.Tracks[idx].SamplesExact = true
	patched.Tracks[idx].SamplesAdvisory = false
	patched.Tracks[idx].SamplesCounted = measured.SamplesCounted
	m := measuredMedia{Forwarder: format.Forwarder{Media: med}, info: &patched}
	if members, ok := med.(format.Composite); ok {
		return measuredComposite{measuredMedia: m, Composite: members}, nil
	}
	return m, nil
}

// measuredComposite is a measuredMedia over a Composite: a restated length
// changes no member, so the members are the wrapped Media's.
type measuredComposite struct {
	measuredMedia
	format.Composite
}

// measuredMedia is a Media with its declared info replaced (MeasuredMedia).
type measuredMedia struct {
	format.Forwarder
	info *format.Info
}

// Info returns the patched description, its live fields refreshed from the
// inner media on every call (see format.Media).
func (m measuredMedia) Info() *format.Info {
	m.info.AdoptLive(m.Media.Info())
	return m.info
}

// Slice bounds med to the sample range [from, to) of its own timeline, as a
// Media whose sample 0 is med's sample from; ToEnd means to the end. The
// returned Media owns med and closes it. A nil med, and a range SpanTrack
// refuses, are CodeInvalidRequest. A span whose source ends short is held to
// its length: a bounded span to to-from, a ToEnd span to an exact length the
// source declared (unless the source reported damage on the way). The read
// then fails with CodeInvalidRequest when the source's total was unknown or
// advisory, CodeInternal for a length MeasureLength counted by decoding
// (container.Track.SamplesCounted), and CodeMalformedInput otherwise. A span
// through a chain with nothing to prime is bit-exact; a resampled one carries
// a short transient at its sample 0, except in a segmented run, which primes
// from the audio ahead of from (Headroom, ADR-0002).
func Slice(med format.Media, from, to int64) (format.Media, error) {
	s, err := newSlice(context.Background(), med, from, to)
	if err != nil {
		return nil, err // nil, not a Media holding a nil span
	}
	return s, nil
}

// newSlice is Slice returning the concrete span, built under ctx, which bounds
// the seek its first read runs (see ensureStart).
func newSlice(ctx context.Context, med format.Media, from, to int64) (*slice, error) {
	if med == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: Slice of a nil Media")
	}
	track := med.Info().Default()
	spanned, err := SpanTrack(track, from, to)
	if err != nil {
		return nil, err
	}
	s := &slice{med: med, ctx: ctx, from: from, limit: ToEnd, fmt: track.Fmt,
		blind: track.Samples < 0 || track.SamplesAdvisory, damage: damageIn(med)}
	// limit is the clamp; the track's Samples is what the span advertises. An
	// open span inherits the source's length, held against a shortfall
	// (endOfSource) but never clamped, which would truncate a mis-declared one.
	if to >= 0 {
		s.limit = to - from
	}
	in := med.Info()
	s.info = &format.Info{
		Container: in.Container,
		Tracks:    []container.Track{spanned},
		Chapters:  spanChapters(in.Chapters, from, s.limit, track.Fmt.Rate),
		Warnings:  in.Warnings,
		Notes:     in.Notes,
	}
	return s, nil
}

// spanChapters rebases chapters onto the window [from, from+limit): shifted
// so the window starts at zero, clipped to it, and dropped outside it. limit
// is ToEnd for an unbounded window, which clips nothing at the far end. A
// chapter straddling an edge keeps its title and the part inside. A zero End
// (the start-only form, see container.Chapter) stays zero: the next chapter
// is in the rebased list and the stream ends where the window does, so an
// end written here would be one the source never declared. A rate of zero
// places nothing, and the answer is nil.
func spanChapters(chapters []container.Chapter, from, limit int64, rate int) []container.Chapter {
	if len(chapters) == 0 || rate <= 0 {
		return nil
	}
	start := SampleTime(from, rate)
	end := time.Duration(-1)
	if limit >= 0 {
		end = SampleTime(from+limit, rate)
	}
	var out []container.Chapter
	for i, ch := range chapters {
		// Outside if it begins at or after the window's end, or is over
		// before its start. A start-only chapter beginning exactly at the
		// window's start is inside even when the next chapter shares its
		// instant, so a span from the top of a file lists what a probe of the
		// file lists.
		if end >= 0 && ch.Start >= end {
			continue
		}
		if e := chapterEnd(chapters, i); e >= 0 && e <= start && !(ch.End == 0 && ch.Start == start) {
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

// chapterEnd is where chapter i ends, for deciding whether it reaches a
// window: its own End, or the next chapter's start for the start-only form.
// -1 means it runs to the end of the stream (the last start-only chapter),
// which every window reaches. spanChapters filters on this but never writes
// it.
func chapterEnd(chapters []container.Chapter, i int) time.Duration {
	if e := chapters[i].End; e > 0 {
		return e
	}
	if i+1 < len(chapters) {
		return chapters[i+1].Start
	}
	return -1
}

// SampleTime is sample n's position on a stream's clock at rate, rounded
// toward zero. A span's chapter retiming and a merge's chapter offsets
// (internal/jobs) both use it, so both round alike. It splits off the whole
// seconds, so it does not overflow on a long file, as n*time.Second does.
func SampleTime(n int64, rate int) time.Duration {
	sec, rem := n/int64(rate), n%int64(rate)
	return time.Duration(sec)*time.Second + time.Duration(rem)*time.Second/time.Duration(rate)
}

// SpanTrack is the track a Slice of track to [from, to) presents: the same
// format and codec, the window's length, and no trims. It is a pure function
// of the header, and Slice resolves its track through it, so a plan made
// from a probed track and the run cannot disagree. to is exclusive; ToEnd
// means to the end of track. A negative from, a to below ToEnd, a to before
// from, and a from or to past a known track.Samples, exact or not, are
// CodeInvalidRequest (ADR-0002). A bounded span's length is exact; a ToEnd
// span keeps the source's length less from, and its claim about it.
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

	// Past the end is refused, not clamped: a span is content identity, so cut
	// points past the end describe some other file. The bound is the declared
	// length whatever SamplesExact says (ADR-0002).
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
		out.Samples, out.SamplesExact, out.SamplesAdvisory = to-from, true, false
	case total >= 0:
		out.Samples = total - from
	}
	// The Media delivered trimmed PCM, so a trim passed through would be cut
	// twice; the mid-run trims describe packets, and a decoded view has none.
	out.Delay, out.Padding, out.MidPadding, out.MidTrims = 0, 0, 0, nil
	out.TrimsUnread = false
	// The source's codec is kept: it names the decoder revision in a plan's
	// Versions.
	return out, nil
}

// slice is Slice's Media: one source, positioned at the window's start and
// cut off at its end. What has a right answer on the window is rebased onto
// it, and what has none is not answered. Chapters are rebased
// (spanChapters). format.Composite is not forwarded, since no member list
// describes a window. format.MixedWidth is, since a window onto mixed-width
// samples is mixed-width (ADR-0010). Warnings and Notes are med's, live.
// container.Indexer needs no forwarding: the engine's sidecar save wraps the
// Media under this one and fires on Close, which this delegates. It
// implements format.LengthConfirmer and not format.Walker: a span is not one
// measurable file, but it can make its declared length good before a header
// commits to it (ADR-0002).
type slice struct {
	med  format.Media
	info *format.Info
	fmt  audio.Format
	// ctx is the context the span was built under, which bounds the seek its
	// first read runs: a read carries none, and the window is a request's.
	ctx context.Context
	// from is the window's first sample on med's timeline.
	from int64
	// limit is the window's length, ToEnd for an unbounded one.
	limit int64
	// blind marks a span whose cut points were checked against no length the
	// source stood behind: its total was unknown or advisory. See endOfSource.
	blind bool

	// damage is med's Damage count (format.Info.Damage) when the span was
	// made, so a shortfall that comes with more is known to be reported.
	damage int

	// lost is the fill the window's reads delivered (format.Info.Lost) over
	// the runs of them already ended, and base is med's own count where the
	// run in flight entered the window. counting is false until a read is
	// positioned in the window: one in the headroom is not the window's.
	lost     int64
	base     int64
	counting bool

	pos     int64 // delivered-timeline position of the next frame out
	started bool  // med has been positioned at from
	discont bool
	closed  bool
}

// Info returns the span's description: Warnings and Notes refreshed live from
// med, Tracks and Chapters rebased once. Lost is the fill the window's own
// reads delivered. Truncated is the source's for a window that reaches its
// end, and 0 for one that stops short of it.
func (s *slice) Info() *format.Info {
	in := s.med.Info()
	s.info.AdoptLive(in)
	s.info.Lost = s.lost
	if s.counting {
		s.info.Lost += in.Lost - s.base
	}
	if !s.reachesEnd(in) {
		// A window that stops short of the end holds all it asked for.
		s.info.Truncated = 0
	}
	return s.info
}

// reachesEnd reports whether the window runs to the end of its source, whose
// Info is in: an open one does, and so does one bounded at or past the
// source's reported length. A cut draws the same line (cutResult.toEnd).
func (s *slice) reachesEnd(in *format.Info) bool {
	if s.limit < 0 {
		return true
	}
	total := in.Default().Samples
	return total >= 0 && s.from+s.limit >= total
}

// enter starts counting med's fill as the window's, from where med stands.
func (s *slice) enter() { s.base, s.counting = s.med.Info().Lost, true }

// leave ends the run of reads in flight and keeps what it counted.
func (s *slice) leave() {
	if s.counting {
		s.lost += s.med.Info().Lost - s.base
		s.counting = false
	}
}

// ConfirmLength implements format.LengthConfirmer: it runs what med defers
// (med's own ConfirmLength, or a walk) and re-derives the span's track
// through SpanTrack. A bounded span keeps its length; an open one adopts the
// confirmed total less from. A confirmed source that no longer covers the
// window is CodeMalformedInput, before a header commits to it. blind is
// re-derived from the confirmed track.
func (s *slice) ConfirmLength(ctx context.Context) error {
	declared := s.med.Info().Default().Samples
	// A nested span confirms itself the same way, one level down.
	if inner, ok := s.med.(format.LengthConfirmer); ok {
		if err := inner.ConfirmLength(ctx); err != nil {
			return err
		}
	} else if err := format.WalkMedia(ctx, s.med); err != nil {
		// Unconditional: a walk that finished some other way has measured
		// the payload, and only Walk, free by then, puts that on the Media.
		return err
	}
	to := int64(ToEnd)
	if s.limit >= 0 {
		to = s.from + s.limit
	}
	confirmed := s.med.Info().Default()
	spanned, err := SpanTrack(confirmed, s.from, to)
	if err != nil {
		end := "end"
		if to >= 0 {
			end = strconv.FormatInt(to, 10)
		}
		return waxerr.Wrap(waxerr.CodeMalformedInput, fmt.Sprintf(
			"waxflow: the source holds %d samples, less than the %d its headers declared; the span [%d, %s) no longer fits",
			confirmed.Samples, declared, s.from, end), err)
	}
	s.info.Tracks = []container.Track{spanned}
	// Derived, not cleared: the confirmed source may stand behind no length.
	s.blind = confirmed.Samples < 0 || confirmed.SamplesAdvisory
	return nil
}

// Headroom is the audio ahead of the window: from, plus what med can itself
// reach back to, all of which SeekSample delivers. Reporting from alone would
// under-report a nested span and quietly prime a chain short.
func (s *slice) Headroom() int64 {
	return s.from + format.MediaHeadroom(s.med)
}

// MixedWidth forwards med's answer (format.MixedWidth): a conversion refused
// over a mixed-width whole is refused over any window onto it.
func (s *slice) MixedWidth() bool { return format.MediaMixedWidth(s.med) }

func (s *slice) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.med.Close()
}

// ensureStart positions med at the window's start under ctx, once. A span
// from 0 never seeks, so a pure end trim needs no seekable source. A failed
// seek leaves the span unstarted and the next read retries it: the seek has
// one target and a failure writes nothing. A concat latches instead, since
// its seek moves the state its position depends on (ADR-0009).
func (s *slice) ensureStart(ctx context.Context) error {
	if s.started {
		return nil
	}
	if s.from == 0 {
		s.started = true
		s.enter()
		return nil
	}
	landed, err := s.med.SeekSample(ctx, s.from)
	if err != nil {
		return err
	}
	// container.Seeker lets a seek land past the ask at a late first sync
	// point, and then the span starts late and says so. A landing short of
	// from means the source ended first (a seek lands at or before its target
	// and decodes forward to it), so 0 is the honest position, from which
	// endOfSource counts. SeekSample does not floor: its negative targets are
	// headroom.
	s.pos = max(landed-s.from, 0)
	s.started = true
	s.enter()
	return nil
}

// ReadChunk fills dst from the window. med fills dst directly, except on the
// read that reaches an edge of the window (its end, or its sample 0 from the
// headroom), which goes through a right-sized buffer (read) so med never
// delivers past the edge and its Lost is split at sample 0. A chunk that
// crosses sample 0 is still one full chunk, read in two parts.
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
	if err := s.ensureStart(s.ctx); err != nil {
		return err
	}
	if s.limit >= 0 && s.pos >= s.limit {
		return io.EOF
	}
	dst.N = 0
	room := int64(dst.Cap())
	switch {
	case s.pos < 0:
		room = min(room, -s.pos)
	case s.limit >= 0:
		room = min(room, s.limit-s.pos)
	}
	err := s.read(dst, int(room))
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
	if s.pos < 0 && s.pos+int64(dst.N) == 0 {
		// The headroom ends here and the window's own count begins. The
		// chunk goes on into the window, as full as one that never stopped.
		s.enter()
		rest := int64(dst.Cap() - dst.N)
		if s.limit >= 0 {
			rest = min(rest, s.limit)
		}
		if rest > 0 {
			// The source ending here is the next read's to find. Any other
			// failure fails this read, with the span at sample 0.
			if err := s.read(dst, int(rest)); err != nil && err != io.EOF {
				s.pos = 0
				return err
			}
		}
	}
	dst.Pos = s.pos
	dst.Discont = s.discont
	s.discont = false
	s.pos += int64(dst.N)
	return nil
}

// read appends at most room frames of med to dst: straight in when dst is
// empty and holds no more than that, and through a buffer of exactly that
// capacity otherwise, since a Media fills what it is handed.
func (s *slice) read(dst *audio.Buffer, room int) error {
	if dst.N == 0 && room >= dst.Cap() {
		return s.med.ReadChunk(dst)
	}
	b := audio.Get(s.fmt, room)
	defer audio.Put(b)
	if err := s.med.ReadChunk(b); err != nil {
		return err
	}
	audio.CopyFrames(dst, dst.N, b, 0, b.N)
	dst.N += b.N
	return nil
}

// endOfSource answers med running out. A span is held, against a shortfall
// only, to an exact length it declared: a bounded span's to-from, or an open
// span's inherited exact total, so a plan's segment count never meets a tail
// it cannot fill. An open span whose source reported damage on the way ends
// there instead. It sees a short delivery, not a late landing (ensureStart).
// The code names whose length was wrong: unchecked cut points (blind) are an
// invalid request, a length MeasureLength counted by decoding is internal,
// and any other is the file's, malformed input.
func (s *slice) endOfSource() error {
	declared := s.limit
	if t := s.info.Tracks[0]; declared < 0 && t.SamplesExact && t.Samples >= 0 {
		declared = t.Samples
	}
	if declared < 0 || s.pos >= declared || s.limit < 0 && damageIn(s.med) > s.damage {
		return io.EOF
	}
	where := fmt.Sprintf("waxflow: the source ended %d samples into a span that declared %d", s.pos, declared)
	if !s.blind && s.info.Tracks[0].SamplesCounted {
		return waxerr.New(waxerr.CodeInternal, where+"; its length was counted by decoding this same source")
	}
	return shortSource(where, s.blind)
}

// shortSource is the error for a source that ran short of a length it stood
// behind, where says where. A blind run's cut points were never checked, so
// the request is at fault; otherwise the file is.
func shortSource(where string, blind bool) error {
	if blind {
		return waxerr.New(waxerr.CodeInvalidRequest, where+"; its cut points were never checked against a measured length")
	}
	return waxerr.New(waxerr.CodeMalformedInput, where+"; the file holds less audio than its headers declare")
}

// damageIn counts the damage med has reported (format.Info.Damage).
func damageIn(med format.Media) int { return med.Info().Damage }

// SeekSample repositions to target on the window's timeline. A negative
// target down to -Headroom() is legal and addresses the source's audio ahead
// of the window, which priming a chain for sample 0 asks for (see
// format.Headroomer); below that is CodeInvalidRequest. A target past a
// bounded window's end lands at its end.
func (s *slice) SeekSample(ctx context.Context, target int64) (int64, error) {
	// Bounded by what Headroom advertises, so priming by exactly that is never
	// refused.
	room := s.Headroom()
	switch {
	case s.closed:
		return 0, waxerr.New(waxerr.CodeInternal, "waxflow: SeekSample on a closed span")
	case target < -room:
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"waxflow: seek to %d is %d samples before the source's start; the span has %d samples of headroom",
			target, -target-room, room))
	}
	if s.limit >= 0 {
		target = min(target, s.limit)
	}
	// A seek delivers nothing: what med counts while it positions is no part of
	// the run before the seek or the one after it.
	counting := s.counting
	s.leave()
	landed, err := s.med.SeekSample(ctx, s.from+target)
	if err != nil {
		if counting {
			s.enter() // nothing moved, so the run goes on
		}
		return 0, err
	}
	s.started = true
	// Rebased, and not floored at 0: a landing inside the headroom is a
	// real position on this timeline, just a negative one.
	s.pos = landed - s.from
	if s.limit >= 0 {
		s.pos = min(s.pos, s.limit)
	}
	if s.pos >= 0 {
		s.enter()
	}
	s.discont = true
	return s.pos, nil
}

// ConcatSource is one member of a timeline: its track, as Probe reported it,
// so a timeline can be planned without opening anything, and a function that
// opens it on demand.
type ConcatSource struct {
	// Track describes the member from its headers, and Concat holds the member
	// to it: one that opens in another format or delivers another number of
	// samples fails the run. Its length must be one the decode matches
	// (Samples >= 0 and not SamplesAdvisory), or ConcatTrack refuses it;
	// measure such a member with MeasureLength and pass the track it returns.
	// A Concat confirms no length behind the caller's back, since that would
	// replace the declaration the run enforces.
	Track container.Track
	// Open opens the member's decodable media. Concat calls it when the
	// timeline reaches the member and closes the result on advance. It runs
	// lazily, mid-stream, so any context it binds must be the engine's, never
	// a request's: a live pipeline keeps encoding after its client leaves, and
	// a request context would kill a member's first read at a track boundary.
	// container.Contextual is for this handoff.
	Open func() (format.Media, error)
}

// ConcatOptions configures a Concat. Build one and hand the same value to the
// plan (PlanSegmentsTimeline or PlanTranscodeTimeline) and to the run
// (Concat); Engine.ConcatOptionsFor builds it for a transcode. Nothing checks
// that the two match. A Profile mismatch is a wrong cache key, a Crossfade
// mismatch a plan and a run that disagree about the timeline's length, and a
// Channels mismatch a conversion the run refuses (Engine.TimelineChannels).
type ConcatOptions struct {
	// Profile is the resampler profile for members not at the envelope's rate;
	// empty means resample.HQ. It must be the transcode's own
	// TranscodeOptions.ResampleProfile.
	Profile resample.Profile

	// Crossfade is how many envelope samples of each seam blend the two
	// members meeting there; N members deliver sum(len) - (N-1)*Crossfade.
	// Zero, the default, is a butt-join: sample len(a) is b's sample 0. There
	// is no nonzero default (ADR-0009). The blend is equal-power (cos/sin).
	// ConcatTrack refuses with CodeInvalidRequest a negative value, a member
	// shorter than the zones it carries (one on each edge member), and a zone
	// over maxCrossfadeBytes.
	Crossfade int64

	// Channels is the timeline's channel count. Zero keeps the envelope's, the
	// widest member's. Nonzero makes it the envelope's, and each member of
	// another count is conformed to it by its own chain before it meets its
	// siblings, as TranscodeOptions.Channels converts it read alone (ADR-0010).
	// Set it from Engine.TimelineChannels. ConcatTrack refuses a negative count
	// with CodeInvalidRequest, and with CodeUnsupportedFormat a count with no
	// layout convention, a member whose positions have no place in the target
	// (7.1 into 5.1), and a member at the count whose layout is not the
	// conventional one.
	Channels int
}

// maxCrossfadeBytes bounds one blend buffer at the largest buffer audio.Get
// pools (its top size class, 4 Mi samples of 4 bytes): one sample more and
// every seam allocates and drops 16 MiB (ADR-0009).
const maxCrossfadeBytes = 16 << 20

// concatLayout is the one walk ConcatTrack, Concat, ConcatBoundaries and
// CrossfadeSamples read a timeline's shape from: the envelope, each member's
// normalized length (lens), the starts, the total, and every refusal. starts
// has len(tracks)+1 entries: starts[i] is member i's first sample and the
// last is the total. Under a crossfade of X, starts[i+1]-starts[i] is
// lens[i]-X, while member i still occupies lens[i] samples.
func concatLayout(tracks []container.Track, opts ConcatOptions) (env audio.Format, lens, starts []int64, total int64, err error) {
	if len(tracks) == 0 {
		return audio.Format{}, nil, nil, 0, waxerr.New(waxerr.CodeInvalidRequest,
			"waxflow: a timeline needs at least one member")
	}
	env = audio.Format{Type: audio.Int}
	for i, t := range tracks {
		if err := t.Fmt.Valid(); err != nil {
			return audio.Format{}, nil, nil, 0, waxerr.Wrap(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("waxflow: timeline member %d", i), err)
		}
		// Refused here, where the cause can be named: the run enforces each
		// member's declared length and would fail minutes into a decode with a
		// message about the file.
		if t.Samples < 0 || t.SamplesAdvisory {
			what := "has no declared length"
			if t.Samples >= 0 {
				what = fmt.Sprintf("declares an advisory length (%d samples) the decode is not expected to match", t.Samples)
			}
			return audio.Format{}, nil, nil, 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: timeline member %d %s; measure it before planning a timeline "+
					"(waxflow.MeasureLength)", i, what))
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
	// The widest member: the crossfade bound is checked at it too, so a fold
	// never loosens a bound passed at the wider width.
	widest := env.Channels
	if opts.Channels != 0 {
		if opts.Channels < 0 {
			return audio.Format{}, nil, nil, 0, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("waxflow: negative timeline channel count %d", opts.Channels))
		}
		// Checked on the requested count alone: a target the mix node cannot
		// build is refused here, not one member into the run.
		if audio.DefaultLayout(opts.Channels) == 0 {
			return audio.Format{}, nil, nil, 0, waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("waxflow: no layout convention for %d channels", opts.Channels))
		}
		env.Channels = opts.Channels
	}
	env.Layout = audio.DefaultLayout(env.Channels)

	// A member at the envelope's count runs no mix, so its layout must already
	// be the envelope's; every other must mix into it (ADR-0009).
	for i, t := range tracks {
		// Resolved with dsp.NewChain's zero-mask fallback, so both rules read
		// the same layout for a member that declares none.
		layout := t.Fmt.Layout
		if layout == 0 {
			layout = audio.DefaultLayout(t.Fmt.Channels)
		}
		if t.Fmt.Channels == env.Channels {
			if layout != env.Layout {
				return audio.Format{}, nil, nil, 0, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
					"waxflow: timeline member %d lays its %d channels out as %v, not the conventional %v; "+
						"a timeline normalizes channel counts, not speaker assignments",
					i, t.Fmt.Channels, layout, env.Layout))
			}
			continue
		}
		if _, err := mix.For(layout, env.Layout); err != nil {
			return audio.Format{}, nil, nil, 0, waxerr.Wrap(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("waxflow: timeline member %d cannot be mixed into the %d-channel envelope",
					i, env.Channels), err)
		}
	}

	lens = make([]int64, len(tracks))
	starts = make([]int64, len(tracks)+1)
	for i, t := range tracks {
		lens[i] = concatMemberSamples(t, env)
		total += lens[i]
		// The next member begins at this one's tail zone, X before its end. The
		// last carries no tail, so starts[N] is the total.
		tail := int64(0)
		if i < len(tracks)-1 {
			tail = opts.Crossfade
		}
		starts[i+1] = starts[i] + lens[i] - tail
	}
	if err := checkCrossfade(lens, env.Rate, max(widest, env.Channels), opts.Crossfade); err != nil {
		return audio.Format{}, nil, nil, 0, err
	}
	// One zone per seam, after every member's own ceil; starts[N] agrees.
	total -= int64(len(tracks)-1) * opts.Crossfade
	return env, lens, starts, total, nil
}

// ConcatTrack is the synthetic track a Concat of these members presents:
// PCM in the envelope format (the highest rate, the most channels or
// opts.Channels, the deepest bit depth, float if any member is float), the
// summed normalized length less one Crossfade per seam, exact, and no trims.
// It is a pure function of the headers, so a plan and a run agree
// (ADR-0009). It refuses an empty list and a member whose length is unknown
// or advisory (see ConcatSource.Track) with CodeInvalidRequest; an invalid
// member format, and a member layout the envelope cannot take, with
// CodeUnsupportedFormat; and what ConcatOptions lists. The track carries no
// mark of a timeline, so a plan built from it alone will not refuse a
// conversion across a seam; the run does, off the Media. Ask
// Engine.TimelineChannels for the width first.
func ConcatTrack(tracks []container.Track, opts ConcatOptions) (container.Track, error) {
	env, _, _, total, err := concatLayout(tracks, opts)
	if err != nil {
		return container.Track{}, err
	}
	return concatSynthetic(env, total), nil
}

// concatSynthetic is the track ConcatTrack returns and a Concat reports,
// built once so the two agree. SamplesExact holds because Concat fails a run
// whose members miss their declared lengths.
func concatSynthetic(env audio.Format, total int64) container.Track {
	return container.Track{
		Codec:        codec.PCM,
		Fmt:          env,
		Samples:      total,
		SamplesExact: true,
		Default:      true,
	}
}

// MemberBoundary is one member's place on a concatenated timeline, in
// samples at the envelope rate: OffsetSamples is its start on the timeline
// and DurationSamples its own normalized length. Under a crossfade of X the
// members overlap: member i occupies the DurationSamples from its offset,
// which run X past the next member's offset, so the durations sum to the
// total plus (N-1)X. At X=0 the members tile.
type MemberBoundary struct {
	OffsetSamples   int64 `json:"offsetSamples"`
	DurationSamples int64 `json:"durationSamples"`
}

// ConcatBoundaries reports each member's MemberBoundary and the envelope
// format they are measured on, from the headers alone. It reads the layout
// ConcatTrack and Concat read, so a boundary is where the run plays the
// member; under a crossfade the boundaries overlap.
func ConcatBoundaries(tracks []container.Track, opts ConcatOptions) ([]MemberBoundary, audio.Format, error) {
	env, lens, starts, _, err := concatLayout(tracks, opts)
	if err != nil {
		return nil, audio.Format{}, err
	}
	bounds := make([]MemberBoundary, len(tracks))
	for i := range tracks {
		bounds[i] = MemberBoundary{OffsetSamples: starts[i], DurationSamples: lens[i]}
	}
	return bounds, env, nil
}

// mixedWidthConversion is the one predicate behind both halves of the width
// rule (ADR-0010): Engine.TimelineChannels answers a count exactly when it
// holds, and refuseMixedWidthConversion refuses exactly when it holds. It
// holds when channels asks for a count other than env's over members that do
// not all share env's.
func mixedWidthConversion(members []container.Track, env audio.Format, channels int) bool {
	if channels == 0 || channels == env.Channels {
		return false
	}
	for _, t := range members {
		if t.Fmt.Channels != env.Channels {
			return true
		}
	}
	return false
}

// CrossfadeSamples converts a crossfade in seconds, as the wire spells it, to
// the envelope samples ConcatOptions.Crossfade takes, rounded to the nearest.
// The rate is the one the run blends at, a pure function of the members'
// formats, so a plan and a run convert one signed number alike. A
// non-positive or NaN seconds is 0, without reading tracks. A value too large
// clamps to math.MaxInt64, which ConcatTrack then refuses; the bounds are
// its to enforce. A layout error is returned as it is.
func CrossfadeSamples(tracks []container.Track, seconds float64) (int64, error) {
	if !(seconds > 0) {
		// False for <=0 and for NaN.
		return 0, nil
	}
	env, _, _, _, err := concatLayout(tracks, ConcatOptions{})
	if err != nil {
		return 0, err
	}
	x := math.Round(seconds * float64(env.Rate))
	if x >= float64(math.MaxInt64) {
		return math.MaxInt64, nil
	}
	return int64(x), nil
}

// TimelineChannels is the width to build a timeline of these members at when
// it is transcoded with opts: the count for ConcatOptions.Channels, or 0 to
// leave that zero. The engine answers because the width is a fact about the
// output row. It is nonzero only when the delivered count differs from the
// envelope's and some member's does too (mixedWidthConversion), so a queue
// without the defect keeps its chain and its cache keys (ADR-0010). It logs
// nothing; the run announces the fold. A plan error is returned as it is.
func (e *Engine) TimelineChannels(tracks []container.Track, opts TranscodeOptions) (int, error) {
	env, err := ConcatTrack(tracks, ConcatOptions{})
	if err != nil {
		return 0, err
	}
	plan, err := e.PlanTranscode(env, opts)
	if err != nil {
		return 0, err
	}
	if !mixedWidthConversion(tracks, env.Fmt, plan.Format.Channels) {
		return 0, nil
	}
	return plan.Format.Channels, nil
}

// ConcatOptionsFor is the ConcatOptions convention as one call: the options a
// timeline of tracks transcoded with opts must be built and planned with. The
// profile is opts.ResampleProfile, crossfade is in envelope samples (see
// CrossfadeSamples) and passes through, and the width is TimelineChannels'
// answer, whose errors propagate as they are.
func (e *Engine) ConcatOptionsFor(tracks []container.Track, crossfade int64, opts TranscodeOptions) (ConcatOptions, error) {
	ch, err := e.TimelineChannels(tracks, opts)
	if err != nil {
		return ConcatOptions{}, err
	}
	return ConcatOptions{Profile: opts.ResampleProfile, Crossfade: crossfade, Channels: ch}, nil
}

// refuseMixedWidthConversion refuses with CodeInvalidRequest a conversion to
// channels of a Media whose samples were conformed from several widths
// (format.MixedWidth, which a slice and every wrapper forward): dsp/mix's
// fold of a placed member is not its own fold, and a widening moves a mono
// member off the center (ADR-0010). A channels of 0 or of in's own count
// converts nothing.
func refuseMixedWidthConversion(med format.Media, in audio.Format, channels int) error {
	if channels == 0 || channels == in.Channels || !format.MediaMixedWidth(med) {
		return nil
	}
	return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
		"waxflow: this timeline's members were conformed to %d channels from other widths, so converting it to "+
			"%d is not any member's own conversion (dsp/mix normalizes each row over every source column, "+
			"silent ones included); build the timeline at the delivered width instead: ConcatOptions.Channels, "+
			"see Engine.TimelineChannels",
		in.Channels, channels))
}

// checkCrossfade holds a crossfade x to what the members can carry, refusing
// with CodeInvalidRequest a negative x, a zone over maxCrossfadeBytes at
// channels, and a member shorter than the zones it carries. lens are the
// members' normalized lengths, at rate. channels is the wider of the
// envelope's count and the widest member's, so a fold never loosens a bound
// the members pass at their own width (ADR-0010).
func checkCrossfade(lens []int64, rate, channels int, x int64) error {
	if x == 0 {
		return nil
	}
	if x < 0 {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: negative crossfade %d", x))
	}
	// Divide, not multiply: x*ch*4 overflows on exactly the inputs this
	// refuses, so the message quotes no byte count either.
	if perFrame := int64(channels) * 4; x > maxCrossfadeBytes/perFrame {
		limit := maxCrossfadeBytes / perFrame
		// In the caller's units: frames, and seconds at the rate in use.
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"waxflow: a crossfade of %d samples is more than this timeline can blend; the most it can is "+
				"%d samples (%.1f s at %d Hz, %d channels), which is the largest buffer the sample pool holds",
			x, limit, float64(limit)/float64(rate), rate, channels))
	}
	// head+tail <= L: an edge member carries one zone, a lone member none.
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

// concatMemberSamples is the member's length on the envelope timeline: the
// resampler's own exact output count, so the run's prefix sum and the plan's
// length are one arithmetic.
func concatMemberSamples(t container.Track, env audio.Format) int64 {
	return resample.OutputLen(t.Samples, t.Fmt.Rate, env.Rate)
}

// concatSpec is the chain that takes one member to the envelope, shared by
// the version accounting (timelineVersions) and the run. Dither is pinned to
// TPDF, which keys on absolute position and holds no history, so a member
// started mid-stream requantizes as a continuous run does; Shaped carries
// error feedback and would make a restarted worker's segments differ.
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

// Concat sequences members into one gapless format.Media whose sample len(a)
// is b's sample 0, unless opts.Crossfade blends the seam (ADR-0009). Its track
// is ConcatTrack's. It returns ConcatTrack's refusals, and
// CodeInvalidRequest for a member with no Open. Members open on demand and
// close on advance, so one is open at a time, and a member already in the
// envelope's format is read through with no chain. The returned Media opens
// nothing until it is read or seeked, and closes what it opened on Close. It
// satisfies format.Composite, so a consumer keying its own cache can reach
// the members' tracks.
func Concat(members []ConcatSource, opts ConcatOptions) (format.Media, error) {
	tracks := make([]container.Track, len(members))
	for i := range members {
		if members[i].Open == nil {
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("waxflow: timeline member %d has no Open function", i))
		}
		tracks[i] = members[i].Track
	}
	env, lens, starts, total, err := concatLayout(tracks, opts)
	if err != nil {
		return nil, err
	}
	return &concat{
		members: members,
		tracks:  tracks,
		opts:    opts,
		fmt:     env,
		starts:  starts,
		lens:    lens,
		info:    &format.Info{Container: concatContainer, Tracks: []container.Track{concatSynthetic(env, total)}},

		truncated: make([]int64, len(members)),
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
	// starts[i] is member i's first timeline sample and starts[len(members)]
	// the total; lens[i] is member i's normalized length. A crossfade of X
	// makes them differ (starts[i+1]-starts[i] is lens[i]-X), so a length read
	// off starts is short by the overlap and desyncs the prefix sum.
	starts []int64
	lens   []int64

	cur   int          // the open member's index; len(members) past the end
	med   format.Media // nil when no member is open
	chain *dsp.Chain   // nil when the open member needs no normalization

	// damaged is the finished members' Damage, summed: a count, so a member
	// closed and reopened by a seek adds its own again.
	damaged int
	// lost is the finished members' Lost, summed the same way, each on the
	// envelope's timeline (see memberLost). lostBase is the open member's
	// own count as it was opened.
	lost, lostBase int64
	// truncated is each member's Truncated as its read last left it, on the
	// envelope's timeline. It is a fact about the member's file, so a member
	// reopened by a seek states it again, not twice.
	truncated []int64
	// found and noted are the finished members' Warnings and Notes, each
	// line prefixed with its member's index, folded in as a member closes;
	// Info adds the open member's live lists behind them.
	found, noted []string

	local   int64 // the open member's own position, on the envelope timeline
	pos     int64 // timeline position of the next frame out
	discont bool
	closed  bool
	// blend is the previous member's captured tail while the open member's
	// head zone is read (the same region of the timeline); nil when no zone is
	// in flight, and always at Crossfade 0. It outlives closeMember, since the
	// tail's member closes before its zone is mixed.
	blend    *audio.Buffer
	blendOff int // frames of blend already mixed out
	// unpositioned latches a failed seek: pos no longer describes where the
	// next sample comes from, so reading is refused until a seek succeeds.
	unpositioned bool
}

// Info returns the timeline's description: the synthetic track, and the
// members' Warnings and Notes as the timeline reaches them, each line
// prefixed with the member's index (finished members' as they closed, the
// open member's live), so the full lists exist once it is read to its end.
// Damage and Lost sum the members' counts, so Lost is exact for a timeline
// read through, and a crossfade zone counts each member's own fill. A seek
// counts nothing, which leaves out what a member's chain read ahead of the
// landing and, in a zone, the outgoing member's fill in the rest of it.
// Truncated sums the members'.
func (c *concat) Info() *format.Info {
	c.info.Warnings = append([]string(nil), c.found...)
	c.info.Notes = append([]string(nil), c.noted...)
	c.info.Damage = c.damaged
	c.info.Lost = c.lost
	if c.med != nil {
		in := c.med.Info()
		c.info.Warnings = appendMemberLines(c.info.Warnings, c.cur, in.Warnings)
		c.info.Notes = appendMemberLines(c.info.Notes, c.cur, in.Notes)
		c.info.Damage += in.Damage
		c.info.Lost += c.memberLost(in)
		c.memberTruncated(in)
	}
	c.info.Truncated = 0
	for _, n := range c.truncated {
		c.info.Truncated = satmath.Add(c.info.Truncated, n)
	}
	return c.info
}

// memberLost is the open member's Lost since it was opened, in, on the
// envelope's timeline: a member the timeline resamples lost its samples at
// its own rate.
func (c *concat) memberLost(in *format.Info) int64 {
	n := in.Lost - c.lostBase
	if c.chain != nil {
		l, m := c.chain.Ratio()
		n = rescale(n, l, m)
	}
	return n
}

// memberTruncated records the open member's Truncated, in, on the envelope's
// timeline. A member whose walk is deferred states it only once its read has
// reached the end, so a later look never lowers it.
func (c *concat) memberTruncated(in *format.Info) {
	n := in.Truncated
	if c.chain != nil {
		l, m := c.chain.Ratio()
		n = rescale(n, l, m)
	}
	c.truncated[c.cur] = max(c.truncated[c.cur], n)
}

// rescale maps a count of samples across a chain's rate change of l/m, output
// over input, to the nearest sample (dsp.Chain.Ratio). A count too large to
// scale saturates, since a header can declare any length.
func rescale(n int64, l, m int) int64 {
	return satmath.MulDivRound(n, int64(l), int64(m))
}

// appendMemberLines appends a member's lines to dst under its index, once
// each: a member closed and reopened by a seek reports the same findings.
func appendMemberLines(dst []string, member int, lines []string) []string {
	for _, l := range lines {
		if line := fmt.Sprintf("member %d: %s", member, l); !slices.Contains(dst, line) {
			dst = append(dst, line)
		}
	}
	return dst
}

// Members reports the members' tracks (format.Composite).
func (c *concat) Members() []container.Track {
	return append([]container.Track(nil), c.tracks...)
}

// MixedWidth reports whether any member's channel count differs from the
// timeline's, after ConcatOptions.Channels (format.MixedWidth). It reads the
// declared formats, so it answers before any member opens, and it cannot see
// a member that is itself a mixed-width Concat (ADR-0010).
func (c *concat) MixedWidth() bool {
	for _, t := range c.tracks {
		if t.Fmt.Channels != c.fmt.Channels {
			return true
		}
	}
	return false
}

func (c *concat) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.releaseBlend()
	return c.closeMember()
}

// ReadChunk fills dst from the timeline. A chunk never spans a seam: the
// member in flight is read to its end and the next fills a fresh chunk, so a
// boundary costs one short chunk. A crossfade zone is the seam, and its
// chunks hold both members. Position and continuity are stamped here and
// nowhere else, so the seek's pre-roll drives fill without them.
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
	// An override, never an OR with the member's own: a fresh member's first
	// chunk can carry Discont, which would drain the downstream resampler and
	// re-create the seam, and a zone's chunks are such chunks (ADR-0009).
	dst.Discont = c.discont
	c.discont = false
	c.pos += int64(dst.N)
	return nil
}

// fill puts the timeline's next samples in dst, opening members, capturing
// tails and mixing zones; ReadChunk stamps position and continuity
// (ADR-0006). io.EOF means the timeline is spent.
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
// opened and positioned, and the rest follow in order. A target past the end
// lands at the end; a negative one is CodeInvalidRequest. A failed seek
// latches the timeline unpositioned, and reads refuse until a seek succeeds:
// a failure part way leaves pos describing where the members no longer are,
// and a reopened member would deliver from its start under the old pos.
func (c *concat) SeekSample(ctx context.Context, target int64) (int64, error) {
	switch {
	case c.closed:
		return 0, waxerr.New(waxerr.CodeInternal, "waxflow: SeekSample on a closed timeline")
	case target < 0:
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: negative seek target")
	}
	counted := c.lost
	if c.med != nil {
		counted += c.memberLost(c.med.Info())
	}
	pos, err := c.seekTo(ctx, target)
	if err != nil {
		c.closeMember()
		c.releaseBlend()
		c.unpositioned = true
	}
	// A seek counts nothing: what it positioned counted on the way is set
	// aside (see Info).
	c.lost = counted
	if c.med != nil {
		c.lostBase = c.med.Info().Lost
	}
	if err != nil {
		return 0, err
	}
	c.unpositioned = false
	return pos, nil
}

// seekTo is SeekSample's body, split out so every failure inside it lands on
// the one latch above rather than each error path having to remember.
func (c *concat) seekTo(ctx context.Context, target int64) (int64, error) {
	// A blend describes the position a seek leaves.
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
	// Inside member i's head zone, which is member i-1's tail: a blend no one
	// member's seek reaches.
	if i > 0 && off < c.opts.Crossfade {
		return c.seekIntoBlend(ctx, i, off)
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
	landed, err := c.seekBody(ctx, off)
	if err != nil {
		return 0, err
	}
	c.local = landed
	c.pos = c.starts[i] + landed
	c.discont = true
	return c.pos, nil
}

// seekBody positions the open member at local and refuses with
// CodeMalformedInput a landing past the member's body (all of it but its
// tail zone). container.Seeker allows a late landing, and one inside the
// tail zone would capture a tail shorter than X and blend it against a zone
// that declared X, silently. Landing exactly at the body's end is legal:
// bound() is 0 there and the ordinary capture follows.
func (c *concat) seekBody(ctx context.Context, local int64) (int64, error) {
	landed, err := c.seekMember(ctx, local)
	if err != nil {
		return 0, err
	}
	if body := c.lens[c.cur] - c.tailOf(c.cur); landed > body {
		// The first sync point lies past the whole body, so the member's
		// declared length is not its stream's.
		return 0, waxerr.New(waxerr.CodeMalformedInput, fmt.Sprintf(
			"waxflow: timeline member %d could not be positioned before its crossfade zone: "+
				"a seek to %d landed at %d, past the %d samples that are the member's own",
			c.cur, local, landed, body))
	}
	return landed, nil
}

// seekIntoBlend positions off frames into member i's head zone, which is two
// members and a curve: it seeks member i-1 to its body's end, captures its
// tail as a continuous read does, opens member i, and pre-rolls off frames
// through fill. The incoming half is bit-identical to a continuous read; the
// outgoing half is exact only on a uniform timeline, since member i-1's
// chain starts cold (ADR-0009).
func (c *concat) seekIntoBlend(ctx context.Context, i int, off int64) (int64, error) {
	if err := c.closeMember(); err != nil {
		return 0, err
	}
	if err := c.open(i - 1); err != nil {
		return 0, err
	}
	// seekBody refuses a landing past the body and captureTail one short of
	// it, so the tail captured below is the full X the zone declares.
	landed, err := c.seekBody(ctx, c.lens[i-1]-c.opts.Crossfade)
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

// preRoll pumps n frames of the timeline through fill and discards them. It
// drives fill, not a chain, because a zone's samples come from two members
// and a curve. A right-sized buffer never over-reads, and fill answers no
// empty chunk but io.EOF (readMember), so the loop needs no progress guard.
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
// inside the timeline: the first whose successor starts past it, so a
// zero-length member is skipped. A target in a crossfade zone selects the
// later member, whose head the zone is (see seekIntoBlend).
func (c *concat) memberAt(target int64) int {
	return sort.Search(len(c.members), func(i int) bool { return target < c.starts[i+1] })
}

// open opens member i and wires its normalization.
func (c *concat) open(i int) error {
	med, err := c.members[i].Open()
	if err != nil {
		return err
	}
	// A member that opens as another format would fail deep in the chain, as a
	// buffer mismatch; name it here.
	if got := med.Info().Default().Fmt; got != c.tracks[i].Fmt {
		med.Close()
		return waxerr.New(waxerr.CodeSourceChanged, fmt.Sprintf(
			"waxflow: timeline member %d opened as %v, its headers declared %v", i, got, c.tracks[i].Fmt))
	}
	c.med, c.cur, c.local = med, i, 0
	c.lostBase = med.Info().Lost
	if err := c.buildChain(); err != nil {
		c.closeMember()
		return err
	}
	return nil
}

// buildChain wires the open member's normalization to the envelope, or none
// when it already matches. It always builds a fresh chain: a reused one
// would take a seek's Discont as a splice and drain the pre-seek audio first.
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
		// The chain cannot name the member, which is what a caller needs.
		return waxerr.Annotate(fmt.Sprintf("member %d", c.cur), err)
	}
	c.chain = chain
	return nil
}

// readMember pulls the open member's next normalized chunk and refuses, with
// CodeInternal, an empty chunk that is not io.EOF, which would spin the
// timeline's reader. dsp.NewSource checks the chain path; this is the check
// for a member read straight through.
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

// tailOf is member i's tail zone: X for every member but the last. The head
// zone (X for all but the first) is the previous member's tail, arriving as
// the blend, so it has no function.
func (c *concat) tailOf(i int) int64 {
	if i >= len(c.members)-1 {
		return 0
	}
	return c.opts.Crossfade
}

// bound is how many frames the next read may take, or -1 for no bound; it is
// the one place the zone geometry is read. The fit rule (head+tail <= L)
// keeps a member's zones disjoint, so the live blend bounds the read, then
// the member's tail zone. A member with no tail zone, every one at Crossfade
// 0, is unbounded, so a butt-join never pays the bounded read's copy.
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

// readBounded reads the open member's next chunk, at most n frames, n being
// fill's bound() answer (never 0). A bound at or past dst's capacity reads
// into dst directly, so only a chunk at a zone edge goes through a scratch,
// which audio.Get sizes exactly and so never over-reads.
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

// captureTail reads the open member's tail zone into a blend buffer and
// advances to the next member, whose head zone the buffer is; publishing it
// before a failed advance would mix a member into its own tail. It reads to
// io.EOF, so count and advance hold the member to its length as on any read,
// and since capture starts at local == L-X, count's ceiling (local <= L) is
// the buffer's (blend.N <= X). That start is checked for this reason: a seek
// landing elsewhere is CodeMalformedInput, since a short start would overrun
// the buffer with no other check behind it.
func (c *concat) captureTail() error {
	x := c.tailOf(c.cur)
	if want := c.lens[c.cur] - x; c.local != want {
		// The member's headers do not describe the stream its seek found.
		return waxerr.New(waxerr.CodeMalformedInput, fmt.Sprintf(
			"waxflow: timeline member %d is at sample %d, not the %d where its crossfade zone begins; "+
				"its seek landed somewhere the member's own headers say it should not have", c.cur, c.local, want))
	}
	blend := audio.Get(c.fmt, int(x))
	// One scratch for the capture, not a pool round trip per chunk.
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

// blendFrames mixes out's frames from off (the outgoing member's tail) into
// dst (the incoming member's head) in place, with an equal-power curve over
// a zone x wide: cos out, sin in. t runs over [0,1) of the zone, so the
// zone's first frame is the outgoing member's own sample. It takes no
// dither, being a mix at the working depth (ADR-0009). Int samples round and
// clamp at the quantizer's rails; float is left to the output quantizer
// (ADR-0010).
func blendFrames(dst, out *audio.Buffer, off, x int) {
	n, chans := dst.N, dst.Fmt.Channels
	// The gains are per frame: they do not depend on the channel.
	if dst.Fmt.Type == audio.Float {
		// Unclamped: float is nominal, and the output quantizer absorbs the
		// overshoot (convertStage).
		for k := range n {
			sin, cos := math.Sincos(float64(off+k) / float64(x) * (math.Pi / 2))
			gi, go_ := float32(sin), float32(cos)
			for ch := range chans {
				d, o := &dst.F[ch*dst.Stride+k], out.F[ch*out.Stride+off+k]
				*d = o*go_ + *d*gi
			}
		}
		return
	}
	// Clamped at dither.Quantize's rails, with its rounding: a blend of
	// correlated material peaks at +3 dB, and no later stage (an Int/16
	// envelope to FLAC has none) clamps an int sample to its depth.
	scale := math.Ldexp(1, dst.Fmt.BitDepth-1)
	lo, hi := -scale, scale-1
	for k := range n {
		sin, cos := math.Sincos(float64(off+k) / float64(x) * (math.Pi / 2))
		for ch := range chans {
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
func (c *concat) seekMember(ctx context.Context, local int64) (int64, error) {
	if c.chain == nil {
		return c.med.SeekSample(ctx, local)
	}
	// Mapped back to the source rate, floored, and backed off one more sample
	// so the landing is at or before the target; the slop is discarded below.
	src := local
	if l, m := c.chain.Ratio(); l != m {
		src = local * int64(m) / int64(l)
		if src > 0 {
			src--
		}
	}
	landed, err := c.med.SeekSample(ctx, src)
	if err != nil {
		return 0, err
	}
	// Where the fresh chain's first chunk will start, without reading one:
	// every kernel here anchors on the source position it is handed, and the
	// resampler's anchor is the same ceil(pos*L/M) that OutputSamples is.
	out := c.chain.OutputSamples(landed)
	if out >= local {
		// A late first sync point, which container.Seeker permits: report where
		// the stream starts.
		return out, nil
	}
	if err := c.dropOutput(local - out); err != nil {
		return 0, err
	}
	return local, nil
}

// dropOutput decodes and discards n frames of the open member's normalized
// output, the slop between the source seek's landing and the ask. A scratch
// sized to what is left never over-reads, and dsp.NewSource refuses an empty
// chunk that is not io.EOF, so the loop needs no progress guard.
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

// count advances the open member's position and refuses with
// CodeMalformedInput a member delivering more than its headers declared:
// drift in one member desyncs every position after it (ADR-0009).
func (c *concat) count(n int) error {
	c.local += int64(n)
	if want := c.lens[c.cur]; c.local > want {
		return waxerr.New(waxerr.CodeMalformedInput, fmt.Sprintf(
			"waxflow: timeline member %d holds more audio than the %d samples its headers declared", c.cur, want))
	}
	return nil
}

// advance closes the finished member and moves to the next.
func (c *concat) advance() error {
	if want := c.lens[c.cur]; c.local != want {
		// Malformed input: the bytes were read, and the file's declaration is
		// what does not describe them.
		return waxerr.New(waxerr.CodeMalformedInput, fmt.Sprintf(
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
	if c.med == nil {
		if c.chain != nil {
			c.chain.Release()
			c.chain = nil
		}
		return nil
	}
	med := c.med
	c.med = nil
	in := med.Info()
	// While the chain still states its ratio.
	c.lost += c.memberLost(in)
	c.memberTruncated(in)
	if c.chain != nil {
		c.chain.Release()
		c.chain = nil
	}
	c.found = appendMemberLines(c.found, c.cur, in.Warnings)
	c.noted = appendMemberLines(c.noted, c.cur, in.Notes)
	c.damaged += in.Damage
	return med.Close()
}
