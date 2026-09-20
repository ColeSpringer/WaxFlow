package waxflow

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
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
//
// cut-2: the HE-AAC decode preroll (aac.HESeekPreroll) grew from 4096 to
// 24576 samples, and the preroll picks each span's first kept packet, so
// the same request now cuts different bytes on HE-AAC sources.
//
// cut-3: every interior edge of a multi-span cut snaps INWARD, with no
// pre-roll, so the bytes and the Landed semantics both change for every cut
// of more than one span (ADR-0011). TranscodeOptions.SpliceTrims rides in
// the cache key beside this.
const CutVersion = "cut-3"

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
	// snaps INWARD to the packet grid and says so here: there is no per-splice
	// trim to hide slop in, so the cut gives up under one packet of wanted
	// audio at each interior edge rather than deliver audio the caller asked
	// to remove. A caller that needs to know where its cut points really fell
	// reads them off this.
	//
	// The invariant is one-sided: a cut never delivers audio from outside the
	// request, and each interior edge lands within one packet inside it. What
	// it cannot promise is a clean decoder at a join. A stream copy does not
	// reset the decoder there, so the first packet of an interior span decodes
	// against the previous span's state for as long as that codec's memory
	// runs -- the artefact every stream-copy tool has, and the reason
	// TranscodeOptions.SpliceTrims exists for the destinations that can carry
	// an exact splice. See ADR-0011.
	Landed []Span
}

// cutCodec is one allowlisted codec's cut parameters.
type cutCodec struct {
	// preroll is how far ahead of a kept range's start the packet walk must
	// begin for the decoder's output at that start to be the audio the encoder
	// wrote rather than a cold decoder's approximation of it.
	//
	// It applies to the FIRST span only. That span's pre-roll becomes the
	// synthesized Delay and is trimmed, so the head is exact and the pre-roll
	// costs bytes and no audio. An interior span has no per-splice trim to
	// hide one in, so a pre-roll there is delivered: 1024 samples on AAC-LC,
	// 3840 on Opus and 24576 on HE-AAC -- 21 ms, 80 ms and 512 ms of the span
	// the caller asked to REMOVE, audible at every join and shifting
	// everything after it. So interior heads take none and snap inward
	// instead (ADR-0011), which costs under one packet of wanted audio and
	// reports it in Landed.
	//
	// SpliceTrims buys the pre-roll back on the destinations that can discard
	// it per packet: the same packets are walked and each carries a
	// full-duration trim, so the decoder converges on audio no listener
	// hears. Matroska alone can say that.
	preroll int64
	// reprime rewrites the codec config's own priming field to the cut's
	// synthesized delay, for a codec that carries priming there rather than
	// leaving it to the container. Nil for a codec whose config says nothing
	// about priming, which is every codec but Opus.
	reprime func(cfg []byte, delay int64) ([]byte, error)
	// headOnly marks a codec whose cuts must start at sample 0 (computeCut
	// enforces it). Such a codec still cuts, but CutFormats does not
	// advertise it: a client reading delivery.cutFormats expects an
	// arbitrary from/to span to engage the rung, and for a head-only codec
	// most spans silently fall through to a re-encode instead.
	headOnly bool
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
	// nothing to reprime. HE-AAC adds the SBR chain's history on top, and its
	// priming likewise lives in the container.
	codec.AACLC: {preroll: aac.EncoderDelay},
	// headOnly: an HE-AAC cut must keep the stream head (computeCut's
	// refusal explains why), so most spans a client would ask for fall
	// through to a re-encode.
	codec.HEAAC: {preroll: aac.HESeekPreroll, headOnly: true},
}

// Cuttable reports whether track's codec is one whose packets survive being
// moved within a stream, which is the premise the cut rung rests on. It is the
// exported form of the cutCodecs membership test, so a caller can skip the
// packet-grid walk for a codec no cut could ever serve.
//
// It answers only the codec question, which is the cheap one: a true here does
// not promise the cut will be taken, since a span holding no whole packet,
// two spans too close together, a trim the walk could not place, or a
// destination that cannot signal the trims still declines inside PlanCut.
// It is the fast negative, not a guarantee of the positive. See cutCodecs for
// why the set is an allowlist rather than a lossless rule.
func Cuttable(track container.Track) bool {
	_, ok := cutCodecs[track.Codec]
	return ok
}

