package waxflow

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// RemuxVersion identifies the remux rung's own sample-affecting logic for the
// ADR-0004 cache key.
//
// It is the only version constant this rung adds, and the absence of the others
// is deliberate rather than an oversight. A remux runs no decoder, no DSP, and
// no encoder, so no revision of any of them can change its bytes; keying on
// them would invalidate remuxes for fixes that cannot reach them. The
// progressive muxers carry no version constants at all, which is equally
// deliberate: a whole-file cache entry is self-consistent, so a framing change
// leaves older entries as merely older bytes that still decode identically.
// (The segmented form inherits mp4.SegmenterVersion for free, which exists
// because segments must agree with each other and with a restarted worker
// within one stream.)
//
// What is left is the gapless trailer this rung synthesizes from the input
// track, and that is not merely older bytes: a bug there writes a wrong
// iTunSMPB or a wrong edit list, and wrong gapless metadata is wrong playback.
// That is the one thing here that needs a version, so it is the one that has
// one.
const RemuxVersion = "remux-1"

// RemuxPlan describes what a remux would produce, computed from the source
// track's headers alone.
//
// It embeds TranscodePlan because everything downstream of the ladder reads the
// same facts off a plan whichever rung answered, so the rungs hand their
// consumers one shape. The embedded plan is built from the source track and
// never from a chain, which is what keeps it honest: Format is the source's
// own, since nothing on this rung touches samples; Versions names RemuxVersion
// and no codec revision; and BitRate and EstimatedBytes stay unknown, because
// they are. Reporting an encoder's projected bit rate for packets no encoder
// produced would be exactly the plausible-looking wrong answer this rung must
// not give.
type RemuxPlan struct {
	TranscodePlan
	// Track is what the muxer is opened with and what the trailer is
	// synthesized from: the source's own track, carrying its codec config and
	// its gapless trims across unchanged.
	Track container.Track
}

// PlanRemux reports whether opts can be served by rewriting track's container
// around its existing packets, and how. It is the ladder's middle rung, between
// serving the original bytes and a full re-encode: the codec must survive
// unchanged, so the output row's codec must match the track's and no option may
// transform samples.
//
// A request this rung cannot serve is not an error. PlanRemux returns
// (nil, nil) and the caller falls through to a transcode, which is what makes
// this a rung rather than a gate. An error means the request is wrong for every
// rung (an unsupported format, a container the format cannot produce), and a
// transcode of it would fail identically.
//
// The rule falls out of the existing output table with nothing invented:
// format=opus already means "Ogg-Opus progressive, fMP4-Opus segmented" via the
// row's hls column, so Opus-in-WebM to Opus-in-fMP4 is just this rung with
// format=opus on the segmented path. Remux connects outputs that already exist.
func (e *Engine) PlanRemux(track container.Track, opts TranscodeOptions) (*RemuxPlan, error) {
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	containerName, mediaType, err := resolveContainer(row, opts.Container)
	if err != nil {
		return nil, err
	}
	if !codecSurvives(track.Codec, row.codecID) || !remuxable(opts, track.Fmt) || !gaplessSurvives(track) {
		return nil, nil
	}
	// The muxed track is the source's, normalized the way Transcode normalizes
	// the one it builds: muxers are single-track, so the packet the loop writes
	// is track 0 and this is track 0.
	t := track
	t.ID, t.Default = 0, true
	return &RemuxPlan{
		TranscodePlan: TranscodePlan{
			Format:    track.Fmt,
			Container: containerName,
			MediaType: mediaType,
			Live:      containerLive(row.live, opts.Container),
			Versions:  []string{RemuxVersion},
			Samples:   track.Samples,
			// BytesPerFrame, FrameSize, BitRate, and EstimatedBytes stay at
			// their unknown values. The source's packets have a bit rate, but
			// it is not in its headers: reading it would mean walking the
			// container, and a plan reads headers. A caller that needs a rate
			// promised (a maxBitRate cap) has to decline this rung and take one
			// that can promise one.
			EstimatedBytes: -1,
		},
		Track: t,
	}, nil
}

