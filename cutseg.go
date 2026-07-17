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
// describes its progressive form. It embeds RemuxSegmentPlan for the reason
// CutPlan embeds RemuxPlan: the delivery layer reads the same segment facts off a
// plan whichever rung answered, and a cut is a segmented remux with a packet
// filter in front.
type CutSegmentPlan struct {
	RemuxSegmentPlan
	// Landed is where the requested spans fell, one for one, on the source's own
	// track timeline, exactly as CutPlan.Landed reports for the progressive cut.
	Landed []Span
	// Grid and SourceSamples are the source's packet grid and its exact length,
	// threaded to CutSegments so the run cuts on the same boundaries and
	// synthesizes the same track the plan (and so the cache key) was computed
	// from. SourceSamples is the source's own length even when the run reopens a
	// container that declares none (ADTS AAC-LC reports -1 from its headers), so
	// the run does not compute a different cut than the plan promised. It is -1
	// only when the plan itself was handed a lengthless track.
	Grid          int
	SourceSamples int64
}

// PlanCutSegments plans the segmented form of a cut: the HLS spelling of the cut
// rung, and the mirror of PlanRemuxSegments over a synthesized cut track. It runs
// CutTrack for the cut's own track, plans that track's segmented remux, and then
// applies the destination-trim gate PlanCut applies, so a segmented cut declines
// in exactly the cases the progressive one does.
//
// grid is the source's packet duration from PacketGrid, as PlanRemuxSegments
// takes one. A varying grid (0) declines, as does anything PlanRemuxSegments
// declines, and all of these return (nil, nil): the caller falls through to a
// transcode. An error means the request is wrong for every rung, exactly the
// error/decline seam PlanCut documents.
func (e *Engine) PlanCutSegments(track container.Track, opts TranscodeOptions, spans []Span,
	grid int, segSeconds float64) (*CutSegmentPlan, error) {
	cutTrack, landed, err := CutTrack(track, spans, grid)
	if err != nil {
		// The seam, exactly as PlanCut maps it: CutTrack cannot express a decline
		// through its signature, so a CodeUnsupportedFormat becomes the ladder's
		// (nil, nil) and a CodeInvalidRequest propagates as an error rung 3 would
		// hit identically.
		if waxerr.CodeOf(err) == waxerr.CodeUnsupportedFormat {
			e.log.Debug("segmented cut declined", "codec", track.Codec, "grid", grid, "reason", err)
			return nil, nil
		}
		return nil, err
	}
	rsp, err := e.PlanRemuxSegments(cutTrack, opts, segSeconds, grid)
	if err != nil || rsp == nil {
		return nil, err
	}
	// The cut's trims are new, and PlanRemuxSegments only ever checked the
	// source's. This is the one question nothing else in the segmented ladder
	// asks; see cutTrimsExpressible.
	if !cutTrimsExpressible(rsp.Container, cutTrack.Delay, cutTrack.Padding) {
		e.log.Debug("segmented cut declined", "reason", "the destination cannot signal the cut's trims",
			"outContainer", rsp.Container, "delay", cutTrack.Delay, "padding", cutTrack.Padding)
		return nil, nil
	}
	// RemuxVersion and the segmenter revision the remux plan already named, plus
	// CutVersion for the trims and rewritten config the cut synthesized: the same
	// three the progressive cut keys on, plus the segmenter's own.
	rsp.Versions = []string{RemuxVersion, mp4.SegmenterVersion, CutVersion}
	return &CutSegmentPlan{
		RemuxSegmentPlan: *rsp,
		Landed:           landed,
		Grid:             grid,
		SourceSamples:    track.Samples,
	}, nil
}

