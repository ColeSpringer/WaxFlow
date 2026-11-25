package waxflow

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// CutVersion is the cut rung's term in the ADR-0004 cache key, beside
// RemuxVersion. Bump it for a change to the trims, span edges or codec config
// the cut synthesizes.
const CutVersion = "cut-3"

// Span is a kept sample range [From, To) of a source's own track timeline.
// ToEnd means to the end of the track. The timeline is the gapless-trimmed one
// (sample 0 is the first sample a player hears), as for Slice, SpanTrack and
// the HTTP t= parameter; the cut converts to the raw timeline itself.
type Span struct{ From, To int64 }

// CutPlan describes what a cut would produce, computed from the source track
// and its measured packet grid. It embeds RemuxPlan so a consumer reads one
// shape whichever rung answered. Track is the cut's synthesized track, with
// its trims and rewritten codec config, and Samples is its landed length.
type CutPlan struct {
	RemuxPlan
	// Landed is where each requested span fell, one for one, on the source's
	// track timeline. The first span's head and the last span's tail land as
	// asked, their slop trimmed. Every interior edge snaps inward to the
	// packet grid, within one packet inside the request, except an interior
	// tail under TranscodeOptions.SpliceTrims, which is exact, and a tail
	// touching the next span, which lands at that span's snapped head
	// (ADR-0011). A cut never delivers audio from outside the request; the
	// first packet after a join still decodes against the previous span's
	// state.
	Landed []Span
	// Grid is the packet grid and Source the track the plan was computed
	// from: CutStream takes both, so the run cuts on the keyed boundaries.
	Grid   int
	Source container.Track
}

// cutCodec is one allowlisted codec's cut parameters.
type cutCodec struct {
	// preroll is how far ahead of a kept range's start the walk begins so the
	// decoder's output there is converged. The first span's head takes it, its
	// slop becoming the synthesized Delay. An interior head takes none and
	// snaps inward, since nothing would trim it, except under SpliceTrims,
	// where the pre-roll packets carry full-duration trims (ADR-0011).
	preroll int64
	// reprime rewrites the codec config's own priming field to the cut's
	// synthesized delay; nil for a codec whose config says nothing about
	// priming, which is every codec but Opus.
	reprime func(cfg []byte, delay int64) ([]byte, error)
	// headRefusal, when set, is the refusal cutHeaders gives a cut of this
	// codec that starts past sample 0. CutFormats does not advertise such a
	// codec: a client naming a cut format expects any span to engage the rung.
	headRefusal string
}

// cutCodecs is the allowlist of codecs whose packets mean the same thing at
// another position in the stream, the premise the cut adds to codecSurvives. A
// codec opts in here; ADR-0013 says why each other one stays out.
var cutCodecs = map[codec.ID]cutCodec{
	codec.Opus: {
		preroll: opus.SeekPreroll,
		reprime: func(cfg []byte, delay int64) ([]byte, error) {
			return opus.SetPreSkip(cfg, int(delay))
		},
	},
	// AAC-LC and HE-AAC carry their priming in the container's edit list, so
	// there is nothing to reprime.
	codec.AACLC: {preroll: aac.EncoderDelay},
	codec.HEAAC: {
		preroll: aac.HESeekPreroll,
		headRefusal: "waxflow: an HE-AAC cut must start at sample 0 (the SBR header " +
			"lives in the leading access units; a mid-stream slice would play " +
			"with a muted high band)",
	},
}

// Cuttable reports whether track's codec is on the cut allowlist (cutCodecs),
// for a caller that names the codec as its reason before it plans; PlanCut
// asks it too, before the grid. A true promises nothing more: PlanCut can
// still decline for the spans, the grid, the source's trims or the
// destination.
func Cuttable(track container.Track) bool {
	_, ok := cutCodecs[track.Codec]
	return ok
}

// CutFormats lists, in table order, the output formats the cut rung serves
// without re-encoding: those whose codec cuts without the head-only
// restriction and that have a live progressive form and an HLS form, so the
// list holds on both surfaces. A client names one with a from/to span to reach
// the cut; format=auto never does. HE-AAC still cuts head-anchored spans when
// asked, unadvertised.
func CutFormats() []string {
	var names []string
	for _, o := range outputs {
		// A remux-only row is not advertised; its sources reach the cut
		// through a row a client names (format=aac cuts an HE-AAC source).
		if cc, ok := cutCodecs[o.codecID]; ok && cc.headRefusal == "" && o.forms[0].live && o.hls != nil && !o.remuxOnly {
			names = append(names, o.name)
		}
	}
	return names
}

// cutWindow is a kept range of the source's raw timeline. A to of -1 means to
// the end of the stream: the walk keeps every packet from from onward, and the
// length comes from the header rather than the window.
type cutWindow struct{ from, to int64 }

// cutResult is the whole of the cut's arithmetic, read by CutTrack and Cut so
// the track a muxer opens with and the packets a walk delivers agree. Each
// entry point recomputes it from the caller's own inputs, so the computation
// must stay pure and deterministic: header arithmetic, no I/O.
type cutResult struct {
	track   container.Track
	landed  []Span
	windows []cutWindow
	// lastEnd is the raw end of the audio the cut asked for, which the walk
	// must reach before EOF when the cut's length is exact; -1 otherwise.
	lastEnd int64
	// blind marks a source whose total was absent or advisory, so its cut
	// points were checked against nothing (see slice.blind).
	blind bool
	// trims is the source's inner trims the windows were placed around, the
	// ones the view forwards; a packet carrying any other refuses.
	trims []container.PacketTrim
	// toEnd says the last span reaches the source's end, however it was
	// spelled: ToEnd, an end clamped to the source's, or one landing on it.
	// What a truncated source lost is past every other cut.
	toEnd bool
}

// maxCutSample bounds every addend of the grid arithmetic: a span's bounds,
// the track's Delay, its MidPadding and the grid. That arithmetic overflows
// rather than saturating, and an overflowed span lands instead of failing;
// four addends under 2^61 sum under 2^63, so the snap cannot wrap. A span is
// bounded even where the source declares no length to check it against (ADTS).
// 2^61 samples is over a million years at 48 kHz.
const maxCutSample = 1 << 61

// snapGridDown rounds x down to a multiple of g, clamped at zero.
func snapGridDown(x, g int64) int64 {
	if x <= 0 {
		return 0
	}
	return x / g * g
}

// snapGridUp rounds x up to a multiple of g, clamped at zero. x's addends and
// g are each under maxCutSample, so x + g - 1 stays under 2^63.
func snapGridUp(x, g int64) int64 {
	if x <= 0 {
		return 0
	}
	return (x + g - 1) / g * g
}

