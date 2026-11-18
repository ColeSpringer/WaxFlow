package ogg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/diag"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer  = (*Demuxer)(nil)
	_ container.Seeker   = (*Demuxer)(nil)
	_ container.Warner   = (*Demuxer)(nil)
	_ container.Measurer = (*Demuxer)(nil)
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

// DemuxerVersion is this demuxer's term in the ADR-0004 cache key, carried
// by every Ogg track. Bump it for a change that delivers other samples; the
// ADR says what revision 2 is.
const DemuxerVersion = "ogg-demux-2"

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

// Demuxer reads the first supported logical stream from an Ogg source. The
// codec-specific work is delegated to a mapping; this type owns page framing,
// packet reassembly, resync, and granule bisection.
type Demuxer struct {
	src container.Source

	mapping mapping
	serial  uint32
	track   container.Track
	log     diag.Log
	// truncated is how far the headers' total runs past the pages
	// (container.Truncated).
	truncated int64

	// headerPackets is the count of header packets after the identification
	// packet, or 0 in detect mode (find audio by the mapping's isAudio).
	headerPackets int

	firstData int64 // offset of the page where the first audio packet begins
	// lostAt and lastPage are the damage count and the page offset when the last packet
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
	win      window // the bytes every page read and resync take from
	page     page   // current page of our serial
	seg      int    // next segment index in page
	segOff   int    // byte offset of that segment within the page body
	partial  []byte // packet reassembly across pages
	pktPage  int64  // offset of the page the current packet began on
	skipCont bool   // drop a continued packet head (after seek/resync)
	eos      bool

	running int64 // accumulated output position (accumulating mappings)

	// stated is the length the open gave, which a seek is held to whatever a
	// read settles, and exact says it called a FLAC track's exact, which a
	// hole is held to.
	stated int64
	exact  bool
	// count is what a read from the first audio packet has delivered,
	// durations and what holes took, while whole says the read counts from
	// there.
	count int64
	whole bool
	// landed says the next packet read is the first behind a FLAC seek's
	// landing frame.
	landed bool
	// opened says parse is done: damage out of a resync's reach then ends
	// the stream, where an open refuses it.
	opened bool
	// measured is what MeasureSamples counted, once it has: the source does
	// not change under an open, so neither does the answer.
	measured     int64
	haveMeasured bool

	pending     []byte // one-packet pushback (first frame, seek landing)
	pendingPTS  int64
	pendingDur  int64
	pendingSync bool
	pendingLost int64 // the hole before it, the one a stream starts with
	headJoined  bool  // the first frame is a stray's: no such hole
	havePending bool
	empty       bool // stream carries zero audio packets

	// strayFrom is the raw position of a stray first frame, which is placed
	// at 0 with the frames numbered on from it behind; strayNext is the raw
	// position due next in that run, -1 off it, and strayEnd the page its
	// run ends on, -1 until a seek asks.
	strayFrom, strayNext, strayEnd int64
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
func NewDemuxer(src container.Source, opts *container.DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, totalSize: src.Size(), win: newWindow(src), log: diag.New("ogg: ", opts), whole: true, strayNext: -1, strayEnd: -1}
	if err := d.parse(); err != nil {
		return nil, err
	}
	d.opened = true
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
	return d.log.Warn(off, format, args...)
}

// note records a Warning that Strict must not escalate: this build doing
// something with a well-formed file that a caller should know about, rather
// than damage in the file. See [container.Note].
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.log.Note(off, format, args...)
}

// lookAheadBytes bounds how far a check of the first page's granule reads.
const lookAheadBytes = 1 << 17

// pageFault is why pageAt found no page.
type pageFault int

const (
	pageOK        pageFault = iota
	pagePastEnd             // no room for a header
	pageNoCapture           // no capture pattern
	pageVersion             // an unknown structure version
	pageLacing              // the lacing table runs past the end of file
	pageBody                // the body runs past the end of file
)

