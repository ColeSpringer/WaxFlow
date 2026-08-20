package apen

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"slices"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/id3"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/internal/trailer"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
	_ container.Tagger  = (*Demuxer)(nil)
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxJunk bounds the scan for the file header. A tagger can leave an
	// ID3v2 tag and its padding in front of one; the reference scans a
	// megabyte for the same reason.
	maxJunk = 1 << 20
	// maxHeaderCandidates bounds how many parsable-looking headers inside that
	// megabyte are tried before the file is called unreadable, so a crafted
	// run of them cannot turn the scan into a header-parsing loop. It counts
	// parse attempts, not scan steps: the scan itself covers the whole
	// megabyte byte by byte, the way the reference does.
	maxHeaderCandidates = 1 << 10
	// maxSeekTableBytes bounds the seek table. Four million entries is a
	// hundred hours of the shortest frames the format uses.
	maxSeekTableBytes = 16 << 20
	// frameSlack is how far past its last byte a frame is read. The range
	// coder runs a few bytes ahead of the values it has produced, so the
	// reference hands it the same slack; past the end of the audio the bytes
	// are zero and the decoder never reaches them.
	frameSlack = 4
	// maxWarnings caps the tolerated-damage list.
	maxWarnings = 64
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads one Monkey's Audio track from a native .ape source.
type Demuxer struct {
	opts DemuxerOptions

	hdr      ape.Header
	track    container.Track
	tags     map[string][]string
	warnings []container.Warning

	// base is the byte offset of the file header: nonzero when a tag or
	// padding sits in front of it. Every offset the header states is measured
	// from here.
	base int64
	// seek is the frame table, absolute file offsets, one per frame plus a
	// synthetic end so frame i spans [seek[i], seek[i+1]).
	seek []int64

	frame int // the next frame to deliver
	pkt   []byte
	// src is the raw source, for the frame reads that bypass the window;
	// w stages the header and seek table, which is all it is needed for.
	src container.Source
	w   srcwin.Window
}

// NewDemuxer parses the header of a native Monkey's Audio source and positions
// on the first frame. The returned Demuxer implements container.Seeker,
// container.Warner, and container.Tagger.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, w: srcwin.New(src, src.Size(), "ape: reading the file header")}
	if opts != nil {
		d.opts = *opts
	}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "ape: "+fmt.Sprintf(format, args...))
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return malformed("%s (at offset %d)", msg, off)
	}
	w := container.Warning{Offset: off, Msg: msg}
	if len(d.warnings) < maxWarnings && !slices.Contains(d.warnings, w) {
		d.warnings = append(d.warnings, w)
	}
	return nil
}

func (d *Demuxer) parse() error {
	if err := d.stripTrailers(); err != nil {
		return err
	}
	base, hdr, err := d.findHeader()
	if err != nil {
		return err
	}
	d.base, d.hdr = base, hdr

	if err := d.readSeekTable(); err != nil {
		return err
	}

	cfg := hdr.Config()
	f := cfg.Format()
	if err := f.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "ape: unusable format", err)
	}
	blob, err := cfg.MarshalBinary()
	if err != nil {
		return err
	}
	d.track = container.Track{
		Codec:       codec.APE,
		CodecConfig: blob,
		Fmt:         f,
		Samples:     hdr.Samples(),
		// The length is arithmetic on the same three header fields that size
		// the frames, so the two cannot disagree and calling it exact costs
		// nothing. What it buys is a stream truncated mid-file ending at the
		// length the header states rather than wherever the frames ran out.
		SamplesExact: true,
		Default:      true,
	}
	return d.w.Err()
}

// findHeader locates the file header, allowing for a tag or padding in front
// of it, and parses it.
func (d *Demuxer) findHeader() (int64, ape.Header, error) {
	from := id3.Size(d.w.BytesAt(0, 10))
	limit := min(from+maxJunk, d.w.DataEnd())
	var firstErr error
	tries := 0
	for off := from; off < limit; tries++ {
		if tries >= maxHeaderCandidates {
			return 0, ape.Header{}, malformed("no Monkey's Audio header in the first %d candidates", tries)
		}
		cand, ok := d.nextMagic(off, limit)
		if !ok {
			break
		}
		if h, err := ape.ParseHeader(d.w.BytesAt(cand, ape.MaxHeaderLen)); err == nil {
			return cand, h, nil
		} else if firstErr == nil {
			firstErr = err
		}
		off = cand + 1
	}
	if d.w.Err() != nil {
		return 0, ape.Header{}, d.w.Err()
	}
	if firstErr != nil {
		return 0, ape.Header{}, firstErr
	}
	return 0, ape.Header{}, malformed("no Monkey's Audio header in the first %d bytes", limit-from)
}