// codecSurvives reports whether a track's codec can cross into the output row's
// unchanged, which is the premise this whole rung rests on.
//
// Matching codec.ID is necessary and, for exactly one codec, not sufficient.
// The rung works because container.Packet.Data is the **codec-native access
// unit** rather than the container's framing, so an AAC raw_data_block or an
// Opus packet means the same bytes in every container that carries it and
// moving it needs no bitstream filter. PCM is the one codec for which that is
// false: its "packet" is raw samples whose wire layout is the *container's*
// choice, not the codec's. RIFF writes little-endian and unsigned 8-bit, AIFF
// big-endian, Matroska signed 8-bit. Two PCM tracks with equal codec.IDs are
// not interchangeable, and the difference lives in CodecConfig where this
// comparison cannot see it.
//
// So PCM declines, and it costs nothing to decline, which is the point rather
// than a consolation. This rung exists to avoid **generation loss**, and PCM has
// none: a PCM-to-PCM transcode is a byte repack that is bit-exact by
// construction (the differential suite pins it). Rung 3 serves those requests
// with the same samples and correct framing, which rung 2 could not do without
// growing a per-container wire-format table for a codec that gains nothing from
// it. Without this, `format=wav` on an AIFF source planned as a remux and then
// died in the muxer with "riff: WAV is little-endian", a request that had worked
// before this rung existed.
func codecSurvives(src, out codec.ID) bool {
	return src == out && src != codec.PCM
}

// remuxable reports whether opts asks for nothing but a container rewrite.
//
// The field list is derived rather than written out, and that is the whole
// point. planOpts is already the exact projection of every option that shapes
// the chain or the plan, and TestPlanOptsCoverage pins it to TranscodeOptions
// field for field. So comparing a request's projection against the projection
// of the same request stripped to its format and container asks precisely "does
// anything but the wrapper differ", and it cannot drift when a new option
// lands: a shaping field must appear in planOpts, and it then appears here for
// free.
//
// Hand-listing the fields is how this bug comes back, and the bitrate family is
// where it would bite hardest and be easiest to forget. On the HLS path bitrate
// is per-variant and plan.BitRate is in the cache key, so a remux that ignored
// a set bitrate would serve one set of source packets under two different
// bitrate labels: two cache entries claiming different things about identical
// bytes.
//
// FromSample is checked separately, because it is deliberately *not* in
// planOpts: the plan cache normalizes the seek out of its key, since two seeks
// of one source share a plan. A remux cannot honor a seek at all, since cutting
// at an arbitrary sample means cutting mid-packet, so the one field the
// derivation cannot see is exactly one this rung must refuse. A derived rule
// with a hole in it would be worse than no derived rule, so the hole is named.
//
// The baseline carries ResampleProfile and Shaping rather than zeroing them,
// and the distinction they draw is the one this whole predicate is about. Both
// say *how* a transform is performed, not *whether* one is: a profile selects a
// resampler kernel and a shaping selects a dither, and a request that reaches
// this rung has neither node in its chain (dsp inserts a dither only to
// quantize float down to int, and a resampler only to change rate). They cannot
// change bytes nobody touches.
//
// Zeroing them instead is not a theoretical mistake, it is a silent one, and it
// shipped here for exactly as long as it took a test to drive the daemon:
// resample.ParseProfile("") resolves to hq, so the server stamps a non-empty
// ResampleProfile on every single request it makes. A baseline of the zero
// value therefore matched nothing the daemon ever sent, and this rung was dead
// code that no unit test could see, because rung 3 answers those requests
// correctly and merely slowly. TestRemuxServesTheMiddleRung is what makes it
// visible, by asserting the metric rather than the bytes.
// A parameter naming what the source already is asks for no transform, so it is
// resolved away before the comparison rather than read as one. This is rung 1's
// own convention, and the asymmetry without it is indefensible:
// directPlayable serves the original bytes for `rate=48000` on a 48 kHz file
// (its clauses compare the request against the track, not against zero), so
// `format=flac&rate=44100` on a 44.1 kHz FLAC direct-plays while
// `format=flac&container=mka&rate=44100` on the same file would re-encode it,
// losing a generation to produce samples it already had.
//
// The three resolved here are exactly the three directPlayable resolves, under
// the same conditions, because they are the conditions under which dsp inserts
// no node at all: a rate equal to the source's builds no resampler, a channel
// count equal to it no mix, and an integer depth equal to it neither a dither
// nor a widen. A float source with a depth request is left alone, since that
// one does quantize.
func remuxable(opts TranscodeOptions, src audio.Format) bool {
	if opts.FromSample != 0 {
		return false
	}
	norm := opts
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
		ResampleProfile: opts.ResampleProfile,
		Shaping:         opts.Shaping,
	}
	return planOptsOf(norm) == planOptsOf(base)
}