// pageAt makes the window hold the page at off, taking its header, lacing
// table and body each as its own need, and returns its bytes. With no page
// there, it returns why, and the header where one was read.
func (d *Demuxer) pageAt(off int64) ([]byte, pageFault, error) {
	if off+headerLen > d.totalSize {
		return nil, pagePastEnd, nil
	}
	hdr, err := d.win.need(off, off+headerLen, off, "ogg: reading page header")
	switch {
	case err != nil:
		return nil, pageOK, err
	case !Match(hdr):
		return hdr, pageNoCapture, nil
	case hdr[4] != 0:
		return hdr, pageVersion, nil
	}
	lacingEnd := off + headerLen + int64(hdr[26])
	if lacingEnd > d.totalSize {
		return hdr, pageLacing, nil
	}
	lacing, err := d.win.need(off+headerLen, lacingEnd, off, "ogg: reading lacing table")
	if err != nil {
		return nil, pageOK, err
	}
	end := lacingEnd
	for _, l := range lacing {
		end += int64(l)
	}
	if end > d.totalSize {
		return nil, pageBody, nil
	}
	if _, err := d.win.need(lacingEnd, end, off, "ogg: reading page body"); err != nil {
		return nil, pageOK, err
	}
	return d.win.view(off, end), pageOK, nil
}

// takePage parses the page pg, found at off, into p, reusing p's buffers.
func takePage(p *page, off int64, pg []byte) {
	bodyAt := headerLen + int(pg[26])
	p.off = off
	p.flags = pg[5]
	p.granule = int64(binary.LittleEndian.Uint64(pg[6:]))
	p.serial = binary.LittleEndian.Uint32(pg[14:])
	p.size = int64(len(pg))
	p.lacing = append(p.lacing[:0], pg[headerLen:bodyAt]...)
	p.body = append(p.body[:0], pg[bodyAt:]...)
}

// readPage reads and CRC-checks the page at off. It does not resync; callers
// own that policy. A nil page with nil error means end of file.
func (d *Demuxer) readPage(off int64, p *page) (bool, error) {
	pg, fault, err := d.pageAt(off)
	if err != nil {
		return false, err
	}
	switch fault {
	case pagePastEnd:
		return false, nil
	case pageNoCapture:
		return false, malformed("no page capture pattern at offset %d", d.log.At(off))
	case pageVersion:
		return false, malformed("unknown page structure version %d", pg[4])
	case pageLacing:
		return false, malformed("page lacing table extends past end of file")
	case pageBody:
		return false, malformed("page body extends past end of file")
	}
	crc := d.win.sum(0, pg[:22])
	crc = d.win.sum(crc, []byte{0, 0, 0, 0})
	crc = d.win.sum(crc, pg[26:])
	if want := binary.LittleEndian.Uint32(pg[22:]); crc != want {
		return false, malformed("page checksum mismatch at offset %d", d.log.At(off))
	}
	takePage(p, off, pg)
	return true, nil
}

