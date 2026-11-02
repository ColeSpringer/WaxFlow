package ogg

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
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
	// damage counts the Damage warnings recorded, those the list's cap and
	// de-duplication keep out included.
	damage int
	// lostAt and lastPage are damage and the page offset when the last packet
	// was read, so a gap after it is known to follow damage (see Lost).
	lostAt   int
	lastPage int64
	// lookLimit, when set, ends a look ahead at that offset, and lookCut says
	// it was reached rather than the end of the stream.
	lookLimit int64
	lookCut   bool
	totalSize int64
	// initialGranule is where the first audio packet starts on the page
	// granule timeline (startGranule); a page granule less it is a raw
	// position.
	initialGranule int64
	// disowned is the offset of a first page whose granule the stream does
	// not back, so no seek or length reads it; -1 when there is none.
	disowned int64

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
	pendingLost int64 // the hole before it, the one a stream starts with
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

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("ogg: ", format, args...)
}

// unsupported names a well-formed stream this build does not cover. It is a
// different answer from malformed and carries a different code: the file is
// fine and we are not, which is a thing a caller can act on. See
// [waxerr.Malformed] for the rule.
func unsupported(format string, args ...any) error {
	return waxerr.Unsupported("ogg: ", format, args...)
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return malformed("%s (at offset %d)", msg, off)
	}
	d.damage++
	d.record(container.Warning{Offset: off, Msg: msg, Kind: container.Damage})
	return nil
}

// note records a Warning that Strict must not escalate: this build doing
// something with a well-formed file that a caller should know about, rather
// than damage in the file. See [container.Note].
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.record(container.Warning{Offset: off, Msg: fmt.Sprintf(format, args...), Kind: container.Note})
}

// lookAheadBytes bounds how far a check of the first page's granule reads.
const lookAheadBytes = 1 << 17

// maxWarnings caps the list, as the other tolerant demuxers in this tree do.
const maxWarnings = 64