// remuxTrailer synthesizes the gapless trailer for a remuxed track, from the
// input track and the decode duration the packet walk actually moved.
//
// codec.Trailer is how an encoder already hands gapless to a muxer, so a remux
// restates the input's trims through it and every muxer then writes its own
// signalling (iTunSMPB, an edit list, the Ogg pre-skip) exactly as it does for
// an encode. Track.CodecConfig carries the ASC or OpusHead across unchanged
// beside it.
//
// The end padding is *derived* rather than copied across, and that is the
// subtle part of this rung. A container is free to express the end trim either
// way: mp4 states it outright in iTunSMPB, while Ogg-Opus encodes it in the
// final page's granule position and so reports Padding 0 with a Samples that is
// already short. Copying such a track's Padding hands the next muxer a zero,
// and a form that needs the count explicitly (Matroska's DiscardPadding) then
// writes no end trim at all: the encoder's tail padding leaks out as audible
// audio, with no error anywhere. Deriving it asks the packets instead of the
// header, which is the one source both conventions agree with, since decoded
// minus delay minus kept audio is the padding by definition.
//
// An unknown source length inverts the arithmetic rather than defeating it: the
// track's Padding is taken at its word (there is nothing to check it against)
// and the *length* is what the walk resolves, since decoded minus the trims is
// the kept audio by the same definition. That matters past the trailer, because
// a transcode reports its encoder's true count and a remux reporting -1 would
// complete a cache entry with a length the run had in fact measured.
//
// The derivation is gated on a primed stream, not applied to everything, and
// the guard is not a special case but the same definition read the other way.
// Padding is the flush of the encoder's lookahead, so a codec with no priming
// has none to flush: FLAC and ALAC declare Delay 0 and every muxer that writes
// them refuses a nonzero trim outright. Deriving against those would turn a
// container's own inconsistency (a FLAC whose STREAMINFO total disagrees with
// its frames, which format.Media tolerates as an oddity) into a nonzero padding
// and a muxer error at End, after a whole file had been written.
func remuxTrailer(t container.Track, decoded int64) codec.Trailer {
	tr := codec.Trailer{Samples: t.Samples, Delay: t.Delay, Padding: t.Padding}
	switch {
	case t.Samples < 0:
		tr.Samples = max(0, decoded-t.Delay-t.Padding)
	case t.Delay > 0:
		tr.Padding = max(0, decoded-t.Delay-t.Samples)
	}
	return tr
}

// gaplessSurvives reports whether a remux's trims would be *refused* by the
// muxer that must write them.
//
// Read the name narrowly: this decides a decline in place of a hard failure, not
// in place of a silent loss. FLAC and ALAC are lossless and have no encoder
// priming, so every muxer that writes them rejects a nonzero delay outright
// (flacn says so in as many words, and mp4's sample entry builder refuses it for
// both). A track declaring one is a container claiming something its codec
// cannot mean. Without this the request would die inside Begin, part way through
// a response; with it, rung 3 decodes and trims for real. The rule is the
// codec's rather than the container's, which is what makes it a line instead of
// a table, and both codecs normally declare no trims at all, so the common case
// pays nothing.
//
// It deliberately does **not** police a container that simply has no gapless
// signalling, and ADTS is the case worth naming because it looks like the same
// thing and is not. Remuxing AAC to ADTS does drop the source's delay. So does
// *transcoding* to ADTS: the trim is applied on decode and the fresh encoder's
// own priming is then equally unsignallable, so both rungs emit about a frame of
// unsignalled priming. Declining here would buy nothing but a re-encode of the
// same defect. ADTS carrying no gapless is a documented property of the
// container (it is why fMP4 is the row's default and adts the opt-out), and a
// property both rungs share is not one this rung can fix.
func gaplessSurvives(t container.Track) bool {
	if t.Delay == 0 && t.Padding == 0 {
		return true
	}
	return t.Codec != codec.FLAC && t.Codec != codec.ALAC
}

