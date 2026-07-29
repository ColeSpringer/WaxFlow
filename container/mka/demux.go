package mka

import (
	"fmt"
	"sort"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads one audio track from a Matroska/WebM segment. It selects the
// sound track, exposes it as a single track (ID 0), and streams block frames
// from the clusters as codec packets.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions
	size int64

	segmentDataOff int64
	segmentEnd     int64
	timestampScale int64   // nanoseconds per tick
	durationTicks  float64 // Info Duration, in ticks; 0 when absent

	entries       []*trackEntry
	seekPositions map[uint32]int64 // element ID -> absolute offset (via SeekHead)

	sel   *trackEntry
	setup codecSetup
	track container.Track

	seekPreRollSamples int64

	firstClusterOff  int64
	haveFirstCluster bool

	// clusterIndex maps each cluster's start offset to the exact cumulative
	// sample position at its first frame, frame-counted by walk. Seeks land on
	// an indexed cluster, sample-exact, because the container's millisecond
	// block timestamps cannot express a sample position. The full walk also
	// yields the gapless raw total and DiscardPadding sum.
	clusterIndex []clusterPos
	walked       bool // the full walk has run; rawTotal and paddingNS are whole
	rawTotal     int64
	paddingNS    int64
	recording    bool // walk is building the index

	// In-flight walk state, carried across calls so a bounded walk extends the
	// index rather than restarting. walkedTo is always a cluster boundary.
	walkCumulative int64 // running sample count during the index walk
	walkPaddingNS  int64 // running DiscardPadding sum, committed only by a full walk
	walkFrames     int   // running frame count, against maxFrames
	walkLimit      int64
	walkedTo       int64
	walkStopped    bool // the walk in progress hit walkLimit rather than the end

	// Cues, parsed lazily on the first seek. Byte offsets only; see seekWalk.
	cues         []cueEntry
	cuesResolved bool

	// Reading state: the frame iterator's cursor over clusters and blocks.
	w              srcwin.Window
	curOff         int64 // next element to read at the segment level
	inCluster      bool
	clusterEnd     int64
	clusterCursor  int64
	clusterUnknown bool

	pending           []frameLoc
	pendingIdx        int
	running           int64 // accumulated output position (raw decoder timeline)
	curBlockDiscardNS int64 // DiscardPadding of the block in pending, in ns

	vorbisPrevBlock int

	warnings              []container.Warning
	warnedNegativeDiscard bool // negative DiscardPadding is surfaced once per file
}

// NewDemuxer parses the segment header and positions on the first cluster.
// The returned Demuxer implements container.Seeker and container.Warner.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{
		src:            src,
		size:           src.Size(),
		timestampScale: defaultTimestampScale,
		seekPositions:  map[uint32]int64{},
		w:              srcwin.New(src, src.Size(), "mka: reading block data"),
		walkLimit:      -1,
	}
	if opts != nil {
		d.opts = *opts
	}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return malformed("%s (at offset %d)", msg, off)
	}
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: msg})
	return nil
}

// note records a Warning that Strict must not escalate: a limitation of this
// decoder against a file that is well formed, rather than damage in the file.
// See mp4's note, which exists for the same reason: Strict rejects mess, and an
// HE-AAC config is not mess.
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: fmt.Sprintf(format, args...)})
}

// readBytes reads n bytes at off into a fresh buffer, bounded by a cap so a
// crafted size cannot force a huge allocation.
func (d *Demuxer) readBytes(off, n, cap int64) ([]byte, error) {
	if n < 0 || n > cap {
		return nil, malformed("element of %d bytes exceeds the %d cap", n, cap)
	}
	buf := make([]byte, n)
	if err := container.ReadFull(d.src, buf, off); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "mka: reading element body", err)
	}
	return buf, nil
}