// record lists w unless it is listed already or the list is full, so a read
// that crosses the same damage again after a seek records nothing new.
func (d *Demuxer) record(w container.Warning) {
	if len(d.warnings) < maxWarnings && !slices.Contains(d.warnings, w) {
		d.warnings = append(d.warnings, w)
	}
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
		if d.lookLimit > 0 && d.off >= d.lookLimit {
			d.lookCut = true
			return false, nil
		}
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
			// A chained stream is a well-formed Ogg carrying a second
			// logical bitstream this build plays none of; the file is not
			// damaged for having one, so Strict must not refuse it.
			d.note(p.off, "chained stream ignored")
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
		return unsupported("no supported stream (found: %s)", joinNames(found))
	}
	if len(found) > 0 {
		d.note(0, "ignoring %d additional stream(s): %s", len(found), joinNames(found))
	}

	// Consume header packets, then find the first audio packet.
	detect := d.headerPackets == 0
	d.off = off
	d.seg, d.segOff = 0, 0
	d.page = page{}
	first := firstPage{granule: -1}
	audioDamage, audioFrom := d.damage, d.off // what came before the audio
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
			audioDamage, audioFrom = d.damage, d.page.off+d.page.size
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
		if fm, ok := d.mapping.(*flacMapping); ok && fm.origin > 0 && d.damage > audioDamage &&
			fm.origin <= flac.MaxSamplesIn(d.pktPage-audioFrom) {
			// Damage took the frames before this one, so it is a hole from
			// 0, not a stream starting late.
			if werr := d.warn(audioFrom, "damage before the first frame took %d samples", fm.origin); werr != nil {
				return werr
			}
			pts += fm.origin
			d.pendingLost, fm.origin = fm.origin, 0
		}
		d.pending = append(d.pending[:0], pkt...)
		d.pendingPTS, d.pendingDur, d.pendingSync = pts, dur, sync
		d.havePending = true
		d.running = pts + dur
		d.firstData = d.pktPage
		first = d.firstPage()
		break
	}
	d.disowned = -1
	start, err := d.startGranule(first)
	if err != nil {
		return err
	}
	first.start = start
	d.initialGranule = start
	// Vorbis I A.2 trims a start before zero only in its layout: the first two
	// packets alone on the page. Otherwise nothing is trimmed, and granules
	// still count from where the stream starts.
	if start < 0 && d.mapping.codecID() == codec.Vorbis && first.packets != 2 {
		d.note(first.off, "the first audio page's granule %d starts before time zero without the layout Vorbis I A.2 requires; positions start at 0",
			first.granule)
		first.start = 0
	}

	// lastEnd is computed lazily, since a mapping may not need it at all. It
	// reads one tail window in the common case and widens only when that
	// window holds nothing to read the end off. Every mapping asks for it now:
	// the FLAC one verifies its declared total against it (mapflac.go), where
	// it used to skip the read whenever a total was declared.
	var (
		unconfirmed bool
		tailErr     error
	)
	lastEnd := func() int64 {
		if d.empty {
			return 0
		}
		t, err := d.readTail()
		if err != nil {
			tailErr = err
			return -1
		}
		if d.mapping.selfTiming() {
			// FLAC frames say where they are, within what the bytes can hold.
			if n := d.totalSize - d.firstData; t.frameEnd > flac.MaxSamplesIn(n) {
				tailErr = d.warn(d.firstData, "the frames claim %d samples, more than %d bytes of frames can hold", t.frameEnd, n)
				return -1
			}
			return t.frameEnd
		}
		if t.unstated >= 0 {
			// The granule read is not where the stream ends.
			unconfirmed = true
			tailErr = d.warn(t.unstated, "the page at offset %d completes a packet but states no granule, after the last page that does; the length is unconfirmed",
				t.unstated)
		}
		if d.disowned >= 0 && t.off == d.disowned {
			// A disowned first page that is also the last states no end
			// either: the stream is the packets it holds.
			return first.samples
		}
		return d.raw(t.granule)
	}
	track, err := d.mapping.finalizeTrack(first, lastEnd, d)
	if err == nil {
		err = tailErr
	}
	if err != nil {
		return err
	}
	if unconfirmed {
		// Packets follow the last granule stated, so a decode runs past it.
		track.SamplesExact, track.SamplesAdvisory = false, true
	}
	if d.empty {
		track.Samples = 0
	}
	d.track = track
	return nil
}

// firstPage reads where the stream starts off the page the first audio packet
// just completed on, which is in hand: its granule, and the samples of every
// packet completing on it. Timing the packets after the first moves the
// mapping's timing state, so the first is timed again to put it back.
func (d *Demuxer) firstPage() firstPage {
	p := firstPage{off: d.page.off, granule: d.page.granule, samples: d.pendingDur,
		packets: 1, eos: d.page.flags&flagEOS != 0}
	from, to := d.segOff, d.segOff
	for _, l := range d.page.lacing[d.seg:] {
		to += int(l)
		if l < 255 {
			if _, dur, _, ok := d.mapping.packetTiming(d.page.body[from:to], 0); ok {
				p.samples += dur
				p.packets++
			}
			from = to
		}
	}
	d.mapping.resetTiming()
	d.mapping.packetTiming(d.pending, 0)
	return p
}

// startGranule is the first page's granule less its samples, where the stream
// backs it: the next page's granule less this one's must be the samples
// completing on it, or less on a page ending the stream. A page with no later
// granule stands alone only if it ends the stream or holds all its samples.
// Otherwise, or when the look ahead met damage or its cap, the page is
// disowned with a note and positions start at 0, as ffmpeg drops a 0 on a
// first page holding audio. A source failing under the look ahead fails the
// open. Ogg FLAC takes none of this: its frames say where they are.
func (d *Demuxer) startGranule(first firstPage) (int64, error) {
	if first.granule < 0 || d.mapping.selfTiming() {
		return 0, nil
	}
	start := first.granule - first.samples
	if start == 0 {
		return 0, nil
	}
	if first.eos {
		if start < 0 {
			return 0, nil // the end trim
		}
		return start, nil
	}
	granule, samples, eos, damaged, found, err := d.nextGranulePage()
	if err != nil {
		return 0, err
	}
	switch {
	case damaged:
		d.note(first.off, "damage after the first audio page leaves its granule %d unconfirmed; positions start at 0",
			first.granule)
		d.disowned = first.off
		return 0, nil
	case found == streamEnds && start > 0:
		return start, nil
	case found == streamEnds:
		d.note(first.off, "the first audio page's granule %d is short of its %d samples but does not end the stream; positions start at 0",
			first.granule, first.samples)
		d.disowned = first.off
		return 0, nil
	case found == unverified:
		d.note(first.off, "no later page within reach states a granule to back the first audio page's %d; positions start at 0",
			first.granule)
		d.disowned = first.off
		return 0, nil
	}
	if diff := granule - first.granule; diff != samples && !(eos && diff >= 0 && diff <= samples) {
		d.note(first.off, "the first audio page's granule %d disagrees with the next page's %d; positions start at 0",
			first.granule, granule)
		d.disowned = first.off
		return 0, nil
	}
	return start, nil
}

