package mpc

import (
	"hash/crc32"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/container"
)

// SV8 framing. After "MPCK" the stream is packets: a two-letter key (A-Z),
// a variable-length size counting the key, the size field and the payload,
// then the payload. SH (stream header) comes first and is mandatory; RG, EI,
// SO, ST and CT are metadata; AP is one block of frames; SE ends the stream
// and tags follow it.

// sv8Stream is the packet walk's result and the seek scanner's cache.
type sv8Stream struct {
	count      uint64 // the header's sample count, 0 unknown
	begSilence uint64
	fpb        int64 // frames per full block
	lastFrames int   // frames in the final delivered block

	// blocks[k] is the offset of audio block k<<stride; the blocks between two
	// entries are reached by hopping packet headers.
	blocks  []int64
	stride  uint
	present int64 // audio blocks in the stream
	seAt    int64 // offset of the end marker, -1 when none
	soAt    int64 // offset the SO packet points at, -1 when none
	ctAt    int64 // offset of the chapter run, -1 when none
	stHead  int64 // offset of the ST packet's header, -1 when none

	// The seek scanner: rng[b] is the noise generator's two words at the start
	// of block b for every block scanned so far. Only the generator is state
	// across SV8 blocks, so that is all that is kept.
	rng  [][2]uint32
	grew bool
}

// sv8Block is one packet header.
type sv8Block struct {
	key     string
	hdrLen  int   // key plus size field
	payload int64 // payload bytes
}

// sv8Header reads the packet header at off. A key outside A-Z, a size that
// does not cover its own header, or a size that runs past the end of the
// stream is not a packet; the last rule is also what keeps every offset sum
// below the end, so none of them can wrap.
func (d *Demuxer) sv8Header(off int64) (sv8Block, bool) {
	b := d.w.BytesAt(off, 2+musepack.MaxVarintBytes)
	if len(b) < 3 {
		return sv8Block{}, false
	}
	for _, c := range b[:2] {
		if c < 'A' || c > 'Z' {
			return sv8Block{}, false
		}
	}
	size, n, ok := musepack.ReadVarint(b[2:])
	if !ok || size < uint64(2+n) || size > uint64(d.w.DataEnd()-off) {
		return sv8Block{}, false
	}
	return sv8Block{key: string(b[:2]), hdrLen: 2 + n, payload: int64(size) - int64(2+n)}, true
}

// parseSV8 reads the header packets up to the first audio block, peels the
// trailers, and walks every packet to the end marker.
func (d *Demuxer) parseSV8() error {
	s := &d.sv8
	*s = sv8Stream{seAt: -1, soAt: -1, ctAt: -1, stHead: -1}
	haveSH := false
	shEnd := int64(0)
	pos := d.base + 4
	var firstAP int64
	for {
		blk, ok := d.sv8Header(pos)
		if !ok {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			return malformed("no whole packet at offset %d before the first audio packet", pos)
		}
		if blk.key == "AP" {
			firstAP = pos
			break
		}
		if blk.payload > maxHeaderPacket {
			return malformed("%s packet of %d bytes exceeds the %d-byte bound", blk.key, blk.payload, maxHeaderPacket)
		}
		payload := d.w.BytesAt(pos+int64(blk.hdrLen), int(blk.payload))
		if int64(len(payload)) != blk.payload {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			return malformed("%s packet truncated", blk.key)
		}
		switch blk.key {
		case "SH":
			if err := d.parseSH(payload); err != nil {
				return err
			}
			haveSH = true
			shEnd = pos + int64(blk.hdrLen) + blk.payload
		case "RG":
			d.parseRG(payload)
		case "EI":
			d.parseEI(payload)
		case "SO":
			if ptr, _, ok := musepack.ReadVarint(payload); ok {
				s.soAt = pos + int64(ptr)
			}
		case "ST":
			s.stHead = pos
		}
		pos += int64(blk.hdrLen) + blk.payload
	}
	if !haveSH {
		return malformed("no stream header before the first audio packet")
	}

	d.stripTrailers(shEnd)
	if err := d.sv8Walk(firstAP); err != nil {
		return err
	}
	init := musepack.InitialState()
	d.sv8.rng = [][2]uint32{init.RNG}
	return nil
}

