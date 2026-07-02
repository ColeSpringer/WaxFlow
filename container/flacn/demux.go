package flacn

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
	// maxMetaBlocks bounds the metadata walk; real files hold a handful,
	// pathological repetition (the IETF suite ships a 1000-block file)
	// stays well under.
	maxMetaBlocks = 4096
	// maxSeekPoints caps the SEEKTABLE allocation; points past the cap
	// only coarsen seeks, so the prefix is kept and the rest skipped.
	maxSeekPoints = 1 << 16
	// maxFrameLen caps a single frame scan. STREAMINFO's frame size field
	// is 24-bit, so no compliant frame can exceed it.
	maxFrameLen = 1 << 24
	// maxResync bounds the scan for a frame sync after damage.
	maxResync = 1 << 20
	// seekWindow is the bisection cutoff: below this span, walk frames.
	seekWindow = 128 << 10
	// windowChunk is the read-ahead granularity.
	windowChunk = 128 << 10
	// maxTrailers bounds trailing-tag peeling at end of stream (tags
	// stack: APEv2 then ID3v1 is a classic).
	maxTrailers = 8
)

// Metadata block types (RFC 9639 section 8).
const (
	blockStreamInfo = 0
	blockSeekTable  = 3
	blockInvalid    = 127
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

type seekPoint struct {
	sample int64
	off    int64 // relative to the first frame
}

// Demuxer reads one FLAC track from a native FLAC source.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	si        flac.StreamInfo
	track     container.Track
	seekTable []seekPoint
	warnings  []container.Warning

	firstFrame int64 // byte offset of the first audio frame
	dataEnd    int64 // end of frame data (trailing tags stripped lazily)

	// num maps coded numbers to sample positions (flac.Numbering,
	// latched from the first frame); varBit is the blocking-strategy
	// header bit, which must not change within a stream.
	num    flac.Numbering
	varBit bool

	off   int64          // offset of the current undelivered frame
	cur   flac.FrameInfo // its parsed header
	valid bool           // false once the stream is exhausted
	empty bool           // stream has zero frames (legal: metadata only)

	win    []byte // read-ahead window starting at winOff
	winOff int64
	ioErr  error // sticky read failure, surfaced by the packet path
}

// NewDemuxer parses the headers of a native FLAC source and positions on
// the first frame. The returned Demuxer implements container.Seeker and
// container.Warner.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src}
	if opts != nil {
		d.opts = *opts
	}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "flacn: "+fmt.Sprintf(format, args...))
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

func (d *Demuxer) parse() error {
	size := d.src.Size()
	var head [4]byte
	if err := container.ReadFull(d.src, head[:], 0); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "flacn: reading marker", err)
	}
	if !Match(head[:]) {
		return malformed("not a FLAC file")
	}

	var (
		siRaw   []byte
		haveSI  bool
		off     = int64(4)
		last    = false
		blockNo = 0
	)
	for !last {
		if blockNo++; blockNo > maxMetaBlocks {
			return malformed("more than %d metadata blocks", maxMetaBlocks)
		}
		var hdr [4]byte
		if err := container.ReadFull(d.src, hdr[:], off); err != nil {
			return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "flacn: reading metadata block header", err)
		}
		last = hdr[0]&0x80 != 0
		typ := int(hdr[0] & 0x7F)
		length := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		if off+4+length > size {
			return malformed("metadata block extends past end of file")
		}
		switch typ {
		case blockStreamInfo:
			if haveSI {
				if err := d.warn(off, "duplicate STREAMINFO ignored"); err != nil {
					return err
				}
				break
			}
			if blockNo != 1 {
				if err := d.warn(off, "STREAMINFO is not the first metadata block"); err != nil {
					return err
				}
			}
			if length != flac.StreamInfoLen {
				return malformed("STREAMINFO of %d bytes", length)
			}
			siRaw = make([]byte, flac.StreamInfoLen)
			if err := container.ReadFull(d.src, siRaw, off+4); err != nil {
				return waxerr.Wrap(waxerr.CodeSourceUnreadable, "flacn: reading STREAMINFO", err)
			}
			var err error
			if d.si, err = flac.ParseStreamInfo(siRaw); err != nil {
				return err
			}
			haveSI = true
		case blockSeekTable:
			if err := d.parseSeekTable(off+4, length); err != nil {
				return err
			}
		case blockInvalid:
			return malformed("forbidden metadata block type 127")
		}
		off += 4 + length
	}
	if !haveSI {
		return malformed("no STREAMINFO metadata block")
	}

	d.firstFrame = off
	d.dataEnd = size

	f := d.si.PCMFormat()
	if err := f.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "flacn: unusable format", err)
	}
	samples := d.si.Samples
	if samples == 0 {
		samples = -1
	}
	d.track = container.Track{
		Codec:       codec.FLAC,
		CodecConfig: siRaw,
		Fmt:         f,
		Samples:     samples,
		Default:     true,
	}

	// Latch the first frame: it fixes the blocking strategy and the
	// fixed-strategy constant block size that converts frame numbers to
	// sample positions everywhere else.
	fi, err := flac.ParseFrameHeader(d.bytesAt(off, flac.MaxFrameHeaderLen))
	if err != nil || !d.consistent(fi) {
		if d.firstFrame >= d.dataEnd {
			// Zero frames and zero audio bytes: a legal stream (encoding
			// empty input produces exactly this, STREAMINFO total 0 and
			// all). Reads return EOF, seeks land at 0. A declared total
			// with no frames to back it is an inconsistency, though.
			if d.si.Samples != 0 {
				if werr := d.warn(off, "STREAMINFO declares %d samples but the stream has no frames", d.si.Samples); werr != nil {
					return werr
				}
			}
			d.track.Samples = 0
			d.empty = true
			return d.ioErr
		}
		fOff, ffi, ok := d.nextCandidate(off, off+maxResync)
		if !ok {
			// A read failure looks identical from here (empty scans); it
			// must not be misreported as an unsupported file.
			if d.ioErr != nil {
				return d.ioErr
			}
			return malformed("no audio frame after the metadata blocks")
		}
		if werr := d.warn(off, "%d unparsable bytes before the first frame", fOff-off); werr != nil {
			return werr
		}
		d.firstFrame, fi = fOff, ffi
	}
	if d.ioErr != nil {
		return d.ioErr
	}
	d.varBit = fi.Variable
	d.num = d.si.Numbering(fi)
	d.off, d.cur, d.valid = d.firstFrame, fi, true
	return nil
}