// parse reads the EBML header, finds the segment, walks its header elements up
// to the first cluster, selects the audio track, and resolves gapless trims.
func (d *Demuxer) parse() error {
	// EBML header at offset 0.
	head, err := d.readElement(0, d.size)
	if err != nil {
		return err
	}
	if head.id != idEBML {
		return malformed("file does not begin with an EBML header")
	}
	if head.unknownSize {
		// The header is a small fixed structure; an unknown length is
		// malformed, and dataEnd() would sit one byte before the Segment.
		return malformed("EBML header has unknown size")
	}
	if err := d.checkDocType(head); err != nil {
		return err
	}

	// Segment follows the EBML header.
	seg, err := d.readElement(head.dataEnd(), d.size)
	if err != nil {
		return err
	}
	if seg.id != idSegment {
		return malformed("no Segment after the EBML header")
	}
	d.segmentDataOff = seg.dataOff
	if seg.unknownSize {
		d.segmentEnd = d.size
	} else {
		d.segmentEnd = seg.dataEnd()
	}

	if err := d.scanSegment(); err != nil {
		return err
	}
	if err := d.resolveDeferred(); err != nil {
		return err
	}
	if err := d.selectTrack(); err != nil {
		return err
	}
	if err := d.finalizeTrack(); err != nil {
		return err
	}
	return nil
}

// checkDocType reads the EBML header's DocType and warns on anything but
// matroska or webm; the four magic bytes already gated the sniff.
func (d *Demuxer) checkDocType(head element) error {
	if head.size <= 0 || head.size > 1<<16 {
		return nil
	}
	body, err := d.readBytes(head.dataOff, head.size, 1<<16)
	if err != nil {
		return err
	}
	var docType string
	_ = walkElements(body, func(id uint32, data []byte) error {
		if id == idDocType {
			docType = string(data)
		}
		return nil
	})
	if docType != "" && docType != "matroska" && docType != "webm" {
		return d.warn(head.dataOff, "unexpected EBML DocType %q", docType)
	}
	return nil
}

// scanSegment walks the segment's children from its start, parsing SeekHead,
// Info, and Tracks, and stops at the first cluster (which marks the media
// start). Anything after the first cluster (Cues, or Tracks that trail the
// media) is not read here; resolveDeferred fetches what is still missing via
// the SeekHead pointers.
func (d *Demuxer) scanSegment() error {
	off := d.segmentDataOff
	for i := 0; off < d.segmentEnd; i++ {
		if i > maxTopLevelElements {
			return malformed("more than %d segment-level elements", maxTopLevelElements)
		}
		e, err := d.readElement(off, d.segmentEnd)
		if err != nil {
			// A damaged header chain is unrecoverable (no resync at this
			// level); use whatever was parsed if a cluster was already found.
			if d.haveFirstCluster {
				break
			}
			return err
		}
		if e.id == idCluster {
			d.firstClusterOff = e.dataOff - headerLen(e, off)
			d.haveFirstCluster = true
			break
		}
		if e.unknownSize {
			// A non-cluster master with no declared length cannot be skipped;
			// stop the header scan and use what was parsed.
			if werr := d.warn(off, "segment element %#x has unknown size", e.id); werr != nil {
				return werr
			}
			break
		}
		if err := d.parseSegmentChild(e); err != nil {
			return err
		}
		off = e.dataEnd()
	}
	return nil
}

// headerLen is the byte length of element e's ID+size header, given the offset
// it was read from. firstClusterOff must point at the cluster's ID so
// ReadPacket can re-read the cluster header and its timestamp.
func headerLen(e element, off int64) int64 { return e.dataOff - off }

// parseSegmentChild parses one non-cluster segment child of interest.
func (d *Demuxer) parseSegmentChild(e element) error {
	switch e.id {
	case idSeekHead:
		body, err := d.readBytes(e.dataOff, e.size, maxHeaderElement)
		if err != nil {
			return err
		}
		d.parseSeekHead(body)
	case idInfo:
		body, err := d.readBytes(e.dataOff, e.size, maxHeaderElement)
		if err != nil {
			return err
		}
		d.parseInfo(body)
	case idTracks:
		body, err := d.readBytes(e.dataOff, e.size, maxHeaderElement)
		if err != nil {
			return err
		}
		if err := d.parseTracks(body); err != nil {
			return err
		}
	}
	return nil
}

