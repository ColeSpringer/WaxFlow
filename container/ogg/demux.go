package ogg

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxPacket caps packet reassembly across pages. No supported codec's
	// packet approaches it (FLAC frame size is 24-bit; Vorbis/Opus packets are
	// far smaller).
	maxPacket = 1 << 24
	// maxBOSPages bounds the concurrently-multiplexed stream count.
	maxBOSPages = 64
	// maxHeaderPackets bounds the mapping's header packets to skip.
	maxHeaderPackets = 1 << 16
	// maxResync bounds the scan for the next page capture after damage.
	maxResync = 1 << 20
	// seekWindow is the bisection cutoff: below this span, walk packets.
	seekWindow = 128 << 10
	// maxSeekPackets bounds the accumulating seek's per-walk packet arrays, so a
	// past-end target or a granule-less span cannot grow them without limit.
	maxSeekPackets = 1 << 16
)

// mappingCtor pairs a BOS signature with a mapping constructor. A nil make is a
// codec recognized (so errors can name it) but not yet decodable.
var mappingCtors = []struct {
	prefix string
	name   string
	make   func() mapping
}{
	{"\x7FFLAC", "flac", func() mapping { return &flacMapping{} }},
	{"\x01vorbis", "vorbis", func() mapping { return &vorbisMapping{} }},
	{"OpusHead", "opus", func() mapping { return &opusMapping{} }},
	{"Speex   ", "speex", nil},
	{"\x80theora", "theora", nil},
	{"fishead\x00", "skeleton", nil},
}

// sniffMapping returns a constructor for a BOS packet's codec (nil when
// unknown) and the codec name for diagnostics.
func sniffMapping(pkt []byte) (func() mapping, string) {
	for _, m := range mappingCtors {
		if len(pkt) >= len(m.prefix) && string(pkt[:len(m.prefix)]) == m.prefix {
			return m.make, m.name
		}
	}
	return nil, "unknown"
}

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads the first supported logical stream from an Ogg source. The
// codec-specific work is delegated to a mapping; this type owns page framing,
// packet reassembly, resync, and granule bisection.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	mapping  mapping
	serial   uint32
	track    container.Track
	warnings []container.Warning

	// headerPackets is the count of header packets after the identification
	// packet, or 0 in detect mode (find audio by the mapping's isAudio).
	headerPackets int

	firstData int64 // offset of the page where the first audio packet begins
	totalSize int64

	off      int64  // next page read position
	scan     []byte // resync/bisection probe window, reused across calls
	page     page   // current page of our serial
	seg      int    // next segment index in page
	segOff   int    // byte offset of that segment within the page body
	partial  []byte // packet reassembly across pages
	pktPage  int64  // offset of the page the current packet began on
	skipCont bool   // drop a continued packet head (after seek/resync)
	eos      bool

	running int64 // accumulated output position (accumulating mappings)

	pending     []byte // one-packet pushback (first frame, seek landing)
	pendingPTS  int64
	pendingDur  int64
	pendingSync bool
	havePending bool
	empty       bool // stream carries zero audio packets
}

// page is one parsed Ogg page.
type page struct {
	off     int64
	granule int64
	serial  uint32
	flags   byte
	lacing  []byte
	body    []byte
	size    int64 // whole page length in bytes
}

// NewDemuxer parses an Ogg source up to its first audio packet. The returned
// Demuxer implements container.Seeker and container.Warner.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, totalSize: src.Size()}
	if opts != nil {
		d.opts = *opts
	}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "ogg: "+fmt.Sprintf(format, args...))
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

