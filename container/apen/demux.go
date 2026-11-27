package apen

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/container/internal/diag"
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

	_ container.Truncated = (*Demuxer)(nil)
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
	// minFrameBytes is the shortest frame the format can express: a CRC word,
	// a byte of coded data and the range coder's four-byte flush.
	minFrameBytes = 9
	// frameSlack is how far past its last byte a frame is read. The range
	// coder runs a few bytes ahead of the values it has produced, so the
	// reference hands it the same slack; past the end of the audio the bytes
	// are zero and the decoder never reaches them.
	frameSlack = 4
)

// DemuxerVersion is this demuxer's term in the ADR-0004 cache key, carried
// by its track. Bump it for a change that makes the demuxer deliver other
// samples or tags for a file that has not changed.
const DemuxerVersion = "apen-demux-2"

// Demuxer reads one Monkey's Audio track from a native .ape source.
type Demuxer struct {
	hdr   ape.Header
	track container.Track
	tags  map[string][]string
	log   diag.Log

	// base is the byte offset of the file header: nonzero when a tag or
	// padding sits in front of it. Every offset the header states is measured
	// from here.
	base int64
	// floor is the lowest offset a trailing tag may start at.
	floor int64
	// seek is the frame table, absolute file offsets, one per frame plus a
	// synthetic end so frame i spans [seek[i], seek[i+1]).
	seek []int64

	// frames is how many frames a read delivers, which is all of them in a
	// file that is whole. cut says the descriptor counts frame data the file
	// does not hold, so the last frame that starts inside the audio is short
	// of its bytes: the open tries it and drops it where it does not decode.
	// truncated is what the header declares past the frames delivered
	// (container.Truncated).
	frames    int
	cut       bool
	truncated int64
	// decodedTo is where a first-frame extent ended that the trailer peel
	// decoded whole, 0 for none, so the cut check does not decode it again.
	decodedTo int64

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
func NewDemuxer(src container.Source, opts *container.DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, w: srcwin.New(src, src.Size(), "ape: reading the file header"), log: diag.New("ape: ", opts)}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("ape: ", format, args...)
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	return d.log.Warn(off, format, args...)
}