// The two timelines a cut works between: a span is on the track's own, where
// the delay and the trims inside the run (container.Track.MidTrims) do not
// exist, and the packet grid holds on the raw one, the sum of every packet's
// Dur. A bound crosses over by stepping past the delay and then past every
// trim before it, in order (a trim stepped over may carry it up to the next).
// Trims end on packet boundaries (placeableTrims), so neither a grid position
// nor a mapped bound is ever inside one.

// rawStart maps the first kept sample of a span onto the raw timeline. A trim
// beginning exactly there is before the sample, so it is stepped over.
func rawStart(trims []container.PacketTrim, delay, t int64) int64 {
	raw := t + delay
	for _, tr := range trims {
		if tr.Pos > raw {
			break
		}
		raw += tr.Samples
	}
	return raw
}

// rawEnd maps the exclusive end of a span onto the raw timeline: one past the
// last kept sample, so a trim beginning there stays and becomes tail slop.
func rawEnd(trims []container.PacketTrim, delay, t int64) int64 {
	return rawStart(trims, delay, t-1) + 1
}

// trackPos maps a grid position back onto the track's timeline: the delay
// and every trim that ends at or before it come off.
func trackPos(trims []container.PacketTrim, delay, raw int64) int64 {
	t := raw - delay
	for _, tr := range trims {
		if tr.Pos+tr.Samples > raw {
			break
		}
		t -= tr.Samples
	}
	return t
}

// trimsWithin returns the trims lying inside the raw range [from, to), a to of
// -1 meaning to the end.
func trimsWithin(trims []container.PacketTrim, from, to int64) []container.PacketTrim {
	var out []container.PacketTrim
	for _, tr := range trims {
		if tr.Pos < from {
			continue
		}
		if to >= 0 && tr.Pos+tr.Samples > to {
			break
		}
		out = append(out, tr)
	}
	return out
}

// plannedTrim returns the size of the trim ending at the raw position end, or
// 0 when none does.
func plannedTrim(trims []container.PacketTrim, end int64) int64 {
	i := sort.Search(len(trims), func(i int) bool { return trims[i].Pos >= end }) - 1
	if i >= 0 && trims[i].Pos+trims[i].Samples == end {
		return trims[i].Samples
	}
	return 0
}

// listedTrims reports whether track's inner trims are listed whole, which a
// cut needs whatever the grid: there are none, or every trim MidPadding sums
// is listed, in order, each positive, and the sum is under maxCutSample like
// the other addends. A walk that ran out of room leaves the list short, a
// track nothing walked has a sum and no list, and a caller's own track may
// say anything.
func listedTrims(track container.Track) bool {
	if track.MidPadding <= 0 && len(track.MidTrims) == 0 {
		return true
	}
	if !track.MidTrimsComplete() || track.MidPadding >= maxCutSample {
		return false
	}
	var prev int64
	for _, tr := range track.MidTrims {
		if tr.Samples <= 0 || tr.Pos < prev {
			return false
		}
		prev = tr.Pos + tr.Samples
	}
	return true
}

// placeableTrims reports whether track's inner trims are ones a cut can plan
// around on a g-sample grid: listed whole, each inside one packet and ending
// on its boundary.
func placeableTrims(track container.Track, g int64) bool {
	if !listedTrims(track) {
		return false
	}
	for _, tr := range track.MidTrims {
		if tr.Samples > g || (tr.Pos+tr.Samples)%g != 0 {
			return false
		}
	}
	return true
}

// wholePacketTrim reports whether any trim empties its packet. Two packets
// then share one position on the delivered timeline, and a seek landing
// there cannot tell which of them the demuxer will deliver first, so the
// seekable cut view declines such a source.
func wholePacketTrim(trims []container.PacketTrim, g int64) bool {
	for _, tr := range trims {
		if tr.Samples >= g {
			return true
		}
	}
	return false
}

// CutTrack synthesizes the track a cut of track to spans would produce (its
// trims, length and rewritten codec config) and where the spans landed. grid
// is the source's packet duration from Engine.PacketGrid. It opens nothing, so
// a plan states the output's length and the run is held to the same one.
//
// Every refusal is an error, declines included: CodeUnsupportedFormat is one
// PlanCut turns into a decline (this rung cannot serve it, a transcode can),
// while CodeInvalidRequest and CodeMalformedInput mean no rung can.
func CutTrack(track container.Track, opts TranscodeOptions, spans []Span, grid int) (container.Track, []Span, error) {
	res, err := computeCut(track, opts, spans, grid)
	if err != nil {
		return container.Track{}, nil, err
	}
	return res.track, res.landed, nil
}

// validateCutSpans refuses, with CodeInvalidRequest, spans that do not
// describe this file, as a transcode would. The messages mirror SpanTrack's.
func validateCutSpans(track container.Track, spans []Span) error {
	if len(spans) == 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, "waxflow: a cut needs at least one span")
	}
	for i, s := range spans {
		last := i == len(spans)-1
		switch {
		case s.From < 0:
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("waxflow: negative span start %d", s.From))
		case s.To < ToEnd:
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span end %d: want a sample offset or %d for the end of the source", s.To, ToEnd))
		case s.To == ToEnd && !last:
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span %d runs to the end of the source but %d more follow it", i, len(spans)-1-i))
		case s.To >= 0 && s.To < s.From:
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("waxflow: span [%d, %d) ends before it starts", s.From, s.To))
		case s.From >= maxCutSample || s.To >= maxCutSample:
			// Checked even with no declared length (see maxCutSample).
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span [%d, %d) overflows the sample timeline", s.From, s.To))
		case s.To == s.From:
			// SpanTrack permits an empty span. A cut cannot express one: its
			// snap would not land empty.
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span [%d, %d) keeps no samples; a cut cannot express an empty span", s.From, s.To))
		case s.To == ToEnd && track.Samples >= 0 && s.From == track.Samples:
			// The same empty span, spelled from the end.
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span [%d, ToEnd) starts at the end of the source's %d samples and so keeps none; a cut cannot express an empty span",
				s.From, track.Samples))
		}
		// A bound is checked only where the source declares one, as SpanTrack
		// does (ADTS declares none).
		if track.Samples >= 0 {
			if s.From > track.Samples {
				return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
					"waxflow: span starts at sample %d, past the source's %d samples", s.From, track.Samples))
			}
			if s.To > track.Samples {
				return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
					"waxflow: span ends at sample %d, past the source's %d samples", s.To, track.Samples))
			}
		}
		if i > 0 && s.From < spans[i-1].To {
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span [%d, %d) starts before span %d ended at %d; spans must be in order and disjoint",
				s.From, s.To, i-1, spans[i-1].To))
		}
	}
	return nil
}