// parseSH reads the stream header packet: a CRC over the rest of it, the
// stream version, the sample count and beginning silence, then the shape.
func (d *Demuxer) parseSH(p []byte) error {
	if len(p) < 4 {
		return malformed("stream header of %d bytes", len(p))
	}
	r := musepack.NewBitReader(p)
	crc := r.Bits(32)
	if crc != crc32.ChecksumIEEE(p[4:]) {
		return malformed("stream header fails its CRC")
	}
	if v := r.Bits(8); v != 8 {
		return malformed("stream header version %d, want 8", v)
	}
	count, ok1 := r.Varint()
	silence, ok2 := r.Varint()
	if !ok1 || !ok2 {
		return malformed("stream header truncated")
	}
	freq := int(r.Bits(3))
	maxBand := int(r.Bits(5)) + 1
	channels := int(r.Bits(4)) + 1
	ms := r.Bits(1) != 0
	blockPwr := int(r.Bits(3)) * 2
	if r.Overrun() {
		return malformed("stream header truncated")
	}
	if musepack.Rates[freq] == 0 {
		return malformed("sample rate index %d is reserved", freq)
	}
	// A count above this is not a length any stream could deliver, and it
	// would overflow the sample arithmetic downstream.
	const maxCount = 1 << 40
	if count > maxCount || silence > count {
		return malformed("stream header declares %d samples with %d of beginning silence", count, silence)
	}
	d.cfg = musepack.Config{
		StreamVersion: 8,
		Rate:          musepack.Rates[freq],
		Channels:      channels,
		MaxBand:       maxBand,
		MS:            ms,
		BlockPwr:      blockPwr,
		PNS:           musepack.PNSUnknown,
		TrueGapless:   true,
	}
	if err := d.cfg.Validate(); err != nil {
		return err
	}
	d.sv8.count, d.sv8.begSilence = count, silence
	d.sv8.fpb = int64(d.cfg.FramesPerBlock())
	return nil
}

// parseRG reads the replay gain packet. Only version 1 is known; the reference
// ignores any other, and so does this.
func (d *Demuxer) parseRG(p []byte) {
	if len(p) < 9 || p[0] != 1 {
		return
	}
	r := musepack.NewBitReader(p[1:])
	d.rg = replayGain{
		titleGain: uint16(r.Bits(16)), titlePeak: uint16(r.Bits(16)),
		albumGain: uint16(r.Bits(16)), albumPeak: uint16(r.Bits(16)),
	}
}

// parseEI reads the encoder info packet, whose PNS flag is what decides
// whether a seek must walk the noise generator.
func (d *Demuxer) parseEI(p []byte) {
	if len(p) < 1 {
		return
	}
	d.cfg.PNS = p[0] & 1
}

