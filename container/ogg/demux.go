package ogg

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

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
	// maxPacket caps packet reassembly across pages; no FLAC frame can
	// exceed it (STREAMINFO's frame size field is 24-bit).
	maxPacket = 1 << 24
	// maxBOSPages bounds the concurrently-multiplexed stream count.
	maxBOSPages = 64
	// maxHeaderPackets bounds the mapping's header packets to skip.
	maxHeaderPackets = 1 << 16
	// maxResync bounds the scan for the next page capture after damage.
	maxResync = 1 << 20
	// seekWindow is the bisection cutoff: below this span, walk packets.
	seekWindow = 128 << 10
)

// Known mapping signatures, so unsupported-format errors can name what
// they found. Only FLAC opens; the rest join with their codecs.
var mappingNames = []struct {
	prefix string
	name   string
}{
	{"\x7FFLAC", "flac"},
	{"\x01vorbis", "vorbis"},
	{"OpusHead", "opus"},
	{"Speex   ", "speex"},
	{"\x80theora", "theora"},
	{"fishead\x00", "skeleton"},
}

func sniffMapping(pkt []byte) string {
	for _, m := range mappingNames {
		if len(pkt) >= len(m.prefix) && string(pkt[:len(m.prefix)]) == m.prefix {
			return m.name
		}
	}
	return "unknown"
}

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads the first FLAC-mapped stream from an Ogg source.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	serial   uint32
	si       flac.StreamInfo
	track    container.Track
	warnings []container.Warning

	// num maps coded numbers to sample positions (flac.Numbering,
	// latched from the first audio frame, exactly as in container/flacn).
	num flac.Numbering

	// headerPackets is the mapping's declared header packet count after
	// the identification packet; 0 means unknown.
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

	pending     []byte // one-packet pushback (first frame, seek landing)
	pendingInfo flac.FrameInfo
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

// NewDemuxer parses an Ogg source up to its first audio packet. The
// returned Demuxer implements container.Seeker and container.Warner.
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

// readPage reads and CRC-checks the page at off. It does not resync;
// callers own that policy. A nil page with nil error means end of file.
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

// nextPageAt scans forward from off for the next valid page, tolerating
// junk in between up to maxResync bytes. Returns false at end of file.
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
		// Exhaust every capture pattern in this window before reading the
		// next one: packet data produces false positives, and re-reading
		// after each would turn hostile input into quadratic I/O.
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
			// Damaged page: resync to the next valid capture.
			if werr := d.warn(d.off, "damaged page, resyncing"); werr != nil {
				return false, werr
			}
			var at int64
			at, ok, err = d.nextPageAt(d.off+1, &p)
			if ok {
				p.off = at
				// Anything mid-assembly is dropped, even though a damaged
				// page belonging to another multiplexed stream leaves our
				// packet intact. Deliberate: a corrupt page's serial field
				// is untrustworthy, and keeping the assembly on a wrong
				// guess splices mismatched bytes into a packet that fails
				// only later, at decode, killing the whole stream instead
				// of costing one frame.
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
			// A BOS after data starts a chained stream; stop before it.
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

// nextPacket reassembles the next packet of our serial. It returns nil
// at end of stream.
func (d *Demuxer) nextPacket() ([]byte, error) {
	for {
		if d.eos {
			return nil, nil
		}
		for d.seg < len(d.page.lacing) {
			// A packet is segments up to one with a lacing value < 255.
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
				// A continuation with nothing to continue: damage unless
				// we just sought or resynced.
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
				// The returned packet may alias d.partial's backing array
				// (and single-page packets alias d.page.body), so the next
				// nextPacket call can overwrite it. That is the Demuxer
				// contract; anything kept across calls is copied by the
				// caller (see SeekSample and the pending stash in parse).
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
			// The packet in flight never finishes: its tail page is gone.
			if werr := d.warn(d.page.off, "unfinished packet dropped at page boundary"); werr != nil {
				return nil, werr
			}
			d.partial = d.partial[:0]
		}
	}
}

// parse walks the BOS section, picks the first FLAC stream, skips its
// header packets, and stashes the first audio packet.
func (d *Demuxer) parse() error {
	var (
		p       page
		found   []string
		haveBOS bool
	)
	// The BOS section: every concurrently multiplexed stream opens with
	// one BOS page before any data page.
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
		name := sniffMapping(pkt)
		if name == "flac" && !haveBOS {
			if err := d.parseFLACBOS(pkt); err != nil {
				return err
			}
			d.serial = p.serial
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
		return malformed("no FLAC stream (found: %s); other mappings land with their codecs",
			joinNames(found))
	}
	if len(found) > 0 {
		if err := d.warn(0, "ignoring %d additional stream(s): %s", len(found), joinNames(found)); err != nil {
			return err
		}
	}

	// Position after the BOS section and consume this stream's header
	// packets. A zero header count means unknown; audio starts at the
	// first packet with a frame sync.
	d.off = off
	d.seg, d.segOff = 0, 0
	d.page = page{} // force advancePage to load the next page
	for skipped := 0; ; skipped++ {
		if skipped > maxHeaderPackets {
			return malformed("more than %d header packets", maxHeaderPackets)
		}
		pkt, err := d.nextPacket()
		if err != nil {
			return err
		}
		if pkt == nil {
			// Headers but no audio: tolerate as an empty stream (reads
			// return EOF, seeks land at 0). It is still damage-shaped
			// (a finished stream would carry an end-of-stream audio
			// page), so strict mode refuses.
			if werr := d.warn(d.off, "stream has no audio packets"); werr != nil {
				return werr
			}
			d.empty = true
			break
		}
		if d.headerPackets > 0 && skipped < d.headerPackets {
			continue
		}
		if !flac.SyncOK(pkt) {
			if d.headerPackets == 0 {
				continue // unknown header count: skip until a frame sync
			}
			return malformed("first audio packet has no frame sync")
		}
		fi, err := flac.ParseFrameHeader(pkt)
		if err != nil {
			return err
		}
		d.num = d.si.Numbering(fi)
		d.pending = append(d.pending[:0], pkt...)
		d.pendingInfo = fi
		d.havePending = true
		d.firstData = d.pktPage
		break
	}

	f := d.si.PCMFormat()
	if err := f.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "ogg: unusable format", err)
	}
	samples := d.si.Samples
	switch {
	case d.empty:
		samples = 0
	case samples == 0:
		// Streaming muxers (ffmpeg included) leave STREAMINFO's total at
		// zero; the last page's granule position carries the stream length.
		samples = d.lastGranule()
	}
	d.track = container.Track{
		Codec:       codec.FLAC,
		CodecConfig: d.track.CodecConfig, // set by parseFLACBOS
		Fmt:         f,
		Samples:     samples,
		Default:     true,
	}
	return nil
}

