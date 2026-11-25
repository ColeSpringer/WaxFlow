package waxflow

import (
	"context"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/muxseek"
	"github.com/colespringer/waxflow/waxerr"
)

// RemuxVersion is the remux rung's term in the ADR-0004 cache key, beside its
// container's MuxerVersion. Bump it for a change to what the rung synthesizes
// from the input track, such as the gapless trailer, rather than to framing.
const RemuxVersion = "remux-6"

// RemuxPlan describes what a remux would produce, computed from the source
// track's headers alone. It embeds TranscodePlan so a consumer reads one shape
// whichever rung answered. The plan is built from the source track: Format is
// the source's, Versions names RemuxVersion and no codec revision, and BitRate
// and EstimatedBytes stay unknown, since no encoder made the packets.
type RemuxPlan struct {
	TranscodePlan
	// Track is the source's track as track 0, its codec config and trims
	// unchanged: what the muxer is opened with and the trailer built from.
	Track container.Track
}

// remuxCodecs resolves the CODECS attribute a segmented remux advertises. On
// an AAC-family row the track's own AudioSpecificConfig decides (PS names
// mp4a.40.29, SBR mp4a.40.5), since RFC 6381 wants the stream's signaling and
// the copied esds is unchanged: an AOT-29 track rides the plain aac row, but
// its esds still says AOT 29. Anything else takes the row's string.
func remuxCodecs(row *output, track container.Track) string {
	if row.codecID == codec.AACLC || row.codecID == codec.HEAAC {
		if cfg, err := aac.ParseASC(track.CodecConfig); err == nil {
			switch {
			case cfg.PS:
				return codecsHEv2
			case cfg.SBR:
				return codecsHEv1
			}
		}
	}
	return row.hls.codecs
}

// remuxRow resolves the output row a remux of track under the named format
// runs as: outputRow, except that format=aac on an HE-AAC source resolves to
// the he-aac row, so a copy keeps an HE source's identity rather than
// declining into an LC re-encode. The two rows share their containers. A
// request with shaping options still falls through to a transcode, which
// encodes LC.
func remuxRow(track container.Track, format string) (*output, error) {
	row, err := outputRow(format)
	if err != nil {
		return nil, err
	}
	if row.codecID == codec.AACLC && track.Codec == codec.HEAAC {
		return outputRow("he-aac")
	}
	return row, nil
}

// PlanRemux reports whether opts can be served by rewriting track's container
// around its existing packets, and how: the ladder's middle rung (ADR-0013).
// The output row's codec must match the track's, no option may transform
// samples, the destination muxer must accept the track (its TrackFits), and
// the track's trims must be ones that muxer can write, except that a front
// or end trim to a container with no gapless signalling (ADTS) is dropped,
// as a transcode drops its own.
//
// A request the rung cannot serve returns (nil, nil), and the caller falls
// through to a transcode. An error means the request is wrong for every rung
// (an invalid option, an unsupported format, a container the format cannot
// produce).
func (e *Engine) PlanRemux(track container.Track, opts TranscodeOptions) (*RemuxPlan, error) {
	plan, _, err := e.planRemux(track, opts, false)
	return plan, err
}

// planRemux is PlanRemux for a progressive or a segmented copy. A segmented
// copy writes fMP4 whatever the form, so it asks the init header instead of
// the form's muxer and takes fMP4's trim rules. refused is that muxer's
// refusal when it alone declined the copy.
func (e *Engine) planRemux(track container.Track, opts TranscodeOptions, segmented bool) (plan *RemuxPlan, refused, err error) {
	if err := opts.Check(); err != nil {
		return nil, nil, err
	}
	row, fm, copies, err := remuxForm(track, opts)
	if err != nil {
		return nil, nil, err
	}
	containerName, out := row.containerName(fm), copyDest(row, fm, segmented)
	if !copies || !gaplessSurvives(track) {
		return nil, nil, nil
	}
	if track.Codec == codec.Vorbis && track.Delay > 0 && !isMatroska(out) {
		// Ogg states a Vorbis front trim only as a negative start, which its
		// muxer does not write (granules run from 0), so a copy would play it.
		return nil, nil, nil
	}
	if track.MidPadding > 0 && !StatesTrimsPerPacket(out) {
		// An Ogg granule or an MP4 edit list names one end trim, so a copy
		// would play an inner trim's frames. MidPadding is a walk's finding;
		// copyPackets refuses mid-copy for a source nothing measured.
		return nil, nil, nil
	}
	// Muxers are single-track, so the muxed track is track 0, as in Transcode.
	t := track
	t.ID, t.Default = 0, true
	if !segmented && fm.trackFits != nil {
		if err := fm.trackFits(t); err != nil {
			e.log.Debug("remux declined", "reason", err, "outContainer", containerName)
			return nil, err, nil
		}
	}
	return &RemuxPlan{
		TranscodePlan: TranscodePlan{
			Format:    track.Fmt,
			Container: containerName,
			MediaType: fm.mediaType,
			Live:      fm.live,
			Versions:  withDemuxer(track, RemuxVersion, fm.version),
			Samples:   track.Samples,
			// BytesPerFrame, FrameSize, BitRate and EstimatedBytes stay
			// unknown: the packets' rate is not in the headers. A caller that
			// needs a rate promised (a maxBitRate cap) declines this rung.
			EstimatedBytes: -1,
		},
		Track: t,
	}, nil, nil
}