// lookahead is how nextGranulePage's search ended.
type lookahead int

const (
	foundGranule lookahead = iota // a later page states a granule
	streamEnds                    // the stream ends before one does
	unverified                    // damage or the packet cap stopped the search
)

// nextGranulePage reads on to the next page of the serial that states a
// granule and returns that granule, the samples of the packets completing
// after the first page up to it, whether it ends the stream, and whether the
// search met tolerated damage on the way. It is a look ahead and leaves
// nothing behind: the cursor, the mapping's timing and the warnings are as
// they were, and strict mode is off throughout, since a read reports what
// this meets when it gets there. Only a source failure is an error, as at any
// other open-time read.
func (d *Demuxer) nextGranulePage() (granule, samples int64, eos, damaged bool, found lookahead, err error) {
	pg := d.page
	pg.lacing = append([]byte(nil), d.page.lacing...)
	pg.body = append([]byte(nil), d.page.body...)
	off, seg, segOff, pktPage, skipCont, ended := d.off, d.seg, d.segOff, d.pktPage, d.skipCont, d.eos
	partial := append([]byte(nil), d.partial...)
	strict, warned, damage := d.opts.Strict, len(d.warnings), d.damage
	d.opts.Strict, d.lookLimit, d.lookCut = false, pg.off+lookAheadBytes, false
	defer func() {
		d.page, d.off, d.seg, d.segOff, d.pktPage, d.skipCont, d.eos = pg, off, seg, segOff, pktPage, skipCont, ended
		d.partial = append(d.partial[:0], partial...)
		d.opts.Strict, d.warnings, d.damage = strict, d.warnings[:warned], damage
		d.lookLimit, d.lookCut = 0, false
		d.mapping.resetTiming()
		d.mapping.packetTiming(d.pending, 0)
	}()
	for range maxSeekPackets {
		p, err := d.nextPacket()
		switch {
		case err != nil && waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable:
			return 0, 0, false, false, unverified, err
		case err != nil:
			return 0, 0, false, true, unverified, nil
		case p == nil && d.lookCut:
			return 0, 0, false, false, unverified, nil
		case p == nil:
			return 0, 0, false, d.damagedSince(damage), streamEnds, nil
		}
		_, dur, _, timed := d.mapping.packetTiming(p, 0)
		if d.page.off == pg.off {
			continue // completes on the first page, whose granule is the one in question
		}
		if timed {
			samples += dur
		} else {
			damaged = true
		}
		if d.page.granule >= 0 && d.lastOfPage() {
			return d.page.granule, samples, d.page.flags&flagEOS != 0, damaged || d.damagedSince(damage), foundGranule, nil
		}
	}
	return 0, 0, false, false, unverified, nil
}

// damagedSince reports whether damage has been recorded past the first n
// warnings.
func (d *Demuxer) damagedSince(n int) bool { return d.damage > n }

