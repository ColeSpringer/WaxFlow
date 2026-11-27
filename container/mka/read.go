package mka

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// Tracks returns the single selected audio track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.log.List() }

// resetReading positions the frame iterator at a cluster boundary and clears
// per-run state (accumulated position, Vorbis timing, end-of-stream).
func (d *Demuxer) resetReading(off int64) {
	d.curOff = off
	d.inCluster = false
	d.clusterEnd = 0
	d.clusterCursor = 0
	d.clusterUnknown = false
	d.clusterClamped = false
	d.pending = d.pending[:0]
	d.pendingIdx = 0
	d.running = 0
	d.vorbisPrevBlock = 0
}

// ReadPacket yields the next codec packet. Packet data aliases the read
// window and is reused across calls.
//
// The frame's duration and its share of its block's DiscardPadding were fixed
// when the block loaded (timeBlock), by the code the walk reads too. The
// position excludes the trims before it (container.Packet.Padding): running
// advances by what the frame delivers, so a seek across an inner trim lands
// where it says.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	f, data, err := d.nextFrame()
	if err != nil {
		return err
	}
	*pkt = container.Packet{
		Track:   0,
		Padding: f.pad,
		Packet: codec.Packet{
			Data: data,
			PTS:  d.running,
			Dur:  f.dur,
			Sync: true, // every audio frame is a sync point
		},
	}
	d.running += f.dur - min(f.pad, f.dur)
	return nil
}

// nextFrame returns the next selected-track frame and its data, or io.EOF at
// end of stream.
func (d *Demuxer) nextFrame() (frameLoc, []byte, error) {
	if d.pendingIdx >= len(d.pending) {
		if err := d.advanceBlock(context.Background()); err != nil {
			return frameLoc{}, nil, err
		}
	}
	f := d.pending[d.pendingIdx]
	d.pendingIdx++
	data, err := d.frameBytes(f)
	if err != nil {
		return frameLoc{}, nil, err
	}
	return f, data, nil
}

// timeBlock fixes the duration of every frame of a block before it is
// installed and spreads the block's DiscardPadding over them from the last
// lace backwards, since the spec attaches the trim to the block's end. Timing
// at load, once per frame in stream order, is what keeps Vorbis right: its
// frame durations depend on the frame before, and the read and the walk both
// load blocks this way. The header prefixes it reads are in the window the
// frames are delivered from.
//
// A trim larger than the whole block stays on the last lace's Padding,
// unhonoured, and is surfaced once per file: a mid-stream one is lost (ffmpeg
// ignores such a trim outright), a final one still comes off the settled
// length through the raw-end cap.
func (d *Demuxer) timeBlock(frames []frameLoc, discardNS int64) error {
	if len(frames) == 0 {
		return nil
	}
	d.w.Trim(frames[0].off)
	for i := range frames {
		dur, err := d.frameDur(frames[i])
		if err != nil {
			return err
		}
		frames[i].dur, frames[i].pad = dur, 0
	}
	pad := nsToSamples(discardNS, d.setup.fmt.Rate)
	if pad <= 0 {
		return nil
	}
	left := pad
	for i := len(frames) - 1; i >= 0 && left > 0; i-- {
		take := min(left, frames[i].dur)
		frames[i].pad = take
		left -= take
	}
	if left > 0 {
		if !d.warnedOversizedDiscard {
			// Latched after the warn, not before: in strict mode warn returns
			// the refusal instead of recording it, and latching first would
			// let a retried walk or a second read past the same block succeed
			// where the first refused.
			if err := d.warn(frames[0].off, "DiscardPadding of %d samples on a block holding %d; only the block is trimmed",
				pad, pad-left); err != nil {
				return err
			}
			d.warnedOversizedDiscard = true
		}
		frames[len(frames)-1].pad += left
	}
	return nil
}

// frameDur returns one frame's output length in samples. PCM derives it from
// the frame size; the compressed codecs read a bounded header prefix, so
// timing never reads a whole (possibly large) frame. For Vorbis it advances
// the inter-frame state, so it is called once per frame, in order.
func (d *Demuxer) frameDur(f frameLoc) (int64, error) {
	if d.setup.id == codec.PCM {
		if d.setup.pcmBytesPerFrame <= 0 {
			return 0, nil
		}
		return int64(f.size / d.setup.pcmBytesPerFrame), nil
	}
	n := min(f.size, 128)
	data := d.w.BytesAt(f.off, n)
	if len(data) != n {
		if e := d.w.Err(); e != nil {
			return 0, e
		}
		return 0, malformed("frame prefix at %d truncated", d.log.At(f.off))
	}
	return d.frameSamples(data), nil
}