// cutHeaders asks the cut's own questions that need no packet grid: the
// spans, the codec allowlist, the head-only rule and the Delay bound. An
// invalid span errors; the rest decline with CodeUnsupportedFormat.
func cutHeaders(track container.Track, spans []Span) error {
	if err := validateCutSpans(track, spans); err != nil {
		return err
	}
	cc, ok := cutCodecs[track.Codec]
	if !ok {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"waxflow: %s packets do not survive being moved within the stream, so this source cannot be cut without re-encoding",
			track.Codec))
	}
	if cc.headRefusal != "" && spans[0].From > 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, cc.headRefusal)
	}
	// The Delay is an addend the container declared, so a bad one declines
	// (the transcode decodes what it can) where a bad span errors. See
	// maxCutSample.
	if track.Delay < 0 || track.Delay >= maxCutSample {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"waxflow: this source declares a %d-sample delay, which is outside the timeline this rung can compute in",
			track.Delay))
	}
	return nil
}

// computeCut is the rung's arithmetic, in one place.
func computeCut(track container.Track, opts TranscodeOptions, spans []Span, grid int) (*cutResult, error) {
	if err := cutHeaders(track, spans); err != nil {
		return nil, err
	}
	cc := cutCodecs[track.Codec]
	if grid <= 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"waxflow: this source's packet durations vary, so there is no grid to cut on")
	}
	// The grid is an addend too, measured rather than the caller's, so a bad
	// one declines as a bad Delay does. See maxCutSample.
	if int64(grid) >= maxCutSample {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"waxflow: this source's %d-sample grid is outside the timeline this rung can compute in", grid))
	}
	g := int64(grid)
	// Past an inner trim every packet starts at a grid position minus the
	// trims so far, which PacketGrid cannot see (Dur is unchanged), so the
	// arithmetic runs on the raw timeline and needs every trim placed on the
	// grid. A list the walk left short, or a caller's own, declines; the cut
	// view refuses mid-walk for a source nothing measured first.
	if (track.MidPadding > 0 || len(track.MidTrims) > 0) && !placeableTrims(track, g) {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"waxflow: this source trims %d samples inside the run at places the walk did not record on its %d-sample grid; this cut cannot be made without re-encoding",
			track.MidPadding, grid))
	}
	n := len(spans)

	// decodedEnd is where the source's decode ends on the raw timeline, inner
	// and tail trims included, or -1 when the source declares no length; the
	// -1 reaches Samples, which the walk resolves (remuxTrailer).
	decodedEnd := int64(-1)
	if track.Samples >= 0 {
		decodedEnd = track.Delay + track.Samples + track.Padding + track.MidPadding
	}
	lastToEnd := spans[n-1].To == ToEnd

	// SpliceTrims makes interior splices exact, but the cut then trims inside
	// its run, which PlanRemux declines everywhere but Matroska; a single-span
	// cut, with no interior edge, ignores it.
	splice := opts.SpliceTrims && n > 1

	// df/dt are the requested span on the raw timeline; sd/su are where the
	// packet grid makes it land. hd is where the audio a listener hears
	// starts, which is sd except on a spliced interior head, where the
	// packets between sd and hd are walked and wholly discarded.
	df := make([]int64, n)
	dt := make([]int64, n)
	sd := make([]int64, n)
	su := make([]int64, n)
	hd := make([]int64, n)
	for i, s := range spans {
		df[i] = rawStart(track.MidTrims, track.Delay, s.From)
		// The first head backs off by the pre-roll before it snaps, or a
		// pre-skip of whole packets would be dropped with the priming and
		// leave a cold decoder at sample 0; the slop becomes the Delay. Every
		// other head snaps inward with no pre-roll, since no trim would hide
		// it (ADR-0011).
		if i == 0 {
			sd[i] = snapGridDown(df[i]-cc.preroll, g)
		} else {
			sd[i] = snapGridUp(df[i], g)
		}
		hd[i] = sd[i]
		if splice && i > 0 {
			// The same splice point, reached with a converged decoder: the
			// pre-roll packets ahead of it are decoded and thrown away.
			sd[i] = snapGridDown(hd[i]-cc.preroll, g)
		}
		switch {
		case s.To != ToEnd:
			dt[i] = rawEnd(track.MidTrims, track.Delay, s.To)
			// The last span's tail snaps out, its slop the Padding. An
			// interior tail snaps in, or under SpliceTrims out, its slop
			// stated as the last kept packet's own trim.
			if i == n-1 || splice {
				su[i] = snapGridUp(dt[i], g)
			} else {
				su[i] = snapGridDown(dt[i], g)
			}
		case decodedEnd >= 0:
			dt[i] = decodedEnd - track.Padding
			su[i] = decodedEnd
		default:
			dt[i], su[i] = -1, -1
		}
	}

	// Touching spans name one range in two pieces, so they share one boundary,
	// the head's snapped up: the packet across it stays with the earlier span,
	// whose Landed reaches into the next (ADR-0011). Not under SpliceTrims,
	// where the next head's pre-roll is inside this window and the overlap
	// check declines.
	for i := 0; !splice && i < n-1; i++ {
		if spans[i].To == spans[i+1].From && su[i] >= 0 {
			su[i] = sd[i+1]
		}
	}

	// Snapping in can leave a span no whole packet, which declines (a
	// transcode serves it exactly) where the nearest packet would be audio
	// from outside the request. After the merge, since a short span touching
	// the next is not empty. The first span's audio starts at its requested
	// head, the Delay trimming the pre-roll, so its tail must land past that.
	for i := range spans {
		head := hd[i]
		if i == 0 {
			head = df[0]
		}
		if su[i] >= 0 && head >= su[i] {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
				"waxflow: span %d keeps no whole packet on this source's %d-sample grid, so this cut cannot be made without re-encoding",
				i, grid))
		}
	}

	// Overlapping windows would emit a packet twice and break Landed's one for
	// one, so they decline rather than merge. By default an interior tail
	// snaps down and the next head up, so no two windows overlap; under
	// SpliceTrims every interior head backs off and every interior tail snaps
	// out. Equality is adjacency.
	for i := 0; i < n-1; i++ {
		if su[i] > sd[i+1] {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
				"waxflow: spans %d and %d sit closer together than this cut can keep them on a %d-sample packet grid, so it cannot be made without re-encoding",
				i, i+1, grid))
		}
	}
	// A bounded last span whose snap passes the source's end reaches the end,
	// as ToEnd does, so it clamps and runs to EOF. A To of Samples snaps past
	// the end whenever the decode total is off the grid, so refusing would
	// decline the plainest cut.
	tailClamped := false
	if !lastToEnd && decodedEnd >= 0 && su[n-1] > decodedEnd {
		su[n-1] = decodedEnd
		tailClamped = true
	}

	out := track
	// The cut's trims are its own arithmetic, whatever the source had read.
	out.TrimsUnread = false
	// The kept packets' trims stay on them (the view forwards each), so they
	// are the cut track's MidPadding, which PlanRemux declines for every
	// destination but Matroska. The final kept packet's is left out: the
	// trailer restates it. Each is placed on the output's raw timeline, the
	// windows back to back.
	out.MidPadding, out.MidTrims = 0, nil
	var outRaw int64
	for i := range spans {
		base := outRaw
		add := func(pos, samples int64) {
			out.MidPadding += samples
			out.MidTrims = append(out.MidTrims, container.PacketTrim{Pos: base + pos - sd[i], Samples: samples})
		}
		// A spliced interior head's pre-roll packets are decoded and discarded
		// whole.
		if splice && i > 0 {
			for p := sd[i]; p < hd[i]; p += g {
				add(p, g)
			}
		}
		// A spliced interior tail's slop is its last kept packet's trim, which
		// makes the splice exact. A source trim on that packet is the same
		// tail, and the larger wins.
		tailSlop := int64(0)
		if splice && i < n-1 && su[i] >= 0 {
			tailSlop = su[i] - dt[i]
		}
		for _, tr := range trimsWithin(track.MidTrims, sd[i], su[i]) {
			if i == n-1 && !lastToEnd && !tailClamped && tr.Pos+tr.Samples == su[i] {
				continue
			}
			if splice && i > 0 && tr.Pos+tr.Samples <= hd[i] {
				// Inside the pre-roll, already discarded whole. Adding it
				// again would count it twice and unsort the list plannedTrim
				// searches.
				continue
			}
			if tailSlop > 0 && tr.Pos+tr.Samples == su[i] {
				// The same packet's tail. It cannot exceed the slop, since a
				// track position never maps into a trim; max holds either way.
				tailSlop = max(tailSlop, tr.Samples)
				continue
			}
			add(tr.Pos, tr.Samples)
		}
		if tailSlop > 0 {
			add(su[i]-tailSlop, tailSlop)
		}
		if su[i] >= 0 {
			outRaw += su[i] - sd[i]
		}
	}
	// The head's snap slop becomes the delay: it is delivered audio the decoder
	// needs and the listener must not hear, counted after the packets' own
	// trims, which the reader drops first.
	out.Delay = df[0] - sd[0]
	for _, tr := range trimsWithin(track.MidTrims, sd[0], df[0]) {
		out.Delay -= tr.Samples
	}

	// A clamped tail runs to EOF, past the end the header names (an Ogg-Opus
	// final packet is granule-truncated), and the trailer resolves it: the
	// length below is exact, and container.SettleLength re-derives the padding
	// from the run for an exact length, delay or no delay.
	switch {
	case lastToEnd && track.Samples < 0:
		// Nothing to resolve the end against: the source's tail trim stands,
		// and the walk resolves the length, remuxTrailer's Samples < 0 branch
		// giving this cut's landed length. ADTS reaches here.
		out.Padding = track.Padding
		out.Samples = -1
	default:
		// The tail's slop is the padding. Where the last span reaches the
		// source's end, su is the decode total and dt the audio's end, so this
		// is the source's own Padding, which takes in any trim the source
		// stated on its last packet.
		out.Padding = su[n-1] - dt[n-1]
		// The landed length, never the requested one, so remuxTrailer's
		// padding comes out at exactly Padding (ADR-0011). The kept packets'
		// trims are not delivered, so they come off.
		var delivered int64
		for i := range spans {
			delivered += su[i] - sd[i]
		}
		out.Samples = delivered - out.MidPadding - out.Delay - out.Padding
	}
	// Stated, not left to the struct copy: a bounded last span's length is
	// computed, so exact; a ToEnd one keeps the source's claim.
	if !lastToEnd {
		out.SamplesExact, out.SamplesAdvisory = true, false
	}
	if cc.reprime != nil {
		cfg, err := cc.reprime(track.CodecConfig, out.Delay)
		if err != nil {
			return nil, err
		}
		out.CodecConfig = cfg
	}

	landed := make([]Span, n)
	windows := make([]cutWindow, n)
	for i := range spans {
		// hd, not sd: a spliced interior head's pre-roll packets are walked
		// and wholly discarded, so the audio starts at the splice point.
		landed[i] = Span{From: trackPos(track.MidTrims, track.Delay, hd[i]), To: trackPos(track.MidTrims, track.Delay, su[i])}
		windows[i] = cutWindow{from: sd[i], to: su[i]}
	}
	// The head and the tail land as asked: their slop is trimmed.
	landed[0].From = spans[0].From
	if splice {
		// The spliced tails are exact; the heads still snap inward, because
		// a head trim would have to discard from the FRONT of a packet and
		// no container states one.
		for i := range n - 1 {
			landed[i].To = spans[i].To
		}
	}
	switch {
	case !lastToEnd:
		landed[n-1].To = spans[n-1].To
	case track.Samples >= 0:
		landed[n-1].To = track.Samples
	default:
		landed[n-1].To = ToEnd
	}
	if lastToEnd || tailClamped {
		// To EOF: the final packet may be short or granule-truncated, and the
		// walk must not cut at a boundary that is not there.
		windows[n-1].to = -1
	}
	// What the walk must reach before EOF (cutDemuxer.endOfPackets): the end of
	// the audio the last span asked for, not its trailing padding, held when
	// the cut's length is exact, which a bounded cut's always is.
	lastEnd := int64(-1)
	if out.SamplesExact && out.Samples >= 0 {
		lastEnd = dt[n-1]
	}
	return &cutResult{track: out, landed: landed, windows: windows, lastEnd: lastEnd,
		blind: track.Samples < 0 || track.SamplesAdvisory, trims: track.MidTrims,
		toEnd: lastToEnd || tailClamped || track.Samples >= 0 && spans[n-1].To >= track.Samples}, nil
}