// nextPageAt returns the first page at or after off whose structure fits and
// whose checksum verifies, scanning a step at a time until a step would start
// more than maxResync bytes on; only the page taken is written to p.
func (d *Demuxer) nextPageAt(off int64, p *page) (int64, bool, error) {
	base := off
	for off < d.totalSize {
		if off-base > maxResync {
			return 0, false, malformed("no page capture pattern within %d bytes", int64(maxResync))
		}
		n := min(scanStep, d.totalSize-off)
		if n < 4 {
			return 0, false, nil
		}
		if _, err := d.win.need(off, off+n, off, "ogg: scanning for page"); err != nil {
			return 0, false, err
		}
		for at := off; ; {
			i := bytes.Index(d.win.view(at, off+n), []byte("OggS"))
			if i < 0 {
				break
			}
			cand := at + int64(i)
			at = cand + 1
			pg, fault, err := d.pageAt(cand)
			if err != nil {
				return 0, false, err
			}
			if fault == pageOK && d.win.pageSum(cand, int64(len(pg))) == binary.LittleEndian.Uint32(pg[22:]) {
				takePage(p, cand, pg)
				return cand, true, nil
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
			if d.opened && waxerr.CodeOf(err) == waxerr.CodeMalformedInput {
				// Out of a resync's reach: a read ends here, the rest dropped.
				// An open has no stream yet to end, and refuses.
				return false, d.warn(d.off, "no page within %d bytes behind the damage, the %d bytes left dropped",
					int64(maxResync), d.totalSize-d.off)
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
		return unsupported("no supported stream (found: %s)", strings.Join(found, ", "))
	}
	if len(found) > 0 {
		d.note(0, "ignoring %d additional stream(s): %s", len(found), strings.Join(found, ", "))
	}

	// Consume header packets, then find the first audio packet.
	detect := d.headerPackets == 0
	d.off = off
	d.seg, d.segOff = 0, 0
	d.page = page{}
	first := firstPage{granule: -1}
	audioDamage, audioFrom := d.log.DamageCount(), d.off // what came before the audio
	holeFrom := int64(-1)                                // where a hole from sample 0 begins
	var mark container.WarningsMark                      // the findings in front of that hole's
	late := false                                        // damage lies in front of a first frame taken for the stream's start
	pair := false                                        // the first frame lies in front of the one the stream starts at
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
			audioDamage, audioFrom = d.log.DamageCount(), d.page.off+d.page.size
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
		// Copied now: a look ahead reuses the page the packet lies in.
		d.pending = append(d.pending[:0], pkt...)
		if fm, ok := d.mapping.(*flacMapping); ok && fm.unsettled() {
			switch next, followed, closing := d.frameAhead(); {
			case followed:
				fm.num = fm.si.NumberingBy(fm.first, next)
			case closing:
				fm.num = fm.si.NumberingLast(fm.first)
			}
			fm.origin = fm.num.Start(fm.first)
		}
		if fm, ok := d.mapping.(*flacMapping); ok && fm.origin > 0 && d.log.DamageCount() > audioDamage &&
			fm.origin <= flac.MaxSamplesIn(d.pktPage-audioFrom) {
			// Damage took the frames before this one, so it is a hole from
			// 0, not a stream starting late.
			mark = d.log.Mark()
			pts += fm.origin
			d.pendingLost, fm.origin, holeFrom = fm.origin, 0, audioFrom
			took := pts
			if at, due := d.dueBefore(0, pts); due {
				took = at // the frame behind this one is read in front of it
			}
			if took > 0 {
				if werr := d.warn(audioFrom, "damage before the first frame took %d samples", took); werr != nil {
					return werr
				}
			}
		} else if ok && fm.origin > 0 {
			late = d.log.DamageCount() > audioDamage
			if late && d.lowerAhead(fm) {
				fm.before = true // a stray, however long the run behind it
			}
			if at, due := d.dueBefore(math.MinInt64, pts); due && !late {
				// The frame due right in front of the first lies behind
				// it: the stream starts at that one, which is read first.
				fm.origin += at
				pts -= at
				d.pendingLost, pair = pts, true
			}
		}
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
			// FLAC frames say where they are, within what the bytes can hold,
			// those a hole at the head took among them.
			from := d.firstData
			if holeFrom >= 0 {
				from = holeFrom
			}
			if n := d.totalSize - from; t.frameEnd > flac.MaxSamplesIn(n) {
				tailErr = d.warn(d.firstData, "the frames claim %d samples, more than %d bytes of frames can hold", t.frameEnd, n)
				return -1
			}
			return t.frameEnd
		}
		if t.unstated >= 0 {
			// The granule read is not where the stream ends.
			unconfirmed = true
			tailErr = d.warn(t.unstated, "the page at offset %d completes a packet but states no granule, after the last page that does; the length is unconfirmed",
				d.log.At(t.unstated))
		}
		if d.disowned >= 0 && t.off == d.disowned {
			// A disowned first page that is also the last states no end
			// either: the stream is the packets it holds.
			return first.samples
		}
		return d.raw(t.granule)
	}
	settled := d.log.Mark()
	track, err := d.mapping.finalizeTrack(first, lastEnd, d)
	if err == nil {
		err = tailErr
	}
	fm, _ := d.mapping.(*flacMapping)
	stray := false
	switch {
	case err != nil || fm == nil:
	case d.pendingLost > 0 && !pair && track.SamplesExact && d.pendingLost >= track.Samples:
		// The first frame is numbered at or past where the closing frame
		// ends, so no hole from 0 holds it.
		d.log.Rollback(mark)
		stray = true
	case late && fm.before:
		// Behind damage, and numbered above frames that follow it: the
		// stream does not start at it, and those frames are its own.
		d.log.Rollback(settled)
		d.pendingPTS, fm.origin, holeFrom = d.pendingPTS+fm.origin, 0, audioFrom
		stray = true
	}
	if stray {
		// A stray first frame is joined on, nothing owed, and placed at 0
		// as a stream starting late is, the frames numbered on from it
		// behind it.
		d.strayFrom, d.strayNext = d.pendingPTS, d.pendingPTS+d.pendingDur
		d.pendingPTS, d.running = 0, d.pendingDur
		d.pendingLost, d.headJoined, fm.before, tailErr = 0, true, false, nil
		if err = d.warn(audioFrom, "damage before the first frame; the frame behind it is numbered past the frames that follow, joined"); err == nil {
			track, err = d.mapping.finalizeTrack(first, lastEnd, d)
		}
		if err == nil {
			err = tailErr
		}
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
	d.track, d.stated = track, track.Samples
	d.exact = d.mapping.selfTiming() && track.SamplesExact
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
	first, damage := d.page.off, d.log.DamageCount()
	d.lookAhead(lookAheadBytes, func() { granule, samples, eos, damaged, found, err = d.granuleAhead(first, damage) })
	d.mapping.resetTiming()
	d.mapping.packetTiming(d.pending, 0)
	return granule, samples, eos, damaged, found, err
}

// hold is the demuxer as it stands, every field of it, for restore: the
// slices a read rewrites in place are copied.
func (d *Demuxer) hold() Demuxer {
	was := *d
	was.page.lacing, was.page.body = slices.Clone(d.page.lacing), slices.Clone(d.page.body)
	was.partial, was.pending = slices.Clone(d.partial), slices.Clone(d.pending)
	return was
}

// restore puts back what hold took. The window and the log are the
// demuxer's as they stand: bytes read stay read, and findings are the
// caller's to keep or drop.
func (d *Demuxer) restore(was Demuxer) {
	was.win, was.log = d.win, d.log
	*d = was
}

// lookAhead runs fn on the read from where it stands, short of limit bytes
// past the current page, and leaves the demuxer as it was (hold) with
// nothing listed. It reports whether fn met no damage. The mapping's own
// state is the mapping's to reset.
func (d *Demuxer) lookAhead(limit int64, fn func()) (clean bool) {
	was := d.hold()
	d.lookLimit, d.lookCut = d.page.off+limit, false
	clean, _ = d.log.Probe(fn)
	d.restore(was)
	return clean
}

// frameAhead is what follows the first audio packet: the header of the FLAC
// frame behind it, or the stream's end with no damage met, closing. It is
// read as nextGranulePage reads ahead, and a read that fails is no answer.
func (d *Demuxer) frameAhead() (next flac.FrameInfo, followed, closing bool) {
	var ended bool
	clean := d.lookAhead(lookAheadBytes, func() {
		p, err := d.nextPacket()
		switch {
		case err != nil:
		case p == nil:
			ended = !d.lookCut
		default:
			var perr error
			next, perr = flac.ParseFrameHeader(p)
			followed = perr == nil
		}
	})
	return next, followed, ended && clean
}

// lowerAhead reports whether the frames numbered on from the first audio
// packet end at one numbered before it, looking as frameAhead does and as
// far as a resync reaches: the first frame is then a stray's, not where
// the stream starts.
func (d *Demuxer) lowerAhead(fm *flacMapping) (lower bool) {
	d.lookAhead(maxResync, func() {
		for due := fm.origin + int64(fm.first.BlockSize); ; {
			p, err := d.nextPacket()
			if err != nil || p == nil {
				return
			}
			fi, err := flac.ParseFrameHeader(p)
			if err != nil {
				continue
			}
			at := fm.num.Start(fi)
			if at != due {
				lower = at < fm.origin
				return
			}
			due = at + int64(fi.BlockSize)
		}
	})
	return lower
}

// granuleAhead is nextGranulePage's search from the first page at first,
// damage being the count of damage met before it.
func (d *Demuxer) granuleAhead(first int64, damage int) (granule, samples int64, eos, damaged bool, found lookahead, err error) {
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
		if d.page.off == first {
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
func (d *Demuxer) damagedSince(n int) bool { return d.log.DamageCount() > n }

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

// Tracks returns the single decoded track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.log.List() }

// DamageCount implements container.Damaged.
func (d *Demuxer) DamageCount() int { return d.log.DamageCount() }

// TruncatedSamples implements container.Truncated. Only a mapping whose
// headers declare a total has one to fall short of: FLAC's STREAMINFO.
func (d *Demuxer) TruncatedSamples(int) int64 { return d.truncated }

func (d *Demuxer) truncatedBy(samples int64) { d.truncated = samples }

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
		if lost > 0 {
			// The frame due in front of the first may lie behind it: it is
			// read first, what is left of the hole owed, and this one next.
			if err := d.headPair(&data, &pts, &dur, &sync, &lost); err != nil {
				return err
			}
		}
		d.lostAt, d.lastPage = d.log.DamageCount(), d.page.off
	} else {
		for {
			p, err := d.nextPacket()
			if err != nil {
				return err
			}
			if p == nil {
				if err := d.settleEnd(); err != nil {
					return err
				}
				return io.EOF
			}
			var ok bool
			pts, dur, sync, ok = d.timing(p, d.running)
			if !ok {
				if werr := d.warn(d.page.off, "packet is not a valid %s packet, dropped", d.mapping.codecID()); werr != nil {
					return werr
				}
				continue
			}
			if d.landed {
				// The frame due before a seek's landing, lying behind it,
				// is behind the seek.
				if d.landed = false; pts+dur == d.pendingPTS {
					if werr := d.warn(d.page.off, "the frame at sample %d lies behind the one at %d; read in order", pts, d.pendingPTS); werr != nil {
						return werr
					}
					continue
				}
			}
			data = p
			if d.mapping.selfTiming() && pts > d.running {
				// FLAC frames say where they are, so a gap behind one is
				// samples lost: to damage, or to frames gone without their
				// numbers.
				// With the frame due in front of this one next, the two are
				// out of order: that one is read first, what the hole in
				// front of it took owed, and this one behind it.
				d.pending = append(d.pending[:0], p...)
				data = d.pending
				if at, due := d.dueBefore(d.running, pts); due {
					q, qpts, qdur, qsync, err := d.frameDue(pts, at)
					if err != nil {
						return err
					}
					if q != nil {
						d.pendingPTS, d.pendingDur, d.pendingSync, d.pendingLost, d.havePending = pts, dur, sync, 0, true
						if qpts > d.running {
							if lost, err = d.gap(qpts); err != nil {
								return err
							}
						}
						if werr := d.warn(d.page.off, "the frame at sample %d lies before the one at %d; read in order", pts, qpts); werr != nil {
							return werr
						}
						d.running = pts + dur
						data, pts, dur, sync = q, qpts, qdur, qsync
						d.lostAt, d.lastPage = d.log.DamageCount(), d.page.off
						break
					}
				}
				if lost, err = d.gap(pts); err != nil {
					return err
				}
			}
			d.running = pts + dur
			d.lostAt, d.lastPage = d.log.DamageCount(), d.page.off
			break
		}
	}
	d.count += lost + dur
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

// settleEnd holds a FLAC track's exact length against where a whole read
// ended. Off it, the read says so and the length is no longer exact; short
// of it the track takes what is there and counts the rest truncated.
func (d *Demuxer) settleEnd() error {
	t := d.track
	fm, ok := d.mapping.(*flacMapping)
	if !ok || !d.whole || !t.SamplesExact || d.count == t.Samples {
		return nil
	}
	// Short of it the frames are damaged; past it they are only more than
	// the open saw, which flac.Settle notes too.
	if msg := flac.ReadOff(fm.si.Samples, t.Samples, d.count); d.count > t.Samples {
		d.note(d.firstData, "%s", msg)
	} else if err := d.warn(d.firstData, "%s", msg); err != nil {
		return err
	}
	tr, n, err := container.SettleRead(t, d.count, flac.RestateStreamInfo)
	d.track, d.truncated = tr, d.truncated+n
	return err
}

// MeasureSamples implements container.Measurer for FLAC: what a read from
// the first frame delivers, the samples its holes take counted, by reading
// the packets as ReadPacket does. A seek past the end lands by the last
// frames' numbers, which is not where a read through frames joined on or out
// of order comes to. It lists nothing and leaves the read and the track as
// they were, costs a read of the stream the first time, and keeps its
// answer; other mappings have none.
func (d *Demuxer) MeasureSamples(ctx context.Context, track int) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("ogg: no track %d", track))
	}
	if !d.mapping.selfTiming() {
		return -1, nil
	}
	if d.empty {
		return 0, nil
	}
	if d.haveMeasured {
		return d.measured, nil
	}
	was := d.hold()
	n, err := int64(-1), error(nil)
	d.log.Probe(func() {
		if _, err = d.seekSelfTiming(ctx, 0); err != nil {
			if waxerr.CodeOf(err) == waxerr.CodeCanceled {
				err = waxerr.Annotate("ogg: measure canceled", ctx.Err())
			}
			return
		}
		var pkt container.Packet
		for i := 0; ; i++ {
			if i%64 == 0 && ctx.Err() != nil {
				err = waxerr.Annotate("ogg: measure canceled", ctx.Err())
				return
			}
			if err = d.ReadPacket(&pkt); err != nil {
				break
			}
		}
		if err == io.EOF {
			n, err = d.count, nil
		}
	})
	d.restore(was)
	d.measured, d.haveMeasured = n, err == nil
	return n, err
}