// frameBytes reads one frame's payload through the window, trimming consumed
// bytes so a long linear read does not accrete the whole file.
func (d *Demuxer) frameBytes(f frameLoc) ([]byte, error) {
	d.w.Trim(f.off)
	data := d.w.BytesAt(f.off, f.size)
	if len(data) != f.size {
		if e := d.w.Err(); e != nil {
			return nil, e
		}
		return nil, malformed("frame at %d truncated (want %d bytes)", d.log.At(f.off), f.size)
	}
	return data, nil
}

// advanceBlock loads the next selected-track block's frames into pending,
// walking clusters and skipping other tracks' blocks. It returns io.EOF at end
// of stream, and ctx's end, read before every element read, as canceled.
func (d *Demuxer) advanceBlock(ctx context.Context) error {
	for {
		if !d.inCluster {
			done, err := d.stepSegment(ctx)
			if err != nil {
				return err
			}
			if done {
				return io.EOF
			}
			continue
		}
		handled, done, err := d.stepCluster(ctx)
		if err != nil {
			return err
		}
		if done {
			return io.EOF
		}
		if handled {
			return nil
		}
	}
}

// stepSegment reads one segment-level element, entering a cluster or skipping
// anything else. done reports end of stream.
func (d *Demuxer) stepSegment(ctx context.Context) (done bool, err error) {
	if d.recording && d.walkLimit >= 0 && d.curOff >= d.walkLimit {
		// Stopping before the element is read is what makes the walk
		// resumable: curOff is exactly the limit cluster's first byte and
		// enterCluster has not stamped it, so a resume stamps it once.
		d.walkStopped = true
		return true, nil
	}
	if d.curOff >= d.segmentEnd {
		return true, nil
	}
	if err := container.WalkCanceled(ctx, "mka: "); err != nil {
		return false, err
	}
	e, kind, err := d.segmentChild(d.curOff)
	if err != nil {
		return false, err // the source failed, not the file
	}
	switch kind {
	case childCut:
		return true, d.warnCut(d.curOff)
	case childJunk:
		// Junk is stepped over to the next Segment-level element; a run past
		// maxJunk ends the stream there.
		next, err := d.resync(d.curOff, d.segmentEnd)
		if err != nil {
			return false, err
		}
		if next < 0 {
			if werr := d.warn(d.curOff, "more than %d bytes head no element, ending stream", maxJunk); werr != nil {
				return false, werr
			}
			return true, nil
		}
		if werr := d.warnJunk(d.curOff, next); werr != nil {
			return false, werr
		}
		d.curOff = next
		return false, nil
	}
	if e.clamped {
		// The file ends inside this element. A cluster's whole blocks are
		// still read; the one the end splits is not.
		if werr := d.warnCut(d.curOff); werr != nil {
			return false, werr
		}
		if e.id != idCluster {
			return true, nil
		}
	}
	if e.id == idCluster {
		d.enterCluster(e, d.curOff)
		return false, nil
	}
	if e.unknownSize {
		return true, nil // a non-cluster master with no length cannot be skipped
	}
	d.curOff = e.dataEnd()
	return false, nil
}

// enterCluster positions the cursor inside a cluster and records its extent.
// While ensureWalk is recording, it also stamps the cluster's start offset with
// the exact cumulative sample position of its first frame.
func (d *Demuxer) enterCluster(e element, startOff int64) {
	if d.recording && len(d.clusterIndex) < maxClusters {
		d.clusterIndex = append(d.clusterIndex, clusterPos{off: startOff, sample: d.walkCumulative,
			frames: int64(d.walkFrames), bytes: d.walkBytes})
	}
	d.inCluster = true
	d.clusterCursor = e.dataOff
	d.clusterUnknown = e.unknownSize
	d.clusterClamped = e.clamped
	if e.unknownSize {
		d.clusterEnd = d.segmentEnd
	} else {
		d.clusterEnd = e.dataEnd()
		d.curOff = e.dataEnd()
	}
}