// copyDest is the container a copy's trim rules are asked of: fMP4 for a
// segmented copy, which the segmenter writes whatever the form, and the
// form's own otherwise.
func copyDest(row *output, fm *form, segmented bool) string {
	if segmented {
		return ContainerFragmented
	}
	return row.containerName(fm)
}

// remuxForm resolves the row and form a copy of track under opts writes, and
// reports whether the codec crosses unchanged with no transform asked: what
// the format and options answer before the track's trims or packets are read.
func remuxForm(track container.Track, opts TranscodeOptions) (*output, *form, bool, error) {
	row, err := remuxRow(track, opts.Format)
	if err != nil {
		return nil, nil, false, err
	}
	fm, err := row.form(opts.Container)
	if err != nil {
		return nil, nil, false, err
	}
	return row, fm, codecSurvives(track.Codec, row.codecID) && remuxable(opts, track.Fmt), nil
}

// codecSurvives reports whether a track's codec crosses into the output row's
// unchanged, the premise of the copy rungs. A matching codec.ID suffices for
// every codec but PCM, whose wire layout (byte order, 8-bit signedness) is the
// container's and lives in CodecConfig, so PCM declines and the transcode
// repacks it bit-exact (ADR-0013).
func codecSurvives(src, out codec.ID) bool {
	return src == out && src != codec.PCM
}

// remuxable reports whether opts asks for nothing but a container rewrite. It
// compares the whole value against the request stripped to its format,
// container, SpliceTrims, ResampleProfile and Shaping, so an option added
// later counts as a transform with no edit here, and so does a seek
// (FromSample), since a copy cannot cut mid-packet. The profile and shaping
// stay because they say how a transform runs, and the server sets a profile
// on every request. A rate, channel count or integer depth equal to the
// source's is resolved away first, as directPlayable does; a depth on a float
// source quantizes and stays (ADR-0013).
func remuxable(opts TranscodeOptions, src audio.Format) bool {
	norm := opts.normalized()
	if norm.Rate == src.Rate {
		norm.Rate = 0
	}
	if norm.Channels == src.Channels {
		norm.Channels = 0
	}
	if src.Type == audio.Int && norm.BitDepth == src.BitDepth {
		norm.BitDepth = 0
	}
	base := TranscodeOptions{
		Format:          opts.Format,
		Container:       opts.Container,
		SpliceTrims:     opts.SpliceTrims,
		ResampleProfile: opts.ResampleProfile,
		Shaping:         opts.Shaping,
	}
	return norm == base
}

// remuxTrailer synthesizes the trailer a remuxed track's muxer writes, so each
// muxer states the source's trims in its own signalling as for an encode. The
// length is what the packets hold (container.SettleLength over run): against a
// declared length that is exact or beside a trim the end padding is derived
// from the run, and where the track states no length or an estimate its
// padding stands and the run gives the length, so a declared count a truncated
// source cannot fill shrinks to the packets. A zero Padding first takes the
// packets' own end trim (copiedRun.endPadding); a stated one is not swapped
// for theirs, since a cut's track carries trims it computed. An unprimed codec
// (FLAC, ALAC) takes the run's count with zero trims (ADR-0013).
func remuxTrailer(t container.Track, run copiedRun) codec.Trailer {
	if run.trimmed && t.Padding == 0 && gaplessSurvives(container.Track{
		Codec: t.Codec, Padding: run.endPadding,
	}) {
		t.Padding = run.endPadding
	}
	s := container.SettleLength(t, run.samples)
	if !gaplessSurvives(s) {
		// The unprimed rule against the settle: its capped arm derives a
		// padding when the run outlives an authoritative length, which a FLAC
		// or ALAC muxer would refuse at End. The packets' count stands.
		return codec.Trailer{Samples: run.samples, Delay: 0, Padding: 0}
	}
	return codec.Trailer{Samples: s.Samples, Delay: s.Delay, Padding: s.Padding}
}

// gaplessSurvives reports whether t's trims are ones its muxer can write. FLAC
// and ALAC have no priming, and every muxer that writes them refuses a
// nonzero trim, so a copy declines rather than failing inside Begin. It does
// not decline a container with no gapless signalling: a remux to ADTS drops
// the source's delay as a transcode to ADTS drops its encoder's (ADR-0013).
func gaplessSurvives(t container.Track) bool {
	if t.Delay == 0 && t.Padding == 0 {
		return true
	}
	return t.Codec != codec.FLAC && t.Codec != codec.ALAC
}

