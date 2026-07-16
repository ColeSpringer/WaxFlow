package waxflow

import (
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// CutVersion identifies the cut rung's own sample-affecting logic for the
// ADR-0004 cache key.
//
// It rides beside RemuxVersion rather than replacing it, because a cut is a
// remux with a packet filter in front: everything RemuxVersion covers still
// applies, and this covers what the filter adds. RemuxVersion's own argument for
// existing is the one that puts this here too. A cut runs no decoder, no DSP and
// no encoder, so no revision of any of them can change its bytes; what it does
// synthesize is a set of trims and a rewritten codec config, and wrong gapless
// metadata is wrong playback rather than merely older bytes.
const CutVersion = "cut-1"

// Span is a kept sample range [From, To) of a source's own track timeline.
// ToEnd means to the end of the track.
//
// The timeline is the track's, which is the gapless-trimmed one: sample 0 is the
// first sample a player hears, not the first sample the decoder emits. That is
// the timeline every other span-shaped thing in this library speaks (Slice,
// SpanTrack, the HTTP t= parameter), and the cut converts to the decode domain
// internally rather than making a caller do it.
type Span struct{ From, To int64 }

// CutPlan describes what a cut would produce, computed from the source track's
// headers alone.
//
// It embeds RemuxPlan for the reason RemuxPlan embeds TranscodePlan: consumers
// read one shape whichever rung answered. Track is the cut's synthesized track,
// carrying the trims and the rewritten codec config the cut computed, and
// Samples is its landed length.
type CutPlan struct {
	RemuxPlan
	// Landed is where the requested spans actually fell, one for one, on the
	// source's own track timeline.
	//
	// The head of the first span and the tail of the last land exactly where
	// they were asked for, because their snap slop is expressed as the
	// synthesized gapless trims rather than delivered. Every interior splice
	// snaps outward to the packet grid and says so here: there is no per-splice
	// trim to hide it in, so a caller that needs to know where its cut points
	// really landed reads them off this.
	Landed []Span
}

// cutCodec is one allowlisted codec's cut parameters.
type cutCodec struct {
	// preroll is how far ahead of a kept range's start the packet walk must
	// begin for the decoder's output at that start to be the audio the encoder
	// wrote rather than a cold decoder's approximation of it.
	//
	// What it costs depends on which span it belongs to, and the difference is
	// this rung's honest limit rather than a detail. The first span's pre-roll
	// becomes the synthesized Delay and is trimmed, so the head is exact and the
	// pre-roll costs only bytes. Every later span has no per-splice trim to hide
	// in, so its pre-roll is delivered as audible audio from before the caller's
	// cut point. That is not a bug being tolerated: it is what makes an interior
	// splice snapped rather than exact, it is why Landed exists to report where
	// the splices really fell, and it is why the interior slop counts toward
	// Samples.
	preroll int64
	// reprime rewrites the codec config's own priming field to the cut's
	// synthesized delay, for a codec that carries priming there rather than
	// leaving it to the container. Nil for a codec whose config says nothing
	// about priming, which is every codec but Opus.
	reprime func(cfg []byte, delay int64) ([]byte, error)
}

// cutCodecs is the set of codecs whose packets survive being moved to a
// different position in the stream, which is the premise this rung adds to the
// remux rung's own.
//
// codecSurvives rests on one premise: Packet.Data is the codec-native access
// unit, so a packet means the same bytes in every container. A cut needs a
// second: the packet must mean the same thing at a different position. Four
// codecs pass the first and fail the second, and none of them fails loudly,
// which is why this is an allowlist. A codec landing later must opt in here
// rather than silently ride a rule that was never checked against it.
//
//   - MP3 fails on the bit reservoir: a frame's main data may begin up to 511
//     bytes inside earlier frames, and an unsatisfied reference decodes to
//     silence rather than erroring. This is the case that makes "lossless
//     declines" the wrong rule. MP3 is lossy, and it passes codecSurvives,
//     gaplessSurvives, and PacketGrid's uniformity check (1152 everywhere). The
//     hole is invisible today only because rung 1 direct-plays format=mp3; this
//     rung is the first thing that could reach it.
//   - FLAC fails on frame numbering, but only for a multi-span cut, which is the
//     shape the sponsor-segment ask actually is. Numbering.Next is relative to
//     the preceding frame rather than anchored to zero, so a contiguous cut (a
//     head-only or tail-only one) re-reads clean; a gap in the ordinals does
//     not, because flacn's demuxer treats ordinal succession as a hard per-frame
//     invariant when it locates a packet's end. What that costs is measured in
//     TestCutFLACMultiSpanBreaks rather than guessed at, and it is worse than a
//     hard failure would be: the boundary scan wants the missing ordinal, never
//     finds it, and runs on until a CRC confirms at a later frame's end (every
//     FLAC frame's CRC-16 covers its own trailer, so the running CRC resets at
//     every boundary and confirms at "boundary plus k whole frames" too). The
//     read returns one glued packet declaring a single frame's duration, the
//     decoder decodes its first frame and drops the rest, and the stream ends in
//     a clean io.EOF having silently swallowed a quarter of the kept audio. No
//     error is raised anywhere. Its STREAMINFO MD5 also goes stale with no way
//     to say "unknown" through MuxerOptions.MD5's nil-means-inherit contract,
//     and its PTS rides the frame's own coded number rather than the container,
//     so retiming the wrapper's PTS cannot make a re-read agree with the cut
//     length. And the clincher that makes all of it moot: FLAC is lossless, so
//     rung 3 re-encodes it for CPU and zero generation loss, and
//     EncoderOptions.FirstFrame already exists so that rung can number a
//     mid-stream slice correctly. That is what this problem's solution looks
//     like, on the encode side, where it belongs.
//   - Vorbis overlap-adds with its predecessor, and the first packet to arrive
//     is treated as priming and emits nothing. packetGrid's prev <= 0 clause
//     already excludes it; it is named here anyway, because a rule that holds by
//     accident of another rule is one waiting to break.
//   - ALAC fails on nothing. Its frames are position-independent. It declines on
//     codecSurvives's own published argument, that this rung exists to avoid
//     generation loss and lossless has none, and mp4's muxer refuses its trims
//     besides, so the head snap's Delay would be unwritable.
//   - PCM is already out at codecSurvives, whose comment says why.
//
// What is left is Opus and AAC-LC.
var cutCodecs = map[codec.ID]cutCodec{
	codec.Opus: {
		preroll: opus.SeekPreroll,
		reprime: func(cfg []byte, delay int64) ([]byte, error) {
			return opus.SetPreSkip(cfg, int(delay))
		},
	},
	// AAC-LC's priming is one frame of MDCT history (aac.EncoderDelay), and it
	// lives in the container's edit list rather than in the ASC, so there is
	// nothing to reprime.
	codec.AACLC: {preroll: aac.EncoderDelay},
}

// cutWindow is a kept range of the source's decode timeline. A to of -1 means
// "to the end of the stream", which is what a ToEnd span becomes: the walk keeps
// every packet from from onward, and the arithmetic resolves the length from the
// header rather than from the window.
type cutWindow struct{ from, to int64 }

// cutResult is the whole of the cut's arithmetic, read by CutTrack (which wants
// the track and the landed spans) and Cut (which wants the windows), so the
// track a muxer is opened with and the packets a walk delivers cannot come to
// disagree.
//
// One function rather than one call: each public entry point recomputes this
// from the caller's own inputs, so a cut runs it two or three times over. That
// is the surface's shape rather than an oversight (CutTrack, Cut and PlanCut all
// take the spans, not an opaque handle a caller would have to thread), and the
// agreement rests on the computation being pure and deterministic rather than on
// its running once. It is header arithmetic with no I/O and no decode, so the
// repeat is not worth an API to avoid.
type cutResult struct {
	track   container.Track
	landed  []Span
	windows []cutWindow
}

// maxCutSample is the ceiling every input to the grid arithmetic must sit
// under: a span's bounds, the track's own Delay, and the grid.
//
// It exists because validateCutSpans cannot bound a span against a length that
// is not declared, and an ADTS source declares none while AAC-LC is on the
// allowlist, so a caller's To reaches the arithmetic below unchecked. That
// arithmetic overflows rather than saturating. An overflowed span does not
// fail, it lands, which is what makes this a guard rather than a comment:
// before it, a To of MaxInt64 was accepted and synthesized a Padding of
// 9223372036854774785.
//
// All three inputs are bounded and not just the caller's, because the positions
// the snap works in are span + Delay and the snap then adds grid - 1 on top. A
// span bound alone leaves the sum free: a container declaring an absurd Delay
// (nothing bounds one above, and mp4's progressive muxer is the only thing in
// the tree that even rejects a negative) overflows the snap just as well, and
// produced a Padding of 1525 where 501 was the answer. Three addends each under
// 2^61 sum to under 2^63, so the arithmetic below provably cannot wrap. That is
// the whole reason for the value.
//
// It is not a claim about real audio: 2^61 samples is about a million and a half
// years at 48 kHz, so nothing legitimate is refused. RemuxSegments guards its
// own sample arithmetic the same way and says the same thing when it fires
// (remux.go's "overflows the sample timeline"); it can afford 1<<62 because it
// bounds a product of two values rather than a sum of three.
const maxCutSample = 1 << 61

// snapGridDown rounds x down to a multiple of g, clamped at zero.
//
// Go truncates integer division toward zero rather than flooring it, so
// (-1500)/960*960 is -960 and not the -1920 a floor would give. The guard is
// what keeps that off the result. It is not the only way: for any x <= 0,
// x/g*g <= 0, so max(0, x/g*g) clamps to the same zero this returns, and the two
// are equivalent. The truncation is worth naming because a reader will wonder
// about it; it does not make the simpler form wrong.
func snapGridDown(x, g int64) int64 {
	if x <= 0 {
		return 0
	}
	return x / g * g
}

// snapGridUp rounds x up to a multiple of g, clamped at zero.
//
// x is a span bound plus the track's Delay, and g is the grid: all three are
// bounded under maxCutSample by the time this runs, which is what keeps the
// x + g - 1 below from overflowing. Bounding the span alone would not, since
// the Delay is the container's number rather than the caller's.
func snapGridUp(x, g int64) int64 {
	if x <= 0 {
		return 0
	}
	return (x + g - 1) / g * g
}

// CutTrack synthesizes the track a cut of track to spans would produce: its
// trims, its length, its rewritten codec config, and where the spans landed.
//
// grid is the source's packet duration from Engine.PacketGrid. Snapping to it is
// packet-aligned by definition, since it is measured as the decode duration
// every packet of the source shares rather than chosen by a caller.
//
// It is a track-level computation for the reason SpanTrack is: a plan must be
// able to state the output's length without opening anything, and a plan's
// length and the run's actual delivery must not be free to drift.
//
// # Errors and declines
//
// This returns errors throughout, including for the four conditions that are
// really declines, because its signature has nowhere to put a (nil, nil).
// PlanCut is what maps them back onto the ladder's published contract:
// CodeUnsupportedFormat here becomes a decline there, and CodeInvalidRequest
// propagates as an error. That split is the seam between "this rung cannot serve
// this" and "no rung can": an invalid span is one rung 3 would refuse
// identically, and a codec off the allowlist is one rung 3 serves happily.
func CutTrack(track container.Track, spans []Span, grid int) (container.Track, []Span, error) {
	res, err := computeCut(track, spans, grid)
	if err != nil {
		return container.Track{}, nil, err
	}
	return res.track, res.landed, nil
}

// validateCutSpans refuses spans that do not describe this file. Every one of
// these is a request rung 3 would fail identically, which is what makes it an
// error rather than a decline. The messages mirror SpanTrack's, since they are
// the same refusals one rung down.
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
			// Checked whether or not the source declares a length, which is the
			// whole point: the bound below cannot fire for a source that
			// declares none, and this arithmetic overflows rather than
			// saturating. See maxCutSample.
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span [%d, %d) overflows the sample timeline", s.From, s.To))
		case s.To == s.From:
			// SpanTrack permits this, because a zero-sample Media is coherent. A
			// zero-sample packet span is not, and it fails in the surprising
			// direction: snapping only ever widens, so this does not land empty,
			// it lands as a whole packet or a pre-roll's worth, and the caller
			// who asked to keep nothing gets audio.
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span [%d, %d) keeps no samples; a cut cannot express an empty span", s.From, s.To))
		case s.To == ToEnd && track.Samples >= 0 && s.From == track.Samples:
			// The same empty span, spelled the other way: starting at the end
			// and running to the end keeps nothing. Refusing one spelling and
			// accepting the other is the worst of both, and it is what happened
			// before this case existed. The ToEnd form did not even fail
			// usefully: it returned Samples 0 with a Delay covering a pre-roll
			// of delivered audio, and a Landed span of zero length, which the
			// rung's own promise (a landed span is never shorter than a grid)
			// says cannot happen.
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"waxflow: span [%d, ToEnd) starts at the end of the source's %d samples and so keeps none; a cut cannot express an empty span",
				s.From, track.Samples))
		}
		// The bound is checked only when there is one to check against, which is
		// SpanTrack's own call: an ADTS source declares no length at all, and a
		// bound that cannot be checked is not checked rather than refused.
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