// stepCluster reads one cluster-level element. handled reports that a
// selected-track block was loaded into pending; done reports end of stream.
func (d *Demuxer) stepCluster(ctx context.Context) (handled, done bool, err error) {
	if d.clusterCursor >= d.clusterEnd {
		d.inCluster = false
		if !d.clusterUnknown {
			d.curOff = d.clusterEnd
		} else {
			d.curOff = d.clusterCursor
		}
		return false, false, nil
	}
	if err := container.WalkCanceled(ctx, "mka: "); err != nil {
		return false, false, err
	}
	e, kind, err := d.readChild(d.clusterCursor, d.clusterEnd)
	if err != nil {
		return false, false, err
	}
	switch kind {
	case childCut:
		return false, true, d.warnCutIn()
	case childJunk:
		if !d.clusterUnknown {
			// A cluster that states its size says where the next begins, so
			// only the rest of this one is lost.
			if werr := d.warn(d.clusterCursor, "damaged cluster element; the rest of the cluster is skipped"); werr != nil {
				return false, false, werr
			}
			d.clusterCursor = d.clusterEnd
			return false, false, nil
		}
		// An unknown-size cluster ends at the next Segment-level element, so
		// junk inside one is stepped over to it.
		next, err := d.resync(d.clusterCursor, d.clusterEnd)
		if err != nil {
			return false, false, err
		}
		if next >= 0 && next < d.clusterEnd {
			if werr := d.warnJunk(d.clusterCursor, next); werr != nil {
				return false, false, werr
			}
			d.inCluster = false
			d.curOff = next
			return false, false, nil
		}
		if werr := d.warn(d.clusterCursor, "damaged cluster element, ending stream"); werr != nil {
			return false, false, werr
		}
		return false, true, nil
	}
	// An unknown-size cluster ends where the next top-level element begins.
	if d.clusterUnknown && isSegmentLevel(e.id) {
		d.inCluster = false
		d.curOff = d.clusterCursor
		return false, false, nil
	}
	if e.clamped {
		// The file ends inside this child: a block it splits is not read.
		return false, true, d.warnCutIn()
	}
	switch e.id {
	case idTimestamp:
		// The cluster timestamp is not needed: positions come from the
		// frame-counted index, not the container's millisecond timeline.
		if e.unknownSize {
			return false, true, nil
		}
		d.clusterCursor = e.dataEnd()
	case idSimpleBlock:
		if e.unknownSize {
			return false, true, nil
		}
		ok, err := d.loadBlock(e.dataOff, e.size, 0)
		if err != nil {
			return false, false, err
		}
		d.clusterCursor = e.dataEnd()
		return ok, false, nil
	case idBlockGroup:
		if e.unknownSize {
			return false, true, nil
		}
		ok, err := d.loadBlockGroup(e)
		if err != nil {
			return false, false, err
		}
		d.clusterCursor = e.dataEnd()
		return ok, false, nil
	default:
		if e.unknownSize {
			return false, true, nil
		}
		d.clusterCursor = e.dataEnd()
	}
	return false, false, nil
}

// warnCutIn reports that the file ends inside a cluster's child, unless the
// cluster's own cut, or the Segment's, was already reported.
func (d *Demuxer) warnCutIn() error {
	if d.clusterClamped {
		return nil
	}
	return d.warnCut(d.clusterCursor)
}

// loadBlock parses a (Simple)Block and, when it belongs to the selected track,
// times it and installs its frames as pending. A damaged block is skipped
// tolerantly; an I/O failure propagates. Nothing is installed on an error, so
// a strict refusal is not followed by the block's frames.
func (d *Demuxer) loadBlock(dataOff, size, discardNS int64) (bool, error) {
	bh, err := parseBlock(&d.w, dataOff, size, d.pending[:0])
	if err != nil {
		if waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable {
			return false, err
		}
		if werr := d.warn(dataOff, "damaged block, skipped"); werr != nil {
			return false, werr
		}
		return false, nil
	}
	if bh.track != d.sel.number {
		return false, nil
	}
	if err := d.timeBlock(bh.frames, discardNS); err != nil {
		return false, err
	}
	d.pending = bh.frames
	d.pendingIdx = 0
	return true, nil
}