// PacketGrid reports the decode duration every packet of src's default track
// but the last shares: the grid a segmented remux lays its segment boundaries
// on. It walks the demuxer and decodes nothing. It returns 0 when those
// durations vary or one is not positive, and for a stream of one packet or
// none: a fact about the source, and the caller's cue to take a rung with its
// own grid. The last packet may be short, an encoder's tail flush (ADR-0013).
// ctx is read before every packet; one that has ended stops the walk with
// canceled.
func (e *Engine) PacketGrid(ctx context.Context, src container.Source, hint string) (int, error) {
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return 0, err
	}
	return packetGrid(ctx, demux, info.Default().ID)
}

func packetGrid(ctx context.Context, demux container.Demuxer, track int) (int, error) {
	grid, prev := int64(0), int64(0)
	// have marks a predecessor, so a zero-duration packet is compared and
	// declines: mp4.Segmenter refuses one, so a plan must not promise it.
	have, uniform := false, true
	var pkt container.Packet
	for {
		if err := container.WalkCanceled(ctx, "waxflow: "); err != nil {
			return 0, err
		}
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if pkt.Track != track {
			continue
		}
		// One packet behind, so the last packet is never compared.
		if have {
			switch {
			case prev <= 0:
				uniform = false
			case grid == 0:
				grid = prev
			case grid != prev:
				uniform = false
			}
		}
		prev, have = pkt.Dur, true
	}
	if !uniform || grid <= 0 {
		// A stream of one packet or none lands here too: it fills at most one
		// segment and needs no grid, so declining it costs nothing.
		return 0, nil
	}
	return int(grid), nil
}

// RemuxSegmentPlan describes the segmented (CMAF) form of a remux, as
// SegmentPlan describes a transcode's, and embeds it so the delivery layer
// reads one shape whichever rung answered.
type RemuxSegmentPlan struct {
	SegmentPlan
	// Track is the source's track as track 0, which RemuxInitSegment builds
	// the init segment from.
	Track container.Track
}

// PlanRemuxSegments plans the segmented form of a remux.
//
// grid measures the source's packet duration, as PacketGrid does. It is asked
// at most once, and only after every check the track's headers answer has
// passed, so a request its format or options rule out never pays for the
// walk. Its error comes back unchanged. A format with no segmented form, an
// invalid option, or a NaN, negative or over-long segSeconds is refused before
// any decline. Past that, it returns (nil, nil) for a copy the rung declines
// (with the segmented depth rule applied, and the init segment asked in place
// of the form's muxer) and for a zero grid. The segment length snaps to a
// whole number of packets, as PlanSegments snaps a transcode's to encoder
// frames, so alignment declines only a source with no uniform grid.
func (e *Engine) PlanRemuxSegments(track container.Track, opts TranscodeOptions, segSeconds float64, grid func() (int, error)) (*RemuxSegmentPlan, error) {
	if err := checkSegmented(opts); err != nil {
		return nil, err
	}
	row, err := remuxRow(track, opts.Format)
	if err != nil {
		return nil, err
	}
	if row.hls == nil {
		// No rung serves a format with no segmented form: an error.
		return nil, noSegmentedForm(opts.Format)
	}
	// The segmented depth rule (segmentBitDepth), applied before the rung is
	// asked so it declines: a copy cannot widen a 20-bit FLAC for MSE.
	rp, _, err := e.planRemux(track, segmentBitDepth(opts, track.Fmt), true)
	if err != nil {
		return nil, err
	}
	// After the other refusals, as in PlanSegments, and before any decline.
	if _, err := segmentSeconds(segSeconds); err != nil {
		return nil, err
	}
	if rp == nil {
		return nil, nil
	}
	// A track the init segment cannot express declines here rather than
	// failing once the playlist is out.
	if _, err := mp4.InitSegment(rp.Track); err != nil {
		return nil, nil
	}
	g, err := grid()
	if err != nil {
		return nil, err
	}
	if g <= 0 {
		return nil, nil
	}
	segSamples, err := snapSegmentSamples(segSeconds, track.Fmt.Rate, g)
	if err != nil {
		return nil, err
	}
	sp := SegmentPlan{
		TranscodePlan:  rp.TranscodePlan,
		SegmentSamples: segSamples,
		// The track's own delay (the source's pre-skip for a remux, the
		// cut's synthesized one for a cut), which the init segment's edit
		// list describes; the row's encoder constant need not match it.
		Delay:              track.Delay,
		Codecs:             remuxCodecs(row, track),
		TotalDecodeSamples: -1,
		Segments:           -1,
	}
	sp.Versions = withDemuxer(track, RemuxVersion, mp4.SegmenterVersion)
	sp.FrameSize = g
	if track.Samples >= 0 {
		sp.TotalDecodeSamples = totalDecodeSamples(track.Samples, track.Delay, int64(g))
		sp.Segments = (sp.TotalDecodeSamples + int64(segSamples) - 1) / int64(segSamples)
	}
	// The peak-rate bound is the PCM wire rate, as for VBR lossless, since the
	// packets' rate is not in the headers. It is loose for a lossy source, but
	// a remux is one variant (a per-variant Bitrate declines), so it picks
	// nothing.
	base := sp.Format.Rate * sp.Format.Channels * max(sp.Format.BitDepth, 16)
	sp.Bandwidth = base + sp.Format.Rate/g*64 + 2000
	return &RemuxSegmentPlan{SegmentPlan: sp, Track: rp.Track}, nil
}

