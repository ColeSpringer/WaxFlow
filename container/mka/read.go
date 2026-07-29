package mka

import (
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
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// resetReading positions the frame iterator at a cluster boundary and clears
// per-run state (accumulated position, Vorbis timing, end-of-stream).
func (d *Demuxer) resetReading(off int64) {
	d.curOff = off
	d.inCluster = false
	d.clusterEnd = 0
	d.clusterCursor = 0
	d.clusterUnknown = false
	d.pending = d.pending[:0]
	d.pendingIdx = 0
	d.running = 0
	d.vorbisPrevBlock = 0
	d.curBlockDiscardNS = 0
}

// ReadPacket yields the next codec packet. Packet data aliases the read
// window and is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	data, dur, sync, err := d.nextFrame()
	if err != nil {
		return err
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: data,
			PTS:  d.running,
			Dur:  dur,
			Sync: sync,
		},
	}
	d.running += dur
	return nil
}

// nextFrame returns the next selected-track frame's data, duration, and sync
// flag, or io.EOF at end of stream. Every audio frame is a sync point.
func (d *Demuxer) nextFrame() ([]byte, int64, bool, error) {
	if d.pendingIdx >= len(d.pending) {
		if err := d.advanceBlock(); err != nil {
			return nil, 0, false, err
		}
	}
	f := d.pending[d.pendingIdx]
	d.pendingIdx++
	data, err := d.frameBytes(f)
	if err != nil {
		return nil, 0, false, err
	}
	return data, d.frameSamples(data), true, nil
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
		return nil, malformed("frame at %d truncated (want %d bytes)", f.off, f.size)
	}
	return data, nil
}