// CutInitSegment builds the CMAF init header for a planned segmented cut. It is
// RemuxInitSegment over the embedded plan with no new box code: the cut track
// already carries the rewritten OpusHead and the synthesized delay, so its sample
// entry and edit list are correct, and the edit list instructs the player to skip
// the head pre-skip priming exactly as a remux's would. That the plan's Delay is
// the cut's and not the source's zero is what PlanRemuxSegments guarantees, since
// it sets SegmentPlan.Delay from the track it is handed, and it is handed the cut
// track.
func (e *Engine) CutInitSegment(plan *CutSegmentPlan) ([]byte, error) {
	return e.RemuxInitSegment(&plan.RemuxSegmentPlan)
}

// CutSegments emits numbered CMAF media segments from a span of src's own
// packets: the run half of the segmented cut rung, and the segmented sibling of
// CutStream. No decode, no DSP, no encode, so no generation loss; each segment
// holds the kept access units byte for byte.
//
// opts, spans, grid, and samples must be the pair PlanCutSegments accepted, for
// the reason CutStream gives: a caller reaching this directly has chosen the rung,
// and the fallback it did not ask for would be the wrong help. samples is the
// source's exact length, threaded from the plan and patched over the reopened
// header's, so an undeclared-length source (ADTS AAC-LC) cuts against the length
// the plan measured rather than the -1 its headers report. Pass a negative
// samples to take the header's own length, which a source that declares one
// already has.
//
// It opens the source, wraps it in a seekable Cut view, and hands that to the
// same segmentWalk RemuxSegments uses, so a mid-stream restart reproduces a
// continuous run's segment bytes for bytes. There is no demux.Close, mirroring
// RemuxSegments: the source.File owns the handle.
func (e *Engine) CutSegments(ctx context.Context, src container.Source, hint string, opts TranscodeOptions,
	spans []Span, grid int, samples int64, segOpts SegmentedOptions, emit func(mp4.Segment) error) (*SegmentedResult, error) {
	if err := validateSegOpts(segOpts); err != nil {
		return nil, err
	}
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, err
	}
	track := info.Default()
	if samples >= 0 {
		// The plan's measured length over the header's, the mirror of CutStream: the
		// cut arithmetic reads it (see computeCut's decodedEnd), and plan and run
		// must read the same one or their windows drift.
		track.Samples, track.SamplesExact = samples, true
	}
	cutTrack, _, err := CutTrack(track, spans, grid)
	if err != nil {
		return nil, err
	}
	// The segmenter's track is PlanRemux's ID-0 normalization of the cut track,
	// exactly as RemuxSegments segments rp.Track; the walk filters on the source's
	// own track ID, which CutTrack preserves onto cutTrack.
	rp, err := e.PlanRemux(cutTrack, opts)
	if err != nil {
		return nil, err
	}
	if rp == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: a %s cut cannot be remuxed to %s with these options; transcode it",
				track.Codec, opts.Format))
	}
	cutDemux, err := cutSeekable(demux, track, spans, grid)
	if err != nil {
		return nil, err
	}
	e.log.Debug("segmented cut started", "container", info.Container, "codec", track.Codec,
		"out", opts.Format, "segment", segOpts.StartSegment, "segSamples", segOpts.SegmentSamples)
	res, err := e.segmentWalk(ctx, cutDemux, rp.Track, cutTrack.ID, segOpts, emit)
	if err != nil {
		return nil, err
	}
	e.log.Debug("segmented cut finished", "samples", res.Samples, "segments", res.Segments)
	return res, nil
}

// cutSeekDemuxer is Cut's seekable view, used only by the segmented cut path. It
// is a cutDemuxer that can reposition, which the progressive Cut deliberately is
// not (see Cut, which warns that a seeker mixing the source's timeline with the
// cut's would be wrong in a way no error surfaces). This is the one caller that
// genuinely needs one, and it needs the inner demuxer to seek too.
type cutSeekDemuxer struct {
	*cutDemuxer
	// seek is the inner demuxer, which must itself be a Seeker; the source's
	// packets are what a cut moves, so seeking the cut means seeking the source.
	seek container.Seeker
}

