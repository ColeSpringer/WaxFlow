package mka

import (
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/codecname"
	"github.com/colespringer/waxflow/container/internal/diag"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
)

// Demuxer reads one audio track from a Matroska/WebM segment. It selects the
// sound track, exposes it as a single track (ID 0), and streams block frames
// from the clusters as codec packets.
type Demuxer struct {
	src  container.Source
	size int64

	segmentDataOff int64
	segmentEnd     int64
	// openEnd says the Segment ends where the file does: it states no size,
	// or one past the file (cutShort, warned at open). An element the file
	// ends inside is then read up to the end rather than refused.
	openEnd        bool
	cutShort       bool
	junkBuf        []byte  // resync's scratch
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
	// block timestamps cannot express a sample position. Positions are on the
	// timeline the reader delivers, so the walk subtracts each frame's share
	// of its block's DiscardPadding as it passes it (see
	// container.Packet.Padding). The full walk also yields the gapless raw
	// total, the two trim totals, and where the inner trims lie.
	clusterIndex []clusterPos
	walked       bool // the full walk has run; the four below are whole
	rawTotal     int64
	padding      int64                  // the trims that run to the end, in samples
	midPadding   int64                  // the trims audio follows, summed
	midCount     int                    // how many packets those came from, for the Note
	midTrims     []container.PacketTrim // those trims, placed; at most maxMidTrims
	recording    bool                   // walk is building the index

	// In-flight walk state, carried across calls so a walk that stopped, at a
	// bound or a cancel, extends the index rather than restarting.
	walkCumulative int64 // running sample count on the delivered timeline
	walkRaw        int64 // running sample count on the raw one, every frame whole
	walkMid        int64 // running sum of the inner trims seen so far
	walkMidCount   int   // packets that contributed to walkMid
	// The end-trim candidate: the trims that run unbroken up to the newest
	// frame, each clamped to its frame, how many frames state them, and
	// where they lie. walkTailOver is what the newest frame's trim states
	// past the frame itself.
	walkTail      int64
	walkTailCount int
	walkTailTrims []container.PacketTrim
	walkTailOver  int64
	walkFrames    int   // running frame count, against maxFrames
	walkBytes     int64 // running byte count of those frames
	walkLimit     int64
	walkStopped   bool // the walk in progress hit walkLimit rather than the end
	// walkCursor is the walk's own reader cursor, where the last walk stood;
	// walkBegun says it holds one, and the counters above describe what lies
	// before it.
	walkCursor readerState
	walkBegun  bool
	// walkErr is a full walk's failure, kept: Walk reports it again and a
	// seek lands on what the walk indexed, with no second walk.
	walkErr error

	// Cues, parsed lazily on the first seek. Byte offsets only; see seekWalk.
	cues         []cueEntry
	cuesResolved bool

	// Reading state: the frame iterator's cursor over clusters and blocks,
	// one struct so a walk can save and restore it whole.
	w srcwin.Window
	readerState

	log                    diag.Log
	warnedNegativeDiscard  bool // negative DiscardPadding is surfaced once per file
	warnedOversizedDiscard bool // one larger than the frame it rides on, likewise
	notedMidPadding        bool // the inner-trim Note is recorded once per file
}

// NewDemuxer parses the segment header and positions on the first cluster.
// The returned Demuxer implements container.Seeker and container.Warner.
func NewDemuxer(src container.Source, opts *container.DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{
		src:            src,
		size:           src.Size(),
		timestampScale: defaultTimestampScale,
		seekPositions:  map[uint32]int64{},
		w:              srcwin.New(src, src.Size(), "mka: reading block data"),
		walkLimit:      -1,
	}
	d.log = diag.New("mka: ", opts)
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// warn records tolerated damage, or fails in strict mode. The list is capped
// and deduplicated; the count says how much damage was met.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	return d.log.Warn(off, format, args...)
}

// DamageCount implements container.Damaged.
func (d *Demuxer) DamageCount() int { return d.log.DamageCount() }

var _ container.Damaged = (*Demuxer)(nil)