// parseFLACBOS parses the Ogg-FLAC identification packet: the 0x7F FLAC
// signature, mapping version, header count, and an embedded fLaC marker
// with the STREAMINFO block.
func (d *Demuxer) parseFLACBOS(pkt []byte) error {
	const want = 13 + 4 + flac.StreamInfoLen
	if len(pkt) < want {
		return malformed("FLAC identification packet of %d bytes, want %d", len(pkt), want)
	}
	if pkt[5] != 1 {
		return malformed("unsupported Ogg-FLAC mapping version %d.%d", pkt[5], pkt[6])
	}
	d.headerPackets = int(pkt[7])<<8 | int(pkt[8])
	if string(pkt[9:13]) != "fLaC" {
		return malformed("identification packet lacks the fLaC marker")
	}
	if typ := pkt[13] & 0x7F; typ != 0 {
		return malformed("first metadata block is type %d, want STREAMINFO", typ)
	}
	siRaw := append([]byte(nil), pkt[17:17+flac.StreamInfoLen]...)
	si, err := flac.ParseStreamInfo(siRaw)
	if err != nil {
		return err
	}
	d.si = si
	d.track.CodecConfig = siRaw
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

// Tracks returns the single FLAC track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// ReadPacket yields one FLAC frame. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	var data []byte
	var fi flac.FrameInfo
	if d.havePending {
		data, fi = d.pending, d.pendingInfo
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
			var perr error
			fi, perr = flac.ParseFrameHeader(p)
			if perr != nil {
				if werr := d.warn(d.page.off, "packet is not a FLAC frame, dropped"); werr != nil {
					return werr
				}
				continue
			}
			data = p
			break
		}
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: data,
			PTS:  d.num.Start(fi),
			Dur:  int64(fi.BlockSize),
			Sync: true,
		},
	}
	return nil
}

// SeekSample bisects on page granule positions, then walks packets to
// the frame containing the target sample and returns that frame's first
// sample; format.Media pre-rolls the remainder.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("ogg: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "ogg: negative seek target")
	}
	if d.empty {
		return 0, nil // nothing to land on; reads stay EOF
	}
	// Past-end targets land on the last frame either way; with a known
	// length, clamping keeps the bisection off a final page that only
	// continues the last packet (a restart there drops the tail and
	// finds nothing).
	if d.track.Samples > 0 && sample >= d.track.Samples {
		sample = d.track.Samples - 1
	}

	// Bisect byte offsets: find the last page of our serial whose granule
	// position is below the target. Frames containing the target begin on
	// or after it.
	lo, hi := d.firstData, d.totalSize
	for hi-lo > seekWindow {
		mid := lo + (hi-lo)/2
		off, granule, ok, err := d.granuleAt(mid, hi)
		if err != nil {
			return 0, err
		}
		if !ok || granule >= sample {
			hi = mid
			continue
		}
		lo = off
	}

	// Restart packet delivery at lo and walk to the target frame. When
	// the restart page turns out to hold only the tail of the final
	// packet (possible with an unknown stream length), the walk comes up
	// empty; back off and re-walk from earlier.
	var (
		lastData []byte
		lastInfo flac.FrameInfo
		haveLast bool
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
			fi, perr := flac.ParseFrameHeader(p)
			if perr != nil {
				continue
			}
			lastData = append(lastData[:0], p...)
			lastInfo = fi
			haveLast = true
			if start := d.num.Start(fi); start+int64(fi.BlockSize) > sample {
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
	d.pendingInfo = lastInfo
	d.havePending = true
	return d.num.Start(lastInfo), nil
}

// lastGranule scans the file tail for the final granule position of our
// serial, which in the FLAC mapping is the stream's total sample count.
// The window grows when nothing of ours completes in it (a final packet
// larger than one window, trailing pages of other streams) before the
// length is declared unknown (-1); the growth cap bounds open-time work.
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

// granuleAt finds the first page of our serial at or after off (bounded
// by limit) that completes a packet, and returns its offset and granule
// position.
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

// restartAt begins page-level streaming at a page boundary, dropping any
// packet continued onto that page.
func (d *Demuxer) restartAt(off int64) {
	d.off = off
	d.page = page{}
	d.seg, d.segOff = 0, 0
	d.partial = d.partial[:0]
	d.skipCont = true
	d.eos = false
	d.havePending = false
}
