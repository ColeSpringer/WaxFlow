package mpc

import (
	"encoding/binary"
	"math"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/container"
)

// SV7 framing. The stream after the magic is little-endian 32-bit words read
// most-significant-bit first, so bit b of the stream is bit 7-(b&7) of the byte
// at (b>>3)^3 within its word. The 28-byte header (magic plus six words) ends
// with the 8-bit encoder version, and the first frame's 20-bit length field
// follows it at bit 200. Frames are bit-packed with no alignment; after the
// header's last frame comes an 11-bit copy of the last frame's sample count,
// and when that count exceeds what the synthesis delay leaves in the final
// frame, one more frame (the decay frame) with its own length.

const (
	sv7HeaderLen  = 28
	sv7FirstFrame = 200
	// minFrameBits is the shortest frame the syntax allows: the two 4-bit
	// resolutions of band zero. A length field below it is damage.
	minFrameBits = 8
	// decayThreshold is the last-frame sample count above which the reference
	// decodes one frame past the header's count: the frame's samples less the
	// 481-sample synthesis delay do not fit in the frame before it.
	decayThreshold = musepack.FrameLength - musepack.SynthDelay
)

// sv7Stream is the hop walk's result and the seek scanner's cache.
type sv7Stream struct {
	frames  int64 // the header's frame count
	endBits int64 // the stream's whole-word length in bits, from base

	// checkpoints[k] is the bit position of frame k*checkpointFrames's length
	// field; frames in between are reached by hopping their lengths.
	checkpoints []int64
	// cursor is the frame whose length field sits at cursorPos, remembered
	// after every lookup so a sequential read hops nothing.
	cursor    int64
	cursorPos int64

	// The seek scanner: states[k] is the decoder state before frame
	// k*checkpointFrames, scanState the state before frame scanned.
	states    []musepack.State
	scanned   int64
	scanState musepack.State
	grew      bool
	scratch   []byte
}

// parseSV7 reads the header, peels trailers, and walks every frame.
func (d *Demuxer) parseSV7() error {
	h := d.w.BytesAt(d.base, sv7HeaderLen)
	if len(h) < sv7HeaderLen {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("SV7 header truncated")
	}
	frames := int64(binary.LittleEndian.Uint32(h[4:]))
	w2 := binary.LittleEndian.Uint32(h[8:])
	w3 := binary.LittleEndian.Uint32(h[12:])
	w4 := binary.LittleEndian.Uint32(h[16:])
	w5 := binary.LittleEndian.Uint32(h[20:])
	maxBand := int(w2 >> 24 & 0x3F)
	freq := int(w2 >> 16 & 3)
	last := int(w5 >> 20 & 0x7FF)
	switch {
	case maxBand < 1 || maxBand > musepack.MaxBands-1:
		return malformed("max band %d outside 1..31", maxBand)
	case last > musepack.FrameLength:
		return malformed("header states %d samples in the last frame, more than a frame holds", last)
	}
	pns := uint8(0)
	if h[3]>>4 != 0 {
		pns = 1
	}
	d.cfg = musepack.Config{
		StreamVersion:    7,
		Rate:             musepack.Rates[freq],
		Channels:         2,
		MaxBand:          maxBand,
		MS:               w2>>30&1 != 0,
		PNS:              pns,
		TrueGapless:      w5>>31 != 0,
		LastFrameSamples: last,
	}
	if err := d.cfg.Validate(); err != nil {
		return err
	}
	d.rg = sv7ReplayGain(uint16(w3>>16), uint16(w3), uint16(w4>>16), uint16(w4))
	d.sv7 = sv7Stream{frames: frames, cursor: -1}

	d.stripTrailers(d.base + sv7HeaderLen)
	d.sv7.endBits = ((d.w.DataEnd() - d.base) &^ 3) * 8
	if err := d.sv7Walk(); err != nil {
		return err
	}
	d.sv7.states = []musepack.State{musepack.InitialState()}
	d.sv7.scanState = d.sv7.states[0]
	return nil
}