// PacketGrid reports the decode duration every packet of src's default track
// shares: the grid a segmented remux must lay its segment boundaries on. It
// returns 0 when the durations vary, which is a fact about the source rather
// than an error, and the caller's cue to take a rung that has its own grid.
//
// This is a demuxer walk, not a decode, in the same cost class as the
// exact-length measure (sub-millisecond for a three-minute file). It costs the
// progressive rung nothing, which needs no grid at all; only the segmented one
// asks.
//
// The final packet is excluded on purpose. A stream's last packet is routinely
// short (an encoder's tail flush), and that is no obstacle to segmenting, since
// the last segment is short anyway. Every other packet must agree, and the
// silent failure it prevents is worth naming: mp4.Segmenter emits once it holds
// SegmentSamples of packets and stamps tfdt = index * SegmentSamples, so a
// short packet in the middle desynchronizes the real decode time from that
// arithmetic and every later segment carries a tfdt that is a lie. A packet
// that straddles a boundary errors loudly; a short one in the middle would not.
func (e *Engine) PacketGrid(src container.Source, hint string) (int, error) {
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return 0, err
	}
	return packetGrid(demux, info.Default().ID)
}

func packetGrid(demux container.Demuxer, track int) (int, error) {
	grid, prev := int64(0), int64(0)
	// The predecessor is tracked with an explicit flag rather than by testing
	// prev > 0, and the distinction is not cosmetic: prev > 0 conflates
	// "no previous packet yet" with "the previous
	// packet had zero duration", so a zero-duration packet would silently skip
	// the comparison around it and let a stream look uniform that is not. An
	// explicit flag makes a zero fall into the check below and decline, which is
	// the answer that keeps the promise: mp4.Segmenter rejects a zero-duration
	// packet outright, so a plan that accepted one would promise a run that
	// cannot finish. Declining here sends it to rung 3, which can serve it.
	have, uniform := false, true
	var pkt container.Packet
	for {
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
		// One packet behind: the packet just read may be the last, whose short
		// duration is expected and must not count against the grid. Comparing
		// the previous one means the last is never compared at all.
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
		// A single-packet stream lands here too, with grid still 0. That is the
		// right answer for the wrong-looking reason: one packet is one segment,
		// which needs no grid, and declining it costs nothing real.
		return 0, nil
	}
	return int(grid), nil
}

// RemuxSegmentPlan describes the segmented (CMAF) form of a remux, as
// SegmentPlan describes a transcode's. It embeds SegmentPlan for the reason
// RemuxPlan embeds TranscodePlan: the delivery layer reads the same facts off a
// plan whichever rung answered.
type RemuxSegmentPlan struct {
	SegmentPlan
	// Track is the source's track, which the init segment is built from and the
	// trailer synthesized against.
	Track container.Track
}