// resolveDeferred reads Tracks via SeekHead when the forward scan stopped at
// the first cluster before encountering it (the uncommon case where Tracks
// follows the media data).
func (d *Demuxer) resolveDeferred() error {
	if len(d.entries) == 0 {
		if err := d.readViaSeek(idTracks, maxHeaderElement, d.parseTracksAt); err != nil {
			return err
		}
	}
	if d.timestampScale <= 0 {
		d.timestampScale = defaultTimestampScale
	}
	return nil
}

// readViaSeek reads the element the SeekHead placed at id's position, if any,
// and hands its body to parse. cap bounds the allocation.
func (d *Demuxer) readViaSeek(id uint32, cap int64, parse func([]byte) error) error {
	off, ok := d.seekPositions[id]
	if !ok || off < d.segmentDataOff || off >= d.segmentEnd {
		return nil
	}
	e, err := d.readElement(off, d.segmentEnd)
	if err != nil || e.id != id || e.unknownSize {
		return nil // a bad SeekHead pointer is tolerated, not fatal
	}
	body, err := d.readBytes(e.dataOff, e.size, cap)
	if err != nil {
		return err
	}
	return parse(body)
}

func (d *Demuxer) parseTracksAt(body []byte) error { return d.parseTracks(body) }

// cueEntry is one index point: a cluster's timestamp and its byte offset.
type cueEntry struct {
	time int64
	off  int64
}

// resolveCues parses the Cues index on first use. Nothing about reading it may
// fail a seek: the index is an optimization, so a bad one leaves it empty and
// the walk unbounded, exactly as a file with no Cues. Raising would also make
// the outcome depend on call order, since cuesResolved latches before the read.
func (d *Demuxer) resolveCues() {
	if d.cuesResolved {
		return
	}
	d.cuesResolved = true
	_ = d.readViaSeek(idCues, maxCuesElement, d.parseCues)
}

// parseCues reads the Cues index: one entry per CuePoint naming the selected
// track, as an absolute cluster offset, sorted by time.
//
// Past maxCuePoints the index is thinned rather than truncated, as recordCue
// does. Truncating in file order would leave a head-only index, where every
// seek past the retained part finds no bound at all.
//
// Offsets are range-checked but not read back; cueLimit checks the one entry a
// seek picks. Checking all of them would cost a scattered read apiece on the
// first seek, and would discard the whole index over one stale offset.
func (d *Demuxer) parseCues(body []byte) error {
	var out []cueEntry
	seen, stride := 0, 1
	err := walkElements(body, func(id uint32, data []byte) error {
		if id != idCuePoint {
			return nil
		}
		e, ok := d.parseCuePoint(data)
		if !ok {
			return nil
		}
		if seen%stride == 0 {
			if len(out) == maxCuePoints {
				for i := 0; i < maxCuePoints/2; i++ {
					out[i] = out[2*i]
				}
				out = out[:maxCuePoints/2]
				stride *= 2
			}
			out = append(out, e)
		}
		seen++
		return nil
	})
	if err != nil {
		return nil // a damaged index is dropped, not raised
	}
	sort.Slice(out, func(i, j int) bool { return out[i].time < out[j].time })
	d.cues = out
	return nil
}

// parseCuePoint reads one CuePoint's time and its cluster position for the
// selected track. One omitting CueTrack is taken; a single-track file often
// writes none.
func (d *Demuxer) parseCuePoint(body []byte) (cueEntry, bool) {
	var e cueEntry
	haveTime, havePos := false, false
	_ = walkElements(body, func(id uint32, data []byte) error {
		switch id {
		case idCueTime:
			if v := int64(beUint(data)); v >= 0 {
				e.time, haveTime = v, true
			}
		case idCueTrackPositions:
			if havePos {
				return nil
			}
			var track uint64
			pos := int64(-1)
			haveTrack := false
			_ = walkElements(data, func(cid uint32, cdata []byte) error {
				switch cid {
				case idCueTrack:
					track, haveTrack = beUint(cdata), true
				case idCueClusterPosition:
					pos = int64(beUint(cdata))
				}
				return nil
			})
			if pos >= 0 && (!haveTrack || track == d.sel.number) {
				// A position near int64's ceiling wraps the sum negative,
				// which the range check below rejects.
				e.off, havePos = d.segmentDataOff+pos, true
			}
		}
		return nil
	})
	if !haveTime || !havePos || e.off < d.segmentDataOff || e.off >= d.segmentEnd {
		return cueEntry{}, false
	}
	return e, true
}