// sv7Walk hops every frame's length field to the end of the stream. It is the
// only way to confirm an SV7 length (a frame states no position), it is where
// the header's last-frame count is checked against the stream's, and it
// records the checkpoint positions the seek path hops from.
func (d *Demuxer) sv7Walk() error {
	s := &d.sv7
	pos := int64(sv7FirstFrame)
	present := int64(0)
	for present < s.frames {
		if present%checkpointFrames == 0 {
			s.checkpoints = append(s.checkpoints, pos)
			d.w.Trim(d.base + pos>>3)
		}
		n, ok := d.sv7Bits(pos, 20)
		if !ok {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			if err := d.warn(d.base+pos>>3, "the header counts %d frames but the stream ends after %d", s.frames, present); err != nil {
				return err
			}
			break
		}
		if int(n) < minFrameBits || int(n) > maxFrameBits {
			if err := d.warn(d.base+pos>>3, "frame %d declares %d bits, which no frame can hold; the stream ends there", present, n); err != nil {
				return err
			}
			break
		}
		if pos+20+int64(n) > s.endBits {
			if err := d.warn(d.base+pos>>3, "frame %d runs past the end of the stream", present); err != nil {
				return err
			}
			break
		}
		pos += 20 + int64(n)
		present++
	}
	d.total = present

	// The declared length: the header's frame count with the last frame's
	// sample count applied for a true-gapless stream, else the synthesis
	// delay dropped from a whole number of frames.
	last := d.cfg.LastFrameSamples
	if last == 0 {
		last = musepack.FrameLength
	}
	if present == s.frames && s.frames > 0 {
		if tail, ok := d.sv7Bits(pos, 11); ok {
			pos += 11
			inStream := int(tail)
			if inStream == 0 {
				inStream = musepack.FrameLength
			}
			if inStream > musepack.FrameLength {
				if err := d.warn(d.base+pos>>3, "the stream states %d samples in its last frame, more than a frame holds; the header's %d stands", inStream, last); err != nil {
					return err
				}
			} else if inStream != last {
				// The reference's output follows the stream's field, not the
				// header's, so the stream's is what the length below uses.
				if err := d.warn(d.base+pos>>3, "the header says the last frame holds %d samples, the stream says %d", last, inStream); err != nil {
					return err
				}
				last = inStream
			}
		} else if d.w.Err() != nil {
			return d.w.Err()
		} else if err := d.warn(d.base+pos>>3, "the stream ends before its last-frame sample count"); err != nil {
			return err
		}
		if d.cfg.TrueGapless && last > decayThreshold {
			n, ok := d.sv7Bits(pos, 20)
			switch {
			case !ok || int(n) < minFrameBits || int(n) > maxFrameBits || pos+20+int64(n) > s.endBits:
				if d.w.Err() != nil {
					return d.w.Err()
				}
				if err := d.warn(d.base+pos>>3, "the last frame's %d samples need a decay frame the stream does not hold", last); err != nil {
					return err
				}
			default:
				if present%checkpointFrames == 0 {
					s.checkpoints = append(s.checkpoints, pos)
				}
				d.total = present + 1
			}
		}
	}

	declared := int64(0)
	switch {
	case s.frames == 0:
	case d.cfg.TrueGapless:
		declared = (s.frames-1)*musepack.FrameLength + int64(last)
	default:
		declared = s.frames*musepack.FrameLength - musepack.SynthDelay
	}
	deliverable := max(d.total*musepack.FrameLength-musepack.SynthDelay, 0)
	samples := declared
	if deliverable < declared {
		if err := d.warn(d.w.DataEnd(), "the header declares %d samples but the frames present deliver %d", declared, deliverable); err != nil {
			return err
		}
		samples = deliverable
	}
	d.track.Samples = samples
	d.track.Delay = musepack.SynthDelay
	// The count is a header claim the trim needs: the decay frame is decoded
	// whole and the trailing samples past the count are the filter's flush.
	d.track.SamplesExact = true
	return nil
}

// sv7Bits reads n <= 32 bits at bit position pos of the word-swapped stream,
// or false when the read would leave the stream's whole-word extent or the
// source fails (the window's sticky error says which).
func (d *Demuxer) sv7Bits(pos int64, n int) (uint32, bool) {
	if pos < 0 || pos+int64(n) > d.sv7.endBits {
		return 0, false
	}
	m0 := pos >> 3
	w0 := m0 &^ 3
	wEnd := (m0+4)&^3 + 4
	buf := d.w.BytesAt(d.base+w0, int(wEnd-w0))
	if buf == nil && d.w.Err() != nil {
		return 0, false
	}
	var acc uint64
	for j := int64(0); j < 5; j++ {
		acc <<= 8
		if i := int((m0 + j - w0) ^ 3); i < len(buf) {
			acc |= uint64(buf[i])
		}
	}
	return uint32(acc << (24 + pos&7) >> (64 - n)), true
}

// sv7Realign copies bitLen bits from bit position pos of the word-swapped
// stream into a byte-aligned run, pad bits zero, and returns it appended to
// dst.
func (d *Demuxer) sv7Realign(dst []byte, pos int64, bitLen int) ([]byte, bool) {
	if pos < 0 || pos+int64(bitLen) > d.sv7.endBits {
		return dst, false
	}
	m0 := pos >> 3
	shift := uint(pos & 7)
	count := (bitLen + 7) / 8
	w0 := m0 &^ 3
	wEnd := (m0+int64(count))&^3 + 4
	buf := d.w.BytesAt(d.base+w0, int(wEnd-w0))
	if buf == nil && d.w.Err() != nil {
		return dst, false
	}
	at := len(dst)
	dst = append(dst, make([]byte, count)...)
	lb := func(m int64) byte {
		if i := int((m - w0) ^ 3); i < len(buf) {
			return buf[i]
		}
		return 0
	}
	for j := 0; j < count; j++ {
		m := m0 + int64(j)
		b := lb(m) << shift
		if shift != 0 {
			b |= lb(m+1) >> (8 - shift)
		}
		dst[at+j] = b
	}
	if pad := count*8 - bitLen; pad > 0 {
		dst[at+count-1] &^= 1<<uint(pad) - 1
	}
	return dst, true
}