// parseSeekTable reads up to maxSeekPoints seek points, dropping
// placeholders and non-monotonic entries.
func (d *Demuxer) parseSeekTable(off, length int64) error {
	if length%18 != 0 {
		if err := d.warn(off, "SEEKTABLE length %d is not whole seek points", length); err != nil {
			return err
		}
	}
	points := length / 18
	if points > maxSeekPoints {
		points = maxSeekPoints
	}
	raw := make([]byte, points*18)
	if err := container.ReadFull(d.src, raw, off); err != nil {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "flacn: reading SEEKTABLE", err)
	}
	d.seekTable = make([]seekPoint, 0, points)
	for i := int64(0); i < points; i++ {
		b := raw[i*18:]
		sample := binary.BigEndian.Uint64(b)
		if sample == 1<<64-1 {
			continue // placeholder point
		}
		p := seekPoint{sample: int64(sample), off: int64(binary.BigEndian.Uint64(b[8:]))}
		if p.sample < 0 || p.off < 0 ||
			(len(d.seekTable) > 0 && p.sample <= d.seekTable[len(d.seekTable)-1].sample) {
			if err := d.warn(off+i*18, "invalid seek point dropped"); err != nil {
				return err
			}
			continue
		}
		d.seekTable = append(d.seekTable, p)
	}
	return nil
}

// Tracks returns the single FLAC track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// consistent reports whether a frame header agrees with STREAMINFO; the
// pipeline's track format is fixed, and consistency is also what makes
// sync-candidate scanning trustworthy.
func (d *Demuxer) consistent(fi flac.FrameInfo) bool {
	rate, bits := fi.Rate, fi.Bits
	if rate == 0 {
		rate = d.si.Rate
	}
	if bits == 0 {
		bits = d.si.Bits
	}
	return rate == d.si.Rate && bits == d.si.Bits && fi.Channels == d.si.Channels
}

// bytesAt returns up to n bytes starting at off, clamped to dataEnd,
// through the read-ahead window. A short or empty result means end of
// data or a read failure; failures stick in d.ioErr and the packet and
// seek paths surface them.
func (d *Demuxer) bytesAt(off int64, n int) []byte {
	if n <= 0 || off >= d.dataEnd || d.ioErr != nil {
		return nil
	}
	if left := d.dataEnd - off; int64(n) > left {
		n = int(left)
	}
	if off >= d.winOff && off+int64(n) <= d.winOff+int64(len(d.win)) {
		i := off - d.winOff
		return d.win[i : i+int64(n)]
	}
	if err := d.loadWindow(off, n); err != nil {
		d.ioErr = err
		return nil
	}
	i := off - d.winOff
	return d.win[i : i+int64(n)]
}