// sv8Walk hops every packet from the first audio block to the end marker,
// recording the audio blocks' positions and the chapter run, and reconciles
// the header's sample count with the blocks present.
func (d *Demuxer) sv8Walk(first int64) error {
	s := &d.sv8
	pos := first
	end := d.w.DataEnd()
	for pos < end {
		blk, ok := d.sv8Header(pos)
		if !ok {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			// A truncated last packet lands here too: its size runs past the
			// end, so it is not a whole packet.
			if err := d.warn(pos, "%d trailing bytes are not a whole packet, dropped", end-pos); err != nil {
				return err
			}
			break
		}
		if blk.key == "SE" {
			s.seAt = pos
			break
		}
		switch blk.key {
		case "AP":
			if blk.payload > maxBlockBytes {
				return malformed("audio block of %d bytes exceeds the %d-byte bound", blk.payload, maxBlockBytes)
			}
			s.retain(pos)
			s.present++
			s.ctAt = -1
		case "CT":
			if s.ctAt < 0 {
				s.ctAt = pos
			}
		default:
			if blk.key == "ST" && pos == s.soAt {
				s.stHead = pos
			}
			s.ctAt = -1
		}
		pos += int64(blk.hdrLen) + blk.payload
		d.w.Trim(pos)
	}
	if s.seAt < 0 {
		if err := d.warn(end, "the stream has no end marker"); err != nil {
			return err
		}
		// The reference reads the chapter run that ends at the end marker;
		// with no marker there is no such run.
		s.ctAt = -1
	}
	if err := d.readChapters(); err != nil {
		return err
	}

	// The frame total follows from the count the way the reference decodes:
	// frames while decoded < count + delay, so ceil((count + 481) / 1152).
	d.track.Delay = musepack.SynthDelay + int64(s.begSilence)
	s.lastFrames = int(s.fpb)
	if s.count == 0 {
		return d.sv8UnknownLength()
	}
	frames := (int64(s.count) + musepack.SynthDelay + musepack.FrameLength - 1) / musepack.FrameLength
	blocks := (frames + s.fpb - 1) / s.fpb
	samples := int64(s.count - s.begSilence)
	switch {
	case s.present < blocks:
		deliverable := max(s.present*s.fpb*musepack.FrameLength-musepack.SynthDelay-int64(s.begSilence), 0)
		if err := d.warn(end, "the header declares %d samples in %d blocks but only %d blocks are present, delivering %d",
			samples, blocks, s.present, deliverable); err != nil {
			return err
		}
		d.total = s.present
		samples = deliverable
	default:
		d.total = blocks
		s.lastFrames = int(frames - (blocks-1)*s.fpb)
	}
	d.track.Samples = samples
	d.track.SamplesExact = true
	return nil
}

// sv8UnknownLength settles a stream whose header states no sample count (a
// shape no encoder writes). Every block but the last is full; the last one
// states nothing, so its frames are counted by parsing it, and the stream
// delivers everything its frames hold less the synthesis delay. A last block
// that does not parse is read as a full one and the length becomes advisory,
// which the decode then reports at that block.
func (d *Demuxer) sv8UnknownLength() error {
	s := &d.sv8
	d.total = s.present
	if s.present == 0 {
		d.track.Samples = 0
		d.track.SamplesExact = true
		return nil
	}
	last := int64(s.lastFrames)
	exact := true
	p, err := d.sv8Payload(s.present - 1)
	if err != nil {
		return err
	}
	if frames, err := musepack.CountSV8Frames(p, d.cfg); err == nil {
		last = int64(frames)
		s.lastFrames = frames
	} else {
		exact = false
		if err := d.warn(d.w.DataEnd(), "the stream states no length and its last block does not parse (%v); reading it as a full block", err); err != nil {
			return err
		}
	}
	d.track.Samples = max((s.present-1)*s.fpb*musepack.FrameLength+last*musepack.FrameLength-d.track.Delay, 0)
	d.track.SamplesExact = exact
	return nil
}

// retain records an audio block's position at the current stride, halving
// the table's density when it outgrows the cap, as the reference does.
func (s *sv8Stream) retain(off int64) {
	if s.present&(1<<s.stride-1) == 0 {
		s.blocks = append(s.blocks, off)
	}
	if len(s.blocks) > maxSeekEntriesKept {
		kept := s.blocks[:0]
		for i := 0; i < len(s.blocks); i += 2 {
			kept = append(kept, s.blocks[i])
		}
		s.blocks = kept
		s.stride++
	}
}