// loadBlockGroup finds the Block inside a BlockGroup and its DiscardPadding,
// then loads it like a SimpleBlock.
func (d *Demuxer) loadBlockGroup(g element) (bool, error) {
	var blockOff, blockSize int64 = -1, 0
	var discardNS int64
	off := g.dataOff
	end := g.dataEnd()
	for off < end {
		e, err := d.readElement(off, end)
		if err != nil {
			if waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable {
				return false, err
			}
			if werr := d.warn(off, "damaged block group element"); werr != nil {
				return false, werr
			}
			break
		}
		switch e.id {
		case idBlock:
			blockOff, blockSize = e.dataOff, e.size
		case idDiscardPadding:
			if e.size > 8 {
				// An 8-byte signed integer at most; a larger one is malformed
				// and tolerated by ignoring the trim, not by failing playback.
				if werr := d.warn(e.dataOff, "ignoring oversized DiscardPadding"); werr != nil {
					return false, werr
				}
				break
			}
			body, err := d.readBytes(e.dataOff, e.size, 8)
			if err != nil {
				return false, err
			}
			discardNS = beInt(body)
			if discardNS < 0 {
				// A negative DiscardPadding is spec-legal (the discard moves
				// to the start of the block) but not honored here; the
				// gapless total treats it as zero, surfaced once per file.
				if !d.warnedNegativeDiscard {
					// Latched after the warn, for timeBlock's reason: a strict
					// warn is a refusal, and latching first would let a second
					// pass over the same block through.
					if werr := d.warn(e.dataOff, "ignoring negative DiscardPadding"); werr != nil {
						return false, werr
					}
					d.warnedNegativeDiscard = true
				}
				discardNS = 0
			}
		}
		if e.unknownSize {
			break
		}
		off = e.dataEnd()
	}
	if blockOff < 0 {
		return false, nil
	}
	return d.loadBlock(blockOff, blockSize, discardNS)
}

// clusterPos anchors a cluster's start offset to the exact cumulative sample
// position of its first frame, and counts the selected track's frames and
// their bytes before it, which mp3Backoff measures main data by.
type clusterPos struct {
	off    int64
	sample int64
	frames int64
	bytes  int64
}

// ensureWalk frame-counts every block once, building the whole seek index and
// recording the gapless raw total and DiscardPadding sum. It runs to the end
// at most once, on the first seek or the first Walk; no open runs it.
func (d *Demuxer) ensureWalk(ctx context.Context) error { return d.walk(ctx, -1) }

var _ container.Walker = (*Demuxer)(nil)

// Walk implements container.Walker: the full cluster walk, which a measure of
// an advisory-length file runs anyway (its ceiling is past the last cue and
// past the seconds cap, so cueLimit answers -1 and the walk runs whole). A
// strict probe of such a file walks its clusters, by name. The walk runs on
// its own cursor (see walk), so a Walk mid-read leaves the next packet where
// it was, and one whose context ends resumes on the next Walk.
func (d *Demuxer) Walk(ctx context.Context) error {
	if err := d.ensureWalk(ctx); err != nil {
		return err
	}
	d.adoptWalkedLength()
	return nil
}

// adoptWalkedLength puts a finished walk's measurement on the track: the
// trims that run unbroken to the end become the track's tail trim, the trims
// before them become MidPadding with their positions in MidTrims, and the raw
// total settles the length through the one delivery rule. No open measures,
// so every mka track arrives here with the advisory Info Duration or nothing
// at all. SettleLength is idempotent on its own output, so running twice
// changes nothing.
//
// The tail trim is the last packet's and, while each packet after it is
// trimmed whole, the one before's: a DiscardPadding trims its own block, so a
// trim longer than the last block is spread over the final ones, and nothing
// plays between them. A trim something plays after is an inner one: the
// timeline already excludes it (see container.Packet.Padding), so it is gone
// from the raw run and from the settled length both, and folding it in here
// would shorten the end by frames that play in the middle.
//
// It is still clamped to the run it trims. A DiscardPadding is a signed 64-bit
// element, so one block's is a hostile input's to choose: unclamped it can
// pass the raw total (a length of zero, which is merely wrong) or wrap past it
// (a length *larger* than the samples the walk counted, marked authoritative,
// which is the one a caller cannot defend against). No stream can discard more
// than it holds, so that is the bound.
func (d *Demuxer) adoptWalkedLength() {
	d.track.Padding = min(max(d.padding, 0), d.rawTotal)
	d.track.MidPadding = d.midPadding
	d.track.MidTrims = d.midTrims
	d.track.TrimsUnread = false
	d.track = container.SettleLength(d.track, d.rawTotal)
	if d.midPadding > 0 && !d.notedMidPadding {
		// A Note rather than a warning: the file is not damaged and the audio
		// is right. It exists so a probe, and the transcode a copy rung
		// declines into, can say why a lossy source was re-encoded.
		d.notedMidPadding = true
		d.note(0, "%d samples are trimmed inside the run by a DiscardPadding on %d of the packets audio follows",
			d.midPadding, d.midCount)
	}
}

