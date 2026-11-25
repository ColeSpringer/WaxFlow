package waxflow

import (
	"context"
	"fmt"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// CutSegmentPlan describes the segmented (CMAF) form of a cut, as CutPlan
// describes its progressive form. It embeds RemuxSegmentPlan so the delivery
// layer reads one shape whichever rung answered.
type CutSegmentPlan struct {
	RemuxSegmentPlan
	// Landed is where each requested span fell, one for one, as
	// CutPlan.Landed reports it.
	Landed []Span
	// Grid is the packet grid and Source the track the plan was handed:
	// CutSegments takes both, so the run cuts on the keyed boundaries from
	// the same length and trims (ADR-0013).
	Grid   int
	Source container.Track
}

// PlanCutSegments plans the segmented form of a cut: CutTrack for the cut's
// track, PlanRemuxSegments over that track, then PlanCut's trim check
// (cutTrimsExpressible) with fMP4's rule. It asks the init header where
// PlanCut asks the destination's muxer, judges the copy at the segmented
// depth (segmentBitDepth), and also declines a source a restart's seek
// cannot place.
//
// Invalid options, a format with no segmented form, an invalid span and then
// a NaN, negative or over-long segSeconds are refused before any decline.
// grid is asked as PlanCut asks it, and 0 declines. A decline returns
// (nil, nil) and the caller falls through to a transcode; an error means no
// rung can serve the request.
func (e *Engine) PlanCutSegments(track container.Track, opts TranscodeOptions, spans []Span,
	grid func() (int, error), segSeconds float64) (*CutSegmentPlan, error) {
	if err := checkSegmented(opts); err != nil {
		return nil, err
	}
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	if row.hls == nil {
		return nil, noSegmentedForm(opts.Format)
	}
	// Refused before any decline, as a transcode of the span refuses them.
	if err := validateCutSpans(track, spans); err != nil {
		return nil, err
	}
	if _, err := segmentSeconds(segSeconds); err != nil {
		return nil, err
	}
	g, asked, err := e.cutGrid(track, segmentBitDepth(opts, track.Fmt), spans, true, grid, func(g int) bool {
		p, err := e.planCutSegmentsOn(track, opts, spans, g, segSeconds)
		return p == nil && err == nil
	})
	if !asked || err != nil {
		return nil, err
	}
	return e.planCutSegmentsOn(track, opts, spans, g, segSeconds)
}

// planCutSegmentsOn is PlanCutSegments on a grid of g.
func (e *Engine) planCutSegmentsOn(track container.Track, opts TranscodeOptions, spans []Span, g int,
	segSeconds float64) (*CutSegmentPlan, error) {
	cutTrack, landed, err := CutTrack(track, opts, spans, g)
	if err != nil {
		// CutTrack states its declines as CodeUnsupportedFormat.
		if waxerr.CodeOf(err) == waxerr.CodeUnsupportedFormat {
			e.log.Debug("segmented cut declined", "codec", track.Codec, "grid", g, "reason", err)
			return nil, nil
		}
		return nil, err
	}
	// A restart seeks the source, and a trim that empties a packet makes the
	// landing ambiguous (see cutSeekable).
	if wholePacketTrim(track.MidTrims, int64(g)) {
		e.log.Debug("segmented cut declined", "reason", "a trim empties a packet, so a restart's seek is ambiguous")
		return nil, nil
	}
	rsp, err := e.PlanRemuxSegments(cutTrack, opts, segSeconds, func() (int, error) { return g, nil })
	if err != nil || rsp == nil {
		return nil, err
	}
	// The cut's Delay and Padding are its own, and PlanRemuxSegments checked
	// only the source's.
	row, fm, _, err := remuxForm(cutTrack, opts)
	if err != nil {
		return nil, err
	}
	if out := copyDest(row, fm, true); !cutTrimsExpressible(out, cutTrack.Delay, cutTrack.Padding) {
		e.log.Debug("segmented cut declined", "reason", "the destination cannot signal the cut's trims",
			"outContainer", out, "delay", cutTrack.Delay, "padding", cutTrack.Padding)
		return nil, nil
	}
	// The segmented remux's terms plus CutVersion, for the trims and codec
	// config the cut synthesized.
	rsp.Versions = withDemuxer(track, RemuxVersion, mp4.SegmenterVersion, CutVersion)
	return &CutSegmentPlan{
		RemuxSegmentPlan: *rsp,
		Landed:           landed,
		Grid:             g,
		Source:           track,
	}, nil
}

// cutInitExpressible reports whether the init header can express probe, a
// track as a cut presents it (cutProbe): the sample entry reads nothing else
// a cut changes.
func cutInitExpressible(probe container.Track) bool {
	_, err := mp4.InitSegment(probe)
	return err == nil
}

// CutInitSegment builds the CMAF init header for a planned segmented cut:
// RemuxInitSegment over the embedded plan. The plan's track and Delay are the
// cut's (PlanRemuxSegments takes Delay from the track it is handed), so the
// sample entry carries the rewritten codec config and the edit list skips the
// cut's priming.
func (e *Engine) CutInitSegment(plan *CutSegmentPlan) ([]byte, error) {
	return e.RemuxInitSegment(&plan.RemuxSegmentPlan)
}

// CutSegments emits numbered CMAF media segments from a span of src's own
// packets, the segmented sibling of CutStream: each holds the kept access
// units byte for byte, and segment n of a run started mid-stream is
// byte-identical to a continuous run's (ADR-0013). The caller owns src.
//
// opts and spans must be a pair PlanCutSegments accepted, grid its Grid and
// measured its Source, read as CutStream reads measured. A decline by CutTrack
// or by the copy rule (PlanRemux at the segmented depth, without its muxer
// check) is an error, never a re-encode; the copy rule's is CodeInvalidRequest
// before a segment goes out. The trim rule (cutTrimsExpressible) is the plan's
// alone. A demuxer that cannot seek is refused with CodeUnsupportedFormat,
// even from segment 0, and a source that runs out early fails the run.
func (e *Engine) CutSegments(ctx context.Context, src container.Source, hint string, opts TranscodeOptions,
	spans []Span, grid int, measured container.Track, segOpts SegmentedOptions, emit func(mp4.Segment) error) (*SegmentedResult, error) {
	if err := checkSegmented(opts); err != nil {
		return nil, err
	}
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	if row.hls == nil {
		return nil, noSegmentedForm(opts.Format)
	}
	if err := validateSegOpts(segOpts); err != nil {
		return nil, err
	}
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, err
	}
	// The plan's measured track over the header's, as in CutStream: plan and
	// run must read one length and one set of trims or their windows drift.
	track, err := cutSource(ctx, demux, info.Default(), measured)
	if err != nil {
		return nil, err
	}
	cutTrack, _, err := CutTrack(track, opts, spans, grid)
	if err != nil {
		return nil, err
	}
	// The plan's copy rule at its depth, as in RemuxSegments. The segmenter
	// takes planRemux's ID-0 track; the walk filters on the source's track
	// ID, which CutTrack keeps on cutTrack.
	rp, _, err := e.planRemux(cutTrack, segmentBitDepth(opts, track.Fmt), true)
	if err != nil {
		return nil, err
	}
	// What planRemux declines, a trim on a kept packet among it, is refused
	// before a segment goes out, not by the walk's backstop mid-run.
	if rp == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: a %s cut cannot be remuxed to %s with these options; transcode it",
				track.Codec, opts.Format))
	}
	cutDemux, err := cutSeekable(demux, track, opts, spans, grid)
	if err != nil {
		return nil, err
	}
	e.log.Debug("segmented cut started", "container", info.Container, "codec", track.Codec,
		"out", opts.Format, "segment", segOpts.StartSegment, "segSamples", segOpts.SegmentSamples)
	res, err := e.segmentWalk(ctx, cutDemux, rp.Track, cutTrack.ID, segOpts, grid, emit)
	if err != nil {
		return nil, err
	}
	e.log.Debug("segmented cut finished", "samples", res.Samples, "segments", res.Segments)
	return res, nil
}