// readPage reads and CRC-checks the page at off. It does not resync; callers
// own that policy. A nil page with nil error means end of file.
func (d *Demuxer) readPage(off int64, p *page) (bool, error) {
	if off+headerLen > d.totalSize {
		return false, nil
	}
	var hdr [headerLen + 255]byte
	if err := container.ReadFull(d.src, hdr[:headerLen], off); err != nil {
		return false, waxerr.Wrap(waxerr.CodeSourceUnreadable, "ogg: reading page header", err)
	}
	if !Match(hdr[:]) {
		return false, malformed("no page capture pattern at offset %d", off)
	}
	if hdr[4] != 0 {
		return false, malformed("unknown page structure version %d", hdr[4])
	}
	nsegs := int(hdr[26])
	if off+headerLen+int64(nsegs) > d.totalSize {
		return false, malformed("page lacing table extends past end of file")
	}
	if err := container.ReadFull(d.src, hdr[headerLen:headerLen+nsegs], off+headerLen); err != nil {
		return false, waxerr.Wrap(waxerr.CodeSourceUnreadable, "ogg: reading lacing table", err)
	}
	bodyLen := 0
	for _, l := range hdr[headerLen : headerLen+nsegs] {
		bodyLen += int(l)
	}
	if off+headerLen+int64(nsegs)+int64(bodyLen) > d.totalSize {
		return false, malformed("page body extends past end of file")
	}

	p.off = off
	p.flags = hdr[5]
	p.granule = int64(binary.LittleEndian.Uint64(hdr[6:]))
	p.serial = binary.LittleEndian.Uint32(hdr[14:])
	p.size = int64(headerLen + nsegs + bodyLen)
	p.lacing = append(p.lacing[:0], hdr[headerLen:headerLen+nsegs]...)
	if cap(p.body) < bodyLen {
		p.body = make([]byte, bodyLen)
	}
	p.body = p.body[:bodyLen]
	if err := container.ReadFull(d.src, p.body, off+headerLen+int64(nsegs)); err != nil {
		return false, waxerr.Wrap(waxerr.CodeSourceUnreadable, "ogg: reading page body", err)
	}

	crc := crc32(0, hdr[:22])
	crc = crc32(crc, []byte{0, 0, 0, 0})
	crc = crc32(crc, hdr[26:headerLen+nsegs])
	crc = crc32(crc, p.body)
	if want := binary.LittleEndian.Uint32(hdr[22:]); crc != want {
		return false, malformed("page checksum mismatch at offset %d", off)
	}
	return true, nil
}

// nextPageAt scans forward from off for the next valid page, tolerating junk in
// between up to maxResync bytes. Returns false at end of file.
func (d *Demuxer) nextPageAt(off int64, p *page) (int64, bool, error) {
	base := off
	if d.scan == nil {
		d.scan = make([]byte, 64<<10)
	}
	for off < d.totalSize {
		if off-base > maxResync {
			return 0, false, malformed("no page capture pattern within %d bytes", int64(maxResync))
		}
		n := min(int64(len(d.scan)), d.totalSize-off)
		if n < 4 {
			return 0, false, nil
		}
		buf := d.scan[:n]
		if err := container.ReadFull(d.src, buf, off); err != nil {
			return 0, false, waxerr.Wrap(waxerr.CodeSourceUnreadable, "ogg: scanning for page", err)
		}
		rel := 0
		for {
			i := bytes.Index(buf[rel:], []byte("OggS"))
			if i < 0 {
				break
			}
			cand := off + int64(rel+i)
			rel += i + 1
			ok, err := d.readPage(cand, p)
			if err == nil && ok {
				return cand, true, nil
			}
			if err != nil && waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable {
				return 0, false, err
			}
		}
		off += n - 3 // a capture pattern may straddle the boundary
	}
	return 0, false, nil
}