// dueBefore reports whether the packet behind a FLAC frame at pts, numbered
// past from, is the frame due right in front of it, at or past from, and
// where that one starts. Damage in front or not, the two are out of order.
func (d *Demuxer) dueBefore(from, pts int64) (at int64, due bool) {
	fm, ok := d.mapping.(*flacMapping)
	if !ok {
		return 0, false
	}
	next, followed, _ := d.frameAhead()
	at = fm.num.Start(next) - fm.origin
	return at, followed && at >= from && at+int64(next.BlockSize) == pts
}

// headPair reads the frame due right in front of the pending first frame,
// where that lies behind it, in its place: the pending frame stays pending,
// owed nothing, and what the hole took in front of the other is its loss.
func (d *Demuxer) headPair(data *[]byte, pts, dur *int64, sync *bool, lost *int64) error {
	from := *pts - *lost
	at, due := d.dueBefore(from, *pts)
	if !due {
		return nil
	}
	q, qpts, qdur, qsync, err := d.frameDue(*pts, at)
	if err != nil || q == nil {
		return err
	}
	if err := d.warn(d.page.off, "the frame at sample %d lies before the one at %d; read in order", *pts, qpts); err != nil {
		return err
	}
	d.havePending = true
	*data, *pts, *dur, *sync, *lost = q, qpts, qdur, qsync, qpts-from
	return nil
}