// cutSeekDemuxer is Cut's view made seekable on the cut's own output
// timeline, for CutSegments alone; Cut is no Seeker (see Cut). Seeking the
// cut seeks the source, through seek, the inner demuxer.
type cutSeekDemuxer struct {
	*cutDemuxer
	seek container.Seeker
}

// cutSeekable is Cut for CutSegments: a view that seeks on its output
// timeline. It refuses with CodeUnsupportedFormat a demuxer that cannot seek,
// a cut whose kept packets trim, and a source with a trim that empties a
// packet.
func cutSeekable(demux container.Demuxer, track container.Track, opts TranscodeOptions, spans []Span, grid int) (container.Demuxer, error) {
	sk, ok := demux.(container.Seeker)
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"waxflow: this container cannot seek, so a mid-stream segmented cut cannot start")
	}
	res, err := computeCut(track, opts, spans, grid)
	if err != nil {
		return nil, err
	}
	// PlanCutSegments declines both first; this is the view's own guard
	// (ADR-0013).
	if res.track.MidPadding > 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"waxflow: a kept packet of this cut trims inside the run, which a segmented run cannot carry; transcode it")
	}
	if wholePacketTrim(res.trims, int64(grid)) {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"waxflow: this source trims a whole packet inside its run, so a seek into it is ambiguous; transcode it")
	}
	return &cutSeekDemuxer{
		cutDemuxer: &cutDemuxer{Demuxer: demux, cut: res.track, track: track.ID, windows: res.windows, lastEnd: res.lastEnd, blind: res.blind, trims: res.trims, toEnd: res.toEnd, damage: container.DamageOf(demux)},
		seek:       sk,
	}, nil
}