// computeCut is the rung's arithmetic, in one place.
func computeCut(track container.Track, spans []Span, grid int) (*cutResult, error) {
	if err := validateCutSpans(track, spans); err != nil {
		return nil, err
	}
	cc, ok := cutCodecs[track.Codec]
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"waxflow: %s packets do not survive being moved within the stream, so this source cannot be cut without re-encoding",
			track.Codec))
	}
	if grid <= 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"waxflow: this source's packet durations vary, so there is no grid to cut on")
	}
	// The other two addends, checked here rather than in validateCutSpans
	// because neither is the caller's number: the Delay is what the container
	// declared and the grid is what PacketGrid measured, so a bad one declines
	// (rung 3 decodes what it can) where a bad span errors. See maxCutSample.
	if track.Delay < 0 || track.Delay >= maxCutSample || int64(grid) >= maxCutSample {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"waxflow: this source declares a %d-sample delay on a %d-sample grid, which is outside the timeline this rung can compute in",
			track.Delay, grid))
	}
	g := int64(grid)
	n := len(spans)

	// decodedEnd is where the source's audio ends on the decode timeline, or -1
	// when the source declares no length. Everything below that needs it guards
	// on it rather than branching the whole computation: an unknown length
	// inverts the arithmetic rather than defeating it, which is remuxTrailer's
	// own precedent, and the -1 propagates to Samples for it to resolve from the
	// walk.
	decodedEnd := int64(-1)
	if track.Samples >= 0 {
		decodedEnd = track.Delay + track.Samples + track.Padding
	}
	lastToEnd := spans[n-1].To == ToEnd

	// df/dt are the requested span on the decode timeline; sd/su are where the
	// packet grid makes it land.
	df := make([]int64, n)
	dt := make([]int64, n)
	sd := make([]int64, n)
	su := make([]int64, n)
	for i, s := range spans {
		df[i] = s.From + track.Delay
		// The head backs off by the codec's pre-roll before it snaps, and that
		// is not a refinement, it is the difference between a cut that works and
		// one that silently destroys the source's priming. opusenc writes a
		// pre-skip of 3840 against a 960 grid, and 3840 is a whole multiple of
		// 960: snapping df[0] alone would land exactly on it, drop all four
		// priming packets, and declare Delay 0, leaving a cold decoder at output
		// sample 0. Backing off fixes that and buys a converged decoder at a
		// From > 0 head besides, which is what makes an exact head mean exact
		// audio rather than an exact index. Our own encoder's 312 escapes the
		// bug, so every fixture in this tree would pass without this.
		sd[i] = snapGridDown(df[i]-cc.preroll, g)
		switch {
		case s.To != ToEnd:
			dt[i] = s.To + track.Delay
			su[i] = snapGridUp(dt[i], g)
		case decodedEnd >= 0:
			dt[i] = track.Delay + track.Samples
			su[i] = decodedEnd
		default:
			dt[i], su[i] = -1, -1
		}
	}

	// Sub-grid gaps overlap. Spans [0,1000) and [1200,2000) at a 960 grid give
	// su[0] = 1920 and sd[1] = 960: the keep windows overlap and the packet
	// between them is emitted twice. Any gap under about two grids does this,
	// and more once a pre-roll is backed off. Decline rather than merge, which
	// would break Landed's one-for-one correspondence with the request. Rung 3
	// serves the tiny gap exactly. Equality is adjacency, not overlap, so it
	// passes.
	for i := 0; i < n-1; i++ {
		if su[i] > sd[i+1] {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
				"waxflow: the gap between spans %d and %d is smaller than the %d-sample packet grid can express, so this cut cannot be made without re-encoding",
				i, i+1, grid))
		}
	}
	// A bounded last span whose snap overshoots the source's own end is not a
	// tail to refuse: it is a span that reaches the end of the track, which is
	// what ToEnd already means. Clamp it to the end and let the window run to
	// EOF, exactly as a ToEnd span's does. The alternative, refusing it, would
	// decline the single most ordinary cut there is ("keep the first minute"),
	// since a To of Samples snaps past the end whenever the source's decode
	// total is not a whole grid.
	tailClamped := false
	if !lastToEnd && decodedEnd >= 0 && su[n-1] > decodedEnd {
		su[n-1] = decodedEnd
		tailClamped = true
	}

	out := track
	// The head's snap slop becomes the delay: it is delivered audio the decoder
	// needs and the listener must not hear.
	out.Delay = df[0] - sd[0]

	// An unanswerable tail, and it is narrower than it looks.
	//
	// When the snap overshoots the end, the walk keeps to EOF and the true tail
	// slop is what the walk delivered minus the requested end. The header cannot
	// name that: a granule-truncated final packet (the Ogg-Opus shape) leaves the
	// packets running past the header's decode total, by 648 samples in the
	// ordinary case. remuxTrailer re-derives the padding from the walk for any
	// track that declares a delay, which repairs exactly this, so the overshoot
	// is harmless there and the arithmetic below is self-correcting.
	//
	// With no delay to trigger that branch, the header's number is the one that
	// ships, and a muxer that writes the count explicitly (Matroska's
	// DiscardPadding) would trim by it. So this declines only the combination
	// that has no repair: an overshot tail on a track whose head is exact, which
	// means a From of 0 on a source with no priming of its own.
	if tailClamped && out.Delay == 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"waxflow: span [%d, %d) ends inside the source's final packet, whose length the header does not state, and this cut has no delay for the trailer to resolve it against; re-encode it",
			spans[n-1].From, spans[n-1].To))
	}

	switch {
	case lastToEnd && track.Samples < 0:
		// Nothing to resolve the end against, so the source's own tail trim is
		// taken at its word (there is nothing to check it against) and the
		// length is what the walk resolves. remuxTrailer's t.Samples < 0 branch
		// computes decoded - Delay - Padding, which is exactly this cut's landed
		// length, so propagating the -1 needs no new branch anywhere. Reachable
		// for real: ADTS declares no length and AAC-LC is on the allowlist.
		out.Padding = track.Padding
		out.Samples = -1
	default:
		// The tail's snap slop becomes the padding, the mirror of the head's
		// becoming the delay. Where the last span runs to the source's own end
		// (a ToEnd span, or a bounded one clamped to it above), su is the decode
		// total and dt is the end of the audio, so this reduces to the source's
		// own Padding by the same definition rather than by a special case.
		out.Padding = su[n-1] - dt[n-1]
		// The landed length, not the requested one. This is the keystone.
		//
		// Walking remuxTrailer's t.Delay > 0 branch with the requested length
		// gives decoded - Delay - Samples = Padding + the interior slop, so a
		// muxer that writes the count explicitly (Matroska's DiscardPadding)
		// would eat that much real audio off the end. The interior slop is
		// delivered audio, because there is no per-splice trim to remove it
		// with, so it is part of the length. Counting it in makes the slop terms
		// cancel: remuxTrailer then yields exactly Padding, and the same number
		// that makes the trailer correct is the number Landed reports.
		var delivered int64
		for i := range spans {
			delivered += su[i] - sd[i]
		}
		out.Samples = delivered - out.Delay - out.Padding
	}
	// Stated rather than left to fall out of the struct copy, which is how it
	// drifts. A bounded last span makes the length computed rather than
	// declared, so it is exact by construction; a ToEnd one inherits whatever
	// the source's own total was worth.
	if !lastToEnd {
		out.SamplesExact = true
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
		landed[i] = Span{From: sd[i] - track.Delay, To: su[i] - track.Delay}
		windows[i] = cutWindow{from: sd[i], to: su[i]}
	}
	// The head and the tail land exactly where they were asked for: their slop
	// is expressed as the trims above rather than delivered to a listener. Only
	// the interior splices really moved.
	landed[0].From = spans[0].From
	switch {
	case !lastToEnd:
		landed[n-1].To = spans[n-1].To
	case track.Samples >= 0:
		landed[n-1].To = track.Samples
	default:
		landed[n-1].To = ToEnd
	}
	if lastToEnd || tailClamped {
		// The window runs to EOF rather than to the computed end. They differ
		// whenever the source's final packet is short or its granule truncates
		// one, and the walk must not try to cut at a boundary that is not there.
		// A clamped tail is the same situation reached from a bounded span: it
		// reaches the source's end, so it ends where the packets do.
		windows[n-1].to = -1
	}
	return &cutResult{track: out, landed: landed, windows: windows}, nil
}