// lastOfPage reports whether the packet nextPacket just returned is the last
// to complete on its page, so the page granule is where it ends.
func (d *Demuxer) lastOfPage() bool {
	for _, l := range d.page.lacing[d.seg:] {
		if l < 255 {
			return false
		}
	}
	return true
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

// DamageCount implements container.Damaged.
func (d *Demuxer) DamageCount() int { return d.damage }

// ReadPacket yields one codec packet. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	var (
		data     []byte
		pts, dur int64
		sync     bool
		lost     int64
	)
	if d.havePending {
		data, pts, dur, sync, lost = d.pending, d.pendingPTS, d.pendingDur, d.pendingSync, d.pendingLost
		d.havePending, d.pendingLost = false, 0
		d.lostAt, d.lastPage = d.damage, d.page.off
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
			if d.mapping.selfTiming() && pts > d.running {
				// FLAC frames say where they are, so a gap behind one is
				// samples lost: to damage, or to frames gone without their
				// numbers.
				if lost, err = d.gap(pts); err != nil {
					return err
				}
			}
			d.running = pts + dur
			d.lostAt, d.lastPage = d.damage, d.page.off
			break
		}
	}
	*pkt = container.Packet{
		Track: 0,
		Lost:  lost,
		Packet: codec.Packet{
			Data:    data,
			PTS:     pts,
			Dur:     dur,
			Sync:    sync,
			Discont: lost > 0,
		},
	}
	return nil
}

// gap is what a self-timing packet at pts, past the running position, lost
// before it. After damage it is what a lost page took, within what its bytes
// could hold. With nothing damaged the numbers skip, and the samples are lost
// where the stream could have the packet, inside its exact length or short of
// one inside what its bytes can hold; numbers past that vouch for nothing, so
// the packet is joined on. Either way it warns.
func (d *Demuxer) gap(pts int64) (int64, error) {
	if d.damage > d.lostAt {
		if pts-d.running <= flac.MaxSamplesIn(d.page.off-d.lastPage) {
			return pts - d.running, nil
		}
		return 0, nil
	}
	vouched := pts <= flac.MaxSamplesIn(d.totalSize-d.firstData)
	if t := d.track; t.SamplesExact && t.Samples >= 0 {
		vouched = pts <= t.Samples
	}
	if vouched {
		return pts - d.running, d.warn(d.page.off, "the frames skip from sample %d to %d: %d samples lost",
			d.running, pts, pts-d.running)
	}
	return 0, d.warn(d.page.off, "the frames skip from sample %d to %d, past what the stream holds; joined", d.running, pts)
}

// SeekSample lands on a sync point at or before target and returns the landed
// position, both on the raw timeline. Self-timing mappings (FLAC) walk frames
// reading their own positions; accumulating mappings (Vorbis, Opus) anchor to
// page granules and land a mapping-defined pre-roll before the target.
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
	// sample is raw, so the end it is held to is the front trim plus the
	// length.
	if end := d.track.Delay + d.track.Samples; d.track.Samples > 0 && sample >= end {
		sample = end - 1
	}
	if d.mapping.selfTiming() {
		return d.seekSelfTiming(sample)
	}
	return d.seekAccumulating(sample)
}