// RemuxInitSegment builds the CMAF init header for a planned segmented remux:
// the source's own sample entry, from the codec config it already carries, so
// the packets the segments hold and the header that describes them come from
// one place. Deterministic, like InitSegment.
func (e *Engine) RemuxInitSegment(plan *RemuxSegmentPlan) ([]byte, error) {
	t := plan.Track
	t.Samples, t.Delay = plan.Samples, plan.Delay
	if t.Codec == codec.FLAC {
		// No segment can vouch for the whole stream's MD5 or patch it here.
		var err error
		if t.CodecConfig, err = flac.ClearMD5(t.CodecConfig); err != nil {
			return nil, err
		}
	}
	return mp4.InitSegment(t)
}

// RemuxSegments emits numbered CMAF media segments from src's own packets: the
// segmented form of the remux rung, for an HLS variant that needs no encoder.
// A run starting mid-stream needs no priming, so a restarted worker's segment n
// is byte-identical to a continuous run's (ADR-0013). A copy planRemux
// declines, an inner trim among them, is refused with CodeInvalidRequest
// before a segment goes out; a packet off the grid, or a mid-stream start on
// a container that cannot seek, ends the run with CodeUnsupportedFormat.
func (e *Engine) RemuxSegments(ctx context.Context, src container.Source, hint string, opts TranscodeOptions,
	segOpts SegmentedOptions, emit func(mp4.Segment) error) (*SegmentedResult, error) {
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
	track := info.Default()
	// PlanRemuxSegments' depth rule, so this refuses the copy the plan
	// declines (a 20-bit FLAC a browser cannot append).
	rp, _, err := e.planRemux(track, segmentBitDepth(opts, track.Fmt), true)
	if err != nil {
		return nil, err
	}
	if rp == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: a %s source cannot be remuxed to %s with these options; transcode it",
				track.Codec, opts.Format))
	}
	// A restart numbers FLAC frames by the stream's block size, its first
	// packet's; CutSegments has it as the grid.
	block := 0
	if track.Codec == codec.FLAC && segOpts.StartSegment > 0 {
		if block, err = firstDur(demux, track.ID); err != nil {
			return nil, err
		}
	}
	e.log.Debug("segmented remux started", "container", info.Container, "codec", track.Codec,
		"out", opts.Format, "segment", segOpts.StartSegment, "segSamples", segOpts.SegmentSamples)
	res, err := e.segmentWalk(ctx, demux, rp.Track, track.ID, segOpts, block, emit)
	if err != nil {
		return nil, err
	}
	e.log.Debug("segmented remux finished", "samples", res.Samples, "segments", res.Segments)
	return res, nil
}

// validateSegOpts rejects segment options no run can honor: a non-positive
// segment length, a negative start, or a start whose product with the length
// overflows the sample timeline. Both segmented entry points call it before
// opening a source, so an invalid request fails on its own inputs rather than
// paying for a container open and a plan first.
func validateSegOpts(segOpts SegmentedOptions) error {
	switch {
	case segOpts.SegmentSamples <= 0:
		return waxerr.New(waxerr.CodeInvalidRequest, "waxflow: segmenter needs a positive SegmentSamples")
	case segOpts.StartSegment < 0:
		return waxerr.New(waxerr.CodeInvalidRequest, "waxflow: negative StartSegment")
	case segOpts.StartSegment > (1<<62)/int64(segOpts.SegmentSamples):
		return waxerr.New(waxerr.CodeInvalidRequest, "waxflow: StartSegment overflows the sample timeline")
	}
	return nil
}