// advanceBlock loads the next selected-track block's frames into pending,
// walking clusters and skipping other tracks' blocks. It returns io.EOF at end
// of stream.
func (d *Demuxer) advanceBlock() error {
	for {
		if !d.inCluster {
			done, err := d.stepSegment()
			if err != nil {
				return err
			}
			if done {
				return io.EOF
			}
			continue
		}
		handled, done, err := d.stepCluster()
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
func (d *Demuxer) stepSegment() (done bool, err error) {
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
	e, err := d.readElement(d.curOff, d.segmentEnd)
	if err != nil {
		// No resync at this level; tolerate trailing damage as end of stream.
		if werr := d.warn(d.curOff, "damaged segment element, ending stream"); werr != nil {
			return false, werr
		}
		return true, nil
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
		d.clusterIndex = append(d.clusterIndex, clusterPos{off: startOff, sample: d.walkCumulative})
	}
	d.inCluster = true
	d.clusterCursor = e.dataOff
	d.clusterUnknown = e.unknownSize
	if e.unknownSize {
		d.clusterEnd = d.segmentEnd
	} else {
		d.clusterEnd = e.dataEnd()
		d.curOff = e.dataEnd()
	}
}

// stepCluster reads one cluster-level element. handled reports that a
// selected-track block was loaded into pending; done reports end of stream.
func (d *Demuxer) stepCluster() (handled, done bool, err error) {
	if d.clusterCursor >= d.clusterEnd {
		d.inCluster = false
		if !d.clusterUnknown {
			d.curOff = d.clusterEnd
		} else {
			d.curOff = d.clusterCursor
		}
		return false, false, nil
	}
	e, err := d.readElement(d.clusterCursor, d.clusterEnd)
	if err != nil {
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

// loadBlock parses a (Simple)Block and, when it belongs to the selected track,
// installs its frames as pending. A damaged block is skipped tolerantly; an I/O
// failure propagates.
func (d *Demuxer) loadBlock(dataOff, size, discardNS int64) (bool, error) {
	bh, err := parseBlock(&d.w, dataOff, size)
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
	d.pending = bh.frames
	d.pendingIdx = 0
	d.curBlockDiscardNS = discardNS
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
					d.warnedNegativeDiscard = true
					if werr := d.warn(e.dataOff, "ignoring negative DiscardPadding"); werr != nil {
						return false, werr
					}
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
// position of its first frame.
type clusterPos struct {
	off    int64
	sample int64
}

// ensureWalk frame-counts every block once, building the whole seek index and
// recording the gapless raw total and DiscardPadding sum. It runs at most once:
// eagerly at open for a CodecDelay track, otherwise on the first seek.
func (d *Demuxer) ensureWalk() error { return d.walk(-1) }

// walk frame-counts blocks into the seek index up to limit, a segment-level
// byte offset, or -1 for the whole stream. Reading is restored to the first
// cluster afterward.
//
// Only a walk that reached the end commits rawTotal, paddingNS, and the walked
// flag; a half-summed padding would leave finalizeTrack reading a total for
// audio it has not seen.
//
// A bounded walk extends rather than restarts, since clusterIndex is a correct
// prefix and walkedTo is a cluster boundary. Restarting would make a scrub cost
// 0.1n + 0.2n + 0.3n where one full walk pays n and later seeks are free.
func (d *Demuxer) walk(limit int64) error {
	if d.walked {
		return nil
	}
	if limit >= 0 && !d.boundedWalkSafe() {
		limit = -1
	}
	if limit >= 0 && d.walkedTo >= limit {
		return nil // already counted past this bound
	}
	if limit < 0 {
		d.walked = true // a full walk runs at most once, even if it fails
	}
	fail := func(e error) error {
		d.recording = false
		d.walkLimit = -1
		// The counters advanced past walkedTo, so it no longer names what they
		// describe. Drop it, or the next walk counts that stretch twice.
		d.walkedTo = 0
		d.resetReading(d.firstClusterOff)
		return e
	}
	if d.walkedTo > 0 {
		d.resetReading(d.walkedTo) // resume: the counters carry the prefix
	} else {
		d.walkCumulative = 0
		d.walkPaddingNS = 0
		d.walkFrames = 0
		d.clusterIndex = d.clusterIndex[:0]
		d.resetReading(d.firstClusterOff)
	}
	d.recording = true
	d.walkLimit = limit
	d.walkStopped = false
	for {
		err := d.advanceBlock()
		if err == io.EOF {
			break
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
			dur, derr := d.frameDurAt(f)
			if derr != nil {
				return fail(derr)
			}
			d.walkCumulative += dur
		}
		if d.curBlockDiscardNS > 0 {
			d.walkPaddingNS += d.curBlockDiscardNS
		}
	}
	if d.walkStopped {
		d.walkedTo = d.curOff
	} else {
		// The stream ended, so the counts are whole even if a bound was set.
		d.rawTotal = d.walkCumulative
		d.paddingNS = d.walkPaddingNS
		d.walkedTo = d.segmentEnd
		d.walked = true
	}
	d.recording = false
	d.walkLimit = -1
	d.resetReading(d.firstClusterOff)
	return nil
}

// boundedWalkSafe reports whether the walk may stop short and resume later.
// resetReading clears vorbisPrevBlock and a Vorbis frame's duration depends on
// the previous block's size, so a mid-file resume would mis-time it. This is a
// condition rather than a circumstance of needsGaplessWalk putting Vorbis on
// the eager walk, so editing that one cannot break this one silently.
func (d *Demuxer) boundedWalkSafe() bool {
	return d.setup.id != codec.Vorbis // the one codec carrying inter-frame duration state
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
func (d *Demuxer) cueLimit(sample int64) int64 {
	d.resolveCues()
	rate := int64(d.setup.fmt.Rate)
	if len(d.cues) == 0 || rate <= 0 {
		return -1
	}
	if sample <= 0 {
		// Zero lands on the first cluster whatever the index says, and
		// SeekSample reads that off firstClusterOff with nothing indexed.
		// Otherwise the search picks the cue after the zero-timestamped first
		// one and walks that whole cluster for an answer it already had.
		return d.firstClusterOff
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
	if e, err := d.readElement(off, d.segmentEnd); err != nil || e.id != idCluster {
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
func (d *Demuxer) seekWalk(sample int64) error {
	if d.walked {
		return nil
	}
	limit := d.cueLimit(sample)
	if limit < 0 {
		return d.ensureWalk()
	}
	return d.walk(limit)
}

// frameDurAt returns a frame's output length in samples for the index walk. PCM
// derives it from the frame size; the compressed codecs read a bounded header
// prefix, so the walk never reads a whole (possibly large) frame just to time
// it.
func (d *Demuxer) frameDurAt(f frameLoc) (int64, error) {
	if d.setup.id == codec.PCM {
		if d.setup.pcmBytesPerFrame <= 0 {
			return 0, nil
		}
		return int64(f.size / d.setup.pcmBytesPerFrame), nil
	}
	n := f.size
	if n > 128 {
		n = 128
	}
	d.w.Trim(f.off)
	data := d.w.BytesAt(f.off, n)
	if len(data) != n {
		if e := d.w.Err(); e != nil {
			return 0, e
		}
		return 0, malformed("frame prefix at %d truncated", f.off)
	}
	return d.frameSamples(data), nil
}

// SeekSample lands on the indexed cluster at or before the target in the raw
// decoder timeline and returns its exact sample position. The frame-counted
// index makes this sample-accurate where the container's millisecond block
// timestamps could not. format.Media pre-rolls the remainder (decode and
// discard) for a sample-exact landing, so for Opus the SeekPreRoll margin is
// decoded through and the decoder reconverges.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mka: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "mka: negative seek target")
	}
	if !d.haveFirstCluster {
		return 0, nil
	}
	if err := d.seekWalk(sample); err != nil {
		return 0, err
	}
	searchRaw := sample - d.seekPreRollSamples
	if searchRaw < 0 {
		searchRaw = 0
	}
	clusterOff, landed := d.firstClusterOff, int64(0)
	if len(d.clusterIndex) > 0 {
		// The last cluster whose first frame is at or before the search point.
		idx := sort.Search(len(d.clusterIndex), func(i int) bool {
			return d.clusterIndex[i].sample > searchRaw
		}) - 1
		if idx < 0 {
			idx = 0
		}
		clusterOff = d.clusterIndex[idx].off
		landed = d.clusterIndex[idx].sample
	}
	d.resetReading(clusterOff)
	d.running = landed
	return landed, nil
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

// codecName trims the Matroska "A_" prefix for diagnostics.
func codecName(codecID string) string {
	if codecID == "" {
		return "unknown"
	}
	if len(codecID) > 2 && codecID[:2] == "A_" {
		return codecID[2:]
	}
	return codecID
}

// joinNames renders a codec-name list for diagnostics.
func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