// adoptMeasured overlays measured's length and per-packet trims (see
// container.Track.MidPadding) onto a fresh header open. A measured track whose
// Samples is negative or advisory, or whose trims are unread, measured
// nothing, and track comes back as it was. Plan and run both read through
// this, so their windows, trailer and init segment agree.
func adoptMeasured(track, measured container.Track) container.Track {
	if measured.Samples < 0 || measured.SamplesAdvisory || measured.TrimsUnread {
		return track
	}
	track.Samples, track.SamplesExact, track.SamplesAdvisory = measured.Samples, true, false
	track.Padding, track.MidPadding, track.MidTrims = measured.Padding, measured.MidPadding, measured.MidTrims
	track.TrimsUnread = false
	return track
}

// cutSource is the source track a cut runs from: the header's, with the
// caller's measurement over it. Handed none for a track whose trims are
// unread, the run walks the source once, as a copy does: the cut view has
// no walk of its own, and a cut from an estimate ends where the estimate does.
func cutSource(ctx context.Context, demux container.Demuxer, header, measured container.Track) (container.Track, error) {
	track := adoptMeasured(header, measured)
	if !track.TrimsUnread {
		return track, nil
	}
	walked, ok, err := walkedTrack(ctx, demux, header.ID)
	if err != nil {
		return track, err
	}
	if ok {
		return adoptMeasured(header, walked), nil
	}
	return track, nil
}