// frameDue reads the packet dueBefore saw, due at sample at in front of the
// frame at pts. A packet that does not read as that frame is dropped and
// reported, nil returned for it.
func (d *Demuxer) frameDue(pts, at int64) (q []byte, qpts, qdur int64, qsync bool, err error) {
	if q, err = d.nextPacket(); err != nil || q == nil {
		return nil, 0, 0, false, err
	}
	var ok bool
	if qpts, qdur, qsync, ok = d.timing(q, at); ok && qpts == at && at+qdur == pts {
		return q, qpts, qdur, qsync, nil
	}
	return nil, 0, 0, false, d.warn(d.page.off, "packet is not a valid %s packet, dropped", d.mapping.codecID())
}

// timing is the mapping's packetTiming, with a stray head's frames placed
// from 0: the first frame and those numbered on from it, read from there.
func (d *Demuxer) timing(p []byte, running int64) (pts, dur int64, sync, ok bool) {
	pts, dur, sync, ok = d.mapping.packetTiming(p, running)
	switch {
	case !ok || d.strayNext < 0:
	case pts != d.strayNext:
		d.strayNext = -1
	default:
		d.strayNext += dur
		pts -= d.strayFrom
	}
	return pts, dur, sync, ok
}

// strayRunEnd is the page a stray head's run ends on: the first frame not
// numbered on from it begins there. It walks the run once and keeps the
// answer, listing nothing.
func (d *Demuxer) strayRunEnd(ctx context.Context) (int64, error) {
	if d.strayEnd >= 0 {
		return d.strayEnd, nil
	}
	var err error
	d.log.Probe(func() {
		d.restartAt(d.firstData)
		for d.strayNext >= 0 {
			var p []byte
			if err = container.WalkCanceled(ctx, "ogg: "); err != nil {
				return
			}
			if p, err = d.nextPacket(); err != nil {
				return
			}
			if p == nil {
				d.strayEnd = d.totalSize
				return
			}
			d.timing(p, 0)
		}
		d.strayEnd = d.pktPage
	})
	return d.strayEnd, err
}