// advancePage moves the cursor to the next page of our serial, resyncing
// tolerantly on damage.
func (d *Demuxer) advancePage() (bool, error) {
	for {
		var p page
		p.lacing, p.body = d.page.lacing, d.page.body // reuse allocations
		ok, err := d.readPage(d.off, &p)
		if err != nil && waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
			if werr := d.warn(d.off, "damaged page, resyncing"); werr != nil {
				return false, werr
			}
			var at int64
			at, ok, err = d.nextPageAt(d.off+1, &p)
			if ok {
				p.off = at
				d.partial = d.partial[:0]
				d.skipCont = true
			}
		}
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil // end of file
		}
		d.off = p.off + p.size
		if p.serial != d.serial {
			if p.flags&flagBOS == 0 {
				continue // concurrent stream, not ours
			}
			if werr := d.warn(p.off, "chained stream ignored"); werr != nil {
				return false, werr
			}
			return false, nil
		}
		d.page = p
		d.seg, d.segOff = 0, 0
		return true, nil
	}
}

// nextPacket reassembles the next packet of our serial. It returns nil at end
// of stream.
func (d *Demuxer) nextPacket() ([]byte, error) {
	for {
		if d.eos {
			return nil, nil
		}
		for d.seg < len(d.page.lacing) {
			segStart := d.seg
			bodyOff := d.segOff
			segLen := 0
			complete := false
			for d.seg < len(d.page.lacing) {
				l := int(d.page.lacing[d.seg])
				segLen += l
				d.seg++
				if l < 255 {
					complete = true
					break
				}
			}
			d.segOff += segLen
			data := d.page.body[bodyOff : bodyOff+segLen]
			if segStart == 0 && d.page.flags&flagContinued != 0 && len(d.partial) == 0 && !d.skipCont {
				if err := d.warn(d.page.off, "continued packet with no beginning, dropped"); err != nil {
					return nil, err
				}
				d.skipCont = true
			}
			if segStart == 0 && d.page.flags&flagContinued != 0 && d.skipCont {
				if complete {
					d.skipCont = false // dropped the orphaned tail
				}
				continue
			}
			d.skipCont = false
			if len(d.partial) == 0 && !(segStart == 0 && d.page.flags&flagContinued != 0) {
				d.pktPage = d.page.off // a fresh packet begins here
			}
			if len(d.partial)+len(data) > maxPacket {
				return nil, malformed("packet exceeds %d bytes", int64(maxPacket))
			}
			if !complete {
				d.partial = append(d.partial, data...)
				break // packet continues on the next page
			}
			if len(d.partial) > 0 {
				data = append(d.partial, data...)
				d.partial = d.partial[:0]
			}
			return data, nil
		}
		if d.page.flags&flagEOS != 0 {
			if len(d.partial) > 0 {
				if werr := d.warn(d.page.off, "stream ends mid-packet, tail dropped"); werr != nil {
					return nil, werr
				}
				d.partial = d.partial[:0]
			}
			d.eos = true
			return nil, nil
		}
		ok, err := d.advancePage()
		if err != nil {
			return nil, err
		}
		if !ok {
			if len(d.partial) > 0 {
				if werr := d.warn(d.off, "stream ends mid-packet, tail dropped"); werr != nil {
					return nil, werr
				}
				d.partial = d.partial[:0]
			}
			d.eos = true
			return nil, nil
		}
		if len(d.partial) > 0 && d.page.flags&flagContinued == 0 {
			if werr := d.warn(d.page.off, "unfinished packet dropped at page boundary"); werr != nil {
				return nil, werr
			}
			d.partial = d.partial[:0]
		}
	}
}

// endsPage reports whether the packet just returned by nextPacket ended exactly
// at the current page's boundary, so the page granule is that packet's output
// position. Accumulating seeks anchor on this.
func (d *Demuxer) endsPage() bool {
	return d.seg == len(d.page.lacing)
}