// seekSelfTiming seeks a mapping whose packets carry their own positions
// (FLAC): bisect on the frames that begin on each page, then walk packets to
// the frame containing target. No granule plays a part, so none can cost a
// seek.
func (d *Demuxer) seekSelfTiming(sample int64) (int64, error) {
	lo, err := d.bisectFrames(sample)
	if err != nil {
		return 0, err
	}
	var (
		lastData         []byte
		lastPTS, lastDur int64
		prevPTS          int64 // the frame before the last, -1 for none
		lastSync         bool
		haveLast         bool
	)
	for {
		d.restartAt(lo)
		prevPTS, haveLast = -1, false
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
			if haveLast {
				prevPTS = lastPTS
			}
			lastData = append(lastData[:0], p...)
			lastPTS, lastDur, lastSync = pts, dur, sync
			haveLast = true
			if pts+dur > sample {
				break
			}
		}
		if haveLast && lastPTS > sample && lo > d.firstData && prevPTS < 0 {
			haveLast = false // the target is in a hole: back off for a frame before it
		}
		if haveLast && lastPTS > sample && prevPTS >= 0 {
			// The target is in a hole before this frame, so land on the one
			// before it, at or before the target; the read fills the hole.
			if lastData, lastDur, lastSync, err = d.walkToFrame(lo, prevPTS, lastData); err != nil {
				return 0, err
			}
			lastPTS = prevPTS
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
	d.pendingPTS, d.pendingDur, d.pendingSync, d.pendingLost = lastPTS, lastDur, lastSync, 0
	d.havePending = true
	d.running = lastPTS + lastDur
	if lastPTS > sample && prevPTS < 0 {
		// In the hole the stream starts with: land at 0, the read owing it.
		d.pendingLost = lastPTS
		return 0, nil
	}
	return lastPTS, nil
}

// walkToFrame walks from lo to the frame that starts at pts and leaves the read
// just past it, returning it in buf.
func (d *Demuxer) walkToFrame(lo, pts int64, buf []byte) ([]byte, int64, bool, error) {
	d.restartAt(lo)
	for {
		p, err := d.nextPacket()
		if err != nil {
			return nil, 0, false, err
		}
		if p == nil {
			return nil, 0, false, malformed("the frame at %d was not found walking to it again", pts)
		}
		if at, dur, sync, ok := d.mapping.packetTiming(p, 0); ok && at == pts {
			return append(buf[:0], p...), dur, sync, nil
		}
	}
}

// seekAccumulating seeks a mapping whose positions accumulate (Vorbis, Opus).
// It works on the raw timeline, the page granules less the initial granule,
// where page positions are authoritative and which for both mappings is the
// decoder's output timeline. It lands a pre-roll before the target so decode
// reconverges: the restart page's first packet is consumed (priming), and
// output begins at its start.
func (d *Demuxer) seekAccumulating(sample int64) (int64, error) {
	searchGranule := sample - d.mapping.preroll()
	if searchGranule < 0 {
		searchGranule = 0
	}
	lo, err := d.bisect(searchGranule)
	if err != nil {
		return 0, err
	}

	// Walk from lo, deriving each packet's raw end by anchoring accumulated
	// durations to a page granule at or beyond the target. The walk is capped:
	// a past-end target on an unknown-length stream (no clamp) would otherwise
	// scan every packet to EOF, and a granule-less span would grow the arrays
	// without bound (ADR-0005).
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
			end := int64(-1)
			if d.endsPage() && d.page.off != d.disowned {
				end = d.raw(d.page.granule)
			}
			ends = append(ends, end)
			if end >= 0 {
				anchored = true
				if end > sample {
					break // enough to bracket the target
				}
			}
		}
		if !anchored && lo > d.firstData {
			lo = max(d.firstData, lo-seekWindow)
			continue
		}
		// landed (output timeline) = the first delivered packet's start. The
		// decoder consumes that packet as priming; its output begins here.
		// The clamp is for a start that resolvePositions could not anchor:
		// an unanchored end[0] falls back to 0, and Opus (unlike Vorbis, whose
		// resetTiming zeroes durs[0]) carries a real duration there, so
		// starts[0] can come out negative. A walk from the first audio page
		// starts at the first audio packet, raw 0 by definition, whatever
		// granules the stream gets wrong after it, and needs none to anchor.
		landed := int64(0)
		if lo > d.firstData {
			landed = max(resolvePositions(durs, ends)[0], 0)
		}
		d.mapping.resetTiming()
		d.restartAt(lo)
		d.running = landed // re-anchor accumulated PTS to the landing
		return landed, nil
	}
}

// resolvePositions computes each packet's start position from its duration and
// the page-granule anchors (ends[i] >= 0 marks packet i's end). Each anchor
// propagates forward across the packets that follow it (durations accumulate
// from the anchor, and a later anchor re-seeds the accumulation); the backward
// pass then fills only the packets that precede the first anchor. In a healthy
// stream the two directions agree, since durations telescope between
// consecutive anchors.
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

// bisectFrames finds the byte offset of a page of our serial on which a frame
// at or before target begins, a raw position, late enough that the target's
// frame lies within a bisection window after it. A source read failure is
// surfaced, as in bisect.
func (d *Demuxer) bisectFrames(target int64) (int64, error) {
	return d.narrow(func(mid, hi int64) (int64, bool, error) {
		off, pts, ok, err := d.frameAt(mid, hi)
		return off, err == nil && ok && pts <= target, err
	})
}