// walkFrom is where a seek's walk starts for lo: the first frame where lo
// lies in a stray head's run, whose frames only a walk from there places.
func (d *Demuxer) walkFrom(ctx context.Context, lo int64) (int64, error) {
	if lo = max(d.firstData, lo); !d.headJoined || lo == d.firstData {
		return lo, nil
	}
	end, err := d.strayRunEnd(ctx)
	if lo <= end {
		lo = d.firstData
	}
	return lo, err
}

// gap is what a self-timing packet at pts, past the running position, lost
// before it. After damage it is what a lost page took, within what its bytes
// could hold and the exact length. With nothing damaged the numbers skip,
// and the samples are lost where the stream could have the packet, inside
// its exact length or short of one inside what its bytes can hold; numbers
// past that vouch for nothing, so the packet is joined on. Either way it
// warns.
func (d *Demuxer) gap(pts int64) (int64, error) {
	if d.log.DamageCount() > d.lostAt {
		if pts-d.running <= flac.MaxSamplesIn(d.page.off-d.lastPage) && (!d.exact || pts <= d.stated) {
			return pts - d.running, nil
		}
		return 0, nil
	}
	vouched := pts <= flac.MaxSamplesIn(d.totalSize-d.firstData)
	if d.exact {
		vouched = pts <= d.stated
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
func (d *Demuxer) SeekSample(ctx context.Context, track int, sample int64) (int64, error) {
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
	// length the open gave: a seek reaches what it reached before a read.
	if end := d.track.Delay + d.stated; d.stated > 0 && sample >= end {
		sample = end - 1
	}
	// A seek canceled on the way leaves the read and its findings as they
	// stood, and nothing is copied for it: the walks read into a page of
	// their own (restartAt), no packet is in parts between two reads, and
	// only a landing writes the pending packet.
	was, mark, timing := *d, d.log.Mark(), d.mapping.heldTiming()
	d.whole = false
	landed, err := int64(0), error(nil)
	if d.mapping.selfTiming() {
		landed, err = d.seekSelfTiming(ctx, sample)
	} else {
		landed, err = d.seekAccumulating(ctx, sample)
	}
	if waxerr.CodeOf(err) == waxerr.CodeCanceled {
		d.log.Rollback(mark)
		d.restore(was)
		d.mapping.putTiming(timing)
	}
	return landed, err
}

// seekSelfTiming seeks a mapping whose packets carry their own positions
// (FLAC): bisect on the frames that begin on each page, then walk packets to
// the frame containing target. No granule plays a part, so none can cost a
// seek.
func (d *Demuxer) seekSelfTiming(ctx context.Context, sample int64) (int64, error) {
	lo, err := d.bisectFrames(ctx, sample)
	if err == nil {
		lo, err = d.walkFrom(ctx, lo)
	}
	if err != nil {
		return 0, err
	}
	var (
		lastData         []byte
		lastPTS, lastDur int64
		prevPTS          int64 // the frame before the last, -1 for none
		lastSync         bool
		haveLast         bool
		firstPTS         int64 // the first frame the walk read
		first            bool  // the landing is that frame
		// began is where the first walk began, when a hole behind its first
		// frame, at beganPTS, sent the seek back, and direct says the walk
		// is from there again.
		began    = int64(-1)
		beganPTS int64
		direct   bool
	)
	for {
		d.restartAt(lo)
		prevPTS, haveLast = -1, false
		for {
			if err := container.WalkCanceled(ctx, "ogg: "); err != nil {
				return 0, err
			}
			p, err := d.nextPacket()
			if err != nil {
				return 0, err
			}
			if p == nil {
				break
			}
			pts, dur, sync, ok := d.timing(p, 0)
			if !ok {
				continue
			}
			if haveLast {
				prevPTS = lastPTS
			} else {
				firstPTS = pts
			}
			first = !haveLast
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
		if haveLast && lastPTS > sample && prevPTS == firstPTS && lo > d.firstData && !direct {
			// A hole behind the walk's first frame: the frame due in it may
			// lie before that one, so the walk starts further back.
			if began < 0 {
				began, beganPTS = lo, firstPTS
			}
			if lo, err = d.walkFrom(ctx, lo-seekWindow); err != nil {
				return 0, err
			}
			continue
		}
		if haveLast && lastPTS > sample && prevPTS >= 0 && began >= 0 && !direct &&
			(prevPTS == beganPTS || d.exact && lastPTS > d.stated) {
			// From further back the hole lies behind the same frame, with no
			// frame due in it in front, or the frame behind the target is
			// joined on with nothing lost, so no read from there comes to the
			// target: the seek lands by its first walk, as it always did.
			lo, direct = began, true
			continue
		}
		if haveLast && lastPTS > sample && prevPTS >= 0 {
			// The target is in a hole before this frame, so land on the one
			// before it, at or before the target; the read fills the hole.
			if lastData, lastDur, lastSync, err = d.walkToFrame(ctx, lo, prevPTS, lastData); err != nil {
				return 0, err
			}
			lastPTS, first = prevPTS, prevPTS == firstPTS
		}
		if haveLast || lo <= d.firstData {
			break
		}
		if lo, err = d.walkFrom(ctx, lo-seekWindow); err != nil {
			return 0, err
		}
	}
	if !haveLast {
		return 0, malformed("no frame found while seeking")
	}
	d.pending = append(d.pending[:0], lastData...)
	d.pendingPTS, d.pendingDur, d.pendingSync, d.pendingLost = lastPTS, lastDur, lastSync, 0
	d.havePending = true
	// In the hole the stream starts with, the frame behind the landing is
	// behind the target too.
	d.running, d.landed = lastPTS+lastDur, lastPTS <= sample
	// A landing on the stream's first frame counts as a read from the open.
	d.whole = first && lo <= d.firstData
	if lastPTS > sample && prevPTS < 0 {
		// In the hole the stream starts with: land at 0, the read owing it.
		d.count, d.pendingLost = 0, lastPTS
		return 0, nil
	}
	d.count = lastPTS
	return lastPTS, nil
}

// walkToFrame walks from lo to the frame that starts at pts and leaves the read
// just past it, returning it in buf.
func (d *Demuxer) walkToFrame(ctx context.Context, lo, pts int64, buf []byte) ([]byte, int64, bool, error) {
	d.restartAt(lo)
	for {
		if err := container.WalkCanceled(ctx, "ogg: "); err != nil {
			return nil, 0, false, err
		}
		p, err := d.nextPacket()
		if err != nil {
			return nil, 0, false, err
		}
		if p == nil {
			return nil, 0, false, malformed("the frame at %d was not found walking to it again", pts)
		}
		if at, dur, sync, ok := d.timing(p, 0); ok && at == pts {
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
func (d *Demuxer) seekAccumulating(ctx context.Context, sample int64) (int64, error) {
	searchGranule := max(sample-d.mapping.preroll(), 0)
	lo, err := d.bisect(ctx, searchGranule)
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
			if err := container.WalkCanceled(ctx, "ogg: "); err != nil {
				return 0, err
			}
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
	for i := range n {
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
	for i := range n {
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
func (d *Demuxer) bisectFrames(ctx context.Context, target int64) (int64, error) {
	return d.narrow(ctx, func(mid, hi int64) (int64, bool, error) {
		off, pts, ok, err := d.frameAt(mid, hi)
		return off, err == nil && ok && pts <= target, err
	})
}

// narrow bisects the stream's bytes to within a bisection window after the
// last probe that lands before the target: probe reports the page it found at
// or after mid, below hi, and whether what that page states is before it.
func (d *Demuxer) narrow(ctx context.Context, probe func(mid, hi int64) (off int64, before bool, err error)) (int64, error) {
	lo, hi := d.firstData, d.totalSize
	for hi-lo > seekWindow {
		if err := container.WalkCanceled(ctx, "ogg: "); err != nil {
			return 0, err
		}
		mid := lo + (hi-lo)/2
		off, before, err := probe(mid, hi)
		if waxerr.CodeOf(err) == waxerr.CodeMalformedInput && !d.log.Strict() {
			// No page in a resync's reach of mid: a read ends in front of
			// it, so the search looks there.
			before, err = false, nil
		}
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
func (d *Demuxer) bisect(ctx context.Context, target int64) (int64, error) {
	return d.narrow(ctx, func(mid, hi int64) (int64, bool, error) {
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
// Pages are walked by following each one's own length. The first page in the
// window still costs a resync, since the window starts mid-page; from there
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
	if d.strayNext = -1; d.headJoined && off == d.firstData {
		d.strayNext = d.strayFrom
	}
}