// readerState is the packet reader's cursor, everything resetReading clears.
// It is embedded in the Demuxer, so the fields are the reader's own and a
// copy of the struct is a snapshot of the whole cursor.
type readerState struct {
	curOff         int64 // next element to read at the segment level
	inCluster      bool
	clusterEnd     int64
	clusterCursor  int64
	clusterUnknown bool
	clusterClamped bool // the file ends inside this cluster

	pending    []frameLoc // the loaded block's frames, timed (see timeBlock)
	pendingIdx int
	// running is the position the next packet is stamped with, on the timeline
	// the reader delivers: each frame's share of its block's DiscardPadding
	// comes off it as the frame goes out (see container.Packet.Padding).
	running int64

	vorbisPrevBlock int
}

// saveReader snapshots the cursor; pending is cloned, since resetReading
// truncates it in place.
func (d *Demuxer) saveReader() readerState {
	s := d.readerState
	s.pending = append([]frameLoc(nil), d.pending...)
	return s
}

func (d *Demuxer) restoreReader(s readerState) {
	s.pending = append(d.pending[:0], s.pending...)
	d.readerState = s
}

// Walked implements container.Walker: whether the full walk has reached the
// end of the segment. A walk that failed or was canceled is not finished.
// Cues do not enter it, for the reason above.
func (d *Demuxer) Walked() bool { return d.walked }

// walk frame-counts blocks into the seek index up to limit, a segment-level
// byte offset, or -1 for the whole stream.
//
// Only a walk that reached the end commits rawTotal, the two trim totals, and
// the walked flag: whether anything plays after a trim is the whole of what
// separates an inner one from the end trim, and a walk that stopped short
// does not know.
//
// The walk runs on a cursor of its own, swapped in for the packet reader's
// and out again: a Walk mid-read moves nothing, and a walk stopped at a bound
// or a cancel resumes where it stood (a scrub would otherwise cost 0.1n +
// 0.2n + 0.3n where one full walk pays n). The cursor carries the Vorbis
// block state, so every codec takes the cue bound. A failed walk starts over.
func (d *Demuxer) walk(ctx context.Context, limit int64) error {
	if d.walked {
		return nil
	}
	if d.walkErr != nil {
		return d.walkErr
	}
	if limit >= 0 && d.walkBegun && d.walkCursor.curOff >= limit {
		return nil // already counted past this bound
	}
	saved := d.saveReader()
	if d.walkBegun {
		d.restoreReader(d.walkCursor)
	} else {
		d.walkCumulative, d.walkRaw, d.walkBytes = 0, 0, 0
		d.walkMid, d.walkMidCount = 0, 0
		d.walkTail, d.walkTailOver, d.walkTailTrims = 0, 0, d.walkTailTrims[:0]
		d.walkTailCount = 0
		d.walkFrames = 0
		d.midTrims = nil
		d.clusterIndex = d.clusterIndex[:0]
		d.resetReading(d.firstClusterOff)
		d.walkBegun = true
	}
	d.recording, d.walkLimit, d.walkStopped = true, limit, false
	defer func() {
		d.walkCursor = d.saveReader()
		d.restoreReader(saved)
		d.recording, d.walkLimit = false, -1
	}()
	fail := func(e error) error {
		d.walkBegun = false // the counters ran past the cursor: start over
		// The file's own faults are kept; a source that failed to read is
		// tried again by the next walk.
		if limit < 0 && waxerr.CodeOf(e) != waxerr.CodeSourceUnreadable {
			d.walkErr = e
		}
		return e
	}
	for {
		err := d.advanceBlock(ctx)
		if err == io.EOF {
			break
		}
		if waxerr.CodeOf(err) == waxerr.CodeCanceled {
			return err // paused between elements; the cursor stands
		}
		if err != nil {
			return fail(err)
		}
		for d.pendingIdx < len(d.pending) {
			f := d.pending[d.pendingIdx]
			d.pendingIdx++
			if d.walkFrames++; d.walkFrames > maxFrames {
				return fail(malformed("more than %d frames", int64(maxFrames)))
			}
			d.walkFrame(f)
		}
	}
	if !d.walkStopped {
		// The stream ended, so the counts are whole even if a bound was set.
		// The trims that run to the end go back into the raw run, since that
		// is where SettleLength expects the end trim's frames to be; the
		// inner ones stay out of both sides.
		d.rawTotal = d.walkCumulative + d.walkTail
		d.padding = d.walkTail + d.walkTailOver
		d.midPadding = d.walkMid
		d.midCount = d.walkMidCount
		d.walked = true
	}
	return nil
}