// Cut returns a view of demux holding only track's packets that fall in spans,
// retimed to be contiguous: the packet-domain sibling of Slice. As with Slice,
// a source that runs out before the end the cut is held to fails the read. A
// span is applied by wrapping, never through TranscodeOptions (ADR-0013). The
// view is no container.Seeker, since a seek on the source's timeline under the
// cut's packets would be silently wrong; it forwards Warnings and
// TruncatedSamples and nothing else outside container.Demuxer.
//
// The caller owns demux. Packets stay borrowed as through a plain remux: the
// view sets only PTS, Padding and Lost. Padding is the plan's trim for the
// packet, the source's clamped into it or the discard SpliceTrims places.
func Cut(demux container.Demuxer, track container.Track, opts TranscodeOptions, spans []Span, grid int) (container.Demuxer, error) {
	res, err := computeCut(track, opts, spans, grid)
	if err != nil {
		return nil, err
	}
	return &cutDemuxer{Demuxer: demux, cut: res.track, track: track.ID, windows: res.windows, damage: container.DamageOf(demux),
		lastEnd: res.lastEnd, blind: res.blind, trims: res.trims, toEnd: res.toEnd}, nil
}

// cutDemuxer is Cut's view: the windows in source raw coordinates, the walk's
// position in them, and the output position it retimes onto.
type cutDemuxer struct {
	container.Demuxer
	cut     container.Track
	track   int
	windows []cutWindow
	lastEnd int64                  // raw end the walk is held to, -1 for none
	blind   bool                   // the source's total was absent or advisory
	damage  int                    // source's damage count when made
	trims   []container.PacketTrim // source's inner trims the plan placed
	toEnd   bool                   // last span reaches the source's end
	cur     int
	pos     int64 // the next source packet's raw position
	out     int64 // next kept packet's output position (delivered)
	outRaw  int64 // out untrimmed; cut.MidTrims is placed on it
	prevPad int64 // previous source packet's own trim, clamped to it
	// havePrev says prevPad describes a packet this view read: false at the
	// start and after a seek, when the packet before the cursor is unknown.
	havePrev bool
	// prevKept says the view delivered that packet: it lay in a window.
	prevKept bool
	// tailPad is the trims the plan placed nowhere, the first ending at the
	// raw position tailAt, 0 for none: the end trim's while every packet
	// after the first is trimmed whole.
	tailPad, tailAt int64
}

// Tracks reports the cut's own track, the one ReadPacket's packets belong to.
// The embedded Demuxer would promote the source's (uncut length, original
// OpusHead, the source's trims), and format.FromDemuxer builds a Media's Info
// from this. One track: ReadPacket drops every other track's packets.
func (c *cutDemuxer) Tracks() []container.Track { return []container.Track{c.cut} }

// Warnings forwards the source's list: embedding the Demuxer interface
// promotes nothing outside it, and the copy rung reads this through
// container.Warner for the damage its walk found.
func (c *cutDemuxer) Warnings() []container.Warning {
	if w, ok := c.Demuxer.(container.Warner); ok {
		return w.Warnings()
	}
	return nil
}

// TruncatedSamples forwards the source's count (container.Truncated) for a
// cut whose last span reaches the end of the source, which is where a file
// cut short lost its audio. One that stops short of it holds all it asked
// for. A Slice draws the same line, so the count does not turn on the rung.
func (c *cutDemuxer) TruncatedSamples(track int) int64 {
	if !c.toEnd {
		return 0
	}
	return container.TruncatedOf(c.Demuxer, track)
}

func (c *cutDemuxer) ReadPacket(pkt *container.Packet) error {
	for {
		if c.cur == len(c.windows) {
			// Every window is behind us: stop without reading the rest, so a
			// head-only cut costs only the head. Trims still held ride on the
			// last kept packets.
			return c.endOfCut(c.tailPad)
		}
		if err := c.Demuxer.ReadPacket(pkt); err != nil {
			if err == io.EOF {
				return c.endOfPackets()
			}
			return err
		}
		if pkt.Track != c.track {
			continue
		}
		// A hole is source time: the packet starts past it. One in a gap goes
		// with the gap; silence inside a window would move the trims the plan
		// placed there, so a hole the windows keep any of is refused.
		if kept := c.keptIn(c.pos, c.pos+pkt.Lost); kept > 0 {
			return waxerr.New(waxerr.CodeMalformedInput, fmt.Sprintf(
				"waxflow: damage took %d samples inside the cut, before sample %d, which a copy cannot place; transcode it",
				kept, c.pos+pkt.Lost))
		}
		c.pos, pkt.Lost = c.pos+pkt.Lost, 0
		start, end := c.pos, c.pos+pkt.Dur
		c.pos = end
		// A trim on a packet something followed is an inner one and must be
		// the one the plan placed there, or the plan was made for another
		// file: the backstop for a source nothing measured first or a stale
		// measure. Checked before the windows advance, so the last kept packet
		// is covered; the packet that ends the walk never is. A trim placed
		// nowhere may belong to the end trim (a walk counts that back from the
		// last packet while each is trimmed whole), so it is held until a
		// packet plays after it.
		own := min(max(pkt.Padding, 0), pkt.Dur)
		if c.havePrev {
			if planned := plannedTrim(c.trims, start); planned != c.prevPad {
				if planned != 0 {
					return cutTrimMismatch(c.prevPad, planned, start)
				}
				if c.tailPad == 0 {
					c.tailAt = start
				}
				c.tailPad += c.prevPad
			}
			if c.tailPad > 0 && own < pkt.Dur {
				return cutTrimMismatch(c.tailPad, 0, c.tailAt)
			}
		}
		c.prevPad, c.havePrev, c.prevKept = own, true, false
		for c.cur < len(c.windows) && c.windows[c.cur].to >= 0 && start >= c.windows[c.cur].to {
			c.cur++
		}
		if c.cur == len(c.windows) {
			continue
		}
		w := c.windows[c.cur]
		if end <= w.from {
			continue // in a gap, or ahead of the first window
		}
		// The grid is re-checked rather than trusted: a plan computed against
		// another source (a stale memo, a replaced file) would otherwise
		// splice mid-packet with no error, and Ogg's endGranule = Delay +
		// Samples takes the cut's Samples unchecked.
		if start < w.from {
			return cutStraddle(start, end, w.from)
		}
		if w.to >= 0 && end > w.to {
			return cutStraddle(start, end, w.to)
		}
		// The plan's trim, read off the cut track's own list by output
		// position (under SpliceTrims it holds discards the source never
		// stated). The source's own wins where the plan left one out: the
		// final kept packet's.
		pad := max(c.prevPad, plannedTrim(c.cut.MidTrims, c.outRaw+pkt.Dur))
		pkt.Padding = pad
		c.outRaw += pkt.Dur
		// Contiguous from 0, the cut track's Delay trimming the head. A
		// packet's own trim stays on it and the position advances past it
		// (container.Packet.Padding), so a wholly discarded pre-roll packet
		// shares the splice's timestamp.
		pkt.PTS = c.out
		c.out += pkt.Dur - pad
		c.prevKept = true
		return nil
	}
}