// sv7Frame returns the bit position and length of frame i's data, hopping
// from the frame after the last one looked up when that is i, else from the
// nearest checkpoint. The 11-bit tail field between the header's last frame
// and the decay frame is part of the hop.
func (d *Demuxer) sv7Frame(i int64) (pos int64, bitLen int, err error) {
	s := &d.sv7
	j := i / checkpointFrames * checkpointFrames
	pos = s.checkpoints[i/checkpointFrames]
	if i == s.cursor {
		j, pos = i, s.cursorPos
	}
	for ; ; j++ {
		n, ok := d.sv7Bits(pos, 20)
		if !ok {
			if d.w.Err() != nil {
				return 0, 0, d.w.Err()
			}
			return 0, 0, malformed("frame %d is no longer readable", j)
		}
		next := pos + 20 + int64(n)
		if j == s.frames-1 {
			next += 11
		}
		if j == i {
			s.cursor, s.cursorPos = i+1, next
			return pos + 20, int(n), nil
		}
		pos = next
	}
}

// sv7Packet builds the packet for frame i.
func (d *Demuxer) sv7Packet(i int64, pkt *container.Packet) error {
	pos, bitLen, err := d.sv7Frame(i)
	if err != nil {
		return err
	}
	d.w.Trim(d.base + pos>>3)
	head := d.packetHeader(1, bitLen)
	var ok bool
	if d.pkt, ok = d.sv7Realign(d.pkt[:head], pos, bitLen); !ok {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("frame %d runs past the end of the stream", i)
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: d.pkt,
			PTS:  i * musepack.FrameLength,
			Dur:  musepack.FrameLength,
			// No SV7 frame is a sync point on its own; the first is, and so
			// is one carrying the state the frames before it left behind.
			Sync: i == 0 || d.pending != nil,
		},
	}
	return nil
}

// sv7Advance scans frames [from, to) on top of st.
func (d *Demuxer) sv7Advance(st *musepack.State, from, to int64) error {
	s := &d.sv7
	for i := from; i < to; i++ {
		pos, bitLen, err := d.sv7Frame(i)
		if err != nil {
			return err
		}
		var ok bool
		if s.scratch, ok = d.sv7Realign(s.scratch[:0], pos, bitLen); !ok {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			return malformed("frame %d runs past the end of the stream", i)
		}
		if err := musepack.ScanSV7Frame(s.scratch, bitLen, d.cfg, st); err != nil {
			return err
		}
	}
	return nil
}

// sv7StateAt returns the decoder state before frame i, scanning frames from
// the nearest cached checkpoint. The scan is incremental and remembered, and
// each interval is committed whole: a scan that fails partway leaves the cache
// where it was, so the retry does not start from a half-advanced state.
func (d *Demuxer) sv7StateAt(i int64) (musepack.State, error) {
	s := &d.sv7
	if i < s.scanned {
		k := i / checkpointFrames
		s.scanState = s.states[k]
		s.scanned = k * checkpointFrames
	}
	for s.scanned < i {
		next := min(i, (s.scanned/checkpointFrames+1)*checkpointFrames)
		st := s.scanState
		if err := d.sv7Advance(&st, s.scanned, next); err != nil {
			return musepack.State{}, err
		}
		s.scanState, s.scanned = st, next
		if s.scanned%checkpointFrames == 0 && int64(len(s.states)) == s.scanned/checkpointFrames {
			s.states = append(s.states, s.scanState)
			s.grew = true
		}
	}
	return s.scanState, nil
}

// replayGain is the stream's gain and peak information in the reference's
// storage form: a gain is a loudness in Q8.8 dB (replay gain = 64.82 - value/256),
// a peak is 20*log10 of the 16-bit peak in Q8.8. Zero means not set.
type replayGain struct {
	titleGain, titlePeak, albumGain, albumPeak uint16
}

// oldGainRef is the reference level the SV7 header's gains were measured
// against, MPC_OLD_GAIN_REF in the reference.
const oldGainRef = 64.82

// sv7ReplayGain converts the SV7 header's fields, which hold the gain in
// hundredths of a dB and the peak as a 16-bit sample value, into the storage
// form (streaminfo_read_header_sv7's conversion).
func sv7ReplayGain(titleGain, titlePeak, albumGain, albumPeak uint16) replayGain {
	conv := func(g uint16) uint16 {
		if g == 0 {
			return 0
		}
		v := int((oldGainRef-float64(int16(g))/100)*256 + .5)
		if v >= 1<<16 || v < 0 {
			return 0
		}
		return uint16(v)
	}
	peak := func(p uint16) uint16 {
		if p == 0 {
			return 0
		}
		return uint16(math.Log10(float64(p))*20*256 + .5)
	}
	return replayGain{conv(titleGain), peak(titlePeak), conv(albumGain), peak(albumPeak)}
}