// segmentWalk is the demuxer walk RemuxSegments and CutSegments share: the
// segmenter, the seek to the restart point, the one-behind buffering that lets
// a short final packet through and rejects a short interior one, and the grid
// recheck. segTrack builds the segmenter (a plan's ID-0 track); walkTrackID is
// the source track the walk filters on. CutSegments hands it a seekable Cut
// view whose retimed packets are the source's bytes, so a cut segment is built
// as a remux segment is. block is a FLAC stream's block size, which the
// segmenter numbers a run from past 0 by.
func (e *Engine) segmentWalk(ctx context.Context, demux container.Demuxer, segTrack container.Track,
	walkTrackID int, segOpts SegmentedOptions, block int, emit func(mp4.Segment) error) (*SegmentedResult, error) {
	// No confirm step: the length an init segment declares comes from the
	// plan's track (RemuxInitSegment), and segTrack is only a sample entry to
	// validate. The caller validates segOpts (validateSegOpts); NewSegmenter
	// re-guards the length and start it can express.
	seg, err := mp4.NewSegmenter(segTrack, &mp4.SegmenterOptions{
		SegmentSamples: segOpts.SegmentSamples, StartSegment: segOpts.StartSegment, Block: block,
	})
	if err != nil {
		return nil, err
	}
	p0 := segOpts.StartSegment * int64(segOpts.SegmentSamples)
	pos, err := seekPackets(ctx, demux, walkTrackID, p0)
	if err != nil {
		return nil, err
	}

	res := &SegmentedResult{}
	emitSeg := func(s mp4.Segment) error {
		res.Segments++
		return emit(s)
	}

	// The grid is re-checked as the walk runs: a plan made against another
	// source (a stale memo, a replaced file) would otherwise hand the
	// segmenter a short middle packet, which nothing else would surface. The
	// run stays one packet behind because only a packet something followed is
	// known not to be the last, whose duration is routinely short. The held
	// payload is copied, since the demuxer reuses pkt.Data on the next read.
	// One buffer serves the stream: held.Data aliases it, so release comes
	// before refill, and reuse is safe because WritePacket does not retain
	// pkt.Data (container.Muxer).
	grid := int64(0)
	var held codec.Packet
	var have bool
	var scratch []byte

	// release emits the held packet. checked is false only for the stream's
	// last, which has no successor to prove it is not short by right.
	release := func(checked bool) error {
		if !have {
			return nil
		}
		if checked {
			switch {
			case grid == 0:
				grid = held.Dur
			case held.Dur != grid:
				return waxerr.New(waxerr.CodeUnsupportedFormat,
					fmt.Sprintf("waxflow: source packet of %d samples breaks the %d-sample grid; this stream cannot be segmented without re-encoding",
						held.Dur, grid))
			}
		}
		have = false
		return seg.WritePacket(held, emitSeg)
	}
	// false: fMP4 states no trim per packet, so an inner trim is refused here,
	// a backstop to both callers' refusal before the run. The position
	// arithmetic below stays raw, the delivered timeline since no emitted
	// packet carries one.
	fill, err := silenceFor(segTrack)
	if err != nil {
		return nil, err
	}
	run, err := copyPackets(ctx, demux, walkTrackID, false, fill, func(pkt container.Packet) error {
		if pos < p0 {
			pos += pkt.Lost + pkt.Dur
			return nil // still walking up to the restart point
		}
		if err := release(true); err != nil {
			return err
		}
		scratch = append(scratch[:0], pkt.Data...)
		held, have = pkt.Packet, true
		held.Data = scratch
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The final packet, unchecked since nothing followed it; the segmenter
	// still rejects a zero-length or straddling one.
	if err := release(false); err != nil {
		return nil, err
	}
	if err := seg.End(emitSeg); err != nil {
		return nil, err
	}
	res.Samples = run.samples
	return res, nil
}

// firstDur is the duration of track's first packet, from a demuxer that has
// delivered none: a stream's block size, when a later frame exists.
func firstDur(demux container.Demuxer, track int) (int, error) {
	var pkt container.Packet
	for {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if pkt.Track == track {
			return int(pkt.Dur), nil
		}
	}
}

// seekPackets positions demux at or before target and reports the decode
// position it landed on. A run from the top does not seek at all, which is what
// keeps a whole-stream remux working on a demuxer that cannot seek.
func seekPackets(ctx context.Context, demux container.Demuxer, track int, target int64) (int64, error) {
	if target == 0 {
		return 0, nil
	}
	sk, ok := demux.(container.Seeker)
	if !ok {
		return 0, waxerr.New(waxerr.CodeUnsupportedFormat,
			"waxflow: this container cannot seek, so a mid-stream segmented remux cannot start")
	}
	landed, err := sk.SeekSample(ctx, track, target)
	if err != nil {
		return 0, err
	}
	if landed > target {
		return 0, waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("waxflow: seek landed at %d, past the segment start %d", landed, target))
	}
	return landed, nil
}

// Remux rewrites src's container around its existing packets, with no decode,
// DSP or encode: the output holds the source's access units byte for byte.
// opts must be one PlanRemux accepts. A request it declines is an error (the
// destination muxer's own refusal, or CodeInvalidRequest), never a silent
// re-encode, since a caller reaching this directly has chosen the rung.
func (e *Engine) Remux(ctx context.Context, src container.Source, hint string, dst io.Writer, opts TranscodeOptions, run ...RunOption) (*TranscodeResult, error) {
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, err
	}
	return e.RemuxDemuxer(ctx, demux, info.Default(), dst, opts, run...)
}