// PlanRemuxSegments plans the segmented form of a remux: the rung that carries
// WaxTap's motivating case, since format=opus already means "Ogg-Opus
// progressive, fMP4-Opus segmented" and so Opus-in-WebM to Opus-in-fMP4 is this
// with nothing invented.
//
// grid is the source's packet duration from PacketGrid. A zero grid declines,
// as does anything PlanRemux declines, and both return (nil, nil).
//
// The alignment rule is to snap, not to refuse, and that is a correction to
// this milestone's plan worth stating where the code is. The plan feared that a
// 60 ms-frame Opus source has no whole-packet boundary in a 4 s segment
// (192000/2880 = 66.67) and concluded such a request must fall to rung 3. But
// segment length is not fixed at the request's ask: PlanSegments already snaps
// it to a whole number of encoder frames, which is exactly why a transcode's
// grid "is ours by construction". Handing it the packet duration as the frame
// size makes the same snap produce 67 packets of 2880 (a 4.02 s segment,
// aligned) with no new rule at all. A FLAC transcode already rounds 4 s to
// 4.01 s this way, so the behavior is not even new.
//
// What genuinely cannot be served is a source whose packet durations vary,
// since there is then no grid to snap to. That is the real decline, and it is
// the one PacketGrid reports.
func (e *Engine) PlanRemuxSegments(track container.Track, opts TranscodeOptions, segSeconds float64, grid int) (*RemuxSegmentPlan, error) {
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	if row.hls == nil {
		// Not a decline: the format has no segmented form at all, so no rung
		// serves this and the caller must hear why.
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("waxflow: %s has no segmented (HLS) form (available: %s)", opts.Format, strings.Join(SegmentedFormats(), ", ")))
	}
	rp, err := e.PlanRemux(track, opts)
	if err != nil || rp == nil {
		return nil, err
	}
	if grid <= 0 {
		return nil, nil
	}
	// The init segment declares the sample entry, so a track it cannot express
	// must decline here rather than fail once the playlist is out.
	if _, err := mp4.InitSegment(rp.Track); err != nil {
		return nil, nil
	}
	segSamples, err := snapSegmentSamples(segSeconds, track.Fmt.Rate, grid)
	if err != nil {
		return nil, err
	}
	sp := SegmentPlan{
		TranscodePlan:  rp.TranscodePlan,
		SegmentSamples: segSamples,
		// The source's own pre-skip, not the row's encoder constant. They are
		// the same number for a stream our encoder wrote and need not be for one
		// it did not, and the init segment's edit list must describe the packets
		// it actually has.
		Delay:              track.Delay,
		Codecs:             row.hls.codecs,
		TotalDecodeSamples: -1,
		Segments:           -1,
	}
	sp.Versions = []string{RemuxVersion, mp4.SegmenterVersion}
	sp.FrameSize = grid
	if track.Samples >= 0 {
		sp.TotalDecodeSamples = totalDecodeSamples(track.Samples, track.Delay, int64(grid))
		sp.Segments = (sp.TotalDecodeSamples + int64(segSamples) - 1) / int64(segSamples)
	}
	// The peak-rate bound falls back to the PCM wire rate, as it does for VBR
	// lossless, because a plan reads headers and the packets' real rate is not
	// in them. It is loose for a lossy source, and that costs nothing here: a
	// bitrate ladder is a per-variant OpusBitrate, which this rung declines, so
	// a remux is always a single variant and this number picks nothing.
	base := sp.Format.Rate * sp.Format.Channels * max(sp.Format.BitDepth, 16)
	sp.Bandwidth = base + sp.Format.Rate/grid*64 + 2000
	return &RemuxSegmentPlan{SegmentPlan: sp, Track: rp.Track}, nil
}

// RemuxInitSegment builds the CMAF init header for a planned segmented remux:
// the source's own sample entry, from the codec config it already carries, so
// the packets the segments hold and the header that describes them come from
// one place. Deterministic, like InitSegment.
func (e *Engine) RemuxInitSegment(plan *RemuxSegmentPlan) ([]byte, error) {
	t := plan.Track
	t.Samples, t.Delay = plan.Samples, plan.Delay
	return mp4.InitSegment(t)
}