// Cut returns a view of demux holding only track's packets that fall in spans,
// retimed to be contiguous: the packet-domain sibling of Slice, and the input
// side of a cut.
//
// It is a wrapper rather than a TranscodeOptions field for the reason Slice is,
// and for one more that is this rung's own. The remux rung shares
// TranscodeOptions deliberately: remuxable derives its rule by comparing option
// projections instead of hand-listing fields, which is what keeps it from
// drifting as options land, and a parallel options struct would destroy that
// derivation. The codebase has already answered "how do I express a span" twice,
// and both answers say the same thing: a span is applied by wrapping, never
// through the options.
//
// The returned Demuxer implements neither container.Seeker nor Warner nor
// Chapterer, which embedding the interface gives for free: a method set outside
// container.Demuxer is not promoted. That is the wanted answer rather than a
// gap. A segmented cut would need a Seeker, and one that seeked the source's
// timeline while the packets ran on the cut's would be wrong in a way no error
// would surface, so it fails loudly at the type assert instead.
//
// The caller owns demux. Packets stay borrowed exactly as they are through a
// plain remux: this delegates ReadPacket inward and mutates only PTS, so
// copyPackets' borrow contract holds verbatim and no copy is added.
func Cut(demux container.Demuxer, track container.Track, spans []Span, grid int) (container.Demuxer, error) {
	res, err := computeCut(track, spans, grid)
	if err != nil {
		return nil, err
	}
	return &cutDemuxer{Demuxer: demux, cut: res.track, track: track.ID, windows: res.windows}, nil
}