// parseSeekHead records the byte positions SeekHead advertises for Tracks,
// Info, and Cues. Positions are relative to the segment's data start.
func (d *Demuxer) parseSeekHead(body []byte) {
	_ = walkElements(body, func(id uint32, data []byte) error {
		if id != idSeek {
			return nil
		}
		var seekID uint32
		var pos int64
		haveID, havePos := false, false
		_ = walkElements(data, func(cid uint32, cdata []byte) error {
			switch cid {
			case idSeekID:
				seekID = uint32(beUint(cdata))
				haveID = true
			case idSeekPosition:
				pos = int64(beUint(cdata))
				havePos = true
			}
			return nil
		})
		if haveID && havePos {
			if _, seen := d.seekPositions[seekID]; !seen {
				d.seekPositions[seekID] = d.segmentDataOff + pos
			}
		}
		return nil
	})
}

// parseInfo reads TimestampScale and Duration.
func (d *Demuxer) parseInfo(body []byte) {
	_ = walkElements(body, func(id uint32, data []byte) error {
		switch id {
		case idTimestampScale:
			if v := int64(beUint(data)); v > 0 {
				d.timestampScale = v
			}
		case idDuration:
			if f, ok := beFloat(data); ok && f > 0 {
				d.durationTicks = f
			}
		}
		return nil
	})
}

// parseTracks parses each TrackEntry.
func (d *Demuxer) parseTracks(body []byte) error {
	return walkElements(body, func(id uint32, data []byte) error {
		if id != idTrackEntry {
			return nil
		}
		if len(d.entries) >= maxTracks {
			return malformed("more than %d tracks", maxTracks)
		}
		t, err := d.parseTrackEntry(data)
		if err != nil {
			// One malformed track (an over-cap CodecPrivate, say) does not
			// doom a file that still carries a decodable audio track.
			if werr := d.warn(-1, "skipping malformed track: %v", err); werr != nil {
				return werr
			}
			return nil
		}
		d.entries = append(d.entries, t)
		return nil
	})
}