// parse walks the BOS section, picks the first supported stream, skips its
// header packets, and stashes the first audio packet.
func (d *Demuxer) parse() error {
	var (
		p       page
		found   []string
		haveBOS bool
	)
	off := int64(0)
	for i := 0; ; i++ {
		if i > maxBOSPages {
			return malformed("more than %d beginning-of-stream pages", maxBOSPages)
		}
		ok, err := d.readPage(off, &p)
		if err != nil {
			return err
		}
		if !ok || p.flags&flagBOS == 0 {
			break
		}
		pkt := p.body
		ctor, name := sniffMapping(pkt)
		if ctor != nil && !haveBOS {
			m := ctor()
			extra, err := m.parseID(pkt)
			if err != nil {
				return err
			}
			d.mapping = m
			d.serial = p.serial
			d.headerPackets = extra
			if extra == detectHeaders {
				d.headerPackets = 0
			}
			haveBOS = true
		} else {
			found = append(found, name)
		}
		off = p.off + p.size
	}
	if !haveBOS {
		if len(found) == 0 {
			return malformed("no streams")
		}
		return malformed("no supported stream (found: %s)", joinNames(found))
	}
	if len(found) > 0 {
		if err := d.warn(0, "ignoring %d additional stream(s): %s", len(found), joinNames(found)); err != nil {
			return err
		}
	}

	// Consume header packets, then find the first audio packet.
	detect := d.headerPackets == 0
	d.off = off
	d.seg, d.segOff = 0, 0
	d.page = page{}
	for skipped := 0; ; skipped++ {
		if skipped > maxHeaderPackets {
			return malformed("more than %d header packets", maxHeaderPackets)
		}
		pkt, err := d.nextPacket()
		if err != nil {
			return err
		}
		if pkt == nil {
			if werr := d.warn(d.off, "stream has no audio packets"); werr != nil {
				return werr
			}
			d.empty = true
			break
		}
		if skipped < d.headerPackets {
			if err := d.mapping.parseHeader(pkt); err != nil {
				return err
			}
			continue
		}
		if !d.mapping.isAudio(pkt) {
			if detect {
				continue // unknown header count: skip until audio
			}
			return malformed("first audio packet is not an audio packet")
		}
		pts, dur, sync, ok := d.mapping.packetTiming(pkt, d.running)
		if !ok {
			if detect {
				continue
			}
			return malformed("first audio packet has no valid timing")
		}
		d.pending = append(d.pending[:0], pkt...)
		d.pendingPTS, d.pendingDur, d.pendingSync = pts, dur, sync
		d.havePending = true
		d.running = pts + dur
		d.firstData = d.pktPage
		break
	}

	// lastGranule is computed lazily: it is a multi-MiB tail scan, and a
	// mapping only needs it when the length is not otherwise known (a FLAC
	// STREAMINFO with a nonzero total skips it entirely, the common case).
	lastGranule := func() int64 {
		if d.empty {
			return 0
		}
		return d.lastGranule()
	}
	track, err := d.mapping.finalizeTrack(lastGranule)
	if err != nil {
		return err
	}
	if d.empty {
		track.Samples = 0
	}
	d.track = track
	return nil
}

func joinNames(names []string) string {
	var b bytes.Buffer
	for i, n := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(n)
	}
	return b.String()
}

// Tracks returns the single decoded track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// ReadPacket yields one codec packet. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	var (
		data     []byte
		pts, dur int64
		sync     bool
	)
	if d.havePending {
		data, pts, dur, sync = d.pending, d.pendingPTS, d.pendingDur, d.pendingSync
		d.havePending = false
	} else {
		for {
			p, err := d.nextPacket()
			if err != nil {
				return err
			}
			if p == nil {
				return io.EOF
			}
			var ok bool
			pts, dur, sync, ok = d.mapping.packetTiming(p, d.running)
			if !ok {
				if werr := d.warn(d.page.off, "packet is not a valid %s packet, dropped", d.mapping.codecID()); werr != nil {
					return werr
				}
				continue
			}
			data = p
			d.running = pts + dur
			break
		}
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: data,
			PTS:  pts,
			Dur:  dur,
			Sync: sync,
		},
	}
	return nil
}