// RemuxSegments emits numbered CMAF media segments from src's own packets: the
// segmented form of the middle rung, and the back end of an HLS variant that
// needs no encoder.
//
// A run starting mid-stream needs no priming at all, which is the one way this
// is simpler than its transcode sibling rather than harder. Priming exists to
// settle a resampler's window and an encoder's cross-frame state, and this rung
// has neither: the packets are the source's, already independently decodable,
// so segment n from a restarted worker is byte-identical to a continuous run's
// because it is built from the same bytes.
func (e *Engine) RemuxSegments(ctx context.Context, src container.Source, hint string, opts TranscodeOptions,
	segOpts SegmentedOptions, emit func(mp4.Segment) error) (*SegmentedResult, error) {
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, err
	}
	track := info.Default()
	rp, err := e.PlanRemux(track, opts)
	if err != nil {
		return nil, err
	}
	if rp == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: a %s source cannot be remuxed to %s with these options; transcode it",
				track.Codec, opts.Format))
	}
	switch {
	case segOpts.SegmentSamples <= 0:
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: segmenter needs a positive SegmentSamples")
	case segOpts.StartSegment < 0:
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: negative StartSegment")
	case segOpts.StartSegment > (1<<62)/int64(segOpts.SegmentSamples):
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: StartSegment overflows the sample timeline")
	}
	seg, err := mp4.NewSegmenter(rp.Track, &mp4.SegmenterOptions{
		SegmentSamples: segOpts.SegmentSamples, StartSegment: segOpts.StartSegment,
	})
	if err != nil {
		return nil, err
	}
	p0 := segOpts.StartSegment * int64(segOpts.SegmentSamples)
	pos, err := seekPackets(demux, track.ID, p0)
	if err != nil {
		return nil, err
	}

	res := &SegmentedResult{}
	emitSeg := func(s mp4.Segment) error {
		res.Segments++
		return emit(s)
	}
	e.log.Debug("segmented remux started", "container", info.Container, "codec", track.Codec,
		"out", opts.Format, "segment", segOpts.StartSegment, "segSamples", segOpts.SegmentSamples)

	// The grid is re-checked here rather than trusted from the plan, and it is
	// free: the walk is happening anyway. A plan computed against a different
	// source (a stale memo, a file replaced under an unexpired URL) would
	// otherwise hand the segmenter a short middle packet, and that is the one
	// failure mode of this rung that no error would ever surface.
	//
	// The run stays one packet behind for one reason, and it is the whole of why
	// the buffering exists: **only a packet that something followed is known not
	// to be the last**, and the last packet's duration is routinely short (an
	// encoder's tail flush, 1696 samples of a 4096-sample FLAC grid). Checking a
	// packet on arrival would reject every lossless stream in the tree at its
	// final frame. A short last packet is not a hole in the grid, it is the end
	// of the stream, and it lands in the short final segment where it belongs.
	//
	// The payload is copied because it is held across a ReadPacket: the demuxer
	// reuses pkt.Data on the next call (see container.Demuxer), so the held
	// packet would otherwise be overwritten before it is written out. The
	// progressive rung writes through immediately and needs no such copy.
	//
	// One buffer serves the whole stream, because only one packet is ever held.
	// held.Data aliases it, so the order below is load-bearing: release first,
	// refill second. Refilling ahead of the release would rewrite the very packet
	// about to be emitted. Reusing it after the release is safe on exactly the
	// contract this rung already rests on, that WritePacket does not retain
	// pkt.Data (see container.Muxer, and TestMuxersDoNotRetainPacketData, which
	// exists to keep that true).
	grid := int64(0)
	var held codec.Packet
	var have bool
	var scratch []byte
	// release emits the held packet. checked is false only for the stream's last,
	// which has no successor to prove it is not short by right.
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
	decoded, err := copyPackets(ctx, demux, track.ID, func(pkt container.Packet) error {
		if pos < p0 {
			pos += pkt.Dur
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
	// The final packet, released unchecked: nothing followed it, so a short
	// duration here is the stream ending rather than a break in the grid. The
	// segmenter still rejects a zero-length or straddling one, so releasing it
	// unchecked widens nothing.
	if err := release(false); err != nil {
		return nil, err
	}
	if err := seg.End(emitSeg); err != nil {
		return nil, err
	}
	res.Samples = decoded
	e.log.Debug("segmented remux finished", "samples", res.Samples, "segments", res.Segments)
	return res, nil
}

// seekPackets positions demux at or before target and reports the decode
// position it landed on. A run from the top does not seek at all, which is what
// keeps a whole-stream remux working on a demuxer that cannot seek.
func seekPackets(demux container.Demuxer, track int, target int64) (int64, error) {
	if target == 0 {
		return 0, nil
	}
	sk, ok := demux.(container.Seeker)
	if !ok {
		return 0, waxerr.New(waxerr.CodeUnsupportedFormat,
			"waxflow: this container cannot seek, so a mid-stream segmented remux cannot start")
	}
	landed, err := sk.SeekSample(track, target)
	if err != nil {
		return 0, err
	}
	if landed > target {
		return 0, waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("waxflow: seek landed at %d, past the segment start %d", landed, target))
	}
	return landed, nil
}

// Remux rewrites src's container around its existing packets: the ladder's
// middle rung, and the one that makes "direct play, transmux, transcode" true.
// No decode, no DSP, no encode, so no generation loss; the output holds the
// source's own access units byte for byte.
//
// opts must be one PlanRemux accepts; a request it declines is an error here
// rather than a silent re-encode, because a caller reaching this directly has
// already decided which rung it wants and a fallback it did not ask for would
// be the wrong kind of help. The ladder calls PlanRemux first and falls through
// on its own.
func (e *Engine) Remux(ctx context.Context, src container.Source, hint string, dst io.Writer, opts TranscodeOptions) (*TranscodeResult, error) {
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, err
	}
	return e.RemuxDemuxer(ctx, demux, info.Default(), dst, opts)
}