// narrow bisects the stream's bytes to within a bisection window after the
// last probe that lands before the target: probe reports the page it found at
// or after mid, below hi, and whether what that page states is before it.
func (d *Demuxer) narrow(probe func(mid, hi int64) (off int64, before bool, err error)) (int64, error) {
	lo, hi := d.firstData, d.totalSize
	for hi-lo > seekWindow {
		mid := lo + (hi-lo)/2
		off, before, err := probe(mid, hi)
		if err != nil {
			return 0, err
		}
		if !before {
			hi = mid
			continue
		}
		lo = off
	}
	return lo, nil
}

// frameAt finds the first page of our serial at or after off (bounded by
// limit) on which a frame begins whose header reads, and returns the page's
// offset and the frame's raw position. Pages are followed by their own
// lengths, as in readTail, so a frame larger than a page costs its pages and
// not a resync window each.
func (d *Demuxer) frameAt(off, limit int64) (int64, int64, bool, error) {
	var at, pts int64
	found := false
	err := d.walkPages(off, limit, func(o int64, p *page, _ bool) bool {
		if p.serial == d.serial && p.flags&flagBOS == 0 {
			if pts, found = d.firstFrameOn(p); found {
				at = o
			}
		}
		return !found
	})
	if err != nil {
		return 0, 0, false, err
	}
	return at, pts, found, nil
}

// walkPages visits the pages from off to limit, following each one's own
// length and resyncing past damage, which visit is told of. It stops at limit,
// when visit returns false, or on an error: a source failure, or a resync
// that found no page, returned as they are.
func (d *Demuxer) walkPages(off, limit int64, visit func(at int64, p *page, resynced bool) bool) error {
	var p page
	synced := false
	for off < limit {
		at, resynced := off, false
		if synced {
			ok, err := d.readPage(off, &p)
			if waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable {
				return err
			}
			synced = err == nil && ok
		}
		if !synced {
			var ok bool
			var err error
			if at, ok, err = d.nextPageAt(off, &p); err != nil || !ok {
				return err
			}
			synced, resynced = true, true
		}
		if at >= limit || !visit(at, &p, resynced) {
			return nil
		}
		off = at + p.size
	}
	return nil
}

// bisect finds the byte offset of the last page of our serial whose granule is
// below target, a raw position; the target's frames begin on or after it. A
// source read failure is surfaced, not swallowed, so a seek reports the error
// rather than silently landing early.
func (d *Demuxer) bisect(target int64) (int64, error) {
	return d.narrow(func(mid, hi int64) (int64, bool, error) {
		off, granule, ok, err := d.granuleAt(mid, hi)
		// A page before the stream's own start places nothing, so the search
		// looks earlier, as it does for a span with no page at all.
		raw := d.raw(granule)
		return off, err == nil && ok && raw >= 0 && raw < target, err
	})
}

// tail is what the open reads off the end of the file.
type tail struct {
	// off and granule are the last page of the serial stating a granule.
	off, granule int64
	// frameEnd is where the last whole packet read there ends on the raw
	// timeline, from its own header, for a mapping whose packets say where
	// they are (FLAC).
	frameEnd int64
	// unstated is the first page after that one that completes a packet
	// without stating a granule.
	unstated int64
}