// SeekSample lands on a sync point at or before target and returns the landed
// position. Self-timing mappings (FLAC) walk frames reading absolute
// positions; accumulating mappings (Vorbis, Opus) anchor to page granules and
// land a mapping-defined pre-roll before the target.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("ogg: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "ogg: negative seek target")
	}
	if d.empty {
		return 0, nil
	}
	if d.track.Samples > 0 && sample >= d.track.Samples {
		sample = d.track.Samples - 1
	}
	if d.mapping.selfTiming() {
		return d.seekSelfTiming(sample)
	}
	return d.seekAccumulating(sample)
}

// seekSelfTiming seeks a mapping whose packets carry absolute positions (FLAC):
// bisect on page granules, then walk packets to the frame containing target.
func (d *Demuxer) seekSelfTiming(sample int64) (int64, error) {
	lo, err := d.bisect(sample)
	if err != nil {
		return 0, err
	}
	var (
		lastData         []byte
		lastPTS, lastDur int64
		lastSync         bool
		haveLast         bool
	)
	for {
		d.restartAt(lo)
		for {
			p, err := d.nextPacket()
			if err != nil {
				return 0, err
			}
			if p == nil {
				break
			}
			pts, dur, sync, ok := d.mapping.packetTiming(p, 0)
			if !ok {
				continue
			}
			lastData = append(lastData[:0], p...)
			lastPTS, lastDur, lastSync = pts, dur, sync
			haveLast = true
			if pts+dur > sample {
				break
			}
		}
		if haveLast || lo <= d.firstData {
			break
		}
		lo = max(d.firstData, lo-seekWindow)
	}
	if !haveLast {
		return 0, malformed("no frame found while seeking")
	}
	d.pending = append(d.pending[:0], lastData...)
	d.pendingPTS, d.pendingDur, d.pendingSync = lastPTS, lastDur, lastSync
	d.havePending = true
	d.running = lastPTS + lastDur
	return lastPTS, nil
}

// seekAccumulating seeks a mapping whose positions accumulate (Vorbis, Opus).
// It works in the granule timeline (where page positions are authoritative)
// and converts to the decoder's output timeline with the mapping's granule
// shift. It lands a pre-roll before the target so decode reconverges: the
// restart page's first packet is consumed (priming), and output begins at its
// start.
func (d *Demuxer) seekAccumulating(sample int64) (int64, error) {
	shift := d.mapping.granuleShift()
	granuleTarget := sample + shift
	searchGranule := granuleTarget - d.mapping.preroll()
	if searchGranule < 0 {
		searchGranule = 0
	}
	lo, err := d.bisect(searchGranule)
	if err != nil {
		return 0, err
	}

	// Walk from lo, deriving each packet's absolute granule-end by anchoring
	// accumulated durations to a page granule at or beyond the target. The walk
	// is capped: a past-end target on an unknown-length stream (no clamp) would
	// otherwise scan every packet to EOF, and a granule-less span would grow the
	// arrays without bound (ADR-0005).
	for {
		d.mapping.resetTiming()
		d.restartAt(lo)
		var durs []int64
		var ends []int64 // page-granule anchor per packet, or -1
		anchored := false
		for len(durs) < maxSeekPackets {
			p, err := d.nextPacket()
			if err != nil {
				return 0, err
			}
			if p == nil {
				break
			}
			_, dur, _, ok := d.mapping.packetTiming(p, 0)
			if !ok {
				continue
			}
			durs = append(durs, dur)
			if d.endsPage() && d.page.granule >= 0 {
				ends = append(ends, d.page.granule)
				anchored = true
				if d.page.granule > granuleTarget {
					break // enough to bracket the target
				}
			} else {
				ends = append(ends, -1)
			}
		}
		if !anchored {
			if lo <= d.firstData {
				return 0, malformed("no granule anchor found while seeking")
			}
			lo = max(d.firstData, lo-seekWindow)
			continue
		}
		// landed (output timeline) = the first delivered packet's start. The
		// decoder consumes that packet as priming; its output begins here.
		starts := resolvePositions(durs, ends)
		landed := starts[0] - shift
		if landed < 0 {
			landed = 0
		}
		d.mapping.resetTiming()
		d.restartAt(lo)
		d.running = landed // re-anchor accumulated PTS to the landing
		return landed, nil
	}
}