// cutDemuxer is Cut's view: the windows in source decode coordinates, the walk's
// position in them, and the output position it retimes onto.
type cutDemuxer struct {
	container.Demuxer
	cut     container.Track
	track   int
	windows []cutWindow
	cur     int
	pos     int64 // the next source packet's decode position
	out     int64 // the next kept packet's output decode position
}

// Tracks reports the cut's own track, which is the one the packets coming out of
// ReadPacket belong to.
//
// It has to be said explicitly, because the embedded Demuxer would otherwise
// promote the source's answer, and every field that matters would be a lie: the
// uncut length, the un-rewritten OpusHead, and the source's own trims rather
// than the ones the cut synthesized. Nothing in the intended flow reads it
// (RemuxDemuxer is handed a track rather than asking), which is exactly why it
// would have gone unnoticed: format.FromDemuxer builds its Info.Tracks straight
// off this call, so a caller assembling a Media around a cut view would get the
// source's headers over the cut's packets.
//
// One track, because that is what this view is: ReadPacket drops every other
// track's packets, and the rung is single-track by construction (muxers are).
func (c *cutDemuxer) Tracks() []container.Track { return []container.Track{c.cut} }

func (c *cutDemuxer) ReadPacket(pkt *container.Packet) error {
	for {
		if c.cur == len(c.windows) {
			// Every window is behind us. Stopping here rather than reading the
			// rest out is what makes a head-only cut cost only the head, and the
			// bare sentinel is the clean end container.Demuxer specifies.
			return io.EOF
		}
		if err := c.Demuxer.ReadPacket(pkt); err != nil {
			return err
		}
		if pkt.Track != c.track {
			continue
		}
		start, end := c.pos, c.pos+pkt.Dur
		c.pos = end
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
		// The grid is re-checked here rather than trusted from the plan, and it
		// is free: the walk is happening anyway. The boundaries were computed
		// from the header, so a plan computed against a different source (a
		// stale memo, a file replaced under an unexpired URL) would otherwise
		// splice mid-packet, and this is the one failure of this rung that no
		// error would ever surface. It is also what protects Ogg's
		// endGranule = Delay + Samples, into which the cut track's Samples flows
		// unchecked.
		if start < w.from {
			return cutStraddle(start, end, w.from)
		}
		if w.to >= 0 && end > w.to {
			return cutStraddle(start, end, w.to)
		}
		// Retimed to be contiguous: the output's decode timeline runs from 0
		// with no holes, and the cut track's Delay trims its head exactly as a
		// plain remux's does.
		pkt.PTS = c.out
		c.out += pkt.Dur
		return nil
	}
}