// RemuxDemuxer is Remux on an already-opened demuxer, for packets that are not
// one sniffable Source (Cut hands in a filtered, retimed view). The caller
// owns demux. track is the demuxer's own track, never a plan's: the walk
// filters on track.ID, and a plan's track is renumbered to 0.
//
// On a dst the muxer cannot patch, a declared length nothing checked is
// confirmed by walking the source first, as the transcode rung does. A source
// whose trims ride on its packets (container.Track.TrimsUnread) and that
// declares no delay is walked first too for the fragmented MP4 muxer, which
// states an end trim only in the length its Begin writes, unless the caller
// hands in a measurement that read them (WithMeasured).
func (e *Engine) RemuxDemuxer(ctx context.Context, demux container.Demuxer, track container.Track,
	dst io.Writer, opts TranscodeOptions, run ...RunOption) (*TranscodeResult, error) {
	x := newRunConfig(run)
	patchable := muxseek.CanSeek(dst)
	plan, err := e.runPlan(track, opts)
	if err != nil {
		return nil, err
	}
	row, err := remuxRow(track, opts.Format)
	if err != nil {
		return nil, err
	}
	fm, err := row.form(opts.Container)
	if err != nil {
		return nil, err
	}
	// The end trim has to be in hand before Begin for this muxer to state
	// it, and a source that keeps its trims on its packets has not read one.
	// The caller's measurement has it, or a walk reads it. With none, or one
	// beside a trim inside the run, the copy runs as it was planned.
	walked := false
	if fm.trimByLength && unreadEndTrim(track) {
		measured, ok := track, false
		if x.measured != nil {
			measured = adoptMeasured(track, *x.measured)
			ok = !measured.TrimsUnread
		}
		if !ok {
			if measured, ok, err = walkedTrack(ctx, demux, track.ID); err != nil {
				return nil, err
			}
			walked = ok
		}
		if ok && measured.Padding > 0 && measured.MidPadding == 0 {
			track = measured
			if plan, err = e.runPlan(track, opts); err != nil {
				return nil, err
			}
		}
	}
	// Confirmed after the checks above, so a request that cannot work is
	// refused before the payload is read, as in the transcode rung; a walk
	// changes only the length, which no check above reads. A declared bound
	// with no delay beside it is confirmed for any dst (unconfirmedBound): the
	// trailer cannot find a trim nothing states.
	if !patchable || unconfirmedBound(track) {
		if confirm := confirmableLength(track, demux); confirm != nil {
			if err := confirm(ctx); err != nil {
				return nil, err
			}
			for _, t := range demux.Tracks() {
				if t.ID == track.ID {
					track = t
					plan.Track.Samples = t.Samples
					plan.Track.SamplesExact, plan.Track.SamplesAdvisory = t.SamplesExact, t.SamplesAdvisory
					break
				}
			}
		}
	}
	// The transcode's projection rule: an advisory total declares nothing, nor
	// does a count the source could confirm and has not, on a dst that cannot
	// be patched.
	//
	// plan.Samples keeps the estimate as the progress denominator, and the
	// trailer settles the real length from the packets.
	muxTrack := plan.Track
	muxTrack.Samples = projectedLength(muxTrack, demux, patchable)
	// With no delay this muxer refuses an end trim it has no length for, so
	// the source's count goes with one, confirmed or not: a run that ends
	// short of it brings no trim (remuxTrailer). An estimate states nothing,
	// and beside a delay the copy keeps the header it wrote before.
	if fm.trimByLength && muxTrack.Delay == 0 && muxTrack.Padding > 0 && !muxTrack.SamplesAdvisory {
		muxTrack.Samples = plan.Track.Samples
	}
	// nil encoder: this rung has none (see form.mux).
	mux, err := fm.mux(muxTrack, opts, x, nil, dst)
	if err != nil {
		return nil, err
	}
	if err := checkSeekable(mux, dst, opts.Format); err != nil {
		return nil, err
	}
	// The source's MD5 holds for the copy only while it fills no hole, so it
	// goes out only where the muxer can clear it at End, which the first hole
	// does; a header that is final once written states none.
	var clearMD5 func()
	if track.Codec == codec.FLAC {
		if c, ok := mux.(interface{ ClearMD5() }); ok && patchable {
			clearMD5 = c.ClearMD5
		} else if muxTrack.CodecConfig, err = flac.ClearMD5(muxTrack.CodecConfig); err != nil {
			return nil, err
		}
	}
	if err := mux.Begin([]container.Track{muxTrack}); err != nil {
		return nil, err
	}
	e.log.Debug("remux started", "codec", track.Codec,
		"out", opts.Format, "outContainer", plan.Container, "samples", muxTrack.Samples, "walked", walked)

	// A FLAC muxer numbers the frames it writes, the silent ones a hole leaves
	// included; the copy only makes those.
	var done int64
	fill, err := silenceFor(muxTrack)
	if err != nil {
		return nil, err
	}
	if fresh := fill.silence; clearMD5 != nil {
		fill.silence = func(template []byte, n int64) ([]byte, error) {
			clearMD5()
			return fresh(template, n)
		}
	}
	copied, err := copyPackets(ctx, demux, track.ID, StatesTrimsPerPacket(plan.Container), fill, func(pkt container.Packet) error {
		// Padding forwarded: Matroska states trims per packet, and it is the
		// only destination this call lets an inner trim reach.
		if err := mux.WritePacket(container.Packet{Track: 0, Padding: pkt.Padding, Lost: pkt.Lost, Packet: pkt.Packet}); err != nil {
			return err
		}
		// Progress in output samples against the planned track's length, as
		// on every rung. Not the demuxer's: a cut's demuxer is a view of a
		// longer source, whose length would stall progress short of its total.
		if x.progress != nil {
			done += pkt.Lost + pkt.Dur
			x.progress(done, plan.Track.Samples)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	trailer := remuxTrailer(muxTrack, copied)
	if err := mux.End(trailer); err != nil {
		return nil, missedProjection(patchable, muxTrack.Samples, trailer.Samples, err)
	}
	// The trailer's Samples, not the track's: where the container declares no
	// length the trailer resolves it from the walk, as a transcode reports its
	// encoder's count, and a cache entry completes with the measured length.
	e.log.Debug("remux finished", "samples", trailer.Samples)
	// The same list the decode rung carries, from the demuxer this walked:
	// the copy read every packet, so the damage it found on the way is whole.
	var found format.Info
	format.RefreshWarnings(&found, demux)
	return &TranscodeResult{Samples: trailer.Samples, Format: track.Fmt, Container: plan.Container,
		InputWarnings: found.Warnings, LostSamples: copied.lost,
		TruncatedSamples: container.TruncatedOf(demux, track.ID)}, nil
}

// runPlan is planRemux for a run, which refuses what a plan declines.
func (e *Engine) runPlan(track container.Track, opts TranscodeOptions) (*RemuxPlan, error) {
	plan, refused, err := e.planRemux(track, opts, false)
	switch {
	case err != nil:
		return nil, err
	case refused != nil:
		// The destination muxer's own refusal, in its words and code, as its
		// Begin would give it.
		return nil, waxerr.Annotate(fmt.Sprintf("waxflow: a %s source cannot be remuxed to %s", track.Codec, opts.Format), refused)
	case plan == nil:
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: a %s source cannot be remuxed to %s with these options; transcode it",
				track.Codec, opts.Format))
	}
	return plan, nil
}

// unreadEndTrim reports whether t may end in a trim nothing has read: its
// trims ride on its packets, it declares no delay, and its codec has a trim
// to state (gaplessSurvives).
func unreadEndTrim(t container.Track) bool {
	return t.TrimsUnread && t.Delay == 0 && gaplessSurvives(container.Track{Codec: t.Codec, Padding: 1})
}

// walkedTrack walks demux and returns its track id as the walk left it; ok
// is false for a demuxer with no walk.
func walkedTrack(ctx context.Context, demux container.Demuxer, id int) (t container.Track, ok bool, err error) {
	w, ok := demux.(lengthWalker)
	if !ok {
		return t, false, nil
	}
	if err := w.Walk(ctx); err != nil {
		return t, false, err
	}
	for _, t := range demux.Tracks() {
		if t.ID == id {
			return t, true, nil
		}
	}
	return t, false, nil
}

// fillHole writes the pkt.Lost samples damage took before pkt as silent frames
// of the stream's block size, bs, or refuses when the codec has no silence.
func fillHole(pkt container.Packet, bs int64, silence func([]byte, int64) ([]byte, error),
	write func(container.Packet) error) error {
	if silence == nil {
		return waxerr.New(waxerr.CodeMalformedInput, fmt.Sprintf(
			"waxflow: damage took %d samples before sample %d, which a copy of this stream cannot carry", pkt.Lost, pkt.PTS))
	}
	if bs <= 0 {
		bs = pkt.Dur
	}
	at := pkt.PTS - pkt.Lost
	for left := pkt.Lost; left > 0; {
		n := min(left, bs, flac.MaxBlockSize)
		data, err := silence(pkt.Data, n)
		if err != nil {
			return err
		}
		if err := write(container.Packet{Track: pkt.Track, Packet: codec.Packet{Data: data, PTS: at, Dur: n, Sync: true}}); err != nil {
			return err
		}
		at, left = at+n, left-n
	}
	return nil
}

// holeFill is how a copy carries the samples damage took in front of a
// packet (container.Packet.Lost).
type holeFill struct {
	// silence makes the silent frame of n samples the copy writes, from the
	// packet behind the hole; nil for a codec with none.
	silence func(template []byte, n int64) ([]byte, error)
	// inPackets says each packet states its own position, so the hole is
	// already on the timeline the copy writes and needs no frames.
	inPackets bool
}

// silenceFor says how a copy of t carries a hole: silent frames where the
// codec has one (FLAC), the packets alone where each states its own position
// (a WavPack block carries its index), and not at all otherwise.
func silenceFor(t container.Track) (holeFill, error) {
	switch t.Codec {
	case codec.WavPack:
		return holeFill{inPackets: true}, nil
	case codec.FLAC:
		si, err := flac.ParseStreamInfo(t.CodecConfig)
		if err != nil {
			return holeFill{}, err
		}
		return holeFill{silence: func(template []byte, n int64) ([]byte, error) {
			return flac.SilentFrame(template, si, int(n))
		}}, nil
	}
	return holeFill{}, nil
}

// copyPackets walks demux and hands every packet of the given track to write,
// returning what the walk moved; the progressive and segmented forms share it
// so they agree on which packets belong to the stream. Packets are borrowed:
// write must consume pkt.Data before returning, as every muxer does
// (container.Muxer), since a copy per packet would cost the rung its point.
func copyPackets(ctx context.Context, demux container.Demuxer, track int, perPacketTrims bool,
	fill holeFill, write func(container.Packet) error) (copiedRun, error) {
	var pkt container.Packet
	var run copiedRun
	// tail is the trims that run unbroken up to the newest packet, each
	// clamped to its packet: the end trim unless something plays after them.
	var tail int64
	var lastDur int64 // the previous packet's duration, the stream's block size
	for {
		if err := ctx.Err(); err != nil {
			return copiedRun{}, waxerr.Wrap(waxerr.CodeCanceled, "remux canceled", err)
		}
		// The bare io.EOF sentinel is the clean end; a wrapped one is an I/O
		// failure that happens to carry it (see container.Demuxer).
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			// The trims that reach the end go back into the run: they are
			// the track's end trim, which SettleLength takes off again.
			// Every earlier one is already out of both sides.
			run.samples += tail
			run.endPadding += tail
			return run, nil
		}
		if err != nil {
			return copiedRun{}, err
		}
		if pkt.Track != track {
			continue
		}
		if pkt.Lost > 0 {
			// Damage took these samples, and a decode delivers them as
			// silence; the copy keeps the same timeline, in silent frames or
			// in the packets' own positions, so a header's length still holds.
			run.samples += pkt.Lost
			run.lost += pkt.Lost
			if !fill.inPackets {
				if err := fillHole(pkt, lastDur, fill.silence, write); err != nil {
					return copiedRun{}, err
				}
				pkt.Lost = 0 // filled: the packet goes on with no hole in front
			}
		}
		lastDur = pkt.Dur
		// Two clamps. The timeline's trim cannot reach past its packet; the
		// trailer's is bounded only below, since a final trim larger than its
		// frame is still the end trim and the settle removes the excess. Both
		// stop at zero: a caller's demuxer may state a negative trim, which
		// would lengthen the run.
		pad := max(pkt.Padding, 0)
		clamped := min(pad, pkt.Dur)
		// This packet plays after the trims before it, so they were inner
		// ones, which a destination without per-packet trims would play: the
		// copy stops. A packet trimmed whole plays nothing, and extends them.
		if tail > 0 && clamped < pkt.Dur {
			if !perPacketTrims {
				return copiedRun{}, innerTrimRefusal(tail, copyTrimRemedy)
			}
			tail = 0
		}
		run.samples += pkt.Dur - clamped
		tail += clamped
		run.endPadding = pad - clamped
		run.trimmed = run.trimmed || pad > 0
		if err := write(pkt); err != nil {
			return copiedRun{}, err
		}
	}
}