// nextMagic returns the next offset in [from, limit) carrying the file magic.
// It scans a window at a time and searches the whole magic, since the three
// bytes it starts with are an ordinary word that a tag comment or a cover
// image will spell.
func (d *Demuxer) nextMagic(from, limit int64) (int64, bool) {
	for off := from; off < limit; {
		b := d.w.BytesAt(off, srcwin.Chunk)
		// A window too short to hold the magic ends the scan, and it also
		// keeps the step below positive.
		if len(b) < ape.MatchNeed {
			return 0, false
		}
		for rel := 0; rel+ape.MatchNeed <= len(b); {
			i := bytes.Index(b[rel:], ape.Magic)
			if i < 0 {
				break
			}
			rel += i
			if cand := off + int64(rel); cand < limit && ape.Match(b[rel:]) {
				return cand, true
			}
			rel++
		}
		// Step back by the magic length so one straddling the window boundary
		// is not lost between passes.
		off += int64(len(b)) - int64(ape.MatchNeed) + 1
	}
	return 0, false
}

// stripTrailers peels the tags a tagger appended after the audio, shrinking
// the window's data end so the frame extents never run into them. The APEv2
// block is also where the track's tags come from.
func (d *Demuxer) stripTrailers() error {
	// The tag has to leave something behind it, which is all the floor can say
	// here: the file header has not been found yet. NUL padding is not peeled
	// either, since the last frame runs to the end of the audio and the range
	// coder's own trailing bytes can be zeros.
	end, tags := trailer.PeelAll(&d.w, trailer.APEv2|trailer.ID3v1|trailer.ID3v2, 1, d.w.DataEnd())
	d.tags = tags
	// No warning: a tag after the audio is the normal shape of a .ape file
	// rather than damage recovered from, and warning would make strict mode
	// reject every tagged one.
	d.w.SetDataEnd(end)
	return d.w.Err()
}

// readSeekTable reads and validates the frame index. Every entry is checked
// against the file's own bounds up front, so the packet and seek paths can
// treat the table as trustworthy.
func (d *Demuxer) readSeekTable() error {
	h := d.hdr
	n := h.TotalFrames
	if int64(h.SeekTableEntries)*4 > maxSeekTableBytes {
		return malformed("seek table of %d entries exceeds the %d-byte bound", h.SeekTableEntries, maxSeekTableBytes)
	}
	raw := d.w.BytesAt(d.base+h.SeekTableOffset, n*4)
	if len(raw) != n*4 {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("seek table truncated: %d of %d bytes", len(raw), n*4)
	}

	// The table's own entries are 32-bit, so a file past four gigabytes wraps
	// them; the reference recovers the high bits by watching for an entry
	// that went backwards, which a real table never does. They are also
	// measured from the file header rather than from the file, so a tag
	// somebody put in front of one shifts every frame with it.
	d.seek = make([]int64, n+1)
	var carry int64
	prev := uint32(0)
	for i := range n {
		cur := binary.LittleEndian.Uint32(raw[i*4:])
		if i > 0 && cur < prev {
			carry += 1 << 32
		}
		d.seek[i] = d.base + carry + int64(cur)
		prev = cur
	}
	// The synthetic last entry is where the audio ends: the file's end, less
	// the source file's trailing chunks and any tag already peeled off. That
	// is the reference's own answer for a file it can measure; it falls back
	// to the descriptor's byte count only when it cannot see a size at all.
	d.seek[n] = d.w.DataEnd() - h.TerminatingBytes

	// The 3980-and-later descriptor counts those bytes too, so the two
	// disagreeing is the one signal a truncated or padded file gives. Neither
	// number can be repaired from the other, so the file is read as it stands
	// and the disagreement is reported: too few bytes means the last frame
	// will fail its own CRC, and a caller that would rather not find out
	// mid-stream asks for strict mode.
	if h.FrameDataBytes >= 0 {
		present := d.seek[n] - (d.base + h.FrameDataOffset)
		if present != h.FrameDataBytes {
			if err := d.warn(d.seek[n], "the descriptor counts %d bytes of frame data but %d are present",
				h.FrameDataBytes, present); err != nil {
				return err
			}
		}
	}

	// The first entry says where the frames start and the header says the
	// same thing; disagreement means one of them is wrong, and neither is
	// checkable against the other's evidence, so the file is refused.
	if want := d.base + h.FrameDataOffset; d.seek[0] != want {
		return malformed("seek table starts at %d but the header puts the frames at %d", d.seek[0], want)
	}
	maxFrame := d.maxFrameBytes()
	for i := range n {
		switch {
		case d.seek[i+1] < d.seek[i]:
			return malformed("frame %d ends at %d, before its start at %d", i, d.seek[i+1], d.seek[i])
		case d.seek[i+1] > d.w.DataEnd():
			return malformed("frame %d runs to %d, past the %d bytes of audio", i, d.seek[i+1], d.w.DataEnd())
		case d.seek[i+1]-d.seek[i] > maxFrame:
			return malformed("frame %d of %d bytes exceeds the %d-byte bound", i, d.seek[i+1]-d.seek[i], maxFrame)
		}
	}
	return nil
}