// keptIn is how much of the source span [from, to) the windows from the
// current one on keep.
func (c *cutDemuxer) keptIn(from, to int64) int64 {
	var n int64
	for _, w := range c.windows[c.cur:] {
		end := to
		if w.to >= 0 {
			end = min(end, w.to)
		}
		if lo := max(from, w.from); end > lo {
			n += end - lo
		}
	}
	return n
}

// endOfPackets answers the source running out of packets as a slice answers
// its source running out: short of the held end (lastEnd), it fails with the
// slice's codes, an invalid request when no stated total checked the cut and
// malformed input otherwise. Otherwise the walk ends where the packets do.
func (c *cutDemuxer) endOfPackets() error {
	if c.pos >= c.lastEnd || container.DamageOf(c.Demuxer) > c.damage {
		// Reached, or short with the damage reported on the way. The trims
		// held and the last packet's own, if it was kept, end the cut.
		trim := c.tailPad
		if c.prevKept {
			trim += c.prevPad
		}
		return c.endOfCut(trim)
	}
	return shortSource(fmt.Sprintf("waxflow: the source's packets end at %d, short of the cut window ending at %d",
		c.pos, c.lastEnd), c.blind)
}

// endOfCut ends the view, or refuses a cut whose stated length reaches into
// the trim samples its last kept packets trim: the trailer would state them
// as audio. The cut's inner trims are in the raw run and not in its length.
func (c *cutDemuxer) endOfCut(trim int64) error {
	over := c.cut.Delay + c.cut.Samples + c.cut.MidPadding - (c.outRaw - trim)
	if trim > 0 && over > 0 && c.cut.Samples >= 0 && !c.cut.SamplesAdvisory {
		return cutEndTrimMismatch(over, trim)
	}
	return io.EOF
}

func cutStraddle(start, end, at int64) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
		"waxflow: source packet [%d, %d) straddles the cut boundary at %d; this stream cannot be cut without re-encoding",
		start, end, at))
}

// cutTrimMismatch is the cut view's refusal when the packet ending at the raw
// position at carries a trim (got) other than the one the plan placed there
// (want): a source nothing measured first, or one that changed since.
func cutTrimMismatch(got, want, at int64) error {
	if want == 0 {
		return innerTrimRefusal(got, cutTrimRemedy)
	}
	return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
		"waxflow: the plan placed a %d-sample trim ending at %d and the packet there trims %d; the source is not the one that was measured, so this cut cannot be made without re-measuring it",
		want, at, got))
}

// cutEndTrimMismatch is the cut view's refusal when the cut's length states
// over of the trim samples its last kept packets trim between them: a source
// nothing measured first, or one that changed since.
func cutEndTrimMismatch(over, trim int64) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
		"waxflow: the plan's length takes in %d of the %d samples the packets that end this cut trim; the source is not the one that was measured, so this cut cannot be made without re-measuring it",
		over, trim))
}

// PlanCut reports whether opts can be served by cutting track's packets to
// spans and rewriting the container around them, and how. A request the rung
// cannot serve declines with (nil, nil), its reason logged at Debug, and the
// caller falls through to a transcode. An error means no rung can serve it:
// invalid options or spans, a codec config that does not parse, a damaged
// source, or grid's own error, returned unchanged. grid gives the packet
// duration, as Engine.PacketGrid does; it is asked at most once, after every
// question the track, options and spans answer has passed, and 0 declines. A
// plan answers for the track it is handed: one whose trims are unread
// (container.Track.TrimsUnread) hides the end trim the run measures, so
// CutStream can refuse a cut that plan accepted. ADR-0013 lists the declines.
func (e *Engine) PlanCut(track container.Track, opts TranscodeOptions, spans []Span, grid func() (int, error)) (*CutPlan, error) {
	if err := opts.Check(); err != nil {
		return nil, err
	}
	g, asked, err := e.cutGrid(track, opts, spans, false, grid, func(g int) bool {
		p, err := e.planCutOn(track, opts, spans, g)
		return p == nil && err == nil
	})
	if !asked || err != nil {
		return nil, err
	}
	return e.planCutOn(track, opts, spans, g)
}

// planCutOn is PlanCut on a grid of g.
func (e *Engine) planCutOn(track container.Track, opts TranscodeOptions, spans []Span, g int) (*CutPlan, error) {
	cut, landed, err := CutTrack(track, opts, spans, g)
	if err != nil {
		// CutTrack states its declines as CodeUnsupportedFormat.
		if waxerr.CodeOf(err) == waxerr.CodeUnsupportedFormat {
			e.log.Debug("cut declined", "codec", track.Codec, "grid", g, "reason", err)
			return nil, nil
		}
		return nil, err
	}
	rp, err := e.PlanRemux(cut, opts)
	if err != nil || rp == nil {
		return nil, err
	}
	// The cut's Delay and Padding are its own, and PlanRemux checked only the
	// source's.
	row, fm, _, err := remuxForm(cut, opts)
	if err != nil {
		return nil, err
	}
	if out := copyDest(row, fm, false); !cutTrimsExpressible(out, cut.Delay, cut.Padding) {
		e.log.Debug("cut declined", "reason", "the destination cannot signal the cut's trims",
			"outContainer", out, "delay", cut.Delay, "padding", cut.Padding)
		return nil, nil
	}
	// Appended rather than replaced: PlanRemux put the destination muxer's
	// term there (ADR-0004's container term), and a progressive cut writes
	// through that muxer like any other remux does.
	rp.Versions = append(rp.Versions, CutVersion)
	return &CutPlan{RemuxPlan: *rp, Landed: landed, Grid: g, Source: track}, nil
}