// RemuxDemuxer remuxes an already-opened demuxer to dst, the same packet copy
// as Remux without the source-open step. It is the entry point for packets that
// are not a single sniffable Source, and the packet-domain sibling of
// TranscodeMedia: the suffix says which domain the caller is opening into,
// since this one takes a container.Demuxer (packets) rather than a format.Media
// (decoded samples). Cut is the caller it exists for, handing in a filtered,
// retimed view of another demuxer.
//
// track is the demuxer's own track, not a plan's. The distinction is not
// stylistic: the packet walk filters on track.ID while the muxer is opened with
// PlanRemux's ID-0 normalization of it, so handing a plan's Track here would
// filter out every packet of a source whose track ID is not 0 and write an
// empty file. The caller owns demux.
func (e *Engine) RemuxDemuxer(ctx context.Context, demux container.Demuxer, track container.Track,
	dst io.Writer, opts TranscodeOptions) (*TranscodeResult, error) {
	plan, err := e.PlanRemux(track, opts)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: a %s source cannot be remuxed to %s with these options; transcode it",
				track.Codec, opts.Format))
	}
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	// nil encoder: see output.mux. There is no encoder on this rung, which is
	// what the whole seam exists to express.
	mux, err := row.mux(plan.Track, opts, nil, dst)
	if err != nil {
		return nil, err
	}
	if err := checkSeekable(mux, dst, opts.Format); err != nil {
		return nil, err
	}
	if err := mux.Begin([]container.Track{plan.Track}); err != nil {
		return nil, err
	}
	e.log.Debug("remux started", "codec", track.Codec,
		"out", opts.Format, "outContainer", plan.Container, "samples", plan.Track.Samples)

	var done int64
	decoded, err := copyPackets(ctx, demux, track.ID, func(pkt container.Packet) error {
		if err := mux.WritePacket(container.Packet{Track: 0, Packet: pkt.Packet}); err != nil {
			return err
		}
		// Progress means the same thing on every rung, so a caller that gets
		// callbacks from a transcode gets them here. The units match too: the
		// transcode reports output samples against the track's length, and a
		// remux's output samples are the planned track's.
		//
		// The planned track's length, not the demuxer's own. They are the same
		// number for a whole-file remux and are not for a cut, whose demuxer is
		// a filtered view of a longer source: reporting the uncut length there
		// would have progress stall short of its total forever.
		if opts.Progress != nil {
			done += pkt.Dur
			opts.Progress(done, plan.Track.Samples)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	trailer := remuxTrailer(plan.Track, decoded)
	if err := mux.End(trailer); err != nil {
		return nil, err
	}
	// The trailer's Samples, not the track's, and the difference only shows on a
	// container that declares no length: the trailer resolves that from the walk
	// (decoded minus the trims), which is the number a transcode would have
	// reported from its encoder. Handing back the track's -1 would complete a
	// cache entry with an unknown length that this run in fact measured.
	e.log.Debug("remux finished", "samples", trailer.Samples)
	return &TranscodeResult{Samples: trailer.Samples, Format: track.Fmt, Container: plan.Container}, nil
}

// copyPackets walks demux and hands every packet of the given track to write,
// returning the decode duration it moved. It is the whole of what remux does
// with a packet, in one place so the progressive and segmented forms cannot
// come to disagree about which packets belong to the stream.
//
// Packets are borrowed: the demuxer may reuse pkt.Data on the next call, and
// write must consume it before returning. Every muxer in the tree does (see
// container.Muxer), which is why the rule is stated there rather than defended
// with a copy here: a copy per packet would cost the rung its reason to exist.
func copyPackets(ctx context.Context, demux container.Demuxer, track int, write func(container.Packet) error) (int64, error) {
	var pkt container.Packet
	var samples int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, waxerr.Wrap(waxerr.CodeCanceled, "remux canceled", err)
		}
		// The bare io.EOF sentinel is the clean end; a wrapped one is an I/O
		// failure that happens to carry it (see container.Demuxer).
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			return samples, nil
		}
		if err != nil {
			return 0, err
		}
		if pkt.Track != track {
			continue
		}
		samples += pkt.Dur
		if err := write(pkt); err != nil {
			return 0, err
		}
	}
}