// maxFrameBytes bounds one frame's coded size. A frame is legitimately large
// at the deep levels, which multiply the block count, so the bound is built
// from the stream's own shape rather than picked: twice what the frame's
// samples occupy uncompressed, plus room for a short frame's overhead.
func (d *Demuxer) maxFrameBytes() int64 {
	h := d.hdr
	raw := int64(h.BlocksPerFrame) * int64(h.Channels) * int64(h.BitsPerSample/8)
	return 2*raw + 1<<16
}

// Tracks returns the single APE track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// Tags returns the APEv2 tag's fields under canonical uppercase keys.
func (d *Demuxer) Tags() map[string][]string { return d.tags }

// ReadPacket yields one frame, with the block count and alignment the decoder
// needs in front of it. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	if d.frame >= d.hdr.TotalFrames {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return io.EOF
	}
	i := d.frame
	// The range coder reads 32-bit words anchored at the start of the frame
	// data, and a frame begins wherever in a word the one before it ended, so
	// the read starts at the word boundary below it and the decoder skips the
	// bytes in between.
	skip := int((d.seek[i] - d.seek[0]) & 3)
	from := d.seek[i] - int64(skip)
	to := min(d.seek[i+1]+frameSlack, d.w.DataEnd())
	blocks := d.hdr.FrameBlocks(i)
	// Straight from the source into the packet. The seek table is exact, so a
	// read-ahead window would only stage the same bytes a second time, and a
	// frame runs to megabytes at the deep levels.
	n := ape.FrameHeaderLen + int(to-from)
	if cap(d.pkt) < n {
		d.pkt = make([]byte, n)
	}
	d.pkt = d.pkt[:n]
	ape.PutFrameHeader(d.pkt, blocks, skip)
	if err := container.ReadFull(d.src, d.pkt[ape.FrameHeaderLen:], from); err != nil {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "ape: reading frame data", err)
	}

	d.frame++
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: d.pkt,
			PTS:  int64(i) * int64(d.hdr.BlocksPerFrame),
			Dur:  int64(blocks),
			Sync: true,
		},
	}
	return nil
}

// SeekSample repositions to the frame containing the target sample and returns
// that frame's first sample; format.Media pre-rolls the remainder. The seek
// table makes this exact arithmetic rather than a search, at the cost of a
// frame's worth of pre-roll, which the deep levels make long.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("ape: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "ape: negative seek target")
	}
	frame := sample / int64(d.hdr.BlocksPerFrame)
	// A target past the end lands on the last frame, where reads resume and
	// immediately run out, which is what every other demuxer here does.
	frame = min(frame, int64(d.hdr.TotalFrames-1))
	d.frame = int(frame)
	return frame * int64(d.hdr.BlocksPerFrame), nil
}