// readTail scans the file tail for what says where the stream ends, -1 for
// whatever it does not find.
//
// Pages are walked by following each one's own length, which is what keeps the
// scan proportional to the window rather than to the pages in it: nextPageAt
// reads a 64 KiB resync buffer per call, so asking it for every page of a
// densely paged stream re-reads that window once per page. The first page in
// the window still costs a resync, since the window starts mid-page; from there
// the next page begins exactly where the last one ended, and only a stream with
// damage or padding between pages falls back to scanning again.
func (d *Demuxer) readTail() (tail, error) {
	frames := d.mapping.selfTiming()
	for _, window := range []int64{maxPageSize + 64<<10, 1 << 20, 4 << 20} {
		from := max(d.firstData, d.totalSize-window)
		t := tail{off: -1, granule: -1, frameEnd: -1, unstated: -1}
		var (
			hdr  []byte // the head of the packet in progress
			open bool   // a packet is in progress and its head was read
		)
		err := d.walkPages(from, d.totalSize, func(at int64, p *page, resynced bool) bool {
			if resynced {
				open = false // whatever was in progress is lost
			}
			if p.serial == d.serial {
				switch {
				case p.granule >= 0:
					t.off, t.granule, t.unstated = at, p.granule, -1
				case t.granule >= 0 && t.unstated < 0 && completes(p):
					t.unstated = at
				}
				if frames {
					open = d.framesOn(p, &hdr, open, &t.frameEnd)
				}
			}
			return true
		})
		if waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable {
			return tail{}, err
		}
		done := t.granule >= 0
		if frames {
			done = t.frameEnd >= 0
		}
		if done || d.totalSize-window <= d.firstData {
			return t, nil
		}
	}
	return tail{off: -1, granule: -1, frameEnd: -1, unstated: -1}, nil
}

// completes reports whether a packet completes on p.
func completes(p *page) bool {
	for _, l := range p.lacing {
		if l < 255 {
			return true
		}
	}
	return false
}

// framesOn carries the tail scan's packet reassembly across p, a page of the
// serial, keeping only each packet's head, and sets end to where each whole
// packet ends by its header. It returns whether a packet is left in progress.
func (d *Demuxer) framesOn(p *page, hdr *[]byte, open bool, end *int64) bool {
	fresh := p.flags&flagContinued == 0 // a packet begins at the first segment
	if fresh {
		open = false // one left unfinished is dropped, as a read drops it
	}
	at := 0
	for _, l := range p.lacing {
		if fresh {
			*hdr, open, fresh = (*hdr)[:0], true, false
		}
		if open && len(*hdr) < flac.MaxFrameHeaderLen {
			*hdr = append(*hdr, p.body[at:at+min(int(l), flac.MaxFrameHeaderLen-len(*hdr))]...)
		}
		at += int(l)
		if l < 255 {
			if open {
				if pts, dur, _, ok := d.mapping.packetTiming(*hdr, 0); ok {
					*end = pts + dur
				}
			}
			open, fresh = false, true
		}
	}
	return open
}

// firstFrameOn is the raw position of the first frame that begins on p, off
// its own header, when one does and its header reads.
func (d *Demuxer) firstFrameOn(p *page) (int64, bool) {
	at, i := 0, 0
	if p.flags&flagContinued != 0 {
		for i < len(p.lacing) { // the tail of a frame begun on an earlier page
			l := p.lacing[i]
			at += int(l)
			i++
			if l < 255 {
				break
			}
		}
	}
	if i == len(p.lacing) {
		return 0, false
	}
	pts, _, _, ok := d.mapping.packetTiming(p.body[at:], 0)
	return pts, ok
}

// raw maps a page granule onto the raw packet timeline: the granule less the
// initial granule position, saturating at the top of the range. A page with no
// granule, or one before the stream's own start, maps to -1.
func (d *Demuxer) raw(granule int64) int64 {
	switch {
	case granule < 0 || granule < d.initialGranule:
		return -1
	case d.initialGranule < 0 && granule > math.MaxInt64+d.initialGranule:
		return math.MaxInt64
	}
	return granule - d.initialGranule
}

// granuleAt finds the first page of our serial at or after off (bounded by
// limit) that completes a packet, and returns its offset and granule position.
// A disowned first page places nothing, so it is passed over, which matters
// where the first packet begins pages before it.
func (d *Demuxer) granuleAt(off, limit int64) (int64, int64, bool, error) {
	var p page
	for off < limit {
		at, ok, err := d.nextPageAt(off, &p)
		if err != nil || !ok || at >= limit {
			return 0, 0, false, err
		}
		if p.serial == d.serial && p.granule >= 0 && p.flags&flagBOS == 0 && at != d.disowned {
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