// innerTrimRefusal is a packet copy's refusal of a trim in the middle of the
// stream that it cannot carry; remedy names the way past it, which differs by
// rung. A remux into Matroska carries such a trim; a cut carries only the ones
// its plan placed, since past an unplaced one the source's packets leave the
// grid its windows snap to. The padded packet is already out, as with any
// mid-copy refusal. The plan declines a source whose walk found one it cannot
// carry (container.Track.MidPadding); this is the backstop for a source
// nothing measured.
func innerTrimRefusal(pad int64, remedy string) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
		"waxflow: this source trims %d samples in the middle of the stream (a Matroska DiscardPadding), which this rung cannot carry; %s",
		pad, remedy))
}

// copyTrimRemedy and cutTrimRemedy are the two ways past an inner trim, stated
// where the rung that meets one can name the right one.
const (
	copyTrimRemedy = "measure the source first and the plan declines this rung, write mka or webm, or transcode it"
	cutTrimRemedy  = "measure the source first so the plan can place the trim (and write mka or webm to keep it), or transcode it"
)

// copiedRun is what a packet walk observed: the decode duration it moved, and
// what the source said about trims along the way.
type copiedRun struct {
	// samples is the raw decode duration on the packet timeline, which is what
	// SettleLength means by raw: every frame the packets deliver, plus the
	// end trim's and no other trim's (see container.Packet.Padding).
	samples int64
	// endPadding is the end trim the packets state: the final packet's own,
	// and the trim of each packet before it while every later one is trimmed
	// whole. trimmed is whether any packet stated a trim.
	endPadding int64
	trimmed    bool
	// lost is how many of samples are holes (container.Packet.Lost) the copy
	// carried, as a decode's silence is.
	lost int64
}