// walkFrame counts one frame into the walk. Its trim comes off the running
// position, so cluster anchors are the positions a read delivers. The trims
// that run up to the newest frame are the end-trim candidate (walkTail): a
// frame trimmed whole extends them, and one that plays anything turns them
// into inner trims, which is what makes walkMid and the recorded positions
// the inner trims alone. A position is on the raw timeline (every frame
// whole), clamped to its frame; past maxMidTrims the sum keeps counting and
// the list stops.
func (d *Demuxer) walkFrame(f frameLoc) {
	clamped := min(f.pad, f.dur)
	if clamped < f.dur && d.walkTail > 0 {
		d.walkMid += d.walkTail
		d.walkMidCount += d.walkTailCount
		d.midTrims = append(d.midTrims, d.walkTailTrims[:min(len(d.walkTailTrims), maxMidTrims-len(d.midTrims))]...)
		d.walkTail, d.walkTailCount, d.walkTailTrims = 0, 0, d.walkTailTrims[:0]
	}
	d.walkCumulative += f.dur - clamped
	d.walkRaw += f.dur
	d.walkBytes += int64(f.size)
	d.walkTailOver = f.pad - clamped
	if clamped > 0 {
		d.walkTail += clamped
		d.walkTailCount++
		if len(d.midTrims)+len(d.walkTailTrims) < maxMidTrims {
			d.walkTailTrims = append(d.walkTailTrims, container.PacketTrim{Pos: d.walkRaw - clamped, Samples: clamped})
		}
	}
}

// maxSeekSeconds and maxSeekRate bound the operands of cueLimit's conversion to
// timestamp units. samplesToNs multiplies both whole seconds and the remainder
// by a billion, and a declared SamplingFrequency is only required to be
// positive, so bounding the sample count alone would be tuned to one rate.
// Anything past either bound falls back to the full walk.
const (
	maxSeekSeconds = 1 << 32 // ~136 years; sec*1e9 stays well inside int64
	maxSeekRate    = 1 << 24 // ~16 MHz; bounds the remainder product
)

// cueLimit is where the walk may stop when seeking to sample: the first indexed
// cluster past the target, or -1 for no usable bound (walk it all). The raw
// target is used, not the pre-roll-adjusted one, so the bound is never early.
//
// The target is a position on the trimmed timeline and the cue times are the
// container's own, which count the trimmed frames, so the converted target
// lands at or before the cue that holds it: the bound is at least as tight as
// it was and still never early. Block timestamps continue from the trimmed end
// after an mkvmerge seam, which is the case that puts a trim mid-file at all.
func (d *Demuxer) cueLimit(sample int64) int64 {
	if sample <= 0 {
		// Zero lands on the first cluster whatever the index says, and
		// SeekSample reads that off firstClusterOff with nothing indexed.
		// Otherwise the search picks the cue after the zero-timestamped first
		// one and walks that whole cluster for an answer it already had, and a
		// file without Cues walks whole.
		return d.firstClusterOff
	}
	d.resolveCues()
	rate := int64(d.setup.fmt.Rate)
	if len(d.cues) == 0 || rate <= 0 {
		return -1
	}
	if rate > maxSeekRate || sample/rate > maxSeekSeconds {
		return -1
	}
	t := samplesToNs(sample, int(rate)) / d.timestampScale
	i := sort.Search(len(d.cues), func(i int) bool { return d.cues[i].time > t })
	if i >= len(d.cues) {
		return -1 // past the last indexed cluster: nothing to bound with
	}
	// Read back the one entry this seek uses; parseCues checks none of them.
	// One element header, and a stale offset costs only the seeks using it.
	off := d.cues[i].off
	if e, kind, err := d.readChild(off, d.segmentEnd); err != nil || kind != childOK || e.id != idCluster {
		return -1
	}
	return off
}