// sv8BlockAt returns the offset of audio block b, hopping from the nearest
// retained entry over whatever packets sit between blocks.
func (d *Demuxer) sv8BlockAt(b int64) (int64, error) {
	s := &d.sv8
	k := b >> s.stride
	off := s.blocks[k]
	for i := k << s.stride; i < b; {
		blk, ok := d.sv8Header(off)
		if !ok {
			if d.w.Err() != nil {
				return 0, d.w.Err()
			}
			return 0, malformed("audio block %d is no longer readable", i)
		}
		off += int64(blk.hdrLen) + blk.payload
		for {
			next, ok := d.sv8Header(off)
			if !ok {
				if d.w.Err() != nil {
					return 0, d.w.Err()
				}
				return 0, malformed("audio block %d is no longer readable", i+1)
			}
			if next.key == "AP" {
				break
			}
			off += int64(next.hdrLen) + next.payload
		}
		i++
	}
	return off, nil
}

// sv8Frames is how many frames block b delivers.
func (d *Demuxer) sv8Frames(b int64) int {
	if b == d.total-1 {
		return d.sv8.lastFrames
	}
	return int(d.sv8.fpb)
}

// sv8Payload returns block b's payload.
func (d *Demuxer) sv8Payload(b int64) ([]byte, error) {
	off, err := d.sv8BlockAt(b)
	if err != nil {
		return nil, err
	}
	blk, ok := d.sv8Header(off)
	if !ok || blk.key != "AP" {
		if d.w.Err() != nil {
			return nil, d.w.Err()
		}
		return nil, malformed("audio block %d is no longer readable", b)
	}
	d.w.Trim(off)
	p := d.w.BytesAt(off+int64(blk.hdrLen), int(blk.payload))
	if int64(len(p)) != blk.payload {
		if d.w.Err() != nil {
			return nil, d.w.Err()
		}
		return nil, malformed("audio block %d truncated", b)
	}
	return p, nil
}

// sv8Packet builds the packet for block b.
func (d *Demuxer) sv8Packet(b int64, pkt *container.Packet) error {
	p, err := d.sv8Payload(b)
	if err != nil {
		return err
	}
	frames := d.sv8Frames(b)
	head := d.packetHeader(frames, 0)
	d.pkt = append(d.pkt[:head], p...)
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: d.pkt,
			PTS:  b * d.sv8.fpb * musepack.FrameLength,
			Dur:  int64(frames) * musepack.FrameLength,
			// Every block starts with a key frame, but the noise generator
			// runs through blocks unreset, so a block is an exact resume point
			// only when nothing draws from it or the state comes with it, the
			// same rule the SV7 packets follow.
			Sync: b == 0 || d.pending != nil || d.cfg.PNS == 0,
		},
	}
	return nil
}

// sv8Advance scans block b on top of st.
func (d *Demuxer) sv8Advance(st *musepack.State, b int64) error {
	p, err := d.sv8Payload(b)
	if err != nil {
		return err
	}
	return musepack.ScanSV8Block(p, d.sv8Frames(b), d.cfg, st)
}

// sv8StateAt returns the noise-generator state at the start of block b. With
// noise substitution declared off nothing ever draws from it, so the state is
// the initial one; otherwise (on, or undeclared) the blocks before b are
// scanned, incrementally and remembered.
//
// The declaration is the encoder's, and it is trusted the way a LAME tag or
// an iTunSMPB atom is: both encoders' declarations agree with their streams
// on every cell (TestNoiseDeclarationMatchesUse), and a stream that lied would
// resume after a seek with a different noise realisation, not wrong audio.
func (d *Demuxer) sv8StateAt(b int64) (musepack.State, error) {
	s := &d.sv8
	if d.cfg.PNS == 0 {
		return musepack.InitialState(), nil
	}
	for int64(len(s.rng)) <= b {
		i := int64(len(s.rng)) - 1
		st := musepack.State{RNG: s.rng[i]}
		if err := d.sv8Advance(&st, i); err != nil {
			return musepack.State{}, err
		}
		s.rng = append(s.rng, st.RNG)
		s.grew = true
	}
	return musepack.State{RNG: s.rng[b]}, nil
}