// CutFormats lists the output formats the cut rung serves without
// re-encoding, in table order. A format qualifies only where the cut is
// reachable on every surface it may be asked for: its codec is in the cut
// allowlist WITHOUT the head-only restriction (a head-anchored codec cuts,
// but advertising it would promise arbitrary spans it silently re-encodes),
// it has a live progressive form (/stream can serve it), and it has a
// segmented (HLS) form. So the one flat list is honest on both surfaces. A
// client names one of these (with a from/to span) to reach the cut;
// format=auto never does. Today: opus and aac (he-aac cuts head-anchored
// spans when asked, unadvertised).
func CutFormats() []string {
	var names []string
	for _, o := range outputs {
		// A remux-only row is not advertised (Outputs() precedent); its
		// sources reach the cut through the family redirect on the row a
		// client can name (format=aac cuts an HE-AAC source).
		if cc, ok := cutCodecs[o.codecID]; ok && !cc.headOnly && o.live && o.hls != nil && !o.remuxOnly {
			names = append(names, o.name)
		}
	}
	return names
}

// cutWindow is a kept range of the source's decode timeline. A to of -1 means
// "to the end of the stream", which is what a ToEnd span becomes: the walk keeps
// every packet from from onward, and the arithmetic resolves the length from the
// header rather than from the window.
type cutWindow struct{ from, to int64 }

// cutResult is the whole of the cut's arithmetic, read by CutTrack (which wants
// the track and the landed spans) and Cut (which wants the windows and the
// source's trims), so the track a muxer is opened with and the packets a walk
// delivers cannot come to disagree.
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
	// trims is the source's inner trims the windows were placed around, the
	// ones the view forwards; a packet carrying any other refuses.
	trims []container.PacketTrim
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
// All four inputs are bounded and not just the caller's, because the positions
// the snap works in are span + Delay + the trims before it (MidPadding at
// most) and the snap then adds grid - 1 on top. A span bound alone leaves the
// sum free: a container declaring an absurd Delay (nothing bounds one above,
// and mp4's progressive muxer is the only thing in the tree that even rejects
// a negative) overflows the snap just as well, and produced a Padding of 1525
// where 501 was the answer. Four addends each under 2^61 sum to under 2^63, so
// the arithmetic below provably cannot wrap. That is the whole reason for the
// value.
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