// cutSeekable wraps a seekable demuxer in a Cut view that can reposition on its
// output timeline. It is the seekable form of Cut, for CutSegments alone.
func cutSeekable(demux container.Demuxer, track container.Track, spans []Span, grid int) (container.Demuxer, error) {
	sk, ok := demux.(container.Seeker)
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"waxflow: this container cannot seek, so a mid-stream segmented cut cannot start")
	}
	res, err := computeCut(track, spans, grid)
	if err != nil {
		return nil, err
	}
	return &cutSeekDemuxer{
		cutDemuxer: &cutDemuxer{Demuxer: demux, cut: res.track, track: track.ID, windows: res.windows},
		seek:       sk,
	}, nil
}

// SeekSample repositions the cut to outTarget on its own contiguous output
// timeline, so a restarted segment worker resumes exactly where a continuous run
// would be. It maps outTarget back through the windows to a source decode
// position, seeks the inner demuxer there, and resets the walk's cursor, source
// position, and output position so the retimed packets a restarted worker
// produces are the same bytes a continuous run's are.
//
// No pre-roll is backed off, and that is the trap this rung must not fall into. A
// mid-stream segment begins exactly on its grid boundary and RemuxSegments
// discards packets before it, precisely so segment n from a restarted worker is
// byte-identical to a continuous run's. Seeking back by the codec's pre-roll to
// "prime" the segment would duplicate packets and break that; player-side priming
// for a mid-stream seek comes from the codec roll signaling in the init segment,
// not from packets in the media segment. The head pre-roll (segment 0) is a
// different thing and is already in window 0, which CutTrack widened, so it never
// reaches this seek.
func (c *cutSeekDemuxer) SeekSample(track int, outTarget int64) (int64, error) {
	// The cut view exposes one track (the source's own, which CutTrack preserves),
	// so a seek names it exactly as every other demuxer's SeekSample names track 0;
	// c.track then forwards to the inner demuxer, whose ID it is. The walk only ever
	// calls this with that ID, so the guard documents the invariant rather than
	// gating a reachable path.
	if track != c.track {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: cut view has no track %d", track))
	}
	if outTarget < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: negative cut seek target %d", outTarget))
	}
	// Find the window outTarget falls in and that window's start on the output
	// timeline. The output timeline runs the kept windows back to back with no
	// gaps, so within a window the source and output positions advance in lockstep
	// and the map is a single addition. For the single-window HTTP case this loop
	// runs once and outStart is 0.
	var outStart int64
	i := 0
	for ; i < len(c.windows); i++ {
		w := c.windows[i]
		if w.to < 0 {
			break // the last window runs to EOF; every remaining target lands in it
		}
		wlen := w.to - w.from
		if outTarget < outStart+wlen {
			break
		}
		outStart += wlen
	}
	if i == len(c.windows) {
		// Past every window: nothing remains to read. Park the cursor at the end so
		// the next ReadPacket returns io.EOF, and report the target back unchanged.
		c.cur, c.pos, c.out = len(c.windows), 0, outTarget
		return outTarget, nil
	}
	w := c.windows[i]
	landed, err := c.seek.SeekSample(c.track, w.from+(outTarget-outStart))
	if err != nil {
		return 0, err
	}
	// The inner demuxer's next packet begins at landed on the source decode
	// timeline; cur names the window and pos the source position the walk resumes
	// at. A seek is a coarse optimization, not an exact landing: a container that
	// seeks by page (Ogg) routinely lands well before the target, even before the
	// window's own start. The output offset is clamped at zero for exactly that
	// case, so a pre-window landing seeds out at the window's output start and the
	// walk then skips the pre-roll gap and everything up to the boundary itself,
	// exactly as a from-the-top run does. Landing inside the window instead seeds
	// the matching output position directly. Either way out is at or before
	// outTarget, which is the "at or before" contract seekPackets checks, and the
	// walk's own pos < p0 skip carries it the rest of the way to the boundary.
	c.cur, c.pos = i, landed
	c.out = outStart + max(0, landed-w.from)
	return c.out, nil
}