func (d *Demuxer) parse() error {
	base, hdr, err := d.findHeader()
	if err != nil {
		return err
	}
	d.base, d.hdr = base, hdr

	if err := d.stripTrailers(); err != nil {
		return err
	}
	if err := d.readSeekTable(); err != nil {
		return err
	}
	d.tagBehindJunk()
	if d.cut {
		if err := d.tryCutFrame(); err != nil {
			return err
		}
	}

	cfg := hdr.Config()
	f := cfg.Format()
	if err := f.Valid(); err != nil {
		return container.UnusableFormat("ape", f, err)
	}
	blob, err := cfg.MarshalBinary()
	if err != nil {
		return err
	}
	samples := hdr.Samples()
	if d.frames < hdr.TotalFrames {
		// A cut file: the frames in front of the first one that is gone, or
		// that no longer decodes.
		samples = int64(d.frames) * int64(hdr.BlocksPerFrame)
		d.truncated = hdr.Samples() - samples
	}
	d.track = container.Track{
		Codec:          codec.APE,
		CodecConfig:    blob,
		Fmt:            f,
		DemuxerVersion: DemuxerVersion,
		Samples:        samples,
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
// of it, and parses it. A leading tag that states more bytes than the data
// holds is refused by name, since no header can follow it.
func (d *Demuxer) findHeader() (int64, ape.Header, error) {
	from := id3.Size(d.w.Peek(0, 10))
	if end := d.w.DataEnd(); from > end {
		return 0, ape.Header{}, malformed("leading ID3v2 tag declares %d bytes, past the %d the data holds", from, end)
	}
	return d.headerIn(from, from+maxJunk)
}

// headerIn parses the first file header in [from, limit) that parses, of the
// first maxHeaderCandidates magics, keeping the first parse error. The magic's
// letters are a word a tag will spell, so Match is asked about the byte behind.
func (d *Demuxer) headerIn(from, limit int64) (int64, ape.Header, error) {
	limit = min(limit, d.w.DataEnd())
	// The header is where it belongs in every file that is not damaged, so
	// it is read there exactly: the scan then finds it resident and a clean
	// open pays no span.
	if from < limit && ape.Match(d.w.Peek(from, ape.MatchNeed)) {
		d.w.Peek(from, ape.MaxHeaderLen)
	}
	var (
		hdr      ape.Header
		firstErr error
		tries    int
		capped   bool
	)
	off, ok := d.w.Scan(from, limit, ape.Magic, ape.MatchNeed, func(off int64, magic []byte) srcwin.Verdict {
		if !ape.Match(magic) {
			return srcwin.Skip
		}
		h, err := ape.ParseHeader(d.w.Ahead(off, ape.MaxHeaderLen))
		if err == nil {
			hdr = h
			return srcwin.Take
		}
		if firstErr == nil {
			firstErr = err
		}
		if tries++; tries >= maxHeaderCandidates && off+1 < limit {
			capped = true
			return srcwin.Stop
		}
		return srcwin.Skip
	})
	switch {
	case ok:
		return off, hdr, nil
	case d.w.Err() != nil:
		return 0, ape.Header{}, d.w.Err()
	case capped:
		return 0, ape.Header{}, malformed("no Monkey's Audio header in the first %d candidates", tries)
	case firstErr != nil:
		return 0, ape.Header{}, firstErr
	}
	return 0, ape.Header{}, malformed("no Monkey's Audio header in the first %d bytes", limit-from)
}

// stripTrailers peels the tags a tagger appended after the audio, shrinking
// the window's data end so the frame extents never run into them. The APEv2
// block is also where the track's tags come from.
//
// A tag starts no lower than where the first frame ends when that frame is
// whole, which is what keeps a footer's declared length from cutting into
// it. In a file cut inside its first frame the tags sit lower, where the cut
// fell. So trailers that reach below that point are looked for too, and the
// frame's own CRC says whose the bytes are. A frame that does not decode
// with the bytes a whole one has is cut, and the trailers are its tags. One
// that does is whole, and what claims its bytes is no tag, unless it decodes
// as well without them: a cut inside the last bytes of a frame, which the
// coder never reads, leaves a frame that decodes either way, and tags there
// are tags. At most two decodes, of a frame within the size a frame may
// have, and only where a trailer reaches below a whole first frame's end.
func (d *Demuxer) stripTrailers() error {
	// NUL padding is not peeled, since the last frame runs to the end of the
	// audio and the range coder's own trailing bytes can be zeros. The probes
	// read the tail through a window of their own, so the seek table still
	// extends the read the header began.
	const want = trailer.APEv2 | trailer.ID3v1 | trailer.ID3v2 | trailer.Lyrics3
	data := d.base + d.hdr.FrameDataOffset
	floor, known := d.firstFrameEnd()
	end, refused, bad := d.w.DataEnd(), int64(0), false
	var tags map[string][]string
	var readErr error
	if _, err := d.w.Try(false, func() bool {
		end, tags = trailer.PeelAll(&d.w, want, floor, end)
		// A first frame past the size a frame may have is refused once the
		// seek table is read, and is not staged to be tried here.
		if low := data + minFrameBytes; known && low < floor && floor-data <= d.maxFrameBytes() {
			if lowEnd, lowTags := trailer.PeelAll(&d.w, want, low, end); lowEnd < end {
				var theirs bool
				if theirs, readErr = d.tagsBelow(floor, end, lowEnd); readErr != nil {
					return true
				}
				if theirs {
					end = lowEnd
					if tags == nil {
						tags = lowTags
					}
				}
			}
		}
		refused, bad = trailer.Refused(&d.w, floor, end)
		return true
	}); err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	d.tags, d.floor = tags, floor
	// No warning for a tag: one after the audio is the normal shape of a .ape
	// file rather than damage recovered from, and warning would make strict
	// mode reject every tagged one. A footer that points into the first frame
	// is not a tag, and stays.
	if bad {
		if err := d.warn(end-apev2.FooterLen, "an APEv2 footer places its tag at offset %d, inside the first frame; not read", d.log.At(refused)); err != nil {
			return err
		}
	}
	d.w.SetDataEnd(end)
	return nil
}

// firstFrameEnd is where the first frame ends when it is whole, held at the
// data's end, by the seek table's second entry or, for a lone frame, the
// descriptor's byte count. With neither, known is false and the answer is
// the frame data's second byte: something has to be left behind a tag.
func (d *Demuxer) firstFrameEnd() (end int64, known bool) {
	h := d.hdr
	data := d.base + h.FrameDataOffset
	switch {
	case h.TotalFrames > 1:
		if e := d.w.Peek(d.base+h.SeekTableOffset, 8); len(e) == 8 {
			first, second := binary.LittleEndian.Uint32(e), binary.LittleEndian.Uint32(e[4:])
			if d.base+int64(first) == data && second > first {
				return min(d.base+int64(second), d.w.DataEnd()), true
			}
		}
	case h.FrameDataBytes > 0:
		return d.frameDataEnd(), true
	}
	return data + 1, false
}

// tagsBelow reports whether the trailers that start at tagStart, below where
// a whole first frame ends, are tags (see stripTrailers): the frame does not
// decode with a whole one's bytes, of which limit are there, or it decodes
// without theirs too.
func (d *Demuxer) tagsBelow(whole, limit, tagStart int64) (bool, error) {
	ok, err := d.firstFrameDecodes(whole, limit)
	if err != nil || !ok {
		return err == nil, err
	}
	ok, err = d.firstFrameDecodes(tagStart, tagStart)
	if ok {
		d.decodedTo = tagStart
	}
	return ok, err
}

// firstFrameDecodes reports whether the first frame decodes from the bytes
// up to end, with limit the bytes there are.
func (d *Demuxer) firstFrameDecodes(end, limit int64) (bool, error) {
	data := d.base + d.hdr.FrameDataOffset
	if err := d.stageBytes(data, end, limit, data, d.hdr.FrameBlocks(0)); err != nil {
		return false, err
	}
	return d.stagedDecodes(), nil
}

// frameDataEnd is where the descriptor says the frame data ends, held at the
// end of the data: the count is 64 bits a file states, and a sum past what
// the file holds is no offset. Only a header that carries the count asks.
func (d *Demuxer) frameDataEnd() int64 {
	data, end := d.base+d.hdr.FrameDataOffset, d.w.DataEnd()
	if d.hdr.FrameDataBytes > end-data {
		return max(end, data)
	}
	return data + d.hdr.FrameDataBytes
}

// tagBehindJunk reads the APEv2 tag that bytes no peel recognizes hide from
// it, when the peel found none and the frame data stops short of the data's
// end. A header that states no length for it (before 3980) cannot say that
// it does, and is not searched. The seek table still says where the frames
// lie. A read that fails is no answer.
func (d *Demuxer) tagBehindJunk() {
	if d.tags != nil || d.hdr.FrameDataBytes < 0 {
		return
	}
	lo := max(d.floor, d.frameDataEnd()+d.hdr.TerminatingBytes)
	if d.w.DataEnd()-lo < apev2.FooterLen {
		return
	}
	d.w.Try(false, func() bool {
		d.tags, _, _ = trailer.Behind(&d.w, lo, d.w.DataEnd())
		return true
	})
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
	raw := d.w.Peek(d.base+h.SeekTableOffset, n*4)
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
	// and the disagreement is reported. Too few bytes is a file cut short:
	// the frame the cut fell in is tried by the read that reaches it and
	// dropped where it does not decode, and Strict refuses the file here.
	d.frames = n
	if h.FrameDataBytes >= 0 {
		present := d.seek[n] - (d.base + h.FrameDataOffset)
		if present != h.FrameDataBytes {
			if err := d.warn(d.seek[n], "the descriptor counts %d bytes of frame data but %d are present",
				h.FrameDataBytes, present); err != nil {
				return err
			}
		}
		if present < h.FrameDataBytes {
			// Every byte left, up to where the frame data was to end, is frame
			// data, whatever the header says trails it; and a frame that
			// starts at or past the end is not there at all.
			d.cut = true
			end := d.frameDataEnd()
			for d.frames > 1 && d.seek[d.frames-1] >= end {
				d.frames--
			}
			d.seek = d.seek[:d.frames+1]
			d.seek[d.frames] = end
			if d.frames < n {
				if err := d.warn(end, "the audio ends before frame %d of %d", d.frames, n); err != nil {
					return err
				}
			}
		}
	}

	// The first entry says where the frames start and the header says the
	// same thing; disagreement means one of them is wrong, and neither is
	// checkable against the other's evidence, so the file is refused.
	if want := d.base + h.FrameDataOffset; d.seek[0] != want {
		return malformed("seek table starts at %d but the header puts the frames at %d", d.log.At(d.seek[0]), d.log.At(want))
	}
	maxFrame := d.maxFrameBytes()
	for i := range d.frames {
		switch {
		case d.seek[i+1] < d.seek[i]:
			return malformed("frame %d ends at %d, before its start at %d", i, d.log.At(d.seek[i+1]), d.log.At(d.seek[i]))
		case d.seek[i+1] == d.seek[i]:
			// A frame is never empty: it opens with a CRC word and closes
			// with the range coder's four-byte flush, so the shortest one the
			// format can express is nine bytes. This is what a file truncated
			// exactly at its last frame's first byte looks like, since the
			// table's synthetic end entry then ties with the one before it,
			// and it is the only way an entry can equal its predecessor.
			// Refusing here is what keeps the packet the demuxer emits one
			// the decoder can parse.
			return malformed("frame %d holds no bytes: the audio ends where it starts, at %d", i, d.log.At(d.seek[i]))
		case d.seek[i+1] > d.w.DataEnd():
			return malformed("frame %d runs to %d, past the %d bytes of audio", i, d.log.At(d.seek[i+1]), d.log.At(d.w.DataEnd()))
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
func (d *Demuxer) Warnings() []container.Warning { return d.log.List() }

// DamageCount implements container.Damaged.
func (d *Demuxer) DamageCount() int { return d.log.DamageCount() }

// TruncatedSamples implements container.Truncated.
func (d *Demuxer) TruncatedSamples(int) int64 { return d.truncated }

// Tags returns the APEv2 tag's fields under canonical uppercase keys.
func (d *Demuxer) Tags() map[string][]string { return d.tags }

// ReadPacket yields one frame, with the block count and alignment the decoder
// needs in front of it. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	if d.frame >= d.frames {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return io.EOF
	}
	i := d.frame
	blocks, err := d.stage(i)
	if err != nil {
		return err
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

// stage reads frame i into d.pkt behind the header the decoder needs, and
// returns its block count.
func (d *Demuxer) stage(i int) (int, error) {
	blocks := d.hdr.FrameBlocks(i)
	return blocks, d.stageBytes(d.seek[i], d.seek[i+1], d.w.DataEnd(), d.seek[0], blocks)
}

// stageBytes reads the frame of blocks blocks at [start, end) into d.pkt
// behind the header the decoder needs, with the slack the coder runs ahead
// by where limit leaves it. first is where the frame data begins.
func (d *Demuxer) stageBytes(start, end, limit, first int64, blocks int) error {
	// The range coder reads 32-bit words anchored at the start of the frame
	// data, and a frame begins wherever in a word the one before it ended, so
	// the read starts at the word boundary below it and the decoder skips the
	// bytes in between.
	if end < start || end-start > d.maxFrameBytes() {
		return malformed("frame of %d bytes exceeds the %d-byte bound", end-start, d.maxFrameBytes())
	}
	skip := int((start - first) & 3)
	from := start - int64(skip)
	to := min(end+frameSlack, limit)
	// Straight from the source into the packet. The seek table is exact, so a
	// read-ahead window would only stage the same bytes a second time, and a
	// frame runs to megabytes at the deep levels.
	n := ape.FrameHeaderLen + int(to-from)
	if cap(d.pkt) < n {
		d.pkt = make([]byte, n)
	}
	d.pkt = d.pkt[:n]
	ape.PutFrameHeader(d.pkt, blocks, skip, int(end-start))
	if err := container.ReadFull(d.src, d.pkt[ape.FrameHeaderLen:], from); err != nil {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "ape: reading frame data", err)
	}
	return nil
}

// stagedDecodes reports whether the frame staged in d.pkt decodes, its CRC
// included.
func (d *Demuxer) stagedDecodes() bool {
	cfg := d.hdr.Config()
	dec, err := ape.NewDecoder(cfg, cfg.Format())
	if err != nil {
		return false
	}
	defer dec.Release()
	return dec.Decode(d.pkt, func(*audio.Buffer) error { return nil }) == nil
}

// tryCutFrame decodes the frame a cut fell in, at open, and drops it where it
// does not decode, so the length the open states is the one a read delivers.
// A frame carries a CRC over its samples and no length, so trying it is the
// only way to tell one that lost bytes its coder never read from one that
// lost audio. Only a cut file pays for it.
func (d *Demuxer) tryCutFrame() error {
	last := d.frames - 1
	if last == 0 && d.seek[1] == d.decodedTo {
		return nil // the peel already decoded these bytes
	}
	if _, err := d.stage(last); err != nil {
		return err
	}
	if d.stagedDecodes() {
		return nil
	}
	d.frames = last
	return d.warn(d.seek[last], "the last frame is cut short and does not decode, dropped")
}

// SeekSample repositions to the frame containing the target sample and returns
// that frame's first sample; format.Media pre-rolls the remainder. The seek
// table makes this exact arithmetic rather than a search, at the cost of a
// frame's worth of pre-roll, which the deep levels make long.
func (d *Demuxer) SeekSample(_ context.Context, track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("ape: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "ape: negative seek target")
	}
	frame := sample / int64(d.hdr.BlocksPerFrame)
	// A target past the end lands on the last frame, where reads resume and
	// immediately run out, which is what every other demuxer here does.
	frame = max(min(frame, int64(d.frames-1)), 0)
	d.frame = int(frame)
	return frame * int64(d.hdr.BlocksPerFrame), nil
}