// resolvePositions computes each packet's absolute start position from its
// duration and the page-granule anchors (ends[i] >= 0 marks packet i's end).
// Each anchor propagates forward across the packets that follow it (durations
// accumulate from the anchor, and a later anchor re-seeds the accumulation);
// the backward pass then fills only the packets that precede the first anchor.
// In a healthy stream the two directions agree, since durations telescope
// between consecutive anchors.
func resolvePositions(durs, ends []int64) []int64 {
	n := len(durs)
	start := make([]int64, n)
	end := make([]int64, n)
	for i := range end {
		end[i] = -1
	}
	// Seed ends from anchors.
	for i := 0; i < n; i++ {
		if ends[i] >= 0 {
			end[i] = ends[i]
		}
	}
	// Forward: end[i] = end[i-1] + dur[i] where the predecessor is known.
	for i := 1; i < n; i++ {
		if end[i] < 0 && end[i-1] >= 0 {
			end[i] = end[i-1] + durs[i]
		}
	}
	// Backward: end[i] = end[i+1] - dur[i+1].
	for i := n - 2; i >= 0; i-- {
		if end[i] < 0 && end[i+1] >= 0 {
			end[i] = end[i+1] - durs[i+1]
		}
	}
	for i := 0; i < n; i++ {
		if end[i] < 0 {
			end[i] = 0
		}
		start[i] = end[i] - durs[i]
	}
	return start
}

// bisect finds the byte offset of the last page of our serial whose granule is
// below target; the target's frames begin on or after it. A source read
// failure is surfaced, not swallowed, so a seek reports the error rather than
// silently landing early.
func (d *Demuxer) bisect(target int64) (int64, error) {
	lo, hi := d.firstData, d.totalSize
	for hi-lo > seekWindow {
		mid := lo + (hi-lo)/2
		off, granule, ok, err := d.granuleAt(mid, hi)
		if err != nil {
			return 0, err
		}
		if !ok || granule >= target {
			hi = mid
			continue
		}
		lo = off
	}
	return lo, nil
}

// lastGranule scans the file tail for the final granule position of our serial.
func (d *Demuxer) lastGranule() int64 {
	for _, window := range []int64{maxPageSize + 64<<10, 1 << 20, 4 << 20} {
		from := max(d.firstData, d.totalSize-window)
		granule := int64(-1)
		var p page
		for from < d.totalSize {
			at, ok, err := d.nextPageAt(from, &p)
			if err != nil || !ok {
				break
			}
			if p.serial == d.serial && p.granule >= 0 {
				granule = p.granule
			}
			from = at + p.size
		}
		if granule >= 0 || d.totalSize-window <= d.firstData {
			return granule
		}
	}
	return -1
}

// granuleAt finds the first page of our serial at or after off (bounded by
// limit) that completes a packet, and returns its offset and granule position.
func (d *Demuxer) granuleAt(off, limit int64) (int64, int64, bool, error) {
	var p page
	for off < limit {
		at, ok, err := d.nextPageAt(off, &p)
		if err != nil || !ok || at >= limit {
			return 0, 0, false, err
		}
		if p.serial == d.serial && p.granule >= 0 && p.flags&flagBOS == 0 {
			return at, p.granule, true, nil
		}
		off = at + p.size
	}
	return 0, 0, false, nil
}

// restartAt begins page-level streaming at a page boundary, dropping any packet
// continued onto that page.
func (d *Demuxer) restartAt(off int64) {
	d.off = off
	d.page = page{}
	d.seg, d.segOff = 0, 0
	d.partial = d.partial[:0]
	d.skipCont = true
	d.eos = false
	d.havePending = false
}