// placeableTrims reports whether track's inner trims are ones a cut can plan
// around on a g-sample grid: every trim MidPadding sums is listed, in order,
// each inside one packet and ending on its boundary, and the sum is under
// maxCutSample like the other addends. A walk that ran out of room leaves the
// list short, a track nothing walked has a sum and no list, and a caller's
// own track may say anything.
func placeableTrims(track container.Track, g int64) bool {
	if !track.MidTrimsComplete() || track.MidPadding >= maxCutSample {
		return false
	}
	var prev int64
	for _, tr := range track.MidTrims {
		if tr.Samples <= 0 || tr.Samples > g || tr.Pos < prev || (tr.Pos+tr.Samples)%g != 0 {
			return false
		}
		prev = tr.Pos + tr.Samples
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
// CodeUnsupportedFormat here becomes a decline there, while CodeInvalidRequest
// and CodeMalformedInput propagate as errors. That split is the seam between "this rung cannot serve
// this" and "no rung can": an invalid span is one rung 3 would refuse
// identically, and a codec off the allowlist is one rung 3 serves happily.
func CutTrack(track container.Track, opts TranscodeOptions, spans []Span, grid int) (container.Track, []Span, error) {
	res, err := computeCut(track, opts, spans, grid)
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
func computeCut(track container.Track, opts TranscodeOptions, spans []Span, grid int) (*cutResult, error) {
	if err := validateCutSpans(track, spans); err != nil {
		return nil, err
	}
	cc, ok := cutCodecs[track.Codec]
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"waxflow: %s packets do not survive being moved within the stream, so this source cannot be cut without re-encoding",
			track.Codec))
	}
	// An HE-AAC cut must keep the stream head: the SBR header rides in the
	// payload fills at the encoder's cadence, and file-oriented encoders
	// write exactly one, at the first AU. A slice that drops it hands a
	// fresh decoder no header, and the high band conceals to a muted
	// upsample for the rest of the file with no error anywhere. Keeping AU
	// zero makes a cut never decode worse than its source; rung 3 serves a
	// mid-stream slice with the header it read on the way.
	if track.Codec == codec.HEAAC && spans[0].From > 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"waxflow: an HE-AAC cut must start at sample 0 (the SBR header lives in the leading access units; a mid-stream slice would play with a muted high band)")
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
	// A trim inside the run is where the two timelines part: past it every
	// packet starts at a grid position minus the trims so far, which
	// PacketGrid cannot see because Dur is unchanged. So the arithmetic below
	// runs on the raw timeline, carrying each bound past the trims before it,
	// which needs every trim placed on the grid. A walk that ran out of room,
	// a track nothing walked, or a caller's own list declines
	// (CodeUnsupportedFormat, a decline like the grid checks above); the cut
	// view refuses mid-walk for a source nothing measured first.
	if (track.MidPadding > 0 || len(track.MidTrims) > 0) && !placeableTrims(track, g) {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"waxflow: this source trims %d samples inside the run at places the walk did not record on its %d-sample grid; this cut cannot be made without re-encoding",
			track.MidPadding, grid))
	}
	n := len(spans)

	// decodedEnd is where the source's decode ends on the raw timeline, or -1
	// when the source declares no length. Everything below that needs it guards
	// on it rather than branching the whole computation: an unknown length
	// inverts the arithmetic rather than defeating it, which is remuxTrailer's
	// own precedent, and the -1 propagates to Samples for it to resolve from the
	// walk. The inner trims are in the raw run like the tail trim is, and the
	// audio ends a tail trim before it.
	decodedEnd := int64(-1)
	if track.Samples >= 0 {
		decodedEnd = track.Delay + track.Samples + track.Padding + track.MidPadding
	}
	lastToEnd := spans[n-1].To == ToEnd

	// SpliceTrims buys exact interior splices from a destination that states
	// a trim per packet, and costs every other destination: the cut's track
	// then trims inside its run, which PlanRemux declines everywhere but
	// Matroska. A single-span cut has no interior edge, so it is ignored
	// there rather than narrowing a cut for nothing.
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
		// The FIRST span's head backs off by the codec's pre-roll before it
		// snaps, and that is not a refinement, it is the difference between a
		// cut that works and one that silently destroys the source's priming.
		// opusenc writes a pre-skip of 3840 against a 960 grid, and 3840 is a
		// whole multiple of 960: snapping df[0] alone would land exactly on it,
		// drop all four priming packets, and declare Delay 0, leaving a cold
		// decoder at output sample 0. Backing off fixes that and buys a
		// converged decoder at a From > 0 head besides, which is what makes an
		// exact head mean exact audio rather than an exact index. Our own
		// encoder's 312 escapes the bug, so every fixture in this tree would
		// pass without this. The slop it creates is the synthesized Delay, so
		// it costs bytes and no audio.
		//
		// Every other head snaps INWARD, with no pre-roll. An interior splice
		// has no trim to hide slop in, so anything the snap adds is audio from
		// outside the request played to a listener -- and with the pre-roll
		// that was 40 to 100 ms of the span the caller asked to remove, at
		// every join. Snapping in loses under one packet of wanted audio
		// instead, which Landed reports. See ADR-0011.
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
			// The mirror of the head: the last span's tail slop becomes the
			// synthesized Padding and is trimmed, every interior tail snaps
			// in -- or, spliced, snaps out and states the slop as the last
			// kept packet's own trim, which makes it exact.
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

	// Adjacent spans -- one range named in two pieces, which
	// validateCutSpans allows -- have no gap for the two snaps to fall
	// into. An off-grid boundary between them would drop the packet it
	// sits in, and that packet is not audio the caller asked to remove: it
	// is inside the union of the two spans. So the pair shares one
	// boundary, the head's, and the packet goes to the span that starts
	// there. Landed says which, and only there does a span's own landing
	// reach past its own request.
	//
	// Not under SpliceTrims, where the next head's pre-roll is already
	// inside this window; the overlap check below declines that.
	for i := 0; !splice && i < n-1; i++ {
		if spans[i].To == spans[i+1].From && su[i] >= 0 {
			su[i] = sd[i+1]
		}
	}

	// Snapping in can empty a span that snapping out never could: a request
	// narrower than the grid, or one that falls between two packet
	// boundaries, keeps no whole packet. Declining is the honest answer --
	// rung 3 re-encodes it exactly -- where delivering the nearest packet
	// would hand back audio from outside the request. After the merge
	// above, because a short span adjacent to the next one is not empty.
	for i := range spans {
		if su[i] >= 0 && hd[i] >= su[i] {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
				"waxflow: span %d keeps no whole packet on this source's %d-sample grid, so this cut cannot be made without re-encoding",
				i, grid))
		}
	}

	// Overlapping keep windows would emit the packet between them twice, and
	// break Landed's one-for-one correspondence with the request. Inward
	// snapping only ever widens a gap, so under the default policy this can
	// only fire against the two edges that still snap out: the first head's
	// pre-roll reaching into a second span that starts inside it. Under
	// SpliceTrims it fires much as it used to, since every interior head
	// backs off by the pre-roll again and every interior tail snaps out.
	// Decline rather than merge; rung 3 serves the tiny gap exactly.
	// Equality is adjacency, not overlap, so it passes.
	for i := 0; i < n-1; i++ {
		if su[i] > sd[i+1] {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
				"waxflow: spans %d and %d sit closer together than this cut can keep them on a %d-sample packet grid, so it cannot be made without re-encoding",
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
	// The trims the kept packets carry stay on them (the view forwards each,
	// Matroska writes it), so they are the cut track's own MidPadding, which is
	// what PlanRemux declines every other destination on. The final kept
	// packet's is left out: the trailer restates that one, and the tail slop
	// below covers it. Each is re-placed on the output's raw timeline, where
	// the kept windows run back to back.
	out.MidPadding, out.MidTrims = 0, nil
	var outRaw int64
	for i := range spans {
		base := outRaw
		add := func(pos, samples int64) {
			out.MidPadding += samples
			out.MidTrims = append(out.MidTrims, container.PacketTrim{Pos: base + pos - sd[i], Samples: samples})
		}
		// A spliced interior head's pre-roll: every packet before the splice
		// point is decoded and discarded whole, so the decoder converges on
		// audio nobody hears.
		if splice && i > 0 {
			for p := sd[i]; p < hd[i]; p += g {
				add(p, g)
			}
		}
		// A spliced interior tail: the slop past the requested end is the
		// last kept packet's own trim, which is what makes the splice exact.
		// A source trim already on that packet is not additional, it is the
		// same packet's tail, so the larger of the two wins.
		tailSlop := int64(0)
		if splice && i < n-1 && su[i] >= 0 {
			tailSlop = su[i] - dt[i]
		}
		for _, tr := range trimsWithin(track.MidTrims, sd[i], su[i]) {
			if i == n-1 && !lastToEnd && !tailClamped && tr.Pos+tr.Samples == su[i] {
				continue
			}
			if splice && i > 0 && tr.Pos+tr.Samples <= hd[i] {
				// Inside the pre-roll, whose packets the loop above already
				// discarded whole. Adding this again would count the same
				// samples twice and leave the list out of order, which
				// plannedTrim binary-searches.
				continue
			}
			if tailSlop > 0 && tr.Pos+tr.Samples == su[i] {
				// The same packet's tail, not an additional trim. It cannot
				// exceed the slop -- a track position never maps into a
				// trimmed region, so the requested end is at or before the
				// trim's start -- and the max says so rather than assuming it.
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
		// own Padding by the same definition rather than by a special case. It
		// is in the final packet's own samples, which is what a trailer's trim
		// means, so a trim the source stated on that packet is inside it.
		out.Padding = su[n-1] - dt[n-1]
		// The landed length, not the requested one. This is the keystone.
		//
		// Walking remuxTrailer's t.Delay > 0 branch with the requested length
		// gives decoded - Delay - Samples = Padding plus however far the
		// interior edges moved, so a muxer that writes the count explicitly
		// (Matroska's DiscardPadding) would eat that much real audio off the
		// end. The length has to be what the windows deliver, whichever side
		// of the request they fall on: under the default policy they fall
		// inside it, and under SpliceTrims the pre-roll they take in is
		// removed again by the trims below. Either way the terms cancel,
		// remuxTrailer yields exactly Padding, and the number that makes the
		// trailer correct is the number Landed reports. The trims the kept
		// packets carry are not delivered, so they come off.
		var delivered int64
		for i := range spans {
			delivered += su[i] - sd[i]
		}
		out.Samples = delivered - out.MidPadding - out.Delay - out.Padding
	}
	// Stated rather than left to fall out of the struct copy, which is how it
	// drifts. A bounded last span makes the length computed rather than
	// declared, so it is exact by construction; a ToEnd one inherits whatever
	// the source's own total was worth.
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
	// The head and the tail land exactly where they were asked for: their slop
	// is expressed as the trims above rather than delivered to a listener. Only
	// the interior splices really moved.
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
		// The window runs to EOF rather than to the computed end. They differ
		// whenever the source's final packet is short or its granule truncates
		// one, and the walk must not try to cut at a boundary that is not there.
		// A clamped tail is the same situation reached from a bounded span: it
		// reaches the source's end, so it ends where the packets do.
		windows[n-1].to = -1
	}
	return &cutResult{track: out, landed: landed, windows: windows, trims: track.MidTrims}, nil
}

// adoptMeasured overlays onto a fresh header open the facts only a measurement
// establishes: the length, and the gapless trims a container states per packet
// rather than in its header (see container.Track.MidPadding). A measured track
// whose Samples is negative measured nothing, so the header's own stands.
//
// It is one function because plan and run must agree field for field: the cut
// windows, the trailer and the init segment all read these, and a run that
// re-derived a subset of them would bound its spans differently from the plan
// that was advertised and keyed.
func adoptMeasured(track, measured container.Track) container.Track {
	if measured.Samples < 0 {
		return track
	}
	track.Samples, track.SamplesExact, track.SamplesAdvisory = measured.Samples, true, false
	track.Padding, track.MidPadding, track.MidTrims = measured.Padding, measured.MidPadding, measured.MidTrims
	return track
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
// plain remux: this delegates ReadPacket inward and mutates only the two
// fields that are the cut's to state, PTS and Padding, so copyPackets'
// borrow contract holds verbatim over Data and no copy is added. Padding
// comes back as the plan's own trim for that packet, which is the source's
// value clamped into the packet under the default policy and the
// synthesized discard under SpliceTrims.
func Cut(demux container.Demuxer, track container.Track, opts TranscodeOptions, spans []Span, grid int) (container.Demuxer, error) {
	res, err := computeCut(track, opts, spans, grid)
	if err != nil {
		return nil, err
	}
	return &cutDemuxer{Demuxer: demux, cut: res.track, track: track.ID, windows: res.windows, trims: res.trims}, nil
}

// cutDemuxer is Cut's view: the windows in source raw coordinates, the walk's
// position in them, and the output position it retimes onto.
type cutDemuxer struct {
	container.Demuxer
	cut     container.Track
	track   int
	windows []cutWindow
	trims   []container.PacketTrim // the source's inner trims the plan placed
	cur     int
	pos     int64 // the next source packet's raw position
	out     int64 // the next kept packet's output position, on the delivered timeline
	outRaw  int64 // the same position untrimmed, which is what cut.MidTrims is placed on
	prevPad int64 // the previous source packet's own trim, clamped to the packet
	// havePrev says prevPad describes a packet this view read: false at the
	// start and after a seek, when the packet before the cursor is unknown.
	havePrev bool
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

// Warnings forwards the source's list: embedding the Demuxer interface
// promotes nothing outside it, and the copy rung reads this through
// container.Warner for the damage its walk found.
func (c *cutDemuxer) Warnings() []container.Warning {
	if w, ok := c.Demuxer.(container.Warner); ok {
		return w.Warnings()
	}
	return nil
}

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
		// Something followed the previous packet, so its trim was an inner one
		// and must be the one the plan placed there, or the windows were
		// computed for a different file: the plan declines trims it cannot
		// place, and this is the backstop for a source nothing measured first
		// or a stale measure. Checked before the windows advance, so the last
		// kept packet is covered too; the packet that ends the walk never is.
		if c.havePrev {
			if planned := plannedTrim(c.trims, start); planned != c.prevPad {
				return cutTrimMismatch(c.prevPad, planned, start)
			}
		}
		c.prevPad, c.havePrev = min(max(pkt.Padding, 0), pkt.Dur), true
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
		// The trim this packet leaves the view with is the plan's, read off
		// the cut track's own list by output position so the two cannot come
		// to disagree: under SpliceTrims that list holds the pre-roll
		// discards and the exact tail slop, which the source never stated.
		// The source's own trim still wins where the plan left one out --
		// the final kept packet's, which the trailer restates.
		pad := max(c.prevPad, plannedTrim(c.cut.MidTrims, c.outRaw+pkt.Dur))
		pkt.Padding = pad
		c.outRaw += pkt.Dur
		// Retimed to be contiguous: the output's timeline runs from 0 with no
		// holes, and the cut track's Delay trims its head exactly as a plain
		// remux's does. A kept packet's own trim stays on it and the position
		// advances past it as a demuxer's does (container.Packet.Padding), so
		// a wholly discarded pre-roll packet shares the splice's timestamp.
		pkt.PTS = c.out
		c.out += pkt.Dur - pad
		return nil
	}
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
// this rung declines for eight distinct reasons and a caller asking why an Opus
// cut is re-encoding has no other signal, so each is logged at Debug on its way
// out. The prose lives on the error path, where RemuxDemuxer names it.
//
// The eight: a codec off the allowlist, no grid, a span that keeps no whole
// packet, two spans closer together than the windows can be kept apart, a
// source whose Delay or grid is outside the timeline this rung computes in, a
// source that trims inside its run at places the walk did not record, an
// HE-AAC span that does not keep the stream head, and a codec config the
// reprime cannot rewrite. The last is worth naming because it looks like it should be an
// error and is not: a priming this rung computed and OpusHead's 16-bit field
// cannot hold is this rung's limit, not the file's, so the honest answer is to
// hand the request to a rung that re-encodes rather than to refuse it on
// everyone's behalf, and CodeUnsupportedFormat is exactly what a decline is
// made of. Two more come from the destination: PlanRemux declines a cut whose
// kept packets carry a trim for every destination but Matroska (the cut
// track's MidPadding is exactly those), and cutTrimsExpressible declines one
// whose head or tail trim the destination cannot signal.
//
// Only that code declines. A config that will not parse at all is different
// and now errors: since the codes split it says CodeMalformedInput, and a
// config the demuxer built and the decoder accepts does not reach that answer.
// A damaged source errors for the same reason: falling through to rung 3 would
// only fail there with the same words, several minutes of decoding later,
// under a status that told the client to send a different format.
func (e *Engine) PlanCut(track container.Track, opts TranscodeOptions, spans []Span, grid int) (*CutPlan, error) {
	cut, landed, err := CutTrack(track, opts, spans, grid)
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
	if err != nil {
		return nil, err
	}
	if rp == nil {
		// PlanRemux returns a bare nil, so the one decline a caller is most
		// likely to have caused gets named here. SpliceTrims makes the cut's
		// track trim inside its run, and only Matroska states that per
		// packet, so every other container falls through to a re-encode.
		//
		// The source's own inner trims decline the same destinations, and
		// dropping the option would not lift that, so the remedy is only
		// offered where the option is the whole of the reason.
		if opts.SpliceTrims && len(spans) > 1 && cut.MidPadding > 0 {
			reason := "SpliceTrims needs a destination that states a trim per packet; ask for mka or webm, or drop the option and take the snapped splices"
			if track.MidPadding > 0 {
				reason = "SpliceTrims needs a destination that states a trim per packet, and this source trims inside its run too, so only mka or webm can serve it at all"
			}
			e.log.Debug("cut declined", "reason", reason,
				"outFormat", opts.Format, "outContainer", opts.Container)
		}
		return nil, nil
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
	// Appended rather than replaced: PlanRemux put the destination muxer's
	// term there (ADR-0004's container term), and a progressive cut writes
	// through that muxer like any other remux does.
	rp.Versions = append(rp.Versions, CutVersion)
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
	case ContainerProgressive:
		// The flat muxer writes its edit list at End, when it knows everything,
		// so it can express either trim.
		return true
	case "aac", "he-aac":
		// The fragmented muxer (both rows' default), whose edit list is
		// written at Begin from the track's delay. With no delay to write one
		// from, a padding arriving at End has nowhere to go and the muxer
		// says so.
		return delay > 0
	}
	return false
}

// The pieces assemble like this, and the order is the ladder's own: plan, then
// run only what the plan accepted.
//
//	grid, err := e.PacketGrid(src, hint)
//	plan, err := e.PlanCut(track, opts, spans, grid) // (nil, nil) declines
//	cut, landed, err := CutTrack(track, opts, spans, grid)
//	demux, err := Cut(demux, track, opts, spans, grid)
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

// CutStream cuts src's existing packets to spans and rewrites the container
// around them, opening the source itself: the run half of the cut rung, and the
// packet-move sibling of Remux. No decode, no DSP, no encode, so no generation
// loss; the output holds the kept access units byte for byte, retimed to be
// contiguous.
//
// opts and spans must be a pair PlanCut accepted; a request it declines is an
// error here rather than a silent re-encode, for the reason Remux gives about
// PlanRemux: a caller reaching this directly has already chosen the rung, and a
// fallback it did not ask for would be the wrong kind of help. The ladder calls
// PlanCut first and falls through on its own.
//
// grid is the source's packet duration from Engine.PacketGrid, and measured is
// the source's measured track, both threaded in from the plan rather than
// re-derived here, so the bytes this delivers are the ones the plan and the
// cache key were computed against. It is the assembly recipe above, minus the
// plan step, in one call: the engine owns the open-and-assemble exactly as it
// does for Remux.
//
// measured carries what a fresh header open cannot know and the plan can. An
// undeclared-length source (AAC-LC in ADTS) reports Samples -1 from its headers,
// while the plan measured the true length off the same source. That length is
// not cosmetic: the muxer's init segment encodes it (an fMP4 moov duration), so
// running from the header's -1 would write a stream whose own duration disagrees
// with the plan's advertised one. A Matroska source's trims are the same kind of
// fact for the same reason: they live on the blocks rather than in the header, so
// only a walk finds them, and computeCut's decodedEnd reads the tail trim. Plan
// and run must read one track or their windows drift. Everything else (codec,
// config, delay, track ID) a fresh open reads identically. Pass a track whose
// Samples is negative to take the header's own, which is what a source that
// declares its length already has.
//
// It is the SOURCE's track, not CutPlan.Track. The two differ in exactly the
// fields this adopts, and a cut's own track carries a length and trims the
// source never had: under TranscodeOptions.SpliceTrims those trims are the
// pre-roll discards the cut synthesized, and feeding them back in as if the
// source had stated them makes the run plan a different cut from the one the
// plan did. The walk's trim check refuses that rather than writing it, so the
// mistake surfaces as a refusal naming a trim the source does not carry.
func (e *Engine) CutStream(ctx context.Context, src container.Source, hint string, dst io.Writer,
	opts TranscodeOptions, spans []Span, grid int, measured container.Track) (*TranscodeResult, error) {
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, err
	}
	track := adoptMeasured(info.Default(), measured)
	// CutTrack's track, not a plan's, and for the same reason RemuxDemuxer takes
	// the demuxer's own track: the walk filters on the source's track ID, which
	// CutTrack preserves, while a plan normalizes it to 0. There is no
	// demux.Close, mirroring Remux; the source.File owns the handle.
	cut, _, err := CutTrack(track, opts, spans, grid)
	if err != nil {
		return nil, err
	}
	cutDemux, err := Cut(demux, track, opts, spans, grid)
	if err != nil {
		return nil, err
	}
	return e.RemuxDemuxer(ctx, cutDemux, cut, dst, opts)
}