// parseTrackEntry parses one TrackEntry's fields.
func (d *Demuxer) parseTrackEntry(body []byte) (*trackEntry, error) {
	t := &trackEntry{}
	err := walkElements(body, func(id uint32, data []byte) error {
		switch id {
		case idTrackNumber:
			t.number = beUint(data)
		case idTrackType:
			t.trackType = beUint(data)
		case idCodecID:
			t.codecID = string(data)
		case idCodecPrivate:
			if len(data) > maxCodecPrivate {
				return malformed("CodecPrivate of %d bytes exceeds the %d cap", len(data), maxCodecPrivate)
			}
			t.codecPriv = append([]byte(nil), data...)
		case idCodecDelay:
			t.codecDelay = int64(beUint(data))
		case idSeekPreRoll:
			t.seekPreRoll = int64(beUint(data))
		case idFlagDefault:
			t.def = beUint(data) != 0
		case idAudio:
			parseAudioSettings(t, data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// parseAudioSettings reads the Audio element's rate, channels, and bit depth.
func parseAudioSettings(t *trackEntry, body []byte) {
	_ = walkElements(body, func(id uint32, data []byte) error {
		switch id {
		case idSamplingFreq:
			if f, ok := beFloat(data); ok && f > 0 {
				t.rate = int(f + 0.5)
			}
		case idChannels:
			t.channels = int(beUint(data))
		case idBitDepth:
			t.bitDepth = int(beUint(data))
		}
		return nil
	})
}

// selectTrack picks the first audio track carrying a codec this build decodes,
// preferring the container's default track, and resolves its codec setup.
func (d *Demuxer) selectTrack() error {
	var chosen, fallback *trackEntry
	var found []string
	for _, t := range d.entries {
		if t.trackType != trackTypeAudio {
			continue
		}
		if mkvCodecID(t.codecID) == "" {
			found = append(found, codecName(t.codecID))
			continue
		}
		if chosen == nil || (t.def && !chosen.def) {
			chosen = t
		}
		if fallback == nil {
			fallback = t
		}
	}
	if chosen == nil {
		chosen = fallback
	}
	if chosen == nil {
		if len(found) > 0 {
			return malformed("no decodable audio track (found: %s)", joinNames(found))
		}
		return malformed("no audio track")
	}
	setup, err := resolveCodec(chosen)
	if err != nil {
		return err
	}
	if err := setup.fmt.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "mka: unusable audio format", err)
	}
	if setup.warning != "" {
		d.note(0, "%s", setup.warning)
	}
	d.sel = chosen
	d.setup = setup
	return nil
}

// finalizeTrack builds the container.Track, resolving gapless trims. A track
// with CodecDelay (the Opus-in-WebM gapless case) walks the whole stream once
// for an exact sample total; others take an advisory length from Duration.
func (d *Demuxer) finalizeTrack() error {
	rate := d.setup.fmt.Rate
	delay := nsToSamples(d.sel.codecDelay, rate)

	// The SeekPreRoll element wins when present (Opus writes it); otherwise a
	// per-codec default lands the seek far enough before the target for an
	// overlap-add decoder to rebuild its history before the first delivered
	// sample. Without it, the first block after a seek onto a cluster boundary
	// decodes cold and its leading output is wrong.
	d.seekPreRollSamples = nsToSamples(d.sel.seekPreRoll, rate)
	if d.seekPreRollSamples == 0 {
		d.seekPreRollSamples = d.setup.preRoll()
	}

	samples := int64(-1)
	exact := false
	if (d.sel.codecDelay > 0 || d.needsGaplessWalk()) && d.haveFirstCluster {
		// A gapless track needs the exact decoder-output total to place the end
		// trim. Opus signals it with CodecDelay (front) plus DiscardPadding
		// (tail); Vorbis carries no absolute sample count in its bitstream and
		// signals its tail trim with DiscardPadding alone (CodecDelay 0, the
		// priming is inside its first packet), so it always needs the walk to
		// resolve rawTotal - padding. The walk that finds the total also builds
		// the seek index.
		if err := d.ensureWalk(); err != nil {
			return err
		}
		padding := nsToSamples(d.paddingNS, rate)
		samples = d.rawTotal - delay - padding
		if samples < 0 {
			samples = 0
		}
		exact = true
	} else if dur := d.durationSamples(rate); dur >= 0 {
		samples = dur
	}

	d.track = container.Track{
		ID:           0,
		Codec:        d.setup.id,
		CodecConfig:  d.setup.config,
		Fmt:          d.setup.fmt,
		Samples:      samples,
		Delay:        delay,
		SamplesExact: exact,
		Default:      true,
	}
	d.resetReading(d.firstClusterOff)
	return nil
}

// needsGaplessWalk reports whether the selected codec needs the frame-counting
// walk to resolve an exact sample total independent of a CodecDelay signal.
// Vorbis does: its packets carry no absolute position (unlike FLAC's numbered
// frames) and it self-primes with no front delay, so the container's rawTotal
// minus the DiscardPadding tail is the only exact length.
func (d *Demuxer) needsGaplessWalk() bool {
	return d.setup.id == codec.Vorbis
}

// durationSamples converts the Info Duration (in ticks) to a sample count, or
// -1 when no usable duration was declared. It is an advisory length (millisecond
// granularity), so the track it feeds leaves SamplesExact false.
func (d *Demuxer) durationSamples(rate int) int64 {
	if d.durationTicks <= 0 {
		return -1
	}
	ns := int64(d.durationTicks*float64(d.timestampScale) + 0.5)
	return nsToSamples(ns, rate)
}