// cutGrid asks a cut's header questions and then the grid, which it asks
// only when they pass. asked is false for a decline before the ask; an
// invalid span and the grid's own error come back as errors. opts is what
// the copy predicate reads: a segmented cut passes the segmented depth, and
// asks its init header's questions too. declines reports whether the
// planner declines on a grid of g, asked of the one grid a source's inner
// trims fit, since every other declines at those trims.
func (e *Engine) cutGrid(track container.Track, opts TranscodeOptions, spans []Span, segmented bool,
	grid func() (int, error), declines func(g int) bool) (g int, asked bool, err error) {
	msg := "cut declined"
	if segmented {
		msg = "segmented cut declined"
	}
	if err := cutHeaders(track, spans); err != nil {
		if waxerr.CodeOf(err) == waxerr.CodeUnsupportedFormat {
			e.log.Debug(msg, "codec", track.Codec, "reason", err)
			return 0, false, nil
		}
		return 0, false, err
	}
	if reason, out := cutRuledOut(track, opts, spans, segmented); reason != "" {
		e.log.Debug(msg, "reason", reason, "codec", track.Codec, "outFormat", opts.Format, "outContainer", out)
		return 0, false, nil
	}
	if pg, ok := pinnedGrid(track); ok && declines(pg) {
		e.log.Debug(msg, "reason", "the cut declines on the one grid the source's inner trims fit", "grid", pg)
		return 0, false, nil
	}
	g, err = grid()
	return g, true, err
}

// cutRuledOut names what rules out every cut of spans from track under opts
// whatever the grid, and the container it judged, or returns "". A row or
// form error is left to the planner, which reports it once the grid is
// asked.
func cutRuledOut(track container.Track, opts TranscodeOptions, spans []Span, segmented bool) (reason, out string) {
	row, fm, copies, err := remuxForm(track, opts)
	if err != nil {
		return "", ""
	}
	out = copyDest(row, fm, segmented)
	g, most, trimmed := trimGrid(track)
	switch {
	case !copies:
		return "the format or options need a re-encode", out
	case !listedTrims(track):
		return "this source trims inside the run at places the walk did not record", out
	case trimmed && g < most:
		return "this source trims inside the run at places no packet grid holds", out
	}
	if reason := splicedOverlap(track, opts, spans); reason != "" {
		return reason, out
	}
	if reason := cutTrimsRuledOut(track, opts, spans, out, segmented); reason != "" {
		return reason, out
	}
	probe, ok := cutProbe(track, spans)
	if !segmented {
		if ok && fm.trackFits != nil {
			if err := fm.trackFits(probe); err != nil {
				return fmt.Sprintf("the destination cannot write this track (%v)", err), out
			}
		}
		return "", out
	}
	// A trim at the run's start fills the first packet on any grid it fits,
	// and a restart's seek cannot tell such a packet from the next.
	if len(track.MidTrims) > 0 && track.MidTrims[0].Pos == 0 {
		return "a trim empties a packet, so a restart's seek is ambiguous", out
	}
	if ok && !cutInitExpressible(probe) {
		return "the init header cannot express this track", out
	}
	return "", out
}

// trimGrid returns the gcd of the ends of track's inner trims and the
// largest trim: a grid places them only when it divides every end and holds
// every trim. trimmed is false for no trims, or one too far out to add.
func trimGrid(track container.Track) (g, most int64, trimmed bool) {
	for _, tr := range track.MidTrims {
		if tr.Pos >= maxCutSample {
			return 0, 0, false
		}
		g, most = gcd(g, tr.Pos+tr.Samples), max(most, tr.Samples)
	}
	return g, most, len(track.MidTrims) > 0
}

// pinnedGrid returns the one grid that places track's inner trims when no
// other can: their largest trim is over half the gcd of their ends, and
// every divisor of the gcd but itself is at most half of it. A trim at raw
// 0 pins its own length.
func pinnedGrid(track container.Track) (int, bool) {
	g, most, trimmed := trimGrid(track)
	if !trimmed || most > g || 2*most <= g || g >= maxCutSample || g > math.MaxInt {
		return 0, false
	}
	return int(g), true
}

// gcd is the greatest common divisor of a and b, neither negative.
func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// splicedOverlap names two spans whose windows SpliceTrims overlaps on every
// grid, or returns "". Span i backs off by the pre-roll from its head's
// snap, so it reaches into span i-1 when the two touch on the raw timeline
// and snap to one grid point (a trim at the join parts them), and when it
// starts within the pre-roll and backs off to raw 0.
func splicedOverlap(track container.Track, opts TranscodeOptions, spans []Span) string {
	preroll := cutCodecs[track.Codec].preroll
	if !opts.SpliceTrims || preroll <= 0 {
		return ""
	}
	for i := 1; i < len(spans); i++ {
		from := rawStart(track.MidTrims, track.Delay, spans[i].From)
		switch {
		case rawEnd(track.MidTrims, track.Delay, spans[i-1].To) == from:
			return fmt.Sprintf("spans %d and %d touch, so a spliced head's pre-roll reaches into the span before it; join them or drop SpliceTrims", i-1, i)
		case from <= preroll:
			return fmt.Sprintf("span %d starts within the pre-roll, so its spliced head's pre-roll reaches into span %d; drop SpliceTrims", i, i-1)
		}
	}
	return ""
}

// cutProbe is track as a cut of spans presents it to its destination, for the
// questions the cut leaves alone. The reprime writes the cut's own priming
// into the codec config, so the probe carries one, with a delay to match:
// positive, as the first head's back-off leaves it, for a span past sample 0
// or a primed source, and 0 otherwise. A last span to the end keeps the
// source's padding on every grid; another's padding is the grid's, and the
// probe states none. ok is false when the reprime cannot build one, which the
// cut answers once the grid is asked.
func cutProbe(track container.Track, spans []Span) (container.Track, bool) {
	cc := cutCodecs[track.Codec]
	delay := min(cc.preroll, track.Delay+spans[0].From)
	if cc.reprime != nil {
		cfg, err := cc.reprime(track.CodecConfig, delay)
		if err != nil {
			return track, false
		}
		track.CodecConfig = cfg
	}
	track.Delay = delay
	if spans[len(spans)-1].To != ToEnd {
		track.Padding = 0
	}
	return track, true
}