// note records a Warning that Strict must not escalate: a limitation of this
// decoder against a file that is well formed, rather than damage in the file.
// See mp4's note, which exists for the same reason: Strict rejects mess, and an
// HE-AAC config is not mess.
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.log.Note(off, format, args...)
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

	// Segment follows the EBML header. One that states more bytes than the
	// file holds is a file cut short: what is there is read, and the cluster
	// walk ends where the file does.
	seg, _, err := d.readHeader(head.dataEnd(), d.size, true)
	if err != nil {
		return err
	}
	if seg.id != idSegment {
		return malformed("no Segment after the EBML header")
	}
	d.segmentDataOff = seg.dataOff
	switch {
	case seg.unknownSize:
		d.segmentEnd, d.openEnd = d.size, true
	case seg.clamped:
		if err := d.warn(head.dataEnd(), "the Segment states %d bytes more than the file holds; the file is cut short", seg.over); err != nil {
			return err
		}
		d.segmentEnd, d.openEnd, d.cutShort = d.size, true, true
	default:
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
// the SeekHead pointers. Junk between children is stepped over (resync); a
// run longer than maxJunk refuses the file, which has no cluster yet.
func (d *Demuxer) scanSegment() error {
	// With no cluster found the reader starts where the Segment ends, so it
	// reads nothing rather than the file's own header.
	d.firstClusterOff = d.segmentEnd
	off := d.segmentDataOff
	for i := 0; off < d.segmentEnd; i++ {
		if i > maxTopLevelElements {
			return malformed("more than %d segment-level elements", maxTopLevelElements)
		}
		e, kind, err := d.segmentChild(off)
		if err != nil {
			return err
		}
		switch kind {
		case childCut:
			return d.cutBeforeMedia(off)
		case childJunk:
			next, err := d.resync(off, d.segmentEnd)
			if err != nil {
				return err
			}
			if next < 0 {
				return malformed("more than %d bytes at %d head no element", maxJunk, d.log.At(off))
			}
			if err := d.warnJunk(off, next); err != nil {
				return err
			}
			off = next
			continue
		}
		if e.clamped && e.id != idCluster {
			return d.cutBeforeMedia(off)
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

// cutBeforeMedia answers for a file the end cuts, inside the element at off,
// before its first cluster. Without the Tracks there is nothing to read, which
// is damage, not a file with no audio track; with them, the stream is empty
// and the cut is reported.
func (d *Demuxer) cutBeforeMedia(off int64) error {
	if len(d.entries) == 0 {
		return malformed("the file ends at %d, before the Segment's Tracks", d.log.At(d.size))
	}
	return d.warnCut(off)
}

// segmentChild reads the Segment child at off. An element the file ends inside
// must be one a Segment holds; any other ID with a size past the file is junk
// that happens to parse, and resync looks behind it.
func (d *Demuxer) segmentChild(off int64) (element, childKind, error) {
	e, kind, err := d.readChild(off, d.segmentEnd)
	if err == nil && kind == childOK && e.clamped && !isSegmentLevel(e.id) && e.id != idVoid && e.id != idCRC32 {
		kind = childJunk
	}
	return e, kind, err
}

// warnJunk reports the bytes between off and next, which head no element and
// were stepped over.
func (d *Demuxer) warnJunk(off, next int64) error {
	if next >= d.segmentEnd {
		return d.warn(off, "%d bytes at the end of the Segment head no element", next-off)
	}
	return d.warn(off, "%d bytes head no element; read on from %d", next-off, d.log.At(next))
}

// warnCut reports that the file ends inside the element at off, unless the
// Segment already said the file is cut short.
func (d *Demuxer) warnCut(off int64) error {
	if d.cutShort {
		return nil
	}
	return d.warn(off, "the file ends inside the element at %d; it is cut short", d.log.At(off))
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
		// Tracks the SeekHead places past the end of a file cut short are
		// lost with the rest.
		if off, ok := d.seekPositions[idTracks]; ok && len(d.entries) == 0 && d.cutShort && off >= d.segmentEnd {
			return d.cutBeforeMedia(off)
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
				for i := range maxCuePoints / 2 {
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
	// FlagDefault and FlagEnabled default to 1: a track that leaves one out
	// is a default track, and a usable one. Channels and SamplingFrequency
	// default to 1 and 8000 Hz; mkvmerge leaves Channels out of a mono track.
	t := &trackEntry{def: true, enabled: true, channels: 1, rate: 8000}
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
		// An element with no octets is its default, 1 (RFC 8794, 6.1).
		case idFlagDefault:
			t.def = len(data) == 0 || beUint(data) != 0
		case idFlagEnabled:
			t.enabled = len(data) == 0 || beUint(data) != 0
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
// A rate or channel count the element states replaces the format's default,
// a zero or unreadable one included, which the codec setup then refuses. One
// with no octets is the default (RFC 8794, 6.1).
func parseAudioSettings(t *trackEntry, body []byte) {
	_ = walkElements(body, func(id uint32, data []byte) error {
		if len(data) == 0 && (id == idSamplingFreq || id == idChannels) {
			return nil
		}
		switch id {
		case idSamplingFreq:
			t.rate, t.rateStated = 0, true
			if f, ok := beFloat(data); ok && f > 0 && f < math.MaxInt32 {
				t.rate = int(f + 0.5)
			}
		case idChannels:
			t.channels, t.channelsStated = 0, true
			if v := beUint(data); v <= math.MaxInt32 {
				t.channels = int(v)
			}
		case idBitDepth:
			t.bitDepth = int(beUint(data))
		}
		return nil
	})
}

// DemuxerVersion is this demuxer's term in the ADR-0004 cache key, carried
// by its track. Bump it for a change that makes the demuxer
// deliver other samples for a file that has not changed.
const DemuxerVersion = "mka-demux-3"

// selectTrack picks the first audio track carrying a codec this build decodes,
// preferring a track the file marks usable and then one it marks default
// (trackEntry.rank), and resolves its codec setup.
func (d *Demuxer) selectTrack() error {
	var cands []*trackEntry
	var found []string
	for _, t := range d.entries {
		if t.trackType != trackTypeAudio {
			continue
		}
		if mkvCodecID(t.codecID) == "" {
			found = append(found, codecName(t.codecID))
			continue
		}
		cands = append(cands, t)
	}
	slices.SortStableFunc(cands, func(a, b *trackEntry) int { return b.rank() - a.rank() })
	var chosen *trackEntry
	var head []byte
	var headAt int64
	for _, t := range cands {
		if mkvCodecID(t.codecID) == codec.MP3 {
			// A CodecID that says Layer III can be wrong about its frames.
			// The first frame decides: Layer I and II are named and passed
			// over like any codec with no decoder here. Only the candidate
			// in hand is looked at.
			head, headAt, _ = d.firstFrameHead(t)
			if id := layerCodecID(head); id != "" {
				found = append(found, codecName(id))
				continue
			}
		}
		chosen = t
		break
	}
	if chosen == nil {
		if len(found) > 0 {
			return unsupported("no decodable audio track (found: %s)", strings.Join(found, ", "))
		}
		return unsupported("no audio track")
	}
	setup, err := resolveCodec(chosen)
	if err != nil {
		return err
	}
	d.sel = chosen
	if setup.id == codec.MP3 && head != nil {
		// Before Valid, so the format validated is the one decoded with.
		if err := d.mp3AdoptFrameFormat(&setup, head, headAt); err != nil {
			return err
		}
	}
	if err := setup.fmt.Valid(); err != nil {
		return container.UnusableFormat("mka", setup.fmt, err)
	}
	if setup.warning != "" {
		d.note(0, "%s", setup.warning)
	}
	d.setup = setup
	return nil
}

// layerCodecID is the CodecID of the layer a frame header declares when that
// is Layer I or II, "" for any other bytes.
func layerCodecID(head []byte) string {
	switch codecname.MPEGLayer(head) {
	case "MP1":
		return "A_MPEG/L1"
	case "MP2":
		return "A_MPEG/L2"
	}
	return ""
}

// mp3AdoptFrameFormat lets the first frame's header, head at off, decide an
// MP3 track's format. The frame is what the decoder reads, so it wins; an
// Audio element that stated otherwise is warned as damage, and one that
// stated nothing held only the format's defaults. A track whose first frame
// was not found, or is no Layer III frame, keeps what the element said.
func (d *Demuxer) mp3AdoptFrameFormat(s *codecSetup, head []byte, off int64) error {
	h, err := mp3.ParseHeader(head)
	if err != nil {
		return nil
	}
	got := h.PCMFormat()
	if got == s.fmt {
		return nil
	}
	t := d.sel
	if (t.rateStated && t.rate != got.Rate) || (t.channelsStated && t.channels != got.Channels) {
		if err := d.warn(off, "the Audio element says %d Hz, %d channels; the first frame says %v; the frame wins",
			t.rate, t.channels, got); err != nil {
			return err
		}
	}
	s.fmt = got
	return nil
}

// maxProbeChildren bounds how many of the first cluster's children
// firstFrameHead looks through for a track's first block.
const maxProbeChildren = 64

// firstFrameHead reads the opening mp3.HeaderLen bytes of track t's first
// frame, from the first cluster's first block of that track, and where they
// begin. It reports nothing it cannot read, records no damage, and leaves the
// reader where it was: the read meets the same bytes again and reports them
// then.
func (d *Demuxer) firstFrameHead(t *trackEntry) (head []byte, at int64, ok bool) {
	if !d.haveFirstCluster {
		return nil, 0, false
	}
	cl, kind, err := d.readChild(d.firstClusterOff, d.segmentEnd)
	if err != nil || kind != childOK || cl.id != idCluster {
		return nil, 0, false
	}
	end := d.segmentEnd
	if !cl.unknownSize {
		end = cl.dataEnd()
	}
	off := cl.dataOff
	for range maxProbeChildren {
		if off >= end {
			break
		}
		e, kind, err := d.readChild(off, end)
		if err != nil || kind != childOK || e.unknownSize || e.clamped {
			break
		}
		if cl.unknownSize && isSegmentLevel(e.id) {
			break
		}
		block := e
		if e.id == idBlockGroup {
			block = d.groupBlock(e)
		}
		if block.id == idSimpleBlock || block.id == idBlock {
			mine, _ := d.w.Try(true, func() bool {
				bh, err := parseBlock(&d.w, block.dataOff, block.size, nil)
				if err != nil || bh.track != t.number || len(bh.frames) == 0 {
					return false
				}
				f := bh.frames[0]
				if b := d.w.BytesAt(f.off, min(f.size, mp3.HeaderLen)); len(b) == mp3.HeaderLen {
					head, at, ok = append([]byte(nil), b...), f.off, true
				}
				return true
			})
			if mine {
				return head, at, ok
			}
		}
		off = e.dataEnd()
	}
	return nil, 0, false
}

// groupBlock returns the Block inside a BlockGroup, or an element of ID 0
// when the first few children hold none.
func (d *Demuxer) groupBlock(g element) element {
	off, end := g.dataOff, g.dataEnd()
	for range 8 {
		if off >= end {
			break
		}
		e, err := d.readElement(off, end)
		if err != nil || e.unknownSize {
			break
		}
		if e.id == idBlock {
			return e
		}
		off = e.dataEnd()
	}
	return element{}
}

// finalizeTrack builds the container.Track, resolving gapless trims. No open
// reads a payload, so the length is the Info Duration as an advisory total, or
// -1 when there is none: Matroska states its tail trim per block
// (container.Packet.Padding) rather than as a total, so a read is gapless
// without one and only the number waits for Walk.
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
	advisory := false
	if dur := d.durationSamples(rate); dur >= 0 {
		samples, advisory = dur, true
	}

	// Padding and MidPadding stay 0: no header element states a trim, and the
	// blocks that do are payload. Walk splits the ones that run to the end
	// from the ones audio follows and settles the length.
	d.track = container.Track{
		ID:              0,
		DemuxerVersion:  DemuxerVersion,
		Codec:           d.setup.id,
		CodecConfig:     d.setup.config,
		Fmt:             d.setup.fmt,
		Samples:         samples,
		Delay:           delay,
		SamplesAdvisory: advisory,
		TrimsUnread:     true,
		Default:         true,
	}
	d.resetReading(d.firstClusterOff)
	return nil
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