// loadWindow makes [off, off+n) resident. Forward extension appends so
// earlier bytes of the current frame stay addressable; anything else
// rebases the window.
func (d *Demuxer) loadWindow(off int64, n int) error {
	want := max(int64(n), windowChunk)
	if off+want > d.dataEnd {
		want = d.dataEnd - off
	}
	winEnd := d.winOff + int64(len(d.win))
	if off >= d.winOff && off <= winEnd {
		need := off + want - winEnd
		if need <= 0 {
			return nil
		}
		grown := append(d.win, make([]byte, need)...)
		if err := container.ReadFull(d.src, grown[len(d.win):], winEnd); err != nil {
			return waxerr.Wrap(waxerr.CodeSourceUnreadable, "flacn: reading frame data", err)
		}
		d.win = grown
		return nil
	}
	buf := make([]byte, want)
	if err := container.ReadFull(d.src, buf, off); err != nil {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "flacn: reading frame data", err)
	}
	d.win, d.winOff = buf, off
	return nil
}

// trimWindow drops window bytes before off so the window tracks the
// stream position instead of accreting the whole file.
func (d *Demuxer) trimWindow(off int64) {
	if off-d.winOff < windowChunk {
		return
	}
	if off >= d.winOff+int64(len(d.win)) {
		d.win, d.winOff = d.win[:0], off
		return
	}
	kept := d.win[off-d.winOff:]
	d.win = append(d.win[:0:0], kept...)
	d.winOff = off
}

// nextCandidate scans [from, limit) for a sync candidate whose header
// parses and agrees with STREAMINFO. Used for initial positioning,
// resync after damage, and bisection probes; packet boundaries demand
// the stronger findEnd confirmation.
func (d *Demuxer) nextCandidate(from, limit int64) (int64, flac.FrameInfo, bool) {
	limit = min(limit, d.dataEnd)
	for off := from; off < limit; {
		buf := d.bytesAt(off, windowChunk)
		if len(buf) < 2 {
			return 0, flac.FrameInfo{}, false
		}
		i := bytes.IndexByte(buf, 0xFF)
		if i < 0 {
			off += int64(len(buf))
			continue
		}
		cand := off + int64(i)
		if cand >= limit {
			return 0, flac.FrameInfo{}, false
		}
		hdr := d.bytesAt(cand, flac.MaxFrameHeaderLen)
		if flac.SyncOK(hdr) {
			if fi, err := flac.ParseFrameHeader(hdr); err == nil && d.consistent(fi) {
				return cand, fi, true
			}
		}
		off = cand + 1
	}
	return 0, flac.FrameInfo{}, false
}