// cutTrimsRuledOut names the trims containerName cannot carry that every cut
// of spans from track has, whatever the grid, or returns "". A segmented cut
// writes fMP4, which carries no trim inside the run.
func cutTrimsRuledOut(track container.Track, opts TranscodeOptions, spans []Span, containerName string, segmented bool) string {
	preroll := cutCodecs[track.Codec].preroll
	if !StatesTrimsPerPacket(containerName) {
		// Each spliced interior head's pre-roll is trimmed inside the run.
		if opts.SpliceTrims && len(spans) > 1 && preroll > 0 {
			reason := "SpliceTrims needs a destination that states a trim per packet (mka or webm)"
			if segmented {
				reason = "SpliceTrims needs a destination that states a trim per packet, which a segmented cut's fMP4 is not"
			}
			if track.MidPadding > 0 {
				return reason + ", and this source trims inside its run too"
			}
			return reason + "; drop the option to take the snapped splices"
		}
		if keepsSourceTrim(track, spans) {
			if segmented {
				return "the cut keeps a trim inside the source's run, which a segmented cut cannot state"
			}
			return "the cut keeps a trim inside the source's run, which only mka or webm state"
		}
		// The first head backs off by the pre-roll, so a span past sample 0
		// or a primed source leaves the cut a front trim, or moves it inside
		// the run. cutTrimsExpressible answers any front trim by the
		// container alone.
		if preroll > 0 && (spans[0].From > 0 || track.Delay > 0) && !cutTrimsExpressible(containerName, 1, 0) {
			return "the destination cannot signal the cut's trims"
		}
	}
	// From the first sample of an unprimed source to its end, the cut keeps
	// the source's end trim and no front trim.
	if spans[0].From == 0 && track.Delay == 0 && spans[len(spans)-1].To == ToEnd &&
		!cutTrimsExpressible(containerName, 0, track.Padding) {
		return "the destination cannot signal the cut's trims"
	}
	return ""
}

// keepsSourceTrim reports whether the first window keeps a source trim
// whatever the grid: one ending past the first span's pre-roll point and
// inside the span, within the run the source states. That window's head
// snaps down from the pre-roll point, and a trim ends on the grid and holds
// at most a packet, so one ending past the point starts at or after the
// head. The tail keeps every packet ending inside the span.
func keepsSourceTrim(track container.Track, spans []Span) bool {
	from := rawStart(track.MidTrims, track.Delay, spans[0].From) - cutCodecs[track.Codec].preroll
	to := int64(-1)
	if spans[0].To != ToEnd {
		to = rawEnd(track.MidTrims, track.Delay, spans[0].To)
	}
	for _, tr := range track.MidTrims {
		end := tr.Pos + tr.Samples
		if end > from && (to < 0 || end < to) &&
			(track.Samples < 0 || end <= track.Delay+track.Samples+track.Padding+track.MidPadding) {
			return true
		}
	}
	return false
}

// cutTrimsExpressible reports whether containerName can signal the trims a cut
// synthesized: the destination's side of gaplessSurvives. A cut with no trims
// passes anywhere. Fragmented MP4 serves a cut with a delay and declines an
// end trim alone; ADTS and any container not named decline (ADR-0013).
func cutTrimsExpressible(containerName string, delay, padding int64) bool {
	if delay == 0 && padding == 0 {
		return true
	}
	switch containerName {
	case "opus", "mka", "webm":
		// Ogg-Opus carries the front trim in the OpusHead pre-skip (which the
		// cut rewrote) and the end trim in the final page's granule. Matroska
		// carries both outright, as CodecDelay and DiscardPadding.
		return true
	case ContainerProgressive:
		// The flat muxer writes its edit list at End, when it knows everything,
		// so it can express either trim.
		return true
	case "aac", "he-aac", ContainerFragmented:
		// The fragmented muxer (both rows' default) states an end trim as its
		// edit list's length, which not every player reads (ADR-0013).
		return delay > 0
	}
	return false
}

// The pieces assemble like this, plan first and then only what it accepted:
//
//	grid := func() (int, error) { return e.PacketGrid(ctx, src, hint) }
//	plan, err := e.PlanCut(track, opts, spans, grid) // (nil, nil) declines
//	cut, landed, err := CutTrack(track, opts, spans, plan.Grid)
//	demux, err := Cut(demux, track, opts, spans, plan.Grid)
//	res, err := e.RemuxDemuxer(ctx, demux, cut, dst, opts)
//
// The run takes CutTrack's track, which keeps the source's track ID the walk
// filters on; plan.Track is normalized to ID 0 for opening the muxer.
// cutTrimsExpressible is asked by the planners alone, so a caller skipping
// the plan loses its declines.

// CutStream opens src and cuts its packets to spans, rewriting the container
// around them: the run half of the cut rung and the packet-move sibling of
// Remux. The output holds the kept access units byte for byte, retimed to be
// contiguous; a source that runs out early fails the run, as Cut describes.
//
// opts and spans must be a pair PlanCut accepted, with that plan's grid. What
// CutTrack or PlanRemux declines is an error here, never a re-encode; the
// destination's trim rule (cutTrimsExpressible) is the plan's alone. measured
// is the source's measured track (never CutPlan.Track, whose synthesized trims
// the walk would refuse); a negative Samples takes the header's own. Handed
// none for a source whose trims are unread, the run walks it once and cuts
// from that. ADR-0013 says what the plan promises the run.
func (e *Engine) CutStream(ctx context.Context, src container.Source, hint string, dst io.Writer,
	opts TranscodeOptions, spans []Span, grid int, measured container.Track, run ...RunOption) (*TranscodeResult, error) {
	if err := opts.Check(); err != nil {
		return nil, err
	}
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, err
	}
	track, err := cutSource(ctx, demux, info.Default(), measured)
	if err != nil {
		return nil, err
	}
	// CutTrack's track, which keeps the source's track ID the walk filters on.
	// No demux.Close, as in Remux: the source.File owns the handle.
	cut, _, err := CutTrack(track, opts, spans, grid)
	if err != nil {
		return nil, err
	}
	cutDemux, err := Cut(demux, track, opts, spans, grid)
	if err != nil {
		return nil, err
	}
	return e.RemuxDemuxer(ctx, cutDemux, cut, dst, opts, run...)
}