// The source demuxer seeks on the timeline it delivers, which excludes every
// trim before the landing (container.Packet.Padding), while the windows are
// raw. Target and landing are packet starts, so a landing at or before the
// target is so on both timelines. The delay is 0: these are packet
// positions, not a span's.

func (c *cutSeekDemuxer) deliveredPos(raw int64) int64 { return trackPos(c.trims, 0, raw) }

func (c *cutSeekDemuxer) rawPos(delivered int64) int64 { return rawStart(c.trims, 0, delivered) }

// SeekSample repositions the cut at outTarget on its contiguous output
// timeline: it maps outTarget through the windows to a source position, seeks
// the inner demuxer there, and resets the walk's cursor and positions, so the
// packets from outTarget on are retimed as in a continuous run. It backs off
// no pre-roll (ADR-0013). It returns the output position it landed on, at or
// before outTarget, or outTarget itself past every window, where the view
// ends. A track other than the view's, or a negative target, is refused with
// CodeInvalidRequest.
func (c *cutSeekDemuxer) SeekSample(ctx context.Context, track int, outTarget int64) (int64, error) {
	// The view's one track is the source's own ID, the only one the walk seeks.
	if track != c.track {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: cut view has no track %d", track))
	}
	if outTarget < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: negative cut seek target %d", outTarget))
	}
	// The windows run back to back on the output timeline, so inside one the
	// source and output positions move in lockstep.
	var outStart int64
	i := 0
	for ; i < len(c.windows); i++ {
		w := c.windows[i]
		if w.to < 0 {
			break // the last window runs to EOF and holds every later target
		}
		wlen := w.to - w.from
		if outTarget < outStart+wlen {
			break
		}
		outStart += wlen
	}
	if i == len(c.windows) {
		// Past every window: park the cursor at the end, so the next
		// ReadPacket ends the view.
		c.cur, c.pos, c.out, c.prevPad = len(c.windows), 0, outTarget, 0
		c.outRaw = c.out
		return outTarget, nil
	}
	w := c.windows[i]
	landed, err := c.seek.SeekSample(ctx, c.track, c.deliveredPos(w.from+(outTarget-outStart)))
	if err != nil {
		return 0, err
	}
	landed = c.rawPos(landed)
	// A seek may land well before the target, even before the window (Ogg
	// seeks by page). Such a landing seeds out at the window's output start,
	// and one inside the window seeds the matching position; the view skips
	// the packets before the window and the walk skips up to the boundary, as
	// a run from the top does. Either way out is at or before outTarget, as
	// seekPackets requires.
	c.cur, c.pos = i, landed
	c.out = outStart + max(0, landed-w.from)
	// Raw and delivered output positions agree: cutSeekable refused a cut
	// whose kept packets trim.
	c.outRaw = c.out
	// The packet before the cursor is unknown after a seek, so no trim check
	// carries across it.
	c.havePrev, c.prevKept, c.tailPad = false, false, 0
	return c.out, nil
}