func cutStraddle(start, end, at int64) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
		"waxflow: source packet [%d, %d) straddles the cut boundary at %d; this stream cannot be cut without re-encoding",
		start, end, at))
}

// PlanCut reports whether opts can be served by cutting track's existing packets
// to spans and rewriting the container around them, and how.
//
// It is the cut's entry to the ladder, and it keeps the ladder's published
// contract: a request this rung cannot serve is not an error. PlanCut returns
// (nil, nil) and the caller falls through to a transcode, which cuts
// sample-exactly in the decode domain. An error means the request is wrong for
// every rung, and a transcode of it would fail identically.
//
// grid is the source's packet duration from Engine.PacketGrid, exactly as
// PlanRemuxSegments takes one. A varying grid (0) declines the cut as it
// declines the segmented remux.
//
// # Why the declines say nothing to the caller
//
// A decline's reason is not actionable: the caller's answer to every one of them
// is the same re-encode, so the reason is a debugging aid rather than a
// control-flow input, and a bare nil is the shape the ladder is built on. But
// this rung declines for six distinct reasons and a caller asking why an Opus
// cut is re-encoding has no other signal, so each is logged at Debug on its way
// out. The prose lives on the error path, where RemuxDemuxer names it.
//
// The six: a codec off the allowlist, no grid, a sub-grid gap, an unanswerable
// tail, a source whose Delay or grid is outside the timeline this rung computes
// in, and a codec config the reprime cannot rewrite. The last is worth naming
// because it looks like it should be an error and is not: a config this rung
// cannot parse is one the demuxer built and the decoder still can, so the honest
// answer is to hand the request to a rung that decodes rather than to refuse it
// on everyone's behalf. Its own error code says CodeUnsupportedFormat for the
// same reason every malformed-input path here does, and that is precisely what a
// decline is made of.
func (e *Engine) PlanCut(track container.Track, opts TranscodeOptions, spans []Span, grid int) (*CutPlan, error) {
	cut, landed, err := CutTrack(track, spans, grid)
	if err != nil {
		// The seam. CutTrack cannot express a decline through its signature, so
		// it returns codes and this maps them onto the ladder's contract.
		if waxerr.CodeOf(err) == waxerr.CodeUnsupportedFormat {
			e.log.Debug("cut declined", "codec", track.Codec, "grid", grid, "reason", err)
			return nil, nil
		}
		return nil, err
	}
	rp, err := e.PlanRemux(cut, opts)
	if err != nil || rp == nil {
		return nil, err
	}
	// The trims are new, and PlanRemux only ever checked the source's. A cut's
	// track carries a Delay and a Padding the source never had, and the
	// allowlist above screens codecs rather than destinations, so this is the
	// one question nothing else in the ladder asks.
	if !cutTrimsExpressible(rp.Container, cut.Delay, cut.Padding) {
		e.log.Debug("cut declined", "reason", "the destination cannot signal the cut's trims",
			"outContainer", rp.Container, "delay", cut.Delay, "padding", cut.Padding)
		return nil, nil
	}
	rp.Versions = []string{RemuxVersion, CutVersion}
	return &CutPlan{RemuxPlan: *rp, Landed: landed}, nil
}