// findEnd locates the end of the current frame at d.off: the next sync
// candidate that parses consistently, carries the expected position, and
// where the bytes so far checksum as a complete frame (CRC-16). At end
// of data the final span must checksum likewise; recognized trailers
// (ID3v1, APEv2, appended ID3v2, NUL padding) are peeled and retried
// before the tail is declared damaged. end < 0 with a nil error reports
// a tolerantly dropped tail.
func (d *Demuxer) findEnd() (end int64, next flac.FrameInfo, nextOK bool, err error) {
	start := d.off
	crc := uint16(0)
	crcPos := start // crc covers [start, crcPos)

	// A boundary needs at least a header and CRC-16 before it; starting
	// past the fixed header bytes keeps the scan out of our own header.
	scan := start + 4
	for {
		if scan-start > maxFrameLen {
			return 0, flac.FrameInfo{}, false, malformed("frame at offset %d exceeds %d bytes", start, int64(maxFrameLen))
		}
		buf := d.bytesAt(scan, windowChunk)
		if len(buf) == 0 {
			break // end of data
		}
		rel := 0
		for {
			i := bytes.IndexByte(buf[rel:], 0xFF)
			if i < 0 {
				break
			}
			cand := scan + int64(rel+i)
			rel += i + 1
			if cand-2 < crcPos || cand+2 > d.dataEnd {
				continue
			}
			hdr := d.bytesAt(cand, flac.MaxFrameHeaderLen)
			if !flac.SyncOK(hdr) {
				continue
			}
			fi, perr := flac.ParseFrameHeader(hdr)
			if perr != nil || fi.Variable != d.varBit || fi.Coded != d.num.Next(d.cur) {
				continue
			}
			crc = flac.UpdateCRC16(crc, d.bytesAt(crcPos, int(cand-2-crcPos)))
			crcPos = cand - 2
			tail := d.bytesAt(crcPos, 2)
			if len(tail) == 2 && crc == uint16(tail[0])<<8|uint16(tail[1]) {
				// Confirmed boundary. A frame here that disagrees with
				// STREAMINFO is a mid-stream format change: not damage but
				// a stream shape the fixed-format pipeline rejects (RFC
				// 9639 permits this), so it is a hard error either mode.
				if !d.consistent(fi) {
					return 0, flac.FrameInfo{}, false,
						malformed("mid-stream format change at offset %d", cand)
				}
				return cand, fi, true, nil
			}
			// Not a boundary: fold the two checked bytes in and go on.
			crc = flac.UpdateCRC16(crc, tail)
			crcPos += int64(len(tail))
		}
		scan += int64(len(buf))
	}
	if d.ioErr != nil {
		return 0, flac.FrameInfo{}, false, d.ioErr
	}

	// End of data: the last frame must checksum to exactly here, after
	// peeling any recognized trailing tags (taggers bolt ID3v1, APEv2,
	// and appended ID3v2 onto FLAC files; NUL padding shows up too, and
	// tags stack). dataEnd shrinks permanently only once a peel is
	// confirmed by the checksum.
	end = d.dataEnd
	for range maxTrailers {
		if d.tailChecks(crc, crcPos, end) {
			if end != d.dataEnd {
				if werr := d.warn(end, "%d trailing tag or padding bytes ignored", d.dataEnd-end); werr != nil {
					return 0, flac.FrameInfo{}, false, werr
				}
				d.dataEnd = end
			}
			return end, flac.FrameInfo{}, false, nil
		}
		stripped, ok := d.stripTrailer(start, end)
		if !ok {
			break
		}
		end = stripped
	}
	if d.ioErr != nil {
		return 0, flac.FrameInfo{}, false, d.ioErr
	}
	if werr := d.warn(start, "%d trailing bytes are not a valid frame, dropped", d.dataEnd-start); werr != nil {
		return 0, flac.FrameInfo{}, false, werr
	}
	return -1, flac.FrameInfo{}, false, nil
}

// stripTrailer recognizes one trailing non-FLAC structure ending at end
// and returns the end without it: an ID3v1 tag, an APEv2 tag (with its
// optional header), an appended ID3v2 tag, or NUL padding. The caller
// re-checks the frame checksum after each peel, so a false recognition
// costs a retry, never data.
func (d *Demuxer) stripTrailer(start, end int64) (int64, bool) {
	if e := end - 128; e >= start+2 {
		if string(d.bytesAt(e, 3)) == "TAG" {
			return e, true
		}
	}
	if e := end - 32; e >= start+2 {
		if f := d.bytesAt(e, 32); len(f) == 32 && string(f[:8]) == "APETAGEX" {
			// Size covers items plus this footer; a set header flag adds
			// an equally sized preamble.
			total := int64(binary.LittleEndian.Uint32(f[12:16]))
			if binary.LittleEndian.Uint32(f[20:24])&(1<<31) != 0 {
				total += 32
			}
			if total >= 32 && end-total >= start+2 {
				return end - total, true
			}
		}
	}
	if e := end - 10; e >= start+2 {
		if f := d.bytesAt(e, 10); len(f) == 10 && string(f[:3]) == "3DI" &&
			(f[6]|f[7]|f[8]|f[9])&0x80 == 0 {
			// An appended ID3v2 tag: header, syncsafe-sized body, footer.
			size := int64(f[6])<<21 | int64(f[7])<<14 | int64(f[8])<<7 | int64(f[9])
			if total := size + 20; end-total >= start+2 {
				return end - total, true
			}
		}
	}
	if n := min(int64(64<<10), end-(start+2)); n > 0 {
		if tail := d.bytesAt(end-n, int(n)); int64(len(tail)) == n {
			i := int64(len(tail))
			for i > 0 && tail[i-1] == 0 {
				i--
			}
			if i < n {
				return end - (n - i), true
			}
		}
	}
	return end, false
}

// tailChecks reports whether [d.off, end) checksums as a complete frame,
// given crc already covering [d.off, crcPos).
func (d *Demuxer) tailChecks(crc uint16, crcPos, end int64) bool {
	if end-crcPos < 2 || end > d.dataEnd {
		return false
	}
	crc = flac.UpdateCRC16(crc, d.bytesAt(crcPos, int(end-2-crcPos)))
	tail := d.bytesAt(end-2, 2)
	if len(tail) < 2 {
		return false
	}
	return crc == uint16(tail[0])<<8|uint16(tail[1])
}