// seekWalk walks far enough to place a seek to sample. Cues supply byte offsets
// only; the frame walk still counts the samples, since a CueTime is a
// millisecond timestamp and cannot name a sample position.
//
// A wrong index can only make a landing earlier or slower, never wrong: the
// bound decides where the walk stops, and landing early is already
// SeekSample's contract, with format.Media pre-rolling the remainder.
func (d *Demuxer) seekWalk(ctx context.Context, sample int64) error {
	if d.walked {
		return nil
	}
	limit := d.cueLimit(sample)
	var err error
	if limit < 0 {
		err = d.ensureWalk(ctx)
	} else {
		err = d.walk(ctx, limit)
	}
	// A cancel is the seek's own, and so is a bounded walk's failure, which
	// the next seek tries again. A full walk the file's damage stopped is
	// Walk's to report: a strict seek refuses with it, a tolerant one lands
	// on what the walk indexed.
	if err == nil || limit >= 0 || d.log.Strict() || waxerr.CodeOf(err) == waxerr.CodeCanceled {
		return err
	}
	return nil
}

// SeekSample lands on the indexed cluster at or before the target in the raw
// decoder timeline and returns its exact sample position. The frame-counted
// index makes this sample-accurate where the container's millisecond block
// timestamps could not. format.Media pre-rolls the remainder (decode and
// discard) for a sample-exact landing, so for Opus the SeekPreRoll margin is
// decoded through and the decoder reconverges.
func (d *Demuxer) SeekSample(ctx context.Context, track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mka: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "mka: negative seek target")
	}
	if !d.haveFirstCluster {
		return 0, nil
	}
	if err := d.seekWalk(ctx, sample); err != nil {
		return 0, err
	}
	searchRaw := max(sample-d.seekPreRollSamples, 0)
	clusterOff, landed := d.firstClusterOff, int64(0)
	if len(d.clusterIndex) > 0 {
		// The last cluster whose first frame is at or before the search point.
		idx := sort.Search(len(d.clusterIndex), func(i int) bool {
			return d.clusterIndex[i].sample > searchRaw
		}) - 1
		idx = max(idx, 0)
		if d.setup.id == codec.MP3 {
			idx = d.mp3Backoff(idx)
		}
		clusterOff = d.clusterIndex[idx].off
		landed = d.clusterIndex[idx].sample
	}
	d.resetReading(clusterOff)
	d.running = landed
	return landed, nil
}

// mp3Backoff steps a landing on cluster idx back over whole clusters until the
// frames it adds carry enough main data for any bit-reservoir reference the
// first frame of idx makes. The samples half of the backoff is preRoll's;
// this is the bytes half, which a sample count cannot express. The walk
// counted the track's own frames and bytes, so another track's blocks between
// the clusters count for nothing.
func (d *Demuxer) mp3Backoff(idx int) int {
	overhead := mp3FrameOverhead(d.setup.fmt)
	to := d.clusterIndex[idx]
	land := idx
	for n := 0; land > 0 && n < mp3BackoffClusters; n++ {
		from := d.clusterIndex[land]
		if to.bytes-from.bytes-(to.frames-from.frames)*overhead >= mp3ReservoirBytes {
			break
		}
		land--
	}
	return land
}

// isSegmentLevel reports whether id is a top-level segment element, marking the
// end of an unknown-size cluster.
func isSegmentLevel(id uint32) bool {
	switch id {
	case idCluster, idCues, idSeekHead, idInfo, idTracks,
		0x1043A770, // Chapters
		0x1254C367, // Tags
		0x1941A469: // Attachments
		return true
	}
	return false
}

// codecName trims the Matroska "A_" prefix for diagnostics. The two MPEG
// audio layers before III are named as well, since their CodecIDs say only
// "L1" and "L2" and the decoder this build has is Layer III's alone.
func codecName(codecID string) string {
	switch codecID {
	case "":
		return "unknown"
	case "A_MPEG/L1":
		return "MPEG/L1 (MP1)"
	case "A_MPEG/L2":
		return "MPEG/L2 (MP2)"
	}
	if len(codecID) > 2 && codecID[:2] == "A_" {
		return codecID[2:]
	}
	return codecID
}