// cutTrimsExpressible reports whether containerName can signal the trims a cut
// synthesized. It is gaplessSurvives's question asked the other way around: that
// one asks whether the source's trims survive the codec, this one whether the
// cut's own trims survive the destination.
//
// gaplessSurvives argues at length for a codec line over a container table, and
// it is right on its own terms. This is the narrow case that genuinely needs the
// container's side, and it stays narrow: the early-out is gaplessSurvives's own,
// so a cut that synthesized no trims (a From of 0 on a source with no delay,
// running to the end) pays one comparison and is never asked about its
// destination.
//
// Two concrete failures make it necessary, and neither is theoretical:
//
//   - fMP4 dies at End, after the whole file is written. Its guard is not
//     codec-keyed: only the AAC branch of Begin sets the muxer's delay, so an
//     AAC track with Delay 0 (an MP4 muxed without iTunSMPB or an edit list,
//     which is common) cut to a Padding that is not a whole frame trips it about
//     1023 times in 1024. That is precisely the "die inside the muxer part way
//     through a response" failure gaplessSurvives exists to prevent.
//   - ADTS silently plays the audio the caller removed. gaplessSurvives
//     deliberately permits AAC to ADTS with a nonzero delay, and its reasoning
//     is sound for a remux: a transcode to ADTS cannot signal its fresh
//     encoder's priming either, so "a property both rungs share is not one this
//     rung can fix". That inverts here. Rung 3 cuts sample-exactly in the decode
//     domain, while this rung's Delay is at least the pre-roll it just backed
//     off by and covers real source audio from before the cut point. Dropping it
//     means a sponsor-segment cut plays the last 20 to 40 ms of the ad. That is
//     not unsignalled priming, it is the wrong audio, and only this rung has it.
//
// An unknown container declines, which is the allowlist's own posture: a
// destination landing later must be considered here rather than silently ride.
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
	case mp4Progressive:
		// The flat muxer writes its edit list at End, when it knows everything,
		// so it can express either trim.
		return true
	case "aac":
		// The fragmented muxer, whose edit list is written at Begin from the
		// track's delay. With no delay to write one from, a padding arriving at
		// End has nowhere to go and the muxer says so.
		return delay > 0
	}
	return false
}

// The pieces assemble like this, and the order is the ladder's own: plan, then
// run only what the plan accepted.
//
//	grid, err := e.PacketGrid(src, hint)
//	plan, err := e.PlanCut(track, opts, spans, grid) // (nil, nil) declines
//	cut, landed, err := CutTrack(track, spans, grid)
//	demux, err := Cut(demux, track, spans, grid)
//	res, err := e.RemuxDemuxer(ctx, demux, cut, dst, opts)
//
// RemuxDemuxer takes CutTrack's track and not plan.Track, which looks like the
// redundant choice and is not: plan.Track is PlanRemux's ID-0 normalization, for
// opening the muxer with, while the packet walk filters on the source's own
// track ID, which CutTrack's copy preserves. Handing plan.Track to the run would
// filter out every packet of a source whose track is not number 0 and write an
// empty file.
//
// Skipping PlanCut and calling RemuxDemuxer straight is what the destination
// decline cannot protect: PlanCut is where it lives, because a decline is a
// planning answer.