// ReadPacket yields one whole frame, checksum included. Packet data is
// reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	if !d.valid {
		if d.ioErr != nil {
			return d.ioErr
		}
		return io.EOF
	}
	d.trimWindow(d.off)
	end, next, nextOK, err := d.findEnd()
	if err != nil {
		return err
	}
	if end < 0 {
		// Damaged tail, tolerantly dropped.
		d.valid = false
		return io.EOF
	}
	data := d.bytesAt(d.off, int(end-d.off))
	if int64(len(data)) != end-d.off {
		if d.ioErr != nil {
			return d.ioErr
		}
		return waxerr.New(waxerr.CodeSourceUnreadable, "flacn: reading frame data")
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: data,
			PTS:  d.num.Start(d.cur),
			Dur:  int64(d.cur.BlockSize),
			Sync: true,
		},
	}
	d.off, d.cur, d.valid = end, next, nextOK
	return nil
}

// SeekSample repositions to the frame containing the target sample and
// returns that frame's first sample; format.Media pre-rolls the
// remainder. The SEEKTABLE provides the starting point when present and
// trustworthy; otherwise bisection on the frame headers' own position
// numbers narrows the range.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("flacn: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "flacn: negative seek target")
	}
	if d.empty {
		return 0, nil // nothing to land on; reads stay EOF
	}

	off, fi, ok := d.seekTableHint(sample)
	if ok {
		// The table is data, not truth: like every other boundary here,
		// the landing only counts once the pointed-at frame's end is
		// checksum-confirmed. Warnings from a failed probe are dropped
		// with it.
		d.off, d.cur, d.valid = off, fi, true
		saved := len(d.warnings)
		end, _, _, err := d.findEnd()
		if err != nil || end < 0 {
			if d.ioErr != nil {
				return 0, d.ioErr
			}
			d.warnings = d.warnings[:saved]
			ok = false
		}
	}
	if !ok {
		off, fi, ok = d.bisect(sample)
	}
	if !ok {
		if d.ioErr != nil {
			return 0, d.ioErr
		}
		return 0, malformed("cannot relocate any frame for seeking")
	}

	d.off, d.cur, d.valid = off, fi, true
	// Walk forward to the frame containing the target; stop on the last
	// frame for past-the-end targets.
	for d.num.Start(d.cur)+int64(d.cur.BlockSize) <= sample {
		d.trimWindow(d.off)
		end, next, nextOK, err := d.findEnd()
		if err != nil {
			return 0, err
		}
		if end < 0 || !nextOK {
			break
		}
		d.off, d.cur = end, next
	}
	return d.num.Start(d.cur), nil
}

// seekTableHint resolves the target through the SEEKTABLE, verifying the
// pointed-at bytes really are a consistent frame header.
func (d *Demuxer) seekTableHint(sample int64) (int64, flac.FrameInfo, bool) {
	lo, hi := 0, len(d.seekTable)
	for lo < hi {
		mid := (lo + hi) / 2
		if d.seekTable[mid].sample <= sample {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return 0, flac.FrameInfo{}, false
	}
	pt := d.seekTable[lo-1]
	off := d.firstFrame + pt.off
	if off < d.firstFrame || off >= d.dataEnd {
		return 0, flac.FrameInfo{}, false
	}
	hdr := d.bytesAt(off, flac.MaxFrameHeaderLen)
	fi, err := flac.ParseFrameHeader(hdr)
	if err != nil || !d.consistent(fi) || d.num.Start(fi) > sample {
		return 0, flac.FrameInfo{}, false
	}
	return off, fi, true
}

// bisect narrows the byte range containing the target sample using frame
// headers found from probe offsets, then hands back the earliest frame
// of the final window.
func (d *Demuxer) bisect(sample int64) (int64, flac.FrameInfo, bool) {
	lo := d.firstFrame
	loFi, err := flac.ParseFrameHeader(d.bytesAt(lo, flac.MaxFrameHeaderLen))
	if err != nil || !d.consistent(loFi) {
		var ok bool
		lo, loFi, ok = d.nextCandidate(lo, lo+maxResync)
		if !ok {
			return 0, flac.FrameInfo{}, false
		}
	}
	if d.num.Start(loFi) > sample {
		return lo, loFi, true
	}
	hi := d.dataEnd
	for hi-lo > seekWindow {
		mid := lo + (hi-lo)/2
		off, fi, ok := d.nextCandidate(mid, hi)
		if !ok || d.num.Start(fi) > sample {
			hi = mid
			continue
		}
		lo, loFi = off, fi
	}
	return lo, loFi, true
}
